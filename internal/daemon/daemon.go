package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/trust"
)

// Daemon runs the loop (SPEC §8.1) on one repo. It keeps what plan #5's
// assumption 2 lists, in memory only: a snapshot of each open issue, the
// since= cursors, and, in its client, the ETag cache. Its first Iterate
// reads them all from GitHub.
type Daemon struct {
	// Step runs step 1. Iterate replaces its Trust with one loaded from main.
	// Its Commands and Reactions are the command and reaction handlers.
	Step *Step
	// Clone is the bare clone that config and trust are read from.
	Clone *repo.Repo
	// Config is the last config that loaded from main.
	Config *config.Config
	// Work, if set, gets each issue whose step 1 succeeded on the snapshot
	// held, by number, with the Inputs it returned: stage work (Runs.Work).
	Work func(ctx context.Context, ready []Ready) error
	// Digest, if set, keeps the digest issue: Iterate creates it, pins it,
	// and handles its close (digest.go). Set or not, every digest is kept
	// out of the pipeline.
	Digest bool

	digest        digestState
	snaps         map[int]*github.Snapshot
	inputs        map[int][]github.Comment // only for snapshots step 1 succeeded on
	openErrs      map[int]bool             // issues with an open state error after their last step 1
	stale         map[int]bool             // issues to read again
	missing       map[int]bool             // stale issues found gone since their last good read
	issuesSince   time.Time
	commentsSince time.Time
	polled        time.Time // when the last poll began
	relabel       bool      // a reload added models whose labels aren't created yet
}

// CurrentTrust is a github.Trust that asks st.Trust on each call. Iterate's
// reloads replace st.Trust, so a trigger built once, such as
// ApproveByReaction's, takes this to keep up with CODEOWNERS on main.
func (st *Step) CurrentTrust() github.Trust { return currentTrust{st} }

type currentTrust struct{ st *Step }

func (c currentTrust) Trusted(ctx context.Context, login string) (bool, error) {
	ok, err := c.st.Trust.Trusted(ctx, login)
	if err != nil {
		return false, fmt.Errorf("current trust: %w", err)
	}
	return ok, nil
}

// CurrentModels returns d.Config's models.allowed as of the call, or none
// before a config loads. Iterate's reloads replace d.Config, so a handler
// built once, such as SettingCommands', takes this method to keep up with
// the config on main.
func (d *Daemon) CurrentModels() config.Models {
	if d.Config == nil {
		return config.Models{}
	}
	return d.Config.Models
}

// Ready is an issue whose step 1 succeeded on S, with its Inputs.
type Ready struct {
	S      *github.Snapshot
	Inputs []github.Comment
}

// Run iterates until ctx is done, sleeping loop.idle_sleep after each
// iteration, and logs each iteration's errors.
func (d *Daemon) Run(ctx context.Context) {
	for {
		if err := d.Iterate(ctx); err != nil && ctx.Err() == nil {
			logr.Errorf("loop iteration on %s/%s: %v", d.Step.Owner, d.Step.Repo, err)
		}
		t := time.NewTimer(d.Config.Loop.IdleSleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Iterate runs one iteration, all but the sleep. It syncs the clone and
// reloads config and trust, polls for changes, reads changed issues again,
// then runs step 1 on them and on every issue at spec-review, whose 👍s
// change no updated_at (plan #5, assumption 7), on every issue with
// triage:ask, a flag waiting on a 👍 (FlagByReaction), and on every issue
// that had an open state error after its last step 1, waiting on a 👍 too
// (ErrorByReaction). Then it keeps the digest (digestPass). Last it hands Work the
// issues ready for it. It returns the errors of the issues it couldn't read
// or run step 1 on, which the next Iterate reads again. An issue whose
// labels CheckLabels holds back (ErrLabelsHeld) is no error: like a touched
// issue, it gets nothing more this pass, and is read again on the next.
func (d *Daemon) Iterate(ctx context.Context) error {
	if d.snaps == nil {
		d.snaps, d.inputs, d.stale = map[int]*github.Snapshot{}, map[int][]github.Comment{}, map[int]bool{}
		d.missing = map[int]bool{}
	}
	if d.openErrs == nil { // reread may have made the others
		d.openErrs = map[int]bool{}
	}
	d.reload(ctx)
	if d.Step.Trust == nil {
		return errors.New("no trust has loaded from main yet")
	}
	d.polled = time.Now()
	if err := d.poll(ctx); err != nil {
		return fmt.Errorf("poll %s/%s: %w", d.Step.Owner, d.Step.Repo, err)
	}
	// A label left out, which a rename changes no updated_at for, is
	// removed once it falls due: read its issue again then, and no sooner.
	now := d.Step.Client.Now()
	for n := range d.snaps {
		if d.Step.unexplainedDue(n, now) {
			d.stale[n] = true
		}
	}
	var errs []error
	read := map[int]bool{}
	for _, n := range slices.Sorted(maps.Keys(d.stale)) {
		if d.digest.has(n) { // never in the pipeline, as when a command touched it
			delete(d.stale, n)
			continue
		}
		delete(d.snaps, n)
		delete(d.inputs, n)
		s, err := d.Step.Client.Snapshot(ctx, d.Step.Owner, d.Step.Repo, n)
		switch {
		case gone(err) && d.missing[n]:
			// A failed read between the two shows nothing, so it doesn't count.
			logr.Infof("%s/%s#%d is gone, deleted or transferred, on two reads with no good one between, so it's dropped: %v", d.Step.Owner, d.Step.Repo, n, err)
			delete(d.stale, n)
			delete(d.missing, n)
			delete(d.openErrs, n)
			d.Step.forgetLabels(n)
		case gone(err):
			// One answer may be wrong, so it's read again next pass.
			logr.Infof("%s/%s#%d looks gone, deleted or transferred, so it's read again: %v", d.Step.Owner, d.Step.Repo, n, err)
			d.missing[n] = true
		case err != nil:
			errs = append(errs, err)
		default:
			d.snaps[n], read[n] = s, true
			delete(d.stale, n)
			delete(d.missing, n)
		}
	}
	// A command may write to other issues, such as a /hold's descendants,
	// whose snapshots are then out of date: those skip step 1 and Work, and
	// are read again next pass.
	touched := map[int]bool{}
	d.Step.Touched = func(n int) { touched[n] = true }
	defer func() { d.Step.Touched = nil }()
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if touched[n] || (!read[n] && !slices.ContainsFunc(s.Labels, isSpecReview) && !slices.ContainsFunc(s.Labels, flagAsked) && !d.openErrs[n]) {
			continue
		}
		// ReviewVisit, on an issue read again too, posts a revision for a body
		// edit; it does nothing at any other stage than spec-review. The
		// visit's Dirty calls ask trust about each editor once (#174).
		d.Step.trustOnceBegin()
		in, err := d.Step.Run(ctx, s)
		if err == nil {
			err = d.Step.ReviewVisit(ctx, s)
		}
		if err == nil {
			err = d.Step.IntakeVisit(ctx, s)
		}
		if err == nil {
			err = d.Step.FlagVisit(ctx, s)
		}
		if err == nil {
			d.openErrs[n], err = d.Step.openErrAny(ctx, s, in)
		}
		d.Step.trustOnceEnd()
		if errors.Is(err, ErrLabelsHeld) {
			// Like a touched issue: nothing more this pass, and read again.
			logr.Infof("%s/%s#%d waits for its next pass: %v", d.Step.Owner, d.Step.Repo, n, err)
			touched[n] = true
			continue
		}
		if err != nil {
			errs = append(errs, err)
			delete(d.inputs, n)
			d.stale[n] = true
			continue
		}
		d.inputs[n] = in
	}
	for n := range touched {
		delete(d.inputs, n)
		d.stale[n] = true
	}
	if err := d.digestPass(ctx); err != nil {
		errs = append(errs, err)
	}
	if d.Work != nil {
		var ready []Ready
		for _, n := range slices.Sorted(maps.Keys(d.inputs)) {
			ready = append(ready, Ready{S: d.snaps[n], Inputs: d.inputs[n]})
		}
		if err := d.Work(ctx, ready); err != nil {
			errs = append(errs, fmt.Errorf("stage work: %w", err))
		}
	}
	return errors.Join(errs...)
}

// reread makes the next Iterate read issue n again, and gives Work none of
// its snapshot until then. gone counts a read that found it gone, deleted
// or transferred, as one of the two that drop it.
func (d *Daemon) reread(n int, gone bool) {
	if d.stale == nil {
		d.snaps, d.inputs, d.stale, d.missing = map[int]*github.Snapshot{}, map[int][]github.Comment{}, map[int]bool{}, map[int]bool{}
	}
	delete(d.inputs, n)
	d.stale[n] = true
	if gone {
		d.missing[n] = true
	}
}

// reload syncs the clone, then loads config and trust from main. A failure
// keeps the last good ones, and is logged. The Applier's IssueTypes follows
// the config's types. A config that allows a new model
// gets its labels created, until that succeeds.
func (d *Daemon) reload(ctx context.Context) {
	a := d.Step.Applier
	token, err := a.Client.Token(ctx)
	if err == nil {
		err = d.Clone.Sync(ctx, token)
	}
	if err != nil {
		logr.Errorf("syncing the clone of %s/%s failed, so the last good config and trust stay: %v", a.Owner, a.Repo, err)
		return
	}
	if cfg, err := config.Load(ctx, d.Clone); err != nil {
		logr.Errorf("loading the config of %s/%s from main failed, so the last good one stays: %v", a.Owner, a.Repo, err)
	} else {
		d.relabel = d.relabel || newModels(d.Config, cfg)
		d.Config = cfg
	}
	if d.Config != nil {
		a.IssueTypes = d.Config.Types == "issue-types"
	}
	if d.relabel {
		d.relabel = !d.ensureLabels(ctx)
	}
	if tr, err := trust.Load(ctx, d.Clone, a.Client.Client, a.Owner, a.Repo); err != nil {
		logr.Errorf("loading the trust of %s/%s from main failed, so the last good one stays: %v", a.Owner, a.Repo, err)
	} else {
		d.Step.Trust = tr
	}
}

// poll lists the issues, then the comments, updated since the cursors, and
// marks as stale each open issue that changed. Each cursor moves to the
// newest updated_at GitHub returned. A list whose cursor moved is read once
// more from there, so the next poll of an unchanged repo repeats that
// request and gets a 304. Closed issues and pull requests are dropped, and
// so are digests, which it records, with the App's digest-new comments
// (digest.go).
func (d *Daemon) poll(ctx context.Context) error {
	a := d.Step.Applier
	for range 2 {
		since := d.issuesSince
		issues, err := a.Client.IssuesSince(ctx, a.Owner, a.Repo, since)
		if err != nil {
			return fmt.Errorf("poll issues: %w", err)
		}
		for _, is := range issues {
			d.issuesSince = latest(d.issuesSince, is.GetUpdatedAt().Time)
			n := is.GetNumber()
			switch {
			case is.IsPullRequest():
			case d.digest.listed(a.AppSlug, is) || is.GetState() != "open": // a digest is never in the pipeline
				delete(d.snaps, n)
				delete(d.inputs, n)
				delete(d.stale, n)
				delete(d.missing, n) // so a reopened issue starts with no strike
				delete(d.openErrs, n)
				a.forgetLabels(n)
			case changed(d.snaps[n], is):
				d.stale[n] = true
			}
		}
		if d.issuesSince.Equal(since) {
			break
		}
	}
	for range 2 {
		since := d.commentsSince
		comments, err := a.Client.CommentsSince(ctx, a.Owner, a.Repo, 0, since)
		if err != nil {
			return fmt.Errorf("poll comments: %w", err)
		}
		for _, c := range comments {
			d.commentsSince = latest(d.commentsSince, c.GetUpdatedAt().Time)
			n, err := strconv.Atoi(path.Base(c.GetIssueURL()))
			if err != nil {
				logr.Warnf("%s/%s: skipping comment %d, whose issue URL %q names no number", a.Owner, a.Repo, c.GetID(), c.GetIssueURL())
				continue
			}
			d.digest.comment(a.AppSlug, n, c)
			if s := d.snaps[n]; s != nil && commentChanged(s, c) {
				d.stale[n] = true
			}
		}
		if d.commentsSince.Equal(since) {
			break
		}
	}
	return nil
}

// changed reports whether the listed issue is newer than s, or s is nil.
// GitHub's times are to the second, so at the same time it compares the
// title, body, type, labels and, if listed, the parent too.
func changed(s *github.Snapshot, is *gogithub.Issue) bool {
	if s == nil {
		return true
	}
	at := is.GetUpdatedAt().Time
	if !at.Equal(s.UpdatedAt) {
		return at.After(s.UpdatedAt)
	}
	// A label left out of s is no change: its issue is read again once it's
	// due (Applier.unexplainedDue), nor is the type it would give.
	var labels, names []string
	for _, l := range is.Labels {
		if !s.LeftOut[strings.ToLower(l.GetName())] {
			labels, names = append(labels, strings.ToLower(l.GetName())), append(names, l.GetName())
		}
	}
	typ := strings.ToLower(is.GetType().GetName())
	if typ == "" {
		typ = github.TypeOfLabels(names)
	}
	if is.GetTitle() != s.Title || typ != s.Type {
		return true
	}
	if is.ParentIssueURL != nil && parentOf(is.GetParentIssueURL()) != s.Parent {
		return true
	}
	held := make([]string, 0, len(s.Labels))
	for _, l := range s.Labels {
		held = append(held, strings.ToLower(l))
	}
	slices.Sort(labels)
	slices.Sort(held)
	return is.GetBody() != s.Body || !slices.Equal(labels, held)
}

// commentChanged reports whether the listed comment is new to s, or newer
// than s has it, comparing the body at the same time as changed does.
func commentChanged(s *github.Snapshot, c *gogithub.IssueComment) bool {
	i := slices.IndexFunc(s.Comments, func(x github.Comment) bool { return x.ID == c.GetID() })
	if i < 0 {
		return true
	}
	at, held := c.GetUpdatedAt().Time, s.Comments[i]
	return at.After(held.UpdatedAt) || at.Equal(held.UpdatedAt) && c.GetBody() != held.Body
}

// parentOf is the number a parent_issue_url ends in, or 0 for none.
func parentOf(u string) int {
	n, err := strconv.Atoi(path.Base(u))
	if err != nil {
		return 0
	}
	return n
}

// gone reports whether err, from Snapshot, says the issue was deleted (404
// or 410) or transferred (a 301). A failed visit, which may 404 on a deleted
// comment, marks the issue stale, so only Snapshot decides, and Iterate
// drops an issue only when two reads say so with no good read between.
func gone(err error) bool {
	var e *gogithub.ErrorResponse
	if errors.As(err, &e) && e.Response != nil {
		return e.Response.StatusCode == http.StatusNotFound || e.Response.StatusCode == http.StatusGone
	}
	var r *gogithub.RedirectionError
	return errors.Is(err, github.ErrTransferred) || errors.As(err, &r) && r.StatusCode == http.StatusMovedPermanently
}

func isSpecReview(label string) bool {
	stage, ok := state.ParseLabel(label)
	return ok && stage == state.SpecReview
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

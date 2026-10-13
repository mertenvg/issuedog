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
	// held, by number, with the Inputs it returned: stage work (T11).
	Work func(ctx context.Context, ready []Ready) error

	snaps         map[int]*github.Snapshot
	inputs        map[int][]github.Comment // only for snapshots step 1 succeeded on
	stale         map[int]bool             // issues to read again
	missing       map[int]bool             // stale issues found gone since their last good read
	issuesSince   time.Time
	commentsSince time.Time
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
// change no updated_at (plan #5, assumption 7). Last it hands Work the
// issues ready for it. It returns the errors of the issues it couldn't read
// or run step 1 on, which the next Iterate reads again.
func (d *Daemon) Iterate(ctx context.Context) error {
	if d.snaps == nil {
		d.snaps, d.inputs, d.stale = map[int]*github.Snapshot{}, map[int][]github.Comment{}, map[int]bool{}
		d.missing = map[int]bool{}
	}
	d.reload(ctx)
	if d.Step.Trust == nil {
		return errors.New("no trust has loaded from main yet")
	}
	if err := d.poll(ctx); err != nil {
		return fmt.Errorf("poll %s/%s: %w", d.Step.Owner, d.Step.Repo, err)
	}
	var errs []error
	read := map[int]bool{}
	for _, n := range slices.Sorted(maps.Keys(d.stale)) {
		delete(d.snaps, n)
		delete(d.inputs, n)
		s, err := d.Step.Client.Snapshot(ctx, d.Step.Owner, d.Step.Repo, n)
		switch {
		case gone(err) && d.missing[n]:
			// A failed read between the two shows nothing, so it doesn't count.
			logr.Infof("%s/%s#%d is gone, deleted or transferred, on two reads with no good one between, so it's dropped: %v", d.Step.Owner, d.Step.Repo, n, err)
			delete(d.stale, n)
			delete(d.missing, n)
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
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if !read[n] && !slices.ContainsFunc(s.Labels, isSpecReview) {
			continue
		}
		in, err := d.Step.Run(ctx, s)
		if err != nil {
			errs = append(errs, err)
			delete(d.inputs, n)
			d.stale[n] = true
			continue
		}
		d.inputs[n] = in
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

// reload syncs the clone, then loads config and trust from main. A failure
// keeps the last good ones, and is logged.
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
		d.Config = cfg
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
// request and gets a 304. Closed issues and pull requests are dropped.
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
			case is.GetState() != "open":
				delete(d.snaps, n)
				delete(d.inputs, n)
				delete(d.stale, n)
				delete(d.missing, n) // so a reopened issue starts with no strike
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
	if is.GetTitle() != s.Title || github.TypeOf(is) != s.Type {
		return true
	}
	if is.ParentIssueURL != nil && parentOf(is.GetParentIssueURL()) != s.Parent {
		return true
	}
	var labels []string
	for _, l := range is.Labels {
		labels = append(labels, strings.ToLower(l.GetName()))
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

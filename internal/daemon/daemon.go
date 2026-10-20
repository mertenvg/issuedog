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
	// handles its close, lists what waits on the owner in it and announces
	// new items (digest.go). Set or not, every digest is kept out of the
	// pipeline.
	Digest bool
	// Assign, if set, keeps each pipeline issue's assignees: the notify
	// users while something on it waits on the owner, and nobody otherwise
	// (assign.go). Set or not, what waits is listed for every issue.
	Assign bool

	digest        digestState
	digestCache   map[int][]waitItem // what waits on the owner, by issue, after its last good step 1, for the digest and assignment
	assignManaged map[int]bool       // issues whose assignees assignment keeps, as of their last pass (assign.go)
	assignDropped map[string]bool    // logins, in lower case, GitHub wouldn't assign (assignDrop)
	assignFor     string             // the notify list, in lower case, that assignDropped was found with
	snaps         map[int]*github.Snapshot
	inputs        map[int][]github.Comment // only for snapshots step 1 succeeded on
	openErrs      map[int]bool             // issues with an open state error after their last step 1
	stale         map[int]bool             // issues to read again
	missing       map[int]bool             // stale issues found gone since their last good read
	issuesSince   time.Time
	commentsSince time.Time
	polled        time.Time // when the last poll began
	relabel       bool      // a reload added models whose labels aren't created yet

	closingSeen map[int]closingListed   // closed issues whose close is judged, as they were listed then (closing.go)
	closingDue  map[int]*gogithub.Issue // closed issues with a close to judge, as last listed (closingList)
	// closedListed are the issues the poll last listed closed, never a
	// pull request, for a finished story's children (digestDoneStands).
	closedListed map[int]bool
}

// CurrentTrust is a github.Trust that asks st.Trust on each call. Iterate's
// reloads replace st.Trust, so a trigger built once, such as
// ApproveByReaction's, takes this to keep up with CODEOWNERS on main.
// During one of Iterate's visits it asks the visit's trust
// (Step.trustOnceTrust), over st.Trust as it is then, so a held issue's
// reaction triggers, such as ResolveByReaction's, which look for its
// triggers again, ask nobody step 1 asked already (#192's verify note).
func (st *Step) CurrentTrust() github.Trust { return currentTrust{st} }

type currentTrust struct{ st *Step }

func (c currentTrust) Trusted(ctx context.Context, login string) (bool, error) {
	ok, err := c.st.trustOnceTrust().Trusted(ctx, login)
	if err != nil {
		return false, fmt.Errorf("current trust: %w", err)
	}
	return ok, nil
}

// DiscAccess asks st.Trust's DiscAccess (github.DiscTrust), so a trigger
// built once judges write collaborators by CODEOWNERS on main too. A Trust
// without it knows no write collaborator who isn't trusted.
func (c currentTrust) DiscAccess(ctx context.Context, login string) (trusted, writer bool, err error) {
	dt, ok := c.st.trustOnceTrust().(github.DiscTrust)
	if !ok {
		if trusted, err = c.Trusted(ctx, login); err != nil {
			return false, false, fmt.Errorf("current access: %w", err)
		}
		return trusted, false, nil
	}
	if trusted, writer, err = dt.DiscAccess(ctx, login); err != nil {
		return false, false, fmt.Errorf("current access: %w", err)
	}
	return trusted, writer, nil
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
// reloads config and trust, polls for changes, judges the closes still to
// judge (closingPass), reads changed issues again, those it reopened
// included, then runs step 1 on them and on every issue at spec-review or
// plan-review, whose 👍s change no updated_at (plan #5, assumption 7), on every issue with
// triage:ask, a flag waiting on a 👍 (FlagByReaction), and on every issue
// that had an open state error after its last step 1, waiting on a 👍 too
// (ErrorByReaction), on every held issue, for its discussion
// triggers (discHeldLabel), and on every issue at needs-human, for a human
// task's blockers, whose close changes no updated_at of its own
// (humanBlocked, #287), and for the 👍s on a human task itself, which
// change none either (DoneByReaction, #286). Each visit ends with
// ChildrenVisit, which creates the children of a plan approved on the
// issue (children.go); the children it writes to are touched. On each
// issue step 1 succeeds on, it caches what waits on the owner
// (digestCacheSet), with the issue's dirty content, which
// openErrAny takes too; an issue whose read or step 1 fails keeps its last
// items, and one that closes or leaves the listing loses them. Then it
// keeps the digest (digestPass), and then the assignees (assignPass), whose
// failures stop nothing. Last it hands Work the issues ready for it. It returns the errors of the issues it couldn't read
// or run step 1 on, which the next Iterate reads again. An issue whose
// labels CheckLabels holds back (ErrLabelsHeld), or that it closed again
// (closingErrClosedAgain), is no error: like a touched issue, it gets
// nothing more this pass, and is read again on the next.
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
	// A revert of an issue type reads the org's types' names once in the
	// steps below, and only if it needs them; Work's applies read their own.
	d.Step.typeRenamePass(true)
	defer d.Step.typeRenamePass(false)
	d.polled = time.Now()
	if err := d.poll(ctx); err != nil {
		return fmt.Errorf("poll %s/%s: %w", d.Step.Owner, d.Step.Repo, err)
	}
	// Closes still to judge, before the reads: an issue it reopens is read
	// and visited this pass (closing.go).
	var errs []error
	if err := d.closingPass(ctx); err != nil {
		errs = append(errs, err)
	}
	// A label left out, which a rename changes no updated_at for, is
	// removed once it falls due: read its issue again then, and no sooner.
	now := d.Step.Client.Now()
	for n := range d.snaps {
		if d.Step.unexplainedDue(n, now) {
			d.stale[n] = true
		}
	}
	// A login with no permission record is asked about again once its
	// wait is up (discNoRecord): read the issues it was asked for again,
	// so an act it made meanwhile is judged.
	for _, n := range d.Step.discNoRecordDue(now) {
		if _, ok := d.snaps[n]; ok {
			d.stale[n] = true
		}
	}
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
			delete(d.digestCache, n)
			delete(d.assignManaged, n)
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
		if touched[n] || (!read[n] && !slices.ContainsFunc(s.Labels, isReview) && !slices.ContainsFunc(s.Labels, flagAsked) &&
			!slices.ContainsFunc(s.Labels, discHeldLabel) && !slices.ContainsFunc(s.Labels, isNeedsHuman) && !d.openErrs[n]) {
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
		// ChildrenVisit, last, creates the children of a plan the visits
		// above may just have approved, so they land in the same pass. The
		// children it writes to are Touched.
		if err == nil {
			err = d.Step.ChildrenVisit(ctx, s)
		}
		// The open errors and what waits on the owner are read from the
		// snapshot as the visits left it, with one look at its dirty content.
		var dirt Dirt
		var holds []discTrigger
		ownerHeld, blocked := false, false
		if err == nil {
			dirt, err = d.Step.Dirty(ctx, s, d.Step.trustOnceTrust())
		}
		if err == nil {
			d.openErrs[n], err = d.Step.openErrAny(s, in, dirt)
		}
		if err == nil { // a held issue's open triggers, for its hold item (#192)
			holds, ownerHeld, err = d.Step.holdItemFind(ctx, s, dirt)
		}
		if err == nil { // a human task's blockers, for its item (#287)
			blocked, err = d.humanBlocked(ctx, s, dirt)
		}
		d.Step.trustOnceEnd()
		if errors.Is(err, ErrLabelsHeld) || errors.Is(err, closingErrClosedAgain) {
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
		d.digestCacheSet(s, in, dirt, holds, ownerHeld, blocked) // for assignment too, so with the digest off as well
	}
	for n := range touched {
		delete(d.inputs, n)
		d.stale[n] = true
	}
	if err := d.digestPass(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := d.assignPass(ctx); err != nil {
		errs = append(errs, err)
	}
	d.Step.typeRenamePass(false)
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
		d.Step.Notify = d.Config.Notify
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
// (digest.go). One only the digest's title rule would take is judged after
// the comments (digestTake). A closed issue whose close is still to judge
// is kept for closingPass (closingList), and an open one forgets its close.
// Which issues it last listed closed it keeps too (closedListed), adding
// each one listed closed and dropping each one listed open, for a finished
// story's children (#278): a start's first poll lists every issue, and
// each later one every issue that changed, so a close or reopen is never
// missed.
func (d *Daemon) poll(ctx context.Context) error {
	a := d.Step.Applier
	start := d.issuesSince.IsZero() // a start's first poll, which lists every issue
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
			case is.GetState() == "open":
				delete(d.closedListed, n)
			default:
				if d.closedListed == nil {
					d.closedListed = map[int]bool{}
				}
				d.closedListed[n] = true
			}
			switch {
			case is.IsPullRequest():
			case d.digest.listed(a.AppSlug, is) || is.GetState() != "open": // a digest is never in the pipeline
				if is.GetState() != "open" && !d.digest.has(n) {
					// A close still to judge is kept (closing.go).
					d.closingList(is, start, d.snaps[n] != nil && slices.ContainsFunc(d.snaps[n].Labels, isStageLabel))
				}
				d.leave(n)
			case changed(d.snaps[n], is):
				d.closingForget(n)
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
	// Only now that the comments are in, as every one is on a start, can an
	// issue only the digest's title rule would take be judged (digestTake).
	d.digestTake(ctx)
	return nil
}

// leave drops what the daemon holds of issue n, its snapshot and all it
// derived from it, for an issue that left the pipeline: a closed one, or a
// digest, which is never in it.
func (d *Daemon) leave(n int) {
	delete(d.snaps, n)
	delete(d.inputs, n)
	delete(d.stale, n)
	delete(d.missing, n) // so a reopened issue starts with no strike
	delete(d.openErrs, n)
	delete(d.digestCache, n) // a closed issue has nothing waiting
	delete(d.assignManaged, n)
	d.Step.forgetLabels(n)
}

// changed reports whether the listed issue is newer than s, or s is nil.
// GitHub's times are to the second, so at the same time it compares the
// title, body, type, labels, assignees and, if listed, the parent too.
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
	typeChanged := typ != s.Type
	if lo := s.TypeLeftOut; lo != nil {
		// Nor is an issue type left out of s, for the type that stands: its
		// issue is read again once it's due too. A change of the type label
		// an issue with no issue type takes its type from is a label change.
		typeChanged = !github.SameIssueType(is.GetType().GetName(), is.GetType().GetID(), lo.Name, lo.ID)
	}
	if is.GetTitle() != s.Title || typeChanged {
		return true
	}
	if is.ParentIssueURL != nil && parentOf(is.GetParentIssueURL()) != s.Parent {
		return true
	}
	if !assignSame(assignListed(is), s.Assignees) {
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

// humanBlocked reports whether s, as its visit left it with its dirty
// content dirt, is a human task at needs-human (waitHumanTask) that an
// open issue blocks, for its item (waitInput.Blocked, SPEC §9.6, #287). It
// reads the blockers of such a task alone, with a conditional request, so
// an unchanged pass costs a 304, and any other issue costs none. A failed
// read is an error, and Iterate keeps the issue's last items, as for a
// failed step 1.
func (d *Daemon) humanBlocked(ctx context.Context, s *github.Snapshot, dirt Dirt) (bool, error) {
	st := d.Step
	if !waitHumanTask(state.Derive(s, st.AppSlug, dirt.Has)) {
		return false, nil
	}
	blockers, err := st.Client.Blockers(ctx, st.Owner, st.Repo, s.Number)
	if err != nil {
		return false, fmt.Errorf("the blockers of human task %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	return slices.ContainsFunc(blockers, func(b github.Blocker) bool { return !b.Closed }), nil
}

func isNeedsHuman(label string) bool {
	stage, ok := state.ParseLabel(label)
	return ok && stage == state.NeedsHuman
}

// isReview reports whether label is the stage label of spec-review or
// plan-review, where a 👍 on a revision approves it (ApproveByReaction).
func isReview(label string) bool {
	stage, ok := state.ParseLabel(label)
	return ok && (stage == state.SpecReview || stage == state.PlanReview)
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

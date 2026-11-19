package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file opens or updates a passed task's PR to main (SPEC §7.5, §8.1
// step 4, §10.2, §11.5): the head pushed to the task's branch, then the PR,
// found by that branch, then its priority label, then the issuedog/verify
// check on the verified head (SPEC §10.4), and last the task's move to
// pr-open, each checked first. A head that changes nothing on main gets
// none of those, and is handed off at needs-human instead.

// prBase is the branch a task with no parent's PR targets (SPEC §7.5).
const prBase = "main"

// prWant is what the PR step wants for a task: the run of the pass it
// stands on, which keys the task's move out of verifying, and that pass's
// comment; whether the head changes nothing on main, so it wants only the
// hand-off (prNothingMove), and none of the rest; its branch, and the
// branch's tip as the last sync left it, or "" for no branch; whether the
// head is to be pushed there; and the PR's title and body (SPEC §11.5),
// and the task's priority label, which the PR carries.
type prWant struct {
	Run         string
	Pass        int64
	Nothing     bool
	Branch, Tip string
	Push        bool
	Title, Body string
	Label       string
}

// openPR is the PR step for rd's task, whose state is is, once its head
// passed the checks against main's commit main (Runs.passed) and has a
// clean pass of the verifier's (verdictOn): verifyTask calls it in step 4
// (SPEC §8.1), so after a restart, or once main moves, the checks run again
// first. It isn't a stage run: no lease, no claude, no attempt. It reports
// whether it read the issue again, to write, so the pass's stage work
// ends, as the checks' findings move does.
//
// What it wants is read from the snapshot step 1 left and the bare repo
// alone (prWant), at no request: a pass that waits for its follow-ups'
// filed record, two branches with the task's prefix and someone else's
// tip each want nothing, and a head whose own diff is empty wants only its
// hand-off. Otherwise the PR is found by its branch (prFind), with one
// request. Every pass that gets past those waits ends in the task's move
// out of verifying, so the PR list is read only on the pass that writes,
// and the move's pr= names the PR for every pass after it (SPEC §8.1).
//
// Before any write, it reads the issue again, and runs CheckLabels and the
// look for discussion triggers on it, as verifyFindings does (SPEC §8.2). A
// task closed, gone, held, or moved since gets nothing, and what it wants is
// read again from what's there: a move keyed by the pass's run written
// since step 1 read the issue only has its labels finished, by
// CheckLabels, as the App's own move cut short. Then the writes, in
// order, each checked first, so a replay, a restart or a crash between any
// two repeats none and loses none (SPEC §5.2): the push of the head to the
// task's branch (SPEC §3.2), unless it's there already; the PR, opened, or
// reopened and updated, or updated where its title or body differ
// (prWrite); then the priority label on it, unless it has it; then the
// issuedog/verify check on the head, unless the App's newest there is a
// success (prCheck); and last the move to pr-open (prMove), which releases
// the worker (SPEC §8.1). The App never merges the PR. A head that changes
// nothing on main gets none of those writes, but the move to needs-human
// with its hand-off (prNothingMove).
func (r *Runs) openPR(ctx context.Context, rd Ready, is state.Issue, head, main string) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error { return fmt.Errorf("open or update the PR: %w", err) }
	want, ok, err := r.prWant(ctx, rd.S, is, head, main, logr.Debugf)
	if err != nil || !ok {
		if err != nil {
			return false, wrap(err)
		}
		return false, nil
	}
	var pr *github.PullRequest
	if !want.Nothing {
		if pr, err = r.prFind(ctx, want.Branch); err != nil {
			return false, wrap(err)
		}
	}
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, so it gets no PR, and is read again: %v", a.Owner, a.Repo, n, err)
		r.Daemon.reread(n, true)
		return true, nil
	case err != nil:
		return true, wrap(err)
	}
	defer r.Daemon.reread(n, false)
	d, err := a.Dirty(ctx, s, r.Daemon.Step.Trust)
	if err == nil {
		err = a.discCheckLabels(ctx, s, r.Daemon.Step.Trust, d)
	}
	if err != nil {
		return true, wrap(err)
	}
	if s.Closed {
		logr.Infof("%s/%s#%d was closed since its pass, so it gets no PR", a.Owner, a.Repo, n)
		return true, nil
	}
	if _, err := r.Daemon.Step.discussion(ctx, s, d); err != nil {
		return true, wrap(err)
	}
	// Any move since the pass, a hold's or a stage label's record
	// included, is a newer transition marker, so the latest marker's
	// comment tells, as for the findings move. The PR step's own move,
	// written since step 1 read the issue, is one too, and CheckLabels
	// has just finished its labels, as the App's own move cut short.
	now := state.Derive(s, a.AppSlug, d.Has)
	if now.TransitionComment != is.TransitionComment || now.Stage != state.Verifying {
		logr.Infof("%s/%s#%d moved to %s since its pass, so it gets no PR", a.Owner, a.Repo, n, now.Stage)
		return true, nil
	}
	if want, ok, err = r.prWant(ctx, s, now, head, main, logr.Warnf); err != nil || !ok {
		if err != nil {
			return true, wrap(err)
		}
		return true, nil
	}
	// The move's bound is step 1's newest comment, as the findings move's
	// is (verifyFindings), so a comment posted since is above it.
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	if want.Nothing {
		if err := r.prNothingMove(ctx, s, d, want.Run, head, seen); err != nil {
			return true, wrap(err)
		}
		return true, nil
	}
	if want.Push {
		if err := r.prPush(ctx, n, head, want); err != nil {
			if errors.Is(err, repo.ErrPushStale) {
				logr.Infof("%s/%s#%d: its branch %s moved since the sync, so its head %s isn't pushed, and the next pass looks again: %v",
					a.Owner, a.Repo, n, want.Branch, head, err)
				return true, nil
			}
			return true, wrap(err)
		}
		logr.Infof("%s/%s#%d: its head %s is pushed to %s", a.Owner, a.Repo, n, head, want.Branch)
	}
	if pr, err = r.prWrite(ctx, n, want, pr); err != nil {
		return true, wrap(err)
	}
	if !slices.ContainsFunc(pr.Labels, func(l string) bool { return strings.EqualFold(l, want.Label) }) {
		if _, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, pr.Number, []string{want.Label}); err != nil {
			return true, wrap(fmt.Errorf("add %s to PR #%d: %w", want.Label, pr.Number, err))
		}
	}
	logr.Infof("%s/%s#%d: its PR #%d from %s to %s is open at its head %s", a.Owner, a.Repo, n, pr.Number, want.Branch, prBase, head)
	if err := r.prCheck(ctx, n, head, main, waitLink(a.Owner, a.Repo, n, want.Pass)); err != nil {
		return true, wrap(err)
	}
	if err := r.prMove(ctx, s, d, want.Run, head, pr.Number, seen); err != nil {
		return true, wrap(err)
	}
	return true, nil
}

// prWant reads what the PR step wants for task s, whose state is is, at
// head, which passed the checks against main's commit main, from s and the
// bare repo alone, so at no request. It reports false, with no error, when
// the task waits, each logged:
//   - is has no clean pass of the verifier's on head in a comment, as after
//     a read again that finds the pass comment edited;
//   - the move keyed by the pass's run is written already, and a later move
//     put the task back at verifying, as a stage set by hand: that key
//     moves it once (SPEC §5.2), so the pass moves it no more;
//   - the pass has candidates, and no clean filed record of its run stands
//     (state.FiledRecord, SPEC §10.2), or its candidates don't read back,
//     which step 1 reports as a followups state error and never files;
//   - more than one branch has the task's prefix (taskBranch, SPEC §7.5);
//   - the branch's tip isn't one the head may go over (prPushOK).
//
// A head whose own diff, from its merge base with main, is empty wants
// only its hand-off (prWant.Nothing), with no push and no PR, so no branch
// is named for it.
//
// The second and the last two waits are warnings, logged once
// (prWarnOnce). Text of the PR's that doesn't read back is logged with
// warnf (prText): a warning before a write, and else at debug, so it isn't
// warned of on every pass.
func (r *Runs) prWant(ctx context.Context, s *github.Snapshot, is state.Issue, head, main string, warnf func(string, ...any) string) (prWant, bool, error) {
	a, n, clone := r.Daemon.Step.Applier, s.Number, r.Daemon.Clone
	i := passOn(is, head)
	if i < 0 {
		logr.Debugf("%s/%s#%d has no clean pass on its head %s, so it gets no PR", a.Owner, a.Repo, n, head)
		return prWant{}, false, nil
	}
	pass, ok := markerCommentOf(s, is, i)
	if !ok {
		logr.Debugf("%s/%s#%d: its pass on %s is in no comment, so it gets no PR", a.Owner, a.Repo, n, head)
		return prWant{}, false, nil
	}
	run := is.Markers[i].Fields["run"]
	if is.Moved(state.Key{Field: "run", ID: run}) {
		r.prWarnOnce(n, "moved:"+run, "%s/%s#%d: its pass, run %s, moved it out of %s once already, and it's back there since, as by a stage set by hand, so the pass moves it no more: it waits at %s",
			a.Owner, a.Repo, n, run, state.Verifying, state.Verifying)
		return prWant{}, false, nil
	}
	fs, _, err := readVerifyData(pass, a.AppSlug)
	if err != nil {
		logr.Debugf("%s/%s#%d: the candidates of pass comment %d don't read back, so it gets no PR: %v", a.Owner, a.Repo, n, pass.ID, err)
		return prWant{}, false, nil
	}
	if _, filed := state.FiledRecord(is, run); len(fs) > 0 && !filed {
		logr.Debugf("%s/%s#%d: the follow-ups of pass comment %d have no filed record, so it waits at %s for them", a.Owner, a.Repo, n, pass.ID, state.Verifying)
		return prWant{}, false, nil
	}
	base, err := verify.BaseOf(ctx, verify.Facts{Repo: clone, Head: head, Target: main})
	if err != nil {
		return prWant{}, false, fmt.Errorf("read the head's base: %w", err)
	}
	changes, err := clone.Changes(ctx, base, head, false)
	if err != nil {
		return prWant{}, false, fmt.Errorf("read the head's own diff: %w", err)
	}
	if len(changes) == 0 {
		logr.Debugf("%s/%s#%d: its head %s changes nothing on %s since %s, so it gets no push and no PR, but a hand-off", a.Owner, a.Repo, n, head, prBase, base)
		return prWant{Run: run, Pass: pass.ID, Nothing: true}, true, nil
	}
	branches, err := clone.Branches(ctx, taskBranchPrefix(n))
	if err != nil {
		return prWant{}, false, fmt.Errorf("list the task's branches: %w", err)
	}
	branch, tip, err := taskBranch(n, s.Title, branches)
	switch {
	case errors.Is(err, errTwoBranches):
		r.prWarnOnce(n, err.Error(), "%s/%s#%d gets no push and no PR: %v", a.Owner, a.Repo, n, err)
		return prWant{}, false, nil
	case err != nil:
		return prWant{}, false, fmt.Errorf("name the task's branch: %w", err)
	}
	push := tip != head
	if push {
		ok, err := r.prPushOK(ctx, is, tip, head)
		if err != nil {
			return prWant{}, false, fmt.Errorf("judge the branch's tip: %w", err)
		}
		if !ok {
			r.prWarnOnce(n, "tip:"+tip, "%s/%s#%d: its branch %s is at %s, which isn't in its head %s's history, and no App marker names it as a head, so it's someone else's: nothing is pushed, and the task waits at %s",
				a.Owner, a.Repo, n, branch, tip, head, state.Verifying)
			return prWant{}, false, nil
		}
	}
	// A wait that ended is warned of again if it comes back.
	delete(r.prWarned, n)
	title, body := r.prText(s, is, pass, head, main, warnf)
	return prWant{Run: run, Pass: pass.ID, Branch: branch, Tip: tip, Push: push, Title: title, Body: body,
		Label: fmt.Sprintf("priority:p%d", priority(s))}, true, nil
}

// passOn is the index among is's markers of its latest clean pass of the
// verifier's on head, as verdictOn finds one, or -1.
func passOn(is state.Issue, head string) int {
	found := -1
	for i, m := range is.Markers {
		if m.Kind == verifyKind && m.Fields["verdict"] == verdictPass && head != "" && m.Fields["head"] == head {
			found = i
		}
	}
	return found
}

// prWarnOnce logs a warning for task n, unless the last one logged for it
// had the same key, so a wait that lasts is logged once, not every pass,
// and a new cause is logged again.
func (r *Runs) prWarnOnce(n int, key, format string, args ...any) {
	if r.prWarned[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if r.prWarned == nil {
		r.prWarned = map[int]string{}
	}
	r.prWarned[n] = key
	logr.Warnf(format, args...)
}

// prPushOK reports whether the head may be pushed over tip, the task's
// branch's tip as the last sync left it (SPEC §3.4, §7.5): when there's no
// branch, when tip is in head's history, a fast-forward, or when a clean
// App marker on the task, is, names tip as a head, as when a head that
// failed rule 2 was dropped, which the App's bypass of the issuedog
// branches ruleset lets it force over (SPEC §15). Any other tip is someone
// else's commits.
func (r *Runs) prPushOK(ctx context.Context, is state.Issue, tip, head string) (bool, error) {
	if tip == "" {
		return true, nil
	}
	ok, err := r.Daemon.Clone.IsAncestor(ctx, tip, head)
	if err != nil {
		return false, fmt.Errorf("read the branch's tip against the head: %w", err)
	}
	return ok || slices.ContainsFunc(is.Markers, func(m github.Marker) bool { return m.Fields["head"] == tip }), nil
}

// prPush pushes head to want's branch, leased at its tip (repo.Repo.Push),
// with the App's installation token, as the sync fetches.
func (r *Runs) prPush(ctx context.Context, n int, head string, want prWant) error {
	token, err := r.Daemon.Step.Client.Token(ctx)
	if err != nil {
		return fmt.Errorf("push #%d's head: %w", n, err)
	}
	if err := r.Daemon.Clone.Push(ctx, token, head, want.Branch, want.Tip); err != nil {
		return fmt.Errorf("push #%d's head: %w", n, err)
	}
	return nil
}

// prText is the PR's title and body (SPEC §11.5) for task s, whose state
// is is, from the pr-text marker on its latest move to verifying, the
// verify-notes marker on pass, its pass comment on head, and what the
// checks of head against main gave (Runs.passed). Text that doesn't read
// back is left out, and logged with warnf: the move's gives the fallback
// title and body, and the pass's no notes, so the PR still opens.
func (r *Runs) prText(s *github.Snapshot, is state.Issue, pass github.Comment, head, main string, warnf func(string, ...any) string) (string, string) {
	a, n := r.Daemon.Step.Applier, s.Number
	src := prSource{Task: n, Head: head, Main: main, Results: r.passed[n].Results, VerifierLink: waitLink(a.Owner, a.Repo, n, pass.ID)}
	move := -1
	for i, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Verifying {
			move = i
		}
	}
	if move < 0 {
		logr.Debugf("%s/%s#%d has no move to %s, so its PR gets the fallback title and body", a.Owner, a.Repo, n, state.Verifying)
	} else if c, ok := markerCommentOf(s, is, move); ok {
		src.SummaryLink = waitLink(a.Owner, a.Repo, n, c.ID)
		switch title, sum, ok, err := readPRText(c, a.AppSlug); {
		case err != nil:
			warnf("%s/%s#%d: its PR's title and summary don't read back, so it gets the fallback: %v", a.Owner, a.Repo, n, err)
		case ok:
			src.Title, src.Summary = &title, &sum
		}
	}
	notes, _, err := readVerifyNotes(pass, a.AppSlug)
	if err != nil {
		warnf("%s/%s#%d: its PR's verifier notes don't read back, so it has none: %v", a.Owner, a.Repo, n, err)
	}
	src.Notes = notes
	var types []string
	if r.Daemon.Config != nil {
		types = r.Daemon.Config.PR.TitleTypes
	}
	return renderPRTitle(src, types, s.Title), renderPRBody(src)
}

// prFind is the task's PR (SPEC §7.5): of the PRs whose head is branch,
// the ones the App opened, to prBase, not merged, the open one first, or
// else the newest closed one, or nil with none, as a merged one is never
// reused. It's one request, a 304 when the listing is unchanged.
func (r *Runs) prFind(ctx context.Context, branch string) (*github.PullRequest, error) {
	a := r.Daemon.Step.Applier
	prs, err := a.Client.PullRequestsByHead(ctx, a.Owner, a.Repo, branch)
	if err != nil {
		return nil, fmt.Errorf("find the PR: %w", err)
	}
	var closed *github.PullRequest
	for i := range prs {
		p := &prs[i]
		if p.Author != a.AppSlug+"[bot]" || p.Base != prBase || p.Merged {
			continue
		}
		if p.State == "open" {
			return p, nil
		}
		if closed == nil {
			closed = p
		}
	}
	return closed, nil
}

// prWrite makes pr, task n's PR as prFind found it, match want, and returns
// it as GitHub answers: with none, it opens one from want's branch to
// prBase; a closed one is reopened, in a request that sets the state alone,
// so a 422 to it is a refused reopen (github.ErrReopenRefused), and then a
// new one is opened (SPEC §7.5's A closed PR); then a title or a body that
// differs from want's is updated, only those.
func (r *Runs) prWrite(ctx context.Context, n int, want prWant, pr *github.PullRequest) (*github.PullRequest, error) {
	a := r.Daemon.Step.Applier
	create := func() (*github.PullRequest, error) {
		p, err := a.Client.CreatePullRequest(ctx, a.Owner, a.Repo, github.NewPullRequest{Title: want.Title, Head: want.Branch, Base: prBase, Body: want.Body})
		if err != nil {
			return nil, fmt.Errorf("open the PR: %w", err)
		}
		logr.Infof("%s/%s#%d: its PR #%d from %s to %s is opened", a.Owner, a.Repo, n, p.Number, want.Branch, prBase)
		return &p, nil
	}
	if pr == nil {
		return create()
	}
	if pr.State != "open" {
		open := "open"
		p, err := a.Client.UpdatePullRequest(ctx, a.Owner, a.Repo, pr.Number, nil, nil, &open)
		switch {
		case errors.Is(err, github.ErrReopenRefused):
			logr.Infof("%s/%s#%d: GitHub won't reopen its PR #%d, so a new one is opened: %v", a.Owner, a.Repo, n, pr.Number, err)
			return create()
		case err != nil:
			return nil, fmt.Errorf("reopen PR #%d: %w", pr.Number, err)
		}
		logr.Infof("%s/%s#%d: its PR #%d is reopened", a.Owner, a.Repo, n, p.Number)
		pr = &p
	}
	var title, body *string
	if pr.Title != want.Title {
		title = &want.Title
	}
	if pr.Body != want.Body {
		body = &want.Body
	}
	if title == nil && body == nil {
		return pr, nil
	}
	p, err := a.Client.UpdatePullRequest(ctx, a.Owner, a.Repo, pr.Number, title, body, nil)
	if err != nil {
		return nil, fmt.Errorf("update PR #%d: %w", pr.Number, err)
	}
	return &p, nil
}

// prCheckTitle is the title of the issuedog/verify check the App posts on
// a verified head (SPEC §10.4).
const prCheckTitle = "issuedog verified this head"

// prCheck posts the issuedog/verify check run, success, on head, the SHA
// task n's pass marker names, never the PR's head (SPEC §10.4), so the
// check is never a success for a head nobody verified: a PR that shows a
// head someone pushed after the App's has none there, and the main ruleset
// blocks its merge (SPEC §15). Its title is prCheckTitle, its summary
// renderPRCheck's, with what the checks of head against main's commit main
// gave (Runs.passed), so nothing runs again, and its details URL link,
// the verifier's pass comment. It's checked first: it reads the App's
// check runs there (github.Client.VerifyChecks), and posts none when the
// newest of them is a success already (prChecked), so a replay or a
// restart posts one.
func (r *Runs) prCheck(ctx context.Context, n int, head, main, link string) error {
	a := r.Daemon.Step.Applier
	runs, err := a.Client.VerifyChecks(ctx, a.Owner, a.Repo, head)
	if err != nil {
		return fmt.Errorf("read the head's checks: %w", err)
	}
	if prChecked(runs, a.Client.AppID()) {
		logr.Debugf("%s/%s#%d: its head %s has the App's %s as success already", a.Owner, a.Repo, n, head, github.VerifyCheck)
		return nil
	}
	run, err := a.Client.PostVerifyCheck(ctx, a.Owner, a.Repo, github.NewCheckRun{HeadSHA: head, Conclusion: "success",
		Title: prCheckTitle, Summary: renderPRCheck(n, head, main, r.passed[n].Results, link), DetailsURL: link})
	if err != nil {
		return fmt.Errorf("check the head: %w", err)
	}
	logr.Infof("%s/%s#%d: its head %s has %s as success, check run %d", a.Owner, a.Repo, n, head, github.VerifyCheck, run.ID)
	return nil
}

// prChecked reports whether the newest of runs, newest first, that's the
// App's, app's, and named issuedog/verify is a success. Only the App's
// count (SPEC §10.4): the listing asks for them alone, and another App's
// run is skipped here too, so it never counts even if GitHub ignored the
// filter.
func prChecked(runs []github.CheckRun, app int64) bool {
	for _, c := range runs {
		if c.AppID == app && c.Name == github.VerifyCheck {
			return c.Conclusion == "success"
		}
	}
	return false
}

// renderPRCheck is the summary of task n's issuedog/verify check on head
// (SPEC §10.4), escaped: that issuedog verified head for #n, against main's
// commit main, and the verifier passed it, linking its comment, link, then
// the checks' results as the PR's Results has them (prResults).
func renderPRCheck(n int, head, main string, results []verify.CommandResult, link string) string {
	return github.Escape(fmt.Sprintf("issuedog verified `%s` for #%d: the checks against `main` at `%s` passed, and so did the verifier, in [its comment](%s).\n\n%s",
		head, n, main, link, prResults(results)))
}

// prMove moves task s verifying → pr-open on verified (SPEC §7.2), keyed
// by run, its pass's run, with head=, the head the pass verified, which may
// not be the PR's head (SPEC §10.4), pr=, the task's PR, which names it for
// every pass after, at no request (SPEC §8.1), and seen=, step 1's newest
// comment, if any. Its comment is renderPROpen's, with no cc line: the
// digest's item announces the PR. A move written already only has its
// labels finished (Transition). It releases the worker (SPEC §8.1).
func (r *Runs) prMove(ctx context.Context, s *github.Snapshot, d Dirt, run, head string, pr int, seen int64) error {
	a := r.Daemon.Step.Applier
	m := Move{To: state.PROpen, Trigger: state.TriggerVerified, Key: state.Key{Field: "run", ID: run},
		Fields: map[string]string{"head": head, "pr": strconv.Itoa(pr)}, Text: renderPROpen(pr, head)}
	if seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return fmt.Errorf("move the task to %s: %w", state.PROpen, err)
	}
	logr.Infof("%s/%s#%d: its PR #%d and its head %s's check stand, so it's at %s, waiting for the owner's review and merge",
		a.Owner, a.Repo, s.Number, pr, head, state.PROpen)
	return nil
}

// renderPROpen is the comment of a task's move to pr-open (SPEC §7.2): it
// names the PR and the head verified, and says it waits for the owner's
// review and merge, which closes the task. It's the App's text alone.
func renderPROpen(pr int, head string) string {
	return fmt.Sprintf("PR #%d to `main` is open for this task, verified at `%s`. It waits for your review and merge, and merging it closes this task.", pr, head)
}

// prNothingMove hands task s off when the head its pass verified changes
// nothing on main (SPEC §7.2, §9.3, §10.2): verifying → needs-human on
// nothing-to-merge, keyed by run, its pass's run, with head=, that head,
// and seen=, step 1's newest comment, if any. Its comment is
// renderNothingHandoff's, ending in the cc line. A move written already
// only has its labels finished (Transition). It releases the worker
// (SPEC §8.1), and /done or a trusted 👍 on the hand-off sends the task
// back to ready, a new round (handback.go).
func (r *Runs) prNothingMove(ctx context.Context, s *github.Snapshot, d Dirt, run, head string, seen int64) error {
	a := r.Daemon.Step.Applier
	m := Move{To: state.NeedsHuman, Trigger: state.TriggerNothingToMerge, Key: state.Key{Field: "run", ID: run},
		Fields: map[string]string{"head": head}, Text: renderNothingHandoff(head, s.Number, r.Daemon.Config.Notify)}
	if seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return fmt.Errorf("hand off a head that changes nothing: %w", err)
	}
	logr.Infof("%s/%s#%d: its head %s changes nothing on %s, so it gets no PR, and it's handed off to the owner at %s",
		a.Owner, a.Repo, s.Number, head, prBase, state.NeedsHuman)
	return nil
}

// nothingHandoffFields are the fields of the hand-off for a head that
// changes nothing on main (SPEC §9.3), in the daemon's own words, as no
// run gave any. It has no links: the pass it follows is above it.
var nothingHandoffFields = implementerHandoffFields{
	Goal:      "Decide whether this task needs a change on `main`, as its verified head makes none.",
	WhyBlocks: "it's decided whether it needs a change on `main`",
	Steps: []string{
		"Read the task's acceptance criteria, and the verifier's pass above.",
		"If nothing needs to change, close this issue.",
		"If something does, say what in a comment, then reply `/done` for a new round.",
	},
	DoneWhen:      "The issue is closed, or a new round is asked for.",
	EstimatedTime: "A few minutes.",
}

// renderNothingHandoff is the hand-off comment on task n when the head the
// verifier passed, head, changes nothing on main (SPEC §9.3, §10.2),
// escaped: renderHandoff with nothingHandoffFields, opening with the head,
// and that its own diff from its merge base with main is empty, so it
// gets no PR. Only a commit SHA is in it, so it quotes nothing of a run's.
func renderNothingHandoff(head string, n int, notify []string) string {
	intro := fmt.Sprintf("The verifier passed `%s`, but it changes nothing on `main`: its own diff from its merge base with `main` is empty, so it gets no PR.", head)
	return renderHandoff(intro, nothingHandoffFields, n, notify)
}

// nothingHandoffMove reports whether m is the hand-off for a head that
// changes nothing on main (prNothingMove): verifying → needs-human, keyed
// run= by its pass's run, and carrying head=, which neither cap's hand-off
// from verifying carries (verifyCapMove, verifierCapMove).
func nothingHandoffMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.Verifying &&
		state.Stage(m.Fields["to"]) == state.NeedsHuman && m.Fields["run"] != "" && m.Fields["head"] != ""
}

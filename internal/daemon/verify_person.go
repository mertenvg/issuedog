package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is a person's head at verifying (SPEC §3.4, §7.2, §10.1,
// §10.2): a task whose latest move into verifying is step 2's on a new
// head (pr_head.go), a head the daemon didn't push, gets the checks, then
// the verifier, on that head, as any head does. But nothing on it counts
// an attempt, pushes anything, or sends the task to in-progress (SPEC
// §8.3). A rule 2 failure hands the task off at needs-human, naming the
// head and the protected paths (personFindings). Any other result, a
// pass, findings of the checks or the verifier, or no readable verdict,
// takes it back to pr-open on person-pushed (personBack): first the PR's
// body names the head and the new notes, then issuedog/verify on that
// head is success or failure, then the move, each checked first, so a
// replay, a restart or a crash between any two repeats none and loses
// none (SPEC §5.2). A branch that moved on since is left for step 2's new
// head once the task is back at pr-open: nothing here waits on it. A
// verifier's question goes to clarifying, as any question does
// (applyQuestion).

// personMove is the move that put task is at verifying, its latest move
// looking through a hold (taskHeadMove), and true when it's step 2's on a
// new head (newHeadMove), so its head= is a person's head. It's the move
// taskHeadFollow reads, so the head judged is the one the task's head
// follows, even when a crash came between that move and the ref.
func personMove(is state.Issue) (github.Marker, bool) {
	if is.Stage != state.Verifying {
		return github.Marker{}, false
	}
	m := taskHeadMove(is)
	return m, newHeadMove(m)
}

// personHead reports whether task is is at verifying on a person's head
// (personMove).
func personHead(is state.Issue) bool {
	_, ok := personMove(is)
	return ok
}

// personResult is what a person's head goes back to pr-open with
// (personBack): its PR's body, its issuedog/verify check's conclusion,
// summary and details URL, main's commit its checks ran against, which the
// check names (prCheck), and the move's comment, given the PR's number, or
// 0 for none found.
type personResult struct {
	Body                      string
	Conclusion, Summary, Link string
	Main                      string
	Text                      func(pr int) string
}

// personRead reads rd's task, whose state was is when its checks or its
// pass were judged, again before a write, as verifyFindings does: it runs
// CheckLabels and the look for discussion triggers on it (SPEC §8.2), and
// returns it, with its dirt, and true. A task closed, gone, held, or moved
// since, even back to verifying, gives false, so it gets no write: its
// next checks, if any, are on what's there.
func (r *Runs) personRead(ctx context.Context, rd Ready, is state.Issue) (*github.Snapshot, Dirt, bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, so its person's head goes nowhere, and it's read again: %v", a.Owner, a.Repo, n, err)
		r.Daemon.reread(n, true)
		return nil, Dirt{}, false, nil
	case err != nil:
		return nil, Dirt{}, false, fmt.Errorf("read the task again: %w", err)
	}
	r.Daemon.reread(n, false)
	d, err := a.Dirty(ctx, s, r.Daemon.Step.Trust)
	if err == nil {
		err = a.discCheckLabels(ctx, s, r.Daemon.Step.Trust, d)
	}
	if err != nil {
		return nil, Dirt{}, false, fmt.Errorf("check the task's labels again: %w", err)
	}
	if s.Closed {
		logr.Infof("%s/%s#%d was closed since its person's head was judged, so it goes nowhere", a.Owner, a.Repo, n)
		return nil, Dirt{}, false, nil
	}
	if _, err := r.Daemon.Step.discussion(ctx, s, d); err != nil {
		return nil, Dirt{}, false, fmt.Errorf("look for discussion triggers: %w", err)
	}
	now := state.Derive(s, a.AppSlug, d.Has)
	if now.TransitionComment != is.TransitionComment || now.Stage != state.Verifying {
		logr.Infof("%s/%s#%d moved to %s since its person's head was judged, so it goes nowhere", a.Owner, a.Repo, n, now.Stage)
		return nil, Dirt{}, false, nil
	}
	return s, d, true, nil
}

// stepSeen is the newest comment on s, step 1's snapshot the checks or the
// pass started from, as a move's seen= has it (verifyFindings), or 0.
func stepSeen(s *github.Snapshot) int64 {
	var seen int64
	for _, c := range s.Comments {
		seen = max(seen, c.ID)
	}
	return seen
}

// personFindings is verifyFindings for a person's head (personHead): rd's
// task, whose state was is when the checks started, failed them on head
// against main's commit main, with fs and cmds. It reads the issue again
// first (personRead). The move is keyed verifyKey of the comment holding
// the move to verifying, as the checks' moves are, with head= and seen=,
// and no attempt=, as a person's head counts none (SPEC §8.3):
//   - a rule 2 failure moves it verifying → needs-human on attempt-cap,
//     the row whose text names a person's head that fails rule 2, with the
//     hand-off (renderPersonRule2Handoff), naming the head and the paths.
//     That releases the worker, and /done or a trusted 👍 on it sends the
//     task back to ready, a new round (handback.go), whose run never
//     starts from this head (startFrom, SPEC §3.4).
//   - any other failure goes back to pr-open on person-pushed, with
//     issuedog/verify failure on the head (personBack, personFailed).
func (r *Runs) personFindings(ctx context.Context, rd Ready, is state.Issue, head, main string, fs []verify.Finding, cmds []verify.CommandResult) error {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	s, d, ok, err := r.personRead(ctx, rd, is)
	if err != nil || !ok {
		if err != nil {
			return fmt.Errorf("judge a person's head's findings: %w", err)
		}
		return nil
	}
	fields := map[string]string{"head": head}
	if seen := stepSeen(rd.S); seen > 0 {
		fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	key := verifyKey(is.TransitionComment)
	if i := slices.IndexFunc(fs, func(f verify.Finding) bool { return f.Rule == 2 }); i >= 0 {
		m := Move{To: state.NeedsHuman, Trigger: state.TriggerAttemptCap, Key: key, Fields: fields,
			Text: renderPersonRule2Handoff(head, fs[i], n, r.Daemon.Config.Notify)}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return fmt.Errorf("hand off a person's head that fails rule 2: %w", err)
		}
		logr.Warnf("%s/%s#%d: its head %s, which the daemon didn't push, fails rule 2 against main's %s, so it's handed off to the owner at %s",
			a.Owner, a.Repo, n, head, main, state.NeedsHuman)
		return nil
	}
	m := Move{To: state.PROpen, Trigger: state.TriggerPersonPushed, Key: key, Fields: fields}
	res := r.Daemon.personFailed(s, state.Derive(s, a.AppSlug, d.Has), head, findingsText(head, main, 0, r.limit(), fs, cmds))
	res.Main = main
	if err := r.Daemon.personBack(ctx, s, d, m, head, res); err != nil {
		return fmt.Errorf("judge a person's head's findings: %w", err)
	}
	return nil
}

// personPass is the PR step for a person's head (personHead) that passed
// the checks against main's commit main and has a clean pass of the
// verifier's (verdictOn): verifyTask calls it in openPR's place. It waits
// as the PR step does for the pass and its follow-ups (prPassReady), but
// never for the branch: nothing is pushed, and a tip that moved on is
// step 2's new head once the task is back at pr-open. It reads the issue
// again (personRead), then moves the task back to pr-open on
// person-pushed, keyed by the pass's run, with seen= step 1's newest
// comment, after its PR's body names head and the pass's notes (prText),
// and issuedog/verify on head is success, as the PR step posts it
// (personBack). It reports whether it read the issue again, as openPR
// does.
func (r *Runs) personPass(ctx context.Context, rd Ready, is state.Issue, head, main string) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	if _, _, ok := r.prPassReady(rd.S, is, head); !ok {
		return false, nil
	}
	s, d, ok, err := r.personRead(ctx, rd, is)
	if err != nil || !ok {
		if err != nil {
			return true, fmt.Errorf("move a person's head back on its pass: %w", err)
		}
		return true, nil
	}
	now := state.Derive(s, a.AppSlug, d.Has)
	run, pass, ok := r.prPassReady(s, now, head)
	if !ok {
		return true, nil
	}
	_, body := r.prText(s, now, pass, head, main, logr.Warnf)
	link := waitLink(a.Owner, a.Repo, n, pass.ID)
	m := Move{To: state.PROpen, Trigger: state.TriggerPersonPushed, Key: state.Key{Field: "run", ID: run}, Fields: map[string]string{}}
	if seen := stepSeen(rd.S); seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	res := personResult{Body: body, Conclusion: checkSuccess, Summary: renderPRCheck(n, head, main, r.passed[n].Results, link), Link: link,
		Main: main, Text: func(pr int) string { return renderPersonPassed(head, pr) }}
	if err := r.Daemon.personBack(ctx, s, d, m, head, res); err != nil {
		return true, fmt.Errorf("move a person's head back on its pass: %w", err)
	}
	return true, nil
}

// personChanges is applyChanges for a person's head (personHead): the
// verifier's request_changes, or no readable verdict, on f.Head, out,
// takes the task back to pr-open on person-pushed, as r.Move's move, keyed
// by the run, with its seen=, model=, effort= and ran=, and no attempt=,
// with issuedog/verify failure on the head, the verdict as verdictText
// gives it (personBack, personFailed). Its notes and candidates are
// dropped, as any request_changes' are.
func (v *Verifier) personChanges(ctx context.Context, r Result, is state.Issue, out verifierOutput, f verifierFacts) error {
	m := r.Move(state.PROpen, state.TriggerPersonPushed)
	if m.Fields == nil {
		m.Fields = map[string]string{}
	}
	res := v.Daemon.personFailed(r.S, is, f.Head, verdictText(f.Head, f.Attempt, f.Limit, out))
	// The verifier ran on a head whose checks passed against main as the
	// pass's sync left it (startAfterChecks): this pass's main, unless it
	// moved during the run.
	main, err := v.Daemon.Clone.Resolve(ctx, "main")
	if err != nil {
		return fmt.Errorf("move a person's head back on the verifier's findings: read main: %w", err)
	}
	res.Main = main
	if err := v.Daemon.personBack(ctx, r.S, r.Dirt, m, f.Head, res); err != nil {
		return fmt.Errorf("move a person's head back on the verifier's findings: %w", err)
	}
	return nil
}

// personFailed is what a person's head that fails verify goes back to
// pr-open with, task s's, whose state is is: its PR's body naming head as
// failing, with the implementer's summary (prSummary, prSource.Failed),
// and issuedog/verify failure, whose summary is findings, the checks' as
// findingsText or the verifier's as verdictText gives them
// (renderPersonCheck), linking the issue, where the move's comment
// (renderPersonFailed) has them too.
func (d *Daemon) personFailed(s *github.Snapshot, is state.Issue, head, findings string) personResult {
	a, n := d.Step.Applier, s.Number
	src := d.prSummary(s, is, logr.Warnf)
	src.Head, src.Failed = head, true
	return personResult{Body: renderPRBody(src), Conclusion: checkFailure, Summary: renderPersonCheck(n, head, findings),
		Link: waitLink(a.Owner, a.Repo, n, state.BodyID), Text: func(pr int) string { return renderPersonFailed(head, pr, findings) }}
}

// renderPersonCheck is the summary of task n's issuedog/verify failure on
// a person's head, head (SPEC §10.4): a line naming the task and the head,
// then findings, escaped (SPEC §5.1), then cut to checkSummaryMax, ending
// in "…", as GitHub refuses a longer summary.
func renderPersonCheck(n int, head, findings string) string {
	summary := fmt.Sprintf("issuedog checked `%s` for #%d, a head the daemon didn't push, and it fails verify.\n\n%s", head, n, findings)
	return cutText(github.Escape(summary), checkSummaryMax)
}

// personBack moves task s, whose dirt is dirt, back to pr-open on a
// person's head, head, by m, keyed and with its seen= set by the caller,
// with what res says, in order, each write checked first (SPEC §5.2):
//   - the PR's body becomes res.Body, unless it's that already. The PR is
//     found by the task's branch (prFind), its title left as it is. No
//     PR, or a branch the daemon can't name (prTip), updates none.
//   - issuedog/verify on head gets res.Conclusion, unless the App's newest
//     there has it (prCheck). A head the bare repo lacks gets none, as
//     there's nothing to post it on.
//   - the move, with head= and pr=, the PR's number, or else the task's
//     (taskPR), and its comment res.Text's. A move written already only
//     has its labels finished (Transition). It releases the worker (SPEC
//     §8.1).
//
// Nothing is pushed, and no attempt counted. It never waits on the
// branch: a tip that moved on since is step 2's new head at pr-open.
func (d *Daemon) personBack(ctx context.Context, s *github.Snapshot, dirt Dirt, m Move, head string, res personResult) error {
	a, n := d.Step.Applier, s.Number
	wrap := func(err error) error { return fmt.Errorf("move a person's head back to %s: %w", state.PROpen, err) }
	branch, _, named, err := d.prTip(ctx, s)
	if err != nil {
		return wrap(err)
	}
	var pr *github.PullRequest
	if named {
		if pr, _, err = d.prFind(ctx, branch); err != nil {
			return wrap(err)
		}
	}
	number := taskPR(state.Derive(s, a.AppSlug, dirt.Has))
	switch {
	case pr == nil:
		logr.Warnf("%s/%s#%d: no PR of the App's is found from its branch, so no PR body names its head %s", a.Owner, a.Repo, n, head)
	case pr.Body != res.Body:
		number = pr.Number
		if _, err := a.Client.UpdatePullRequest(ctx, a.Owner, a.Repo, pr.Number, nil, &res.Body, nil); err != nil {
			return wrap(fmt.Errorf("update PR #%d's body: %w", pr.Number, err))
		}
		logr.Infof("%s/%s#%d: its PR #%d's body names its head %s", a.Owner, a.Repo, n, pr.Number, head)
	default:
		number = pr.Number
	}
	has, err := d.Clone.HasCommit(ctx, head)
	if err != nil {
		return wrap(fmt.Errorf("read the head: %w", err))
	}
	if has {
		if err := d.prCheck(ctx, n, head, res.Main, res.Conclusion, res.Summary, res.Link); err != nil {
			return wrap(err)
		}
	} else {
		logr.Warnf("%s/%s#%d: its head %s isn't in the bare repo, so it gets no %s", a.Owner, a.Repo, n, head, github.VerifyCheck)
	}
	m.Fields["head"] = head
	if number > 0 {
		m.Fields["pr"] = strconv.Itoa(number)
	}
	m.Text = res.Text(number)
	if err := a.Transition(ctx, s, dirt.Has, m); err != nil {
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: its head %s, which the daemon didn't push, has %s as %s, so it's back at %s",
		a.Owner, a.Repo, n, head, github.VerifyCheck, res.Conclusion, state.PROpen)
	return nil
}

// personPR names the task's PR in a person's head's comment: PR #pr, or
// "its PR" when none was found.
func personPR(pr int) string {
	if pr > 0 {
		return fmt.Sprintf("PR #%d", pr)
	}
	return "its PR"
}

// renderPersonPassed is the comment of a person's head's move back to
// pr-open on its pass (SPEC §7.2): the verifier passed head, which the
// daemon didn't push, so the task is back at pr-open with nothing pushed
// and no attempt counted, and the PR, pr, names it, with its check a
// success there. It's the App's text, and a commit SHA.
func renderPersonPassed(head string, pr int) string {
	return fmt.Sprintf("The verifier passed `%s`, a head the daemon didn't push to this task's branch, so the task is back at `pr-open`, with nothing pushed and no attempt counted. %s's body names the head, and its `issuedog/verify` check there is a success. It waits for your review and merge, and merging it closes this task.",
		head, upperFirst(personPR(pr)))
}

// renderPersonFailed is the comment of a person's head's move back to
// pr-open on findings (SPEC §7.2): head, which the daemon didn't push,
// fails verify, so the task is back at pr-open with nothing pushed and no
// attempt counted, with the PR's check a failure there, which blocks its
// merge, and what the owner can do, then findings, the checks' or the
// verifier's, whose text the daemon didn't write is fenced or made safe
// already. Transition escapes the whole (SPEC §5.1).
func renderPersonFailed(head string, pr int, findings string) string {
	return fmt.Sprintf("`%s`, a head the daemon didn't push to this task's branch, fails verify, so the task is back at `pr-open`, with nothing pushed and no attempt counted. %s's `issuedog/verify` check there is a failure, which blocks its merge. Push a fix to the branch, or review the PR with comments to send the task back to the implementer.\n\n%s",
		head, upperFirst(personPR(pr)), findings)
}

// personRule2Fields are the fields of the hand-off for a person's head
// that fails rule 2 (SPEC §3.4, §9.3), in the daemon's own words. It has
// no links: the paths are above them.
var personRule2Fields = implementerHandoffFields{
	Goal:      "Decide what to do about the head someone pushed to this task's branch, which changes a protected path.",
	WhyBlocks: "its branch's head changes no protected path, or it's started over",
	Steps: []string{
		"Read the protected paths above. Only a change on `main` may change them.",
		"If the change to them is wanted, make it on `main`, in a PR of its own.",
		"Reply `/done` or react 👍 for a new round. Its run never starts from a head that fails rule 2, so it starts from the task's base, and its result replaces the task's branch.",
	},
	DoneWhen:      "The protected paths are as you want them on `main`, and the task can start over.",
	EstimatedTime: "A few minutes, unless a change on `main` is wanted.",
}

// renderPersonRule2Handoff is the hand-off comment on task n when a head
// someone else pushed, head, fails rule 2 (SPEC §3.4, §9.3), escaped:
// renderHandoff with personRule2Fields, opening with the head, that it
// changes a protected path, so no command or verifier ran on it and the
// task doesn't go back to pr-open, then the finding, which names each
// path, fenced (findingSection).
func renderPersonRule2Handoff(head string, f verify.Finding, n int, notify []string) string {
	intro := fmt.Sprintf("`%s`, a head the daemon didn't push to this task's branch, fails rule 2: it changes a protected path, which only a change on `main` may. So no command or verifier ran on it, and the task doesn't go back to `pr-open`.\n\n%s",
		head, findingSection(f))
	return renderHandoff(intro, personRule2Fields, n, notify)
}

// personRule2Move reports whether m is the hand-off for a person's head
// that fails rule 2 (personFindings): verifying → needs-human, keyed
// event=v<comment id> as the checks' moves are, and carrying head=, which
// the verify cap's hand-off from the checks doesn't (verifyCapMove).
func personRule2Move(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.Verifying &&
		state.Stage(m.Fields["to"]) == state.NeedsHuman && strings.HasPrefix(m.Fields["event"], "v") && m.Fields["head"] != ""
}

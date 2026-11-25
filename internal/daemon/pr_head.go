package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is step 2's new heads (SPEC §3.2, §7.2, §8.1): a task at
// pr-open whose branch's tip, in the bare clone as the pass's sync left
// it, isn't the head its PR was verified at goes back to verifying on that
// tip, and the task's head, refs/issuedog/tasks/<n>, follows it. The App
// pushes a task's branch only in the PR step, at verifying, and that
// step's move into pr-open names the head it pushed, so any other tip is a
// head the daemon didn't push, whoever pushed it. It costs no request: the
// sync brings the branch, as it does every pass.
//
// A move out of pr-open into verifying or in-progress takes the worker, so
// it waits while another task holds it (SPEC §8.1's One task at a time),
// kept in memory, and is made on the first pass the worker is free
// (Daemon.prWaiting). After a restart, step 2's read of each open PR and
// the bare clone find it again.

// prHeadOf is the head task is's PR was verified at (SPEC §5): the head= of
// its latest move into pr-open that names one, the PR step's, or a
// person's head's back from verifying (personBack), or "" for none. A move
// into pr-open that names no head, as a trusted reopen's, a return from a
// hold or a stage set by hand, is looked through.
func prHeadOf(is state.Issue) string {
	head := ""
	for _, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.PROpen && m.Fields["head"] != "" {
			head = m.Fields["head"]
		}
	}
	return head
}

// newHeadKey keys a task's move pr-open → verifying on a new head, tip
// (SPEC §5): event=n<tip>.
func newHeadKey(tip string) state.Key {
	return state.Key{Field: "event", ID: "n" + tip}
}

// prNewHead judges task s's branch at pr-open, whose state is is (SPEC
// §7.2's pr-open → verifying), from the bare clone's branches as the
// pass's sync left them, so at no request (prTip), and returns the move it
// makes, or nil. A tip that's neither prHeadOf(is) nor "", as for a branch
// that's gone, which leaves the PR to its close (prJudgeOpen), is a new
// head: the move pr-open → verifying on new-head, keyed event=n<tip>, with
// head=, that tip, and the round bound the task had as seen=
// (state.HeldStackFields), its comment naming the old head and the new one
// (renderNewHead). A tip such a move named once already moves the task no
// more (SPEC §5.2). Two branches with the task's prefix move nothing, as
// the daemon can't tell which is the task's (prTip), and nor does a branch
// while a move of the task's waits (Daemon.prWaiting), as a review's that
// waits for its story branch's tip (prReviewTipWait), which carries the
// branch's tip (prWaitingTip).
func (d *Daemon) prNewHead(ctx context.Context, s *github.Snapshot, is state.Issue) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	branch, tip, ok, err := d.prTip(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("read the task's branch: %w", err)
	}
	if !ok {
		return nil, nil
	}
	if m, waits := d.prWaiting[n]; waits {
		logr.Debugf("%s/%s#%d: its move to %s on %s waits, and carries its branch's tip, so no new head is judged", a.Owner, a.Repo, n, m.To, m.Trigger)
		return nil, nil
	}
	old := prHeadOf(is)
	if tip == "" || tip == old {
		return nil, nil
	}
	key := newHeadKey(tip)
	if is.Moved(key) {
		logr.Debugf("%s/%s#%d: its branch %s is at %s, which moved it to %s once already, so it moves the task no more", a.Owner, a.Repo, n, branch, tip, state.Verifying)
		return nil, nil
	}
	logr.Infof("%s/%s#%d: its branch %s is at %s, not at %s, its PR's verified head, so it's a new head the daemon didn't push", a.Owner, a.Repo, n, branch, tip, old)
	return &Move{To: state.Verifying, Trigger: state.TriggerNewHead, Key: key,
		Fields: state.HeldStackFields(map[string]string{"head": tip}, is.Round), Text: renderNewHead(old, tip)}, nil
}

// prTip is task s's branch and its tip, from the bare clone's branches as
// the pass's sync left them, so at no request, and true. The task's branch
// is the one with its prefix (taskBranch), and a branch that's gone has
// tip "". Two branches with the task's prefix give false, as the daemon
// can't tell which is the task's, warned of once while that lasts
// (prHeadWarnOnce).
func (d *Daemon) prTip(ctx context.Context, s *github.Snapshot) (string, string, bool, error) {
	a, n := d.Step.Applier, s.Number
	branches, err := d.Clone.Branches(ctx, taskBranchPrefix(n))
	if err != nil {
		return "", "", false, fmt.Errorf("list the task's branches: %w", err)
	}
	branch, tip, err := taskBranch(n, s.Title, branches)
	switch {
	case errors.Is(err, errTwoBranches):
		d.prHeadWarnOnce(n, err.Error(), "%s/%s#%d: its branch's tip isn't judged at %s: %v", a.Owner, a.Repo, n, state.PROpen, err)
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("name the task's branch: %w", err)
	}
	// A wait that ended is warned of again if it comes back.
	delete(d.prHeadWarned, n)
	return branch, tip, true, nil
}

// renderNewHead is the comment of a task's move pr-open → verifying on a
// new head, tip, over old, the head its PR was verified at (SPEC §7.2): it
// names both, and says the task is back at verifying for the new one,
// which its PR waits for. It's the App's text, and commit SHAs alone, which
// Transition escapes.
func renderNewHead(old, tip string) string {
	if old == "" {
		return fmt.Sprintf("This task's branch is at `%s`, a head the daemon didn't push, so the task is back at `verifying` to check it. Its PR waits until that head is verified.", tip)
	}
	return fmt.Sprintf("This task's branch is at `%s`, not at `%s`, the head its PR was verified at, and the daemon didn't push it. So the task is back at `verifying` to check the new head, and its PR waits until that head is verified.", tip, old)
}

// newHeadMove reports whether m is a move pr-open → verifying on a new
// head (prNewHead), keyed event=n<tip>.
func newHeadMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen && state.Stage(m.Fields["to"]) == state.Verifying &&
		len(m.Fields["event"]) > 1 && m.Fields["event"][0] == 'n' && m.Fields["head"] != ""
}

// taskHeadFollow moves task n's head, taskRef(n), to the head= of its
// latest move, as is reads it, looking through a hold (taskHeadMove), when
// that move names a head the task's head follows (headMove): a new
// head's, or a review's that carries one, and the ref isn't there already
// (SPEC §3.2). Step 2 calls it after that move, verifyTask on each pass at
// verifying before it names the head, and a Committer's run start before
// it reads the task's head, so a crash between the move and the ref is
// finished there, even after a hold and /resume in between. A head the
// bare repo lacks moves nothing: the checks find that (verifyChecks), and
// a run starts from the task's head as it is.
func (d *Daemon) taskHeadFollow(ctx context.Context, n int, is state.Issue) error {
	a := d.Step.Applier
	latest := taskHeadMove(is)
	if !headMove(latest) {
		return nil
	}
	head := latest.Fields["head"]
	at, _, err := d.Clone.ReadRef(ctx, taskRef(n))
	if err != nil {
		return fmt.Errorf("read the task's head: %w", err)
	}
	if at == head {
		return nil
	}
	has, err := d.Clone.HasCommit(ctx, head)
	if err != nil {
		return fmt.Errorf("read the head its move names: %w", err)
	}
	if !has {
		logr.Debugf("%s/%s#%d: the head %s its move names isn't in the bare repo, so the task's head stays at %q", a.Owner, a.Repo, n, head, at)
		return nil
	}
	if err := d.Clone.UpdateRef(ctx, taskRef(n), head); err != nil {
		return fmt.Errorf("move the task's head to the head its move names: %w", err)
	}
	logr.Infof("%s/%s#%d: the task's head is now the head its move names, %s", a.Owner, a.Repo, n, head)
	return nil
}

// taskHeadMove is task is's latest transition, looking through a hold
// that went back where it came from: a move into held, and a move out of
// held back to the stage the move before the hold went to, as /resume
// makes, are skipped, so the move that put the task there stands (SPEC
// §6.4). taskHeadFollow reads it, and so does personMove, so the head
// the task's head follows and the head a person's verify judges are one.
// Any other move, out of held to another stage, or a stage set by hand,
// is the latest.
func taskHeadMove(is state.Issue) github.Marker {
	var latest github.Marker
	for _, m := range is.Markers {
		if m.Kind != "transition" {
			continue
		}
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		if to == state.Held || from == state.Held && to == state.Stage(latest.Fields["to"]) {
			continue
		}
		latest = m
	}
	return latest
}

// headMove reports whether m is a move whose head= the task's head follows
// (taskHeadFollow, SPEC §3.2): a new head's (newHeadMove), or a review's
// (reviewMove) that carries one, as the task's branch was at a head other
// than its PR's verified head.
func headMove(m github.Marker) bool {
	return newHeadMove(m) || reviewMove(m) && m.Fields["head"] != ""
}

// workerFree reports whether task n may take the one worker (SPEC §8.1's
// One task at a time), from what the daemon holds, at no request: step 1
// finished on every issue this pass (Daemon.step1Gaps), as for the pick
// (Runs.pickGaps), since a task holding the worker may be one it couldn't
// see; no open issue but n that the daemon holds a snapshot of is at
// in-progress or verifying, by its stage labels (pickHolds), which a move
// written this pass updates in place, but one that steps aside for n, a
// failure's bug of its story, as its own work waits on that failure
// (failureAside); and no lease held the worker when Work last ended
// (Daemon.workerLeased). It returns why not, for the log.
func (d *Daemon) workerFree(n int) (bool, string) {
	if len(d.step1Gaps) > 0 {
		return false, "step 1 didn't finish on every issue this pass"
	}
	for _, m := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[m]
		if m == n || s == nil || s.Closed || d.failureAside(s, d.snaps[n]) {
			continue
		}
		if i := slices.IndexFunc(s.Labels, pickHolds); i >= 0 {
			return false, fmt.Sprintf("#%d, at %s, holds it", m, s.Labels[i])
		}
	}
	if d.workerLeased {
		return false, "a run's lease holds it"
	}
	return true, ""
}

// prTakesWorker reports whether m, a move out of pr-open, takes the worker:
// one into verifying or in-progress (SPEC §8.1).
func prTakesWorker(m Move) bool {
	return m.To == state.Verifying || m.To == state.InProgress
}

// prWaitingFor is the move step 2, or step 4 (Runs.mainMoved), found for
// task n at pr-open, whose state is is, with its PR p, that waits for the
// worker (Daemon.prWaiting), and true, while it stands: the task hasn't
// moved since its PR was last judged (Daemon.prJudged), and the poll
// hasn't listed the PR changed since (Daemon.prRead). Otherwise the move
// is dropped, and the PR is judged again.
func (d *Daemon) prWaitingFor(n int, is state.Issue, p listedPR) (Move, bool) {
	m, ok := d.prWaiting[n]
	if !ok {
		return Move{}, false
	}
	if d.prJudged[n] != is.TransitionComment || d.prRead[p.Number] {
		a := d.Step.Applier
		logr.Debugf("%s/%s#%d: its move to %s on %s, which waited for the worker, is dropped, as the task or its PR #%d changed since, so the PR is judged again",
			a.Owner, a.Repo, n, m.To, m.Trigger, p.Number)
		delete(d.prWaiting, n)
		return Move{}, false
	}
	return m, true
}

// prWaitingTip is m, the move found for task s at pr-open, whose state is
// is, that waits for the worker and still stands (prWaitingFor), made to
// fit the task's branch as the pass's sync left it, at no request (prTip),
// and true, or false when it no longer stands, and is dropped, so the PR
// is judged again: a branch that moved on since the move was found, even
// with no change the poll lists, isn't missed. A new head's move stands
// only while the branch is at the head its key names. A review's takes the
// branch's tip as head= when it isn't the PR's verified head, and none
// otherwise (prReviewHead). Two branches with the task's prefix drop a new
// head's move, as prNewHead makes none then, and leave a review's as it
// was found. A move on its target's moving, main's or its story branch's
// tip's, which step 4 found (Runs.mainMoved), stands only while that
// target is at the commit its key names (prWaitingMain).
// A review's move of a task in a story whose story branch's tip has no
// success, or has a failure open that the task isn't the bug of, still
// waits (prReviewTipWait): it's kept, made to fit the branch, and this
// gives false, so the PR's close is judged, at no request, and nothing else
// is, as a move that waits stands (prNewHead, prMerge).
func (d *Daemon) prWaitingTip(ctx context.Context, s *github.Snapshot, is state.Issue, m Move) (Move, bool, error) {
	a, n := d.Step.Applier, s.Number
	_, tip, ok, err := d.prTip(ctx, s)
	if err != nil {
		return Move{}, false, fmt.Errorf("read the task's branch for its waiting move: %w", err)
	}
	switch {
	case m.Trigger == state.TriggerNewHead && (!ok || newHeadKey(tip) != m.Key):
		logr.Debugf("%s/%s#%d: its move to %s on %s, which waited for the worker, is dropped, as its branch is at %q now, so it's judged again",
			a.Owner, a.Repo, n, m.To, m.Trigger, tip)
		delete(d.prWaiting, n)
		return Move{}, false, nil
	case mainKeyed(m.Key), tipKeyed(m.Key):
		stands, err := d.prWaitingMain(ctx, s, m)
		if err != nil {
			return Move{}, false, fmt.Errorf("its waiting move on its target: %w", err)
		}
		return m, stands, nil
	case m.Trigger == state.TriggerReviewChanges && ok:
		m.Fields = prReviewHead(maps.Clone(m.Fields), prHeadOf(is), tip)
	}
	waits, err := d.prReviewTipWait(ctx, s, m)
	switch {
	case err != nil:
		return Move{}, false, fmt.Errorf("its waiting move's story branch: %w", err)
	case waits:
		return Move{}, false, nil
	}
	return m, true, nil
}

// prWait keeps m, the move step 2, or step 4 (Runs.mainMoved), found for
// task n out of pr-open, to make once the worker is free
// (Daemon.prWaiting), logging why it waits: at info the pass it's found,
// and at debug each pass after.
func (d *Daemon) prWait(n int, m Move, why string) {
	d.prWaitOn(n, m, "for the worker, as "+why)
}

// prWaitOn keeps m, a move found for task n out of pr-open, in
// Daemon.prWaiting, as prWait does, logging that it waits on, what it
// waits for: at info the pass it's found, and at debug each pass after. A
// review's move of a task in a story waits this way for its story
// branch's tip too (prReviewTipWait).
func (d *Daemon) prWaitOn(n int, m Move, on string) {
	a := d.Step.Applier
	log := logr.Infof
	if _, ok := d.prWaiting[n]; ok {
		log = logr.Debugf
	}
	if d.prWaiting == nil {
		d.prWaiting = map[int]Move{}
	}
	d.prWaiting[n] = m
	log("%s/%s#%d: its move to %s on %s waits %s", a.Owner, a.Repo, n, m.To, m.Trigger, on)
}

// prHeadWarnOnce logs a warning for task n, unless the last one logged for
// it had the same key, so a wait that lasts is logged once, not every pass,
// as Runs.prWarnOnce does for the PR step.
func (d *Daemon) prHeadWarnOnce(n int, key, format string, args ...any) {
	if d.prHeadWarned[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if d.prHeadWarned == nil {
		d.prHeadWarned = map[int]string{}
	}
	d.prHeadWarned[n] = key
	logr.Warnf(format, args...)
}

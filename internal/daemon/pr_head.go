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
// its latest move into pr-open that names one, the PR step's, or "" for
// none. A move into pr-open that names no head, as a trusted reopen's, a
// return from a hold or a stage set by hand, is looked through.
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
// pass's sync left them, so at no request, and returns the move it makes,
// or nil. The task's branch is the one with its prefix (taskBranch). A tip
// that's neither prHeadOf(is) nor "", as for a branch that's gone, which
// leaves the PR to its close (prJudgeOpen), is a new head: the move
// pr-open → verifying on new-head, keyed event=n<tip>, with head=, that
// tip, and the round bound the task had as seen= (state.HeldStackFields),
// its comment naming the old head and the new one (renderNewHead). A tip
// such a move named once already moves the task no more (SPEC §5.2). Two
// branches with the task's prefix move nothing, as the daemon can't tell
// which is the task's, warned of once while that lasts (prHeadWarnOnce).
func (d *Daemon) prNewHead(ctx context.Context, s *github.Snapshot, is state.Issue) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	branches, err := d.Clone.Branches(ctx, taskBranchPrefix(n))
	if err != nil {
		return nil, fmt.Errorf("list the task's branches: %w", err)
	}
	branch, tip, err := taskBranch(n, s.Title, branches)
	switch {
	case errors.Is(err, errTwoBranches):
		d.prHeadWarnOnce(n, err.Error(), "%s/%s#%d: its branch's tip isn't judged at %s: %v", a.Owner, a.Repo, n, state.PROpen, err)
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("name the task's branch: %w", err)
	}
	// A wait that ended is warned of again if it comes back.
	delete(d.prHeadWarned, n)
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
// latest move, as is reads it, when that move is a new head's
// (newHeadMove), and the ref isn't there already (SPEC §3.2). Step 2 calls
// it after that move, and verifyTask on each pass at verifying before it
// names the head, so a crash between the move and the ref is finished
// there. A head the bare repo lacks moves nothing: the checks find that
// (verifyChecks).
func (d *Daemon) taskHeadFollow(ctx context.Context, n int, is state.Issue) error {
	a := d.Step.Applier
	var latest github.Marker
	for _, m := range is.Markers {
		if m.Kind == "transition" {
			latest = m
		}
	}
	if !newHeadMove(latest) {
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
		return fmt.Errorf("read the new head: %w", err)
	}
	if !has {
		logr.Debugf("%s/%s#%d: its new head %s isn't in the bare repo, so the task's head stays at %q", a.Owner, a.Repo, n, head, at)
		return nil
	}
	if err := d.Clone.UpdateRef(ctx, taskRef(n), head); err != nil {
		return fmt.Errorf("move the task's head to its new head: %w", err)
	}
	logr.Infof("%s/%s#%d: the task's head is now its new head, %s", a.Owner, a.Repo, n, head)
	return nil
}

// workerFree reports whether task n may take the one worker (SPEC §8.1's
// One task at a time), from what the daemon holds, at no request: step 1
// finished on every issue this pass (Daemon.step1Gaps), as for the pick
// (Runs.pickGaps), since a task holding the worker may be one it couldn't
// see; no open issue but n that the daemon holds a snapshot of is at
// in-progress or verifying, by its stage labels (pickHolds), which a move
// written this pass updates in place; and no lease held the worker when
// Work last ended (Daemon.workerLeased). It returns why not, for the log.
func (d *Daemon) workerFree(n int) (bool, string) {
	if len(d.step1Gaps) > 0 {
		return false, "step 1 didn't finish on every issue this pass"
	}
	for _, m := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[m]
		if m == n || s == nil || s.Closed {
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

// prWaitingFor is the move step 2 found for task n at pr-open, whose state
// is is, with its PR p, that waits for the worker (Daemon.prWaiting), and
// true, while it stands: the task hasn't moved since its PR was last
// judged (Daemon.prJudged), and the poll hasn't listed the PR changed
// since (Daemon.prRead). Otherwise the move is dropped, and the PR is
// judged again.
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

// prWait keeps m, the move step 2 found for task n out of pr-open, to make
// once the worker is free (Daemon.prWaiting), logging why it waits: at
// info the pass it's found, and at debug each pass after.
func (d *Daemon) prWait(n int, m Move, why string) {
	a := d.Step.Applier
	log := logr.Infof
	if _, ok := d.prWaiting[n]; ok {
		log = logr.Debugf
	}
	if d.prWaiting == nil {
		d.prWaiting = map[int]Move{}
	}
	d.prWaiting[n] = m
	log("%s/%s#%d: its move to %s on %s waits for the worker, as %s", a.Owner, a.Repo, n, m.To, m.Trigger, why)
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

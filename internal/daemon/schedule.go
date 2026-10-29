package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// pickSearchEvery is how often, by GitHub's clock, the pick searches for
// the blocked ready tasks again though no poll listed a change since its
// last search (SPEC §8.1): a blocked-by link changes no issue's updated_at.
const pickSearchEvery = 10 * time.Minute

// pickTask is step 4's pick (SPEC §8.1), which Work calls once no task at
// in-progress had a run: it picks the next ready task, moves it to
// in-progress, and starts its run in the same pass, through the Stage at
// in-progress, the implementer. It reports whether it picked one.
//
// Nothing is picked while a task holds the one worker (pickHeld), or with
// no Stage at in-progress. The candidates are the open ready issues with a
// stage:ready label and no parent, in pickLess's order. A task with a
// parent waits for its story branch's tip to be named (inStory),
// uncounted. Only once there is a candidate does the pick ask the issue
// search for the blocked ready tasks (pickSearch), and each candidate it
// finds blocked is passed over with no request. The first of the others
// that pickOne picks is the one: a candidate pickOne passes over leaves
// the next its turn.
func (r *Runs) pickTask(ctx context.Context, ready []Ready) (bool, error) {
	st, ok := r.Stages[state.InProgress]
	if !ok {
		return false, nil
	}
	switch held, err := r.pickHeld(ctx, ready); {
	case err != nil:
		return false, fmt.Errorf("the worker: %w", err)
	case held:
		return false, nil
	}
	var candidates []Ready
	for _, rd := range ready {
		if rd.S.Closed || stageOf(rd.S) != state.Ready || inStory(st, rd.S) {
			continue
		}
		candidates = append(candidates, rd)
	}
	if len(candidates) == 0 {
		return false, nil
	}
	less := pickLess(r.Daemon.snaps)
	slices.SortStableFunc(candidates, func(a, b Ready) int { return less(a.S, b.S) })
	blocked, err := r.pickSearch(ctx)
	if err != nil {
		return false, fmt.Errorf("the search: %w", err)
	}
	for _, rd := range candidates {
		if blocked[rd.S.Number] {
			logr.Debugf("%s/%s#%d isn't picked: the issue search finds it blocked", r.Daemon.Step.Owner, r.Daemon.Step.Repo, rd.S.Number)
			continue
		}
		switch picked, err := r.pickOne(ctx, rd, st); {
		case err != nil:
			return picked, fmt.Errorf("a candidate: %w", err)
		case picked:
			return true, nil
		}
	}
	return false, nil
}

// pickHeld reports whether a task holds the one worker (SPEC §8.1): a
// lease holds a run, or an open issue Work got or the daemon holds a
// snapshot of is at in-progress or verifying, by its stage labels, which
// step 1 keeps in line with its latest marker (SPEC §5.1), so it costs no
// request. A task at clarifying, needs-human, pr-open or held waits on
// the owner, and releases it.
func (r *Runs) pickHeld(ctx context.Context, ready []Ready) (bool, error) {
	l, ok, err := r.readLease(ctx)
	if err != nil {
		return false, fmt.Errorf("the worker's lease: %w", err)
	}
	if ok && !l.Failed {
		logr.Debugf("no task is picked: run %s on #%d holds the worker", l.RunID, l.Issue)
		return true, nil
	}
	snaps := make([]*github.Snapshot, 0, len(ready)+len(r.Daemon.snaps))
	for _, rd := range ready {
		snaps = append(snaps, rd.S)
	}
	for _, s := range r.Daemon.snaps {
		snaps = append(snaps, s)
	}
	for _, s := range snaps {
		if s.Closed {
			continue
		}
		if i := slices.IndexFunc(s.Labels, pickHolds); i >= 0 {
			logr.Debugf("no task is picked: #%d, at %s, holds the worker", s.Number, s.Labels[i])
			return true, nil
		}
	}
	return false, nil
}

// pickHolds reports whether label is the stage label of a task that holds
// the worker: in-progress or verifying.
func pickHolds(label string) bool {
	st, ok := state.ParseLabel(label)
	return ok && (st == state.InProgress || st == state.Verifying)
}

// pickLess orders the pick's candidates (SPEC §8.1): by priority, then the
// parent's priority, from its snapshot in snaps, or the task's own when it
// has no parent or none is held, then by number.
func pickLess(snaps map[int]*github.Snapshot) func(a, b *github.Snapshot) int {
	parent := func(s *github.Snapshot) int {
		if p := snaps[s.Parent]; s.Parent != 0 && p != nil {
			return priority(p)
		}
		return priority(s)
	}
	return func(a, b *github.Snapshot) int {
		return cmp.Or(cmp.Compare(priority(a), priority(b)), cmp.Compare(parent(a), parent(b)), cmp.Compare(a.Number, b.Number))
	}
}

// pickSearch returns the open ready tasks the issue search finds blocked
// (github.Client.ReadyBlocked): its last answer, asked again only when a
// poll listed a change since (Daemon.pickStale), as a start's first poll
// does, or once pickSearchEvery has passed by GitHub's clock. A
// repeat with nothing changed is a 304. The search lags behind dependency
// writes, so pickOne confirms whatever it would start.
func (r *Runs) pickSearch(ctx context.Context) (map[int]bool, error) {
	st := r.Daemon.Step
	now := st.Client.Now()
	if !r.Daemon.pickStale && now.Sub(r.pickSearched) < pickSearchEvery {
		return r.pickBlocked, nil
	}
	blocked, err := st.Client.ReadyBlocked(ctx, st.Owner, st.Repo)
	if err != nil {
		return nil, fmt.Errorf("the blocked ready tasks: %w", err)
	}
	r.pickBlocked, r.pickSearched, r.Daemon.pickStale = blocked, now, false
	return blocked, nil
}

// pickText is the text of the pick's move.
const pickText = "I've picked this task, so the implementer starts on it now."

// pickOne picks the ready task rd, unless it's passed over, and reports
// whether it picked it. It's passed over, with nothing written, when its
// latest marker doesn't put it at ready, or it's no task that gets a PR
// (state.AnyTask), or it has an open blocker. Labels that give its run no
// settings (state.ErrLabels) pass it over too, with their state error, as
// a run would get (reportLabels), once for each change to them that stands
// (labelsKey).
//
// Its blockers are read through blocked_by (github.Client.Blockers), with
// a conditional request, as the search may lag behind a link: one with an
// open blocker joins the search's answer, so it's read no more until the
// next search. Then it moves to in-progress, keyed by the comment holding
// its move to ready, event=s<comment ID>, with seen= the newest comment,
// and its run starts (RunStage) on the snapshot the move left, so the
// run's seen is the move's comment, and the move doesn't drop its result
// (SPEC §8.2).
func (r *Runs) pickOne(ctx context.Context, rd Ready, st Stage) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error { return fmt.Errorf("%s/%s#%d: %w", a.Owner, a.Repo, n, err) }
	dirt, err := a.Dirty(ctx, rd.S, r.Daemon.Step.Trust)
	if err != nil {
		return false, wrap(err)
	}
	is := state.Derive(rd.S, a.AppSlug, dirt.Has)
	if class, err := state.ClassOf(is.Item); is.Stage != state.Ready || err != nil || class&state.AnyTask == 0 {
		logr.Debugf("%s/%s#%d isn't picked: it's no task at ready, by its markers", a.Owner, a.Repo, n)
		return false, nil
	}
	_, badLabels := state.RunSettings(state.Agent(st.Agent()), rd.S.Labels, r.Daemon.Config, 1)
	switch {
	case errors.Is(badLabels, state.ErrLabels) && is.Reported(kindLabels, labelsKey(rd.S)):
		logr.Debugf("%s/%s#%d isn't picked: its model and effort labels give no settings, as reported: %v", a.Owner, a.Repo, n, badLabels)
		return false, nil
	case errors.Is(badLabels, state.ErrLabels):
		// Step 1 checked the labels as rd.S has them this pass, or the poll
		// found them unchanged since, so rd.S is the issue as read again.
		r.Daemon.reread(n, false)
		if err := r.reportLabels(ctx, rd.S, rd.S, state.Ready, st, badLabels); err != nil {
			return false, wrap(err)
		}
		return false, nil
	case badLabels != nil:
		return false, wrap(badLabels)
	}
	blockers, err := a.Client.Blockers(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return false, wrap(err)
	}
	if i := slices.IndexFunc(blockers, func(b github.Blocker) bool { return !b.Closed }); i >= 0 {
		logr.Infof("%s/%s#%d isn't picked: #%d blocks it, though the issue search didn't find it blocked yet", a.Owner, a.Repo, n, blockers[i].Number)
		if r.pickBlocked == nil {
			r.pickBlocked = map[int]bool{}
		}
		r.pickBlocked[n] = true
		return false, nil
	}
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	m := Move{To: state.InProgress, Trigger: state.TriggerScheduled, Key: state.Key{Field: "event", ID: "s" + strconv.FormatInt(is.TransitionComment, 10)},
		Text: pickText}
	if seen > 0 {
		m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
	}
	defer r.Daemon.reread(n, false)
	if err := a.Transition(ctx, rd.S, dirt.Has, m); err != nil {
		return false, wrap(err)
	}
	logr.Infof("%s/%s#%d: picked, at priority p%d, and moved to %s", a.Owner, a.Repo, n, priority(rd.S), state.InProgress)
	if _, err := r.RunStage(ctx, Ready{S: rd.S}, st); err != nil {
		return true, wrap(err)
	}
	return true, nil
}

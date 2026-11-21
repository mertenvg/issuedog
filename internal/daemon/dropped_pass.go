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

// This file is step 2 for the issues a revision held for a PR in place of
// their close (droppedOne, SPEC §6.4): a task whose PR was open when its
// parent's newly approved revision dropped it, and each open issue above
// it in the dropped subtree, each held with reason=dropped. Such a hold
// ends only with the issue's close:
//   - a task's PR that merges closes it by its "Closes #N", which the
//     close judge accepts (closingAccepted), held or not, and so does a
//     trusted user's close, the owner's included: neither is this file's;
//   - a task's PR closed without merging, as the poll lists it
//     (Daemon.prs), closes it as not planned, here (droppedPass);
//   - an issue above it closes the same way once nothing below it is
//     open, as the snapshots held show, on the pass that closes the last
//     issue below it or a later one.
//
// It's judged from what the poll listed and the snapshots held, at no
// request, so a pass whose poll lists no change to a held issue's PR, or
// to an issue below it, makes none here (SPEC §8.1). Only the close makes
// requests: the issue's read for its ID, the reads and removals of its
// dependency links (droppedUnlink), then the move.

// droppedSet records the dirty content of s, an issue step 1 just
// succeeded on with dirt, while it's held for a PR (droppedHeld), for
// droppedPass's move, and forgets it otherwise.
func (d *Daemon) droppedSet(s *github.Snapshot, dirt Dirt) {
	if _, ok := droppedHeld(state.Derive(s, d.Step.AppSlug, dirt.Has)); !ok || s.Closed {
		delete(d.droppedDirt, s.Number)
		return
	}
	if d.droppedDirt == nil {
		d.droppedDirt = map[int]Dirt{}
	}
	d.droppedDirt[s.Number] = dirt
}

// droppedPROpen is Step.PROpenOf: the PR of task is (taskPR), and whether
// the poll lists it open, as one the App opened (Daemon.prs).
func (d *Daemon) droppedPROpen(is state.Issue) (int, bool) {
	pr := taskPR(is)
	p, ok := d.prs[pr]
	return pr, ok && p.Open && !p.Merged
}

// droppedPass closes each issue held for a PR (droppedSet) whose hold has
// ended, step 2 (SPEC §6.4, §8.1), after prPass and before the digest and
// assignment, deepest first, so an issue above one it closes closes on the
// same pass. A held issue whose step 1 failed this pass, or that a
// command touched, waits for its next pass, as in prPass. One ends when:
//   - its PR (taskPR), as the poll lists it, is closed without merging, or
//     it names none the App opened, as an issue above a task has none. A
//     PR listed open, or merged, whose "Closes #N" closes the task, leaves
//     it held; and
//   - no open issue is below it, as the snapshots held show (droppedBelow).
//
// Its close removes every dependency link to and from it first
// (droppedUnlink), the issue's ID read for that, then moves it to closed as
// not planned, keyed apart from the hold (droppedClosedKey), with a comment
// saying why. So a crash between the two leaves it held, without links,
// and the next pass closes it, removing none twice. A close the table
// refuses is a state error, reported once on the issue, and logged. The
// issues it closes get their items found again, so the digest and
// assignment show the close this pass, and each is read again next pass.
// It returns them, for assignment (assignPass). A failed read or write is
// the pass's error, and only that issue's, and its ancestors wait too: the
// next pass judges them again.
func (d *Daemon) droppedPass(ctx context.Context) (map[int]bool, error) {
	a := d.Step.Applier
	closed := map[int]bool{}
	var errs []error
	for _, n := range d.droppedOrder() {
		s, dirt := d.snaps[n], d.droppedDirt[n]
		if _, visited := d.inputs[n]; s == nil || s.Closed || d.stale[n] || !visited {
			continue
		}
		is := state.Derive(s, a.AppSlug, dirt.Has)
		hold, ok := droppedHeld(is)
		if !ok {
			continue
		}
		pr := taskPR(is)
		if p, listed := d.prs[pr]; listed && (p.Open || p.Merged) {
			continue
		}
		if below := d.droppedBelow(n); below != 0 {
			logr.Debugf("%s/%s#%d: held for a PR, and #%d below it is open, so it stays held", a.Owner, a.Repo, n, below)
			continue
		}
		if err := d.droppedEnd(ctx, s, dirt, hold, pr); err != nil {
			d.reread(n, false)
			errs = append(errs, fmt.Errorf("close %s/%s#%d, held for a PR: %w", a.Owner, a.Repo, n, err))
			continue
		}
		d.digestCacheSet(s, d.inputs[n], dirt, nil, false, false)
		d.reread(n, false)
		closed[n] = true
	}
	return closed, errors.Join(errs...)
}

// droppedEnd closes s, held for a PR by hold with dirt, whose task PR,
// pr, closed without merging, or with pr naming none, nothing below it is
// open: its links go first (droppedUnlink), then it moves to closed, as
// not planned, keyed apart from hold (droppedClosedKey). A state error is
// logged, and left as Transition reported it.
func (d *Daemon) droppedEnd(ctx context.Context, s *github.Snapshot, dirt Dirt, hold github.Marker, pr int) error {
	a, n := d.Step.Applier, s.Number
	key, ok := droppedHoldKey(hold)
	if !ok {
		return fmt.Errorf("its hold's marker %v has no key", hold.Fields)
	}
	listed, _, err := a.Client.Issues.Get(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return fmt.Errorf("read its ID: %w", err)
	}
	if err := d.Step.droppedUnlink(ctx, n, listed.GetID()); err != nil {
		return fmt.Errorf("unlink: %w", err)
	}
	text := "Closed as not planned: nothing below this issue is open now, and a plan's revision dropped it."
	if _, ok := d.prs[pr]; ok {
		text = fmt.Sprintf("Closed as not planned: its PR #%d was closed without merging, and a plan's revision dropped it.", pr)
	}
	m := Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed, Key: droppedClosedKey(key), Text: text}
	err = a.Transition(ctx, s, dirt.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: left open and held, though its hold for a PR has ended: %v", a.Owner, a.Repo, n, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}
	logr.Infof("%s/%s#%d: closed as not planned, as its hold for a PR has ended", a.Owner, a.Repo, n)
	return nil
}

// droppedHoldKey is the key hold, a hold's transition marker for a PR
// (droppedOne), carries, as droppedKey chose it: its event=, or else any
// key field, and whether it has one.
func droppedHoldKey(hold github.Marker) (state.Key, bool) {
	for _, f := range keyFields {
		if id := hold.Fields[f]; id != "" {
			return state.Key{Field: f, ID: id}, true
		}
	}
	return state.Key{}, false
}

// droppedBelow is an open issue below n, as the snapshots held show, or 0
// for none: one whose snapshot names n as its parent, or one closed with
// its close still to judge, or reopened by that judging, whose last
// snapshot named n (Daemon.closingBelow), as it may be open again.
func (d *Daemon) droppedBelow(n int) int {
	for _, m := range slices.Sorted(maps.Keys(d.snaps)) {
		if s := d.snaps[m]; s != nil && !s.Closed && s.Parent == n && s.ParentRepo == "" {
			return m
		}
	}
	for _, m := range slices.Sorted(maps.Keys(d.closingBelow)) {
		if d.closingBelow[m] == n {
			return m
		}
	}
	return 0
}

// droppedOrder is each issue held for a PR (Daemon.droppedDirt), deepest
// first, as the parents in the snapshots held show, then by number, so an
// issue above one droppedPass closes comes after it.
func (d *Daemon) droppedOrder() []int {
	depth := func(n int) int {
		k, seen := 0, map[int]bool{}
		for s := d.snaps[n]; s != nil && s.Parent != 0 && s.ParentRepo == "" && !seen[s.Parent]; s = d.snaps[s.Parent] {
			seen[s.Parent] = true
			k++
		}
		return k
	}
	order := slices.Sorted(maps.Keys(d.droppedDirt))
	slices.SortStableFunc(order, func(x, y int) int { return depth(y) - depth(x) })
	return order
}

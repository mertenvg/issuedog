package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// An epic planned into stories closes once every child is closed (#251;
// SPEC §7.2's planned → closed row for it, §7.3). It has no story branch
// and no story verification of its own: each of its stories has them. An
// epic planned into tasks acts as its own story (SPEC §4), so it waits
// for story verification instead, as a finished story's item (#278).
//
// Its children are read as a finished story's are (epicCloseChildren,
// which waitDoneChildren takes too): those the clean record of its
// approved plan revision's children names, so a child that revision
// dropped (SPEC §6.4) counts for nothing. A child's close changes nothing
// on the epic, so, as for a finished story, they're read on the epic's
// last visit (Daemon.epicCloseSet), and judged on each pass from what the
// poll listed and the closes judged (digestDoneStands), with no request.
// Only once every one is listed closed, its close judged and accepted, is
// the epic read again and closed (Daemon.epicCloseOne).

// epicCloseChildren returns the children of s, an issue at planned with
// state is, that the clean record of its approved plan revision's
// children names (state.CreatedRecord, #277), in its order, and whether
// s is an epic planned into stories (#251). The children are nil for an
// issue of any type but a story or an epic, or with no approved plan
// revision or no record of it yet, as while its children are still being
// created. Only an epic's plan data is read, to tell a plan of stories
// from one of tasks, and data that doesn't read back is an error.
func epicCloseChildren(s *github.Snapshot, appSlug string, dirty func(int64) bool, is state.Issue) ([]int, bool, error) {
	rev, _, ok := state.ApprovedPlan(s, appSlug, dirty)
	if !ok {
		return nil, false, nil
	}
	children, ok := state.CreatedRecord(is, rev.Rev)
	if !ok {
		return nil, false, nil
	}
	class, err := state.ClassOf(is.Item)
	switch {
	case err != nil: // a type that names no kind of issue is no story
	case class&state.AnyStory != 0:
		return children, false, nil
	case class == state.ClassEpic:
		_, plan, ok, err := approvedPlanData(s, appSlug, dirty)
		if err != nil {
			return nil, false, fmt.Errorf("an epic's plan: %w", err)
		}
		if ok {
			return children, plan.Kind == planStories, nil
		}
	}
	return nil, false, nil
}

// epicCloseKey keys the close of an epic planned into stories (#251):
// event=p<id>, by id, its comment holding the move into planned that
// approved the plan its record names (state.ApprovedPlanMove), as a
// created child's first move is keyed. So the epic closes this way once
// for each approved plan: after a trusted reopen, which holds it (SPEC
// §6.4), and a /resume back to planned, it stays open till the owner
// closes it or approves a revision.
func epicCloseKey(id int64) state.Key {
	return state.Key{Field: "event", ID: "p" + strconv.FormatInt(id, 10)}
}

// epicCloseFind returns the children of s, with state is, that must all
// be closed for it to close (epicCloseChildren), and the key of its
// close (epicCloseKey), or false if s isn't an open epic planned into
// stories at planned with a record of its children, or if a close keyed
// so is written already.
func epicCloseFind(s *github.Snapshot, appSlug string, dirty func(int64) bool, is state.Issue) ([]int, state.Key, bool, error) {
	if s.Closed || is.Stage != state.Planned {
		return nil, state.Key{}, false, nil
	}
	children, stories, err := epicCloseChildren(s, appSlug, dirty, is)
	if err != nil || !stories || children == nil {
		return nil, state.Key{}, false, err
	}
	move, ok := state.ApprovedPlanMove(s, appSlug, dirty)
	if !ok {
		return nil, state.Key{}, false, nil
	}
	key := epicCloseKey(move.ID)
	if is.Moved(key) {
		return nil, state.Key{}, false, nil
	}
	return children, key, true, nil
}

// epicCloseSet keeps, for s, as step 1 left it, with its dirty content
// dirt, the children it waits on to close (epicCloseFind), or forgets s
// if it waits on none. Iterate calls it beside digestCacheSet, on each
// issue step 1 succeeds on. A plan's data that doesn't read back is
// logged, and s waits on nothing till its next visit.
func (d *Daemon) epicCloseSet(s *github.Snapshot, dirt Dirt) {
	st := d.Step
	is := state.Derive(s, st.AppSlug, dirt.Has)
	children, _, ok, err := epicCloseFind(s, st.AppSlug, dirt.Has, is)
	if err != nil {
		logr.Errorf("%s/%s#%d isn't closed with its children: %v", st.Owner, st.Repo, s.Number, err)
	}
	if err != nil || !ok {
		delete(d.epicCloseDue, s.Number)
		return
	}
	if d.epicCloseDue == nil {
		d.epicCloseDue = map[int][]int{}
	}
	d.epicCloseDue[s.Number] = children
}

// epicClosePass closes each epic epicCloseSet keeps whose children have
// all closed (epicCloseOne). Iterate calls it after the visits. Each one
// is judged as a finished story's item is (digestDoneStands): every child
// its record names is one the poll last listed closed, whose close was
// judged and accepted, and no open snapshot names the epic as its parent.
// That makes no request, so a pass with none to close costs nothing. A
// child whose close is still to judge, which may be untrusted, keeps the
// epic open (SPEC §6.4), and so does one whose close is never judged, as
// for a child with no stage label (SPEC §16). An epic that left the
// pipeline is forgotten, and one to be read again, as when its visit
// failed or a command touched it, waits for its next good visit: step 1
// may hold it then, as for a discussion trigger, which epicCloseOne's
// fresh read doesn't look for.
func (d *Daemon) epicClosePass(ctx context.Context) error {
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(d.epicCloseDue)) {
		if d.snaps[n] == nil {
			delete(d.epicCloseDue, n)
			continue
		}
		if d.stale[n] {
			continue
		}
		if !d.digestDoneStands(waitItem{Issue: n, Children: d.epicCloseDue[n]}) {
			continue
		}
		if err := d.epicCloseOne(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// epicCloseOne closes epic n, whose children have all closed, read fresh:
// the label rules run on it first (discCheckLabels), as for any move, and
// it's judged again from that read (epicCloseFind, digestDoneStands).
// Then it moves to closed as completed (Transition, planned → closed on
// children-closed), keyed by epicCloseKey, with one comment naming the
// children, and the close judge sets stage:completed on a later pass
// (closingJudge). Either way it's read again on the next pass, and gets
// no Work on this one. A close the table refuses is a state error that
// Transition reports once on the epic: it's logged, and the epic waits on
// nothing till its next visit.
func (d *Daemon) epicCloseOne(ctx context.Context, n int) error {
	st := d.Step
	delete(d.epicCloseDue, n)
	delete(d.inputs, n)
	d.stale[n] = true
	s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	dirt, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	err = st.discCheckLabels(ctx, s, st.Trust, dirt)
	if errors.Is(err, ErrLabelsHeld) {
		logr.Infof("%s/%s#%d waits for its next pass to close with its children: %v", st.Owner, st.Repo, n, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	is := state.Derive(s, st.AppSlug, dirt.Has)
	children, key, ok, err := epicCloseFind(s, st.AppSlug, dirt.Has, is)
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	if !ok || !d.digestDoneStands(waitItem{Issue: n, Children: children}) {
		logr.Debugf("%s/%s#%d isn't an epic whose children have all closed, as read again, so it stays open", st.Owner, st.Repo, n)
		return nil
	}
	m := Move{To: state.Closed, StateReason: "completed", Trigger: state.TriggerChildrenClosed, Key: key, Text: epicCloseText(children)}
	err = st.Transition(ctx, s, dirt.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: left open, though every child of its plan is closed: %v", st.Owner, st.Repo, n, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	logr.Infof("%s/%s#%d: closed as completed, as every child of its plan is closed", st.Owner, st.Repo, n)
	return nil
}

// epicCloseText is the comment of an epic's close (#251), naming the
// children its record names, or saying its record names none, as after a
// revision that drops every child. Transition escapes it, though only
// numbers are in it.
func epicCloseText(children []int) string {
	if len(children) == 0 { // a revision left none (state.CreatedNone, #297)
		return "Its plan's latest revision left this epic no children, so I've closed it as completed."
	}
	refs := make([]string, len(children))
	for i, c := range children {
		refs[i] = "#" + strconv.Itoa(c)
	}
	return "Every child of this epic is closed, so I've closed it as completed. Its children: " + strings.Join(refs, ", ") + "."
}

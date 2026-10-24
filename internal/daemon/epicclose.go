package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

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
// the epic read again and closed (Daemon.epicCloseOne): as completed if
// any of them closed as completed, and else as not planned.

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
// event=p<id>, by id, its comment holding its latest move into planned
// (state.Issue.TransitionComment), as a created child's first move is
// keyed by a move into planned. So the epic closes this way once each
// time it reaches planned (SPEC §7.3): after a trusted reopen, which
// holds it (SPEC §6.4), the /resume back to planned is a new move, and
// the epic closes again once its children have, a story the owner
// added below it by hand included.
func epicCloseKey(id int64) state.Key {
	return state.Key{Field: "event", ID: "p" + strconv.FormatInt(id, 10)}
}

// epicCloseFind returns the children of s, with state is, that must all
// be closed for it to close (epicCloseChildren), and the key of its
// close (epicCloseKey), or false if s isn't an open epic planned into
// stories at planned with a record of its children. A close keyed so is
// never written already: its marker moves the epic to closed.
func epicCloseFind(s *github.Snapshot, appSlug string, dirty func(int64) bool, is state.Issue) ([]int, state.Key, bool, error) {
	if s.Closed || is.Stage != state.Planned {
		return nil, state.Key{}, false, nil
	}
	children, stories, err := epicCloseChildren(s, appSlug, dirty, is)
	if err != nil || !stories || children == nil {
		return nil, state.Key{}, false, err
	}
	if is.TransitionComment == 0 { // a move is always a comment
		return nil, state.Key{}, false, nil
	}
	return children, epicCloseKey(is.TransitionComment), true, nil
}

// epicCloseSet keeps, for s, as step 1 left it, with its dirty content
// dirt, the children it waits on to close (epicCloseFind), or forgets s
// if it waits on none, with the issues that held it open
// (epicCloseHeld). Iterate calls it beside digestCacheSet, on each
// issue step 1 succeeds on. A plan's data that doesn't read back has s
// wait on nothing till its next visit. It isn't logged here: the reading
// is one (epicCloseChildren), so digestCacheSet, called just before on
// the same issue, logs the same error, as waitFind's (waitDoneChildren).
func (d *Daemon) epicCloseSet(s *github.Snapshot, dirt Dirt) {
	st := d.Step
	is := state.Derive(s, st.AppSlug, dirt.Has)
	children, _, ok, err := epicCloseFind(s, st.AppSlug, dirt.Has, is)
	if err != nil || !ok {
		delete(d.epicCloseDue, s.Number)
		delete(d.epicCloseHeld, s.Number)
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
// for a child with no stage label (SPEC §16), and so does an issue that
// was held open below it whose close is still to judge (closingBelowSet).
// So does an open issue whose link below the epic someone untrusted
// removed, once epicCloseOne found it, till it changes (epicCloseHolds),
// with no request either. An epic that left
// the pipeline is forgotten, and one to be read again, as when its visit
// failed or a command touched it, waits for its next good visit: step 1
// may hold it then, as for a discussion trigger, which epicCloseOne's
// fresh read doesn't look for.
func (d *Daemon) epicClosePass(ctx context.Context) error {
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(d.epicCloseDue)) {
		if d.snaps[n] == nil {
			delete(d.epicCloseDue, n)
			delete(d.epicCloseHeld, n)
			continue
		}
		if d.stale[n] || d.epicCloseHolds(n) {
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
// An open issue whose link below it someone untrusted removed holds it
// open then (epicCloseUnlinked), from that read's removals, as one still
// below it does, and is kept for epicClosePass to judge with no request.
// Else it moves to closed
// (Transition, planned → closed on children-closed), as completed if any
// child closed as completed and else as not planned (epicCloseReason),
// keyed by epicCloseKey, with one comment naming the children, and the
// close judge sets the closed stage that names on a later pass
// (closingJudge). Either way it's read again on the next pass, and gets
// no Work on this one. epicCloseFind finds only an epic at planned, which
// the table moves to closed, so Transition reports no state error.
func (d *Daemon) epicCloseOne(ctx context.Context, n int) error {
	st := d.Step
	delete(d.epicCloseDue, n)
	delete(d.epicCloseHeld, n)
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
	held, err := d.epicCloseUnlinked(ctx, s)
	if err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	if len(held) > 0 {
		if d.epicCloseHeld == nil {
			d.epicCloseHeld = map[int]map[int]time.Time{}
		}
		d.epicCloseHeld[n] = map[int]time.Time{}
		for _, c := range held {
			var at time.Time // none for one closed, with a close to judge
			if cs := d.snaps[c]; cs != nil {
				at = cs.UpdatedAt
			}
			d.epicCloseHeld[n][c] = at
		}
		logr.Infof("%s/%s#%d stays open, as someone untrusted took open issues %v from below it", st.Owner, st.Repo, n, held)
		return nil
	}
	reason := d.epicCloseReason(children)
	m := Move{To: state.Closed, StateReason: reason, Trigger: state.TriggerChildrenClosed, Key: key, Text: epicCloseText(children, reason)}
	if err := st.Transition(ctx, s, dirt.Has, m); err != nil {
		return fmt.Errorf("close %s/%s#%d with its children: %w", st.Owner, st.Repo, n, err)
	}
	logr.Infof("%s/%s#%d: closed as %s, as every child of its plan is closed", st.Owner, st.Repo, n, reason)
	return nil
}

// epicCloseReason is the state reason of the close of an epic whose
// children have all closed, their closes judged and accepted
// (digestDoneStands): completed if any of them closed as completed, as
// the closed stage its judged close left shows (closingSeen), and else
// not_planned, as when the owner closed every one as not planned, or
// when its record names none, after a revision that drops every child.
func (d *Daemon) epicCloseReason(children []int) string {
	done, _ := state.Completed.Label()
	for _, c := range children {
		if slices.Contains(strings.Split(d.closingSeen[c].stages, ","), done) {
			return "completed"
		}
	}
	return "not_planned"
}

// epicCloseUnlinked returns the issues that may still be open
// (closingUnsettled) whose link below s, an epic as just read, someone
// untrusted removed (SPEC §7.3): each holds the epic open, as an open
// issue still below it does (digestDoneStands), so an untrusted user
// can't close an epic by taking an issue the owner added by hand from
// below it, nor by closing it once taken, while that close is still to
// judge. Each sub-issue's newest removal on s's timeline counts
// (Snapshot.SubIssueRemovals, read with s's comments, so it costs no
// request): the App's or a trusted user's (closingTrusted) lets the issue
// go. One of an issue in another repo counts for nothing, as no snapshot
// is held of it.
func (d *Daemon) epicCloseUnlinked(ctx context.Context, s *github.Snapshot) ([]int, error) {
	st := d.Step
	newest := map[int]github.SubIssueRemoval{}
	for _, r := range s.SubIssueRemovals {
		if strings.EqualFold(r.Repo, st.Owner+"/"+st.Repo) {
			newest[r.Number] = r
		}
	}
	var held []int
	for _, c := range slices.Sorted(maps.Keys(newest)) {
		if !d.closingUnsettled(c) || state.IsApp(newest[c].By, st.AppSlug) {
			continue
		}
		ok, err := closingTrusted(ctx, st.Trust, newest[c].By)
		if err != nil {
			return nil, fmt.Errorf("who took #%d from below it: %w", c, err)
		}
		if !ok {
			held = append(held, c)
		}
	}
	return held, nil
}

// epicCloseHolds reports whether the issues epicCloseUnlinked found
// holding epic n open still do: one may still be open (closingUnsettled),
// and none changed since, as its snapshot held shows. One that changed, as
// when the owner linked it below the epic again and removed that link, has
// epicClosePass judge the epic again, from a fresh read (epicCloseOne), so
// a later trusted removal lets it go. It makes no request, so a pass that
// changed nothing costs nothing while it holds.
func (d *Daemon) epicCloseHolds(n int) bool {
	held := d.epicCloseHeld[n]
	for c, at := range held {
		if s := d.snaps[c]; s != nil && !s.UpdatedAt.Equal(at) {
			return false
		}
	}
	return slices.ContainsFunc(slices.Collect(maps.Keys(held)), d.closingUnsettled)
}

// epicCloseText is the comment of an epic's close (#251) with state
// reason reason (epicCloseReason), naming the children its record names,
// or saying its record names none, as after a revision that drops every
// child. Transition escapes it, though only numbers are in it.
func epicCloseText(children []int, reason string) string {
	if len(children) == 0 { // a revision left none (state.CreatedNone, #297)
		return "Its plan's latest revision left this epic no children, so I've closed it as not planned."
	}
	refs := make([]string, len(children))
	for i, c := range children {
		refs[i] = "#" + strconv.Itoa(c)
	}
	closed := "Every child of this epic is closed, so I've closed it as completed."
	if reason != "completed" {
		closed = "Every child of this epic is closed, none as completed, so I've closed it as not planned."
	}
	return closed + " Its children: " + strings.Join(refs, ", ") + "."
}

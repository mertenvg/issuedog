package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// This file applies the rest of an approved revision to its children
// (#297; SPEC §6.4), after its new children are filed and
// linked and its changed ones rewritten (childWrite, childrenChanged): a
// changed child's links to its siblings follow the revision
// (revisedLink), and each dropped item's child is closed as not planned,
// with every open issue below it, deepest first, and loses its links
// (droppedClose), but for a task whose PR is open, which is held for its
// PR instead, with each open issue above it in the dropped subtree
// (droppedOne): step 2 closes those once their PR merges or closes, or
// nothing below them is open (dropped_pass.go). ChildrenVisit posts the
// revision's record only after these, so once it stands no pass reads the
// children again.

// revisedLink makes the links of each changed item's child, among items,
// a revision's in plan order, to its siblings follow the revision: it
// adds a blocked-by link to the child of each item its "Depends on"
// names that the child lacks, then removes each of its blocked-by links
// to a sibling that the revision no longer lists. A sibling is a child of
// any item of the plan, in children, by issue ID, found or filed, a
// dropped item's included. A link to any other issue, such as one filed
// by hand or in another repo, stays. Its blocked-by links are read first
// (BlockedBy), so a replay adds and removes none twice, and the child is
// reported to Touched before its first write. A changed child that's
// closed gets nothing, as childrenChanged gives it nothing either: one
// the listing found closed, or one childrenChanged marked closed, as
// when the label rules closed it in this pass. Each changed item's child is
// in children, and so is each dependency's, as a changed item depends on no
// dropped one (SPEC §11.3), a new one's child is filed by now, and
// ChildrenVisit stops a revision whose kept or changed child isn't found
// before any write (childrenGone): one that isn't is an error, as in
// childrenLink. A link GitHub refuses to add with a 422 is a failure no
// later pass gets past, as in childrenLink (childBlockErr), unless it
// stands by then (childBlock), and so is one it refuses to remove with a
// 422 (childUnblockErr). The links of a kept child, and the links into a
// changed child from its siblings, are each the other child's own: a kept
// item keeps its dependencies (keptDeps), and a new or changed one's are
// its own item's.
func (st *Step) revisedLink(ctx context.Context, items []planItem, children map[string]childFound) error {
	siblings := make(map[int64]bool, len(children))
	for _, f := range children {
		siblings[f.ID] = true
	}
	for _, it := range items {
		if it.Status != planChanged {
			continue
		}
		c, ok := children[it.ID]
		if !ok {
			return fmt.Errorf("item %s: the revision changes it, but it has no child", it.ID)
		}
		if c.Closed {
			logr.Debugf("%s/%s: item %s's child is closed, so its links to its siblings are left as they are", st.Owner, st.Repo, it.ID)
			continue
		}
		listed, err := st.Client.BlockedBy(ctx, st.Owner, st.Repo, c.Number)
		if err != nil {
			return fmt.Errorf("item %s: %w", it.ID, err)
		}
		has := make(map[int64]bool, len(listed))
		for _, is := range listed {
			has[is.GetID()] = true
		}
		want := map[int64]bool{}
		for _, dep := range it.DependsOn {
			b, ok := children[dep]
			if !ok {
				return fmt.Errorf("item %s: it depends on %s, which has no child", it.ID, dep)
			}
			want[b.ID] = true
			if has[b.ID] {
				continue
			}
			st.touch(c.Number)
			added, err := st.childBlock(ctx, it.ID, c, dep, b)
			if err != nil {
				return fmt.Errorf("revised links: %w", err)
			}
			if added {
				logr.Infof("%s/%s#%d: blocked by #%d, as item %s of a plan's revision depends on item %s", st.Owner, st.Repo, c.Number, b.Number, it.ID, dep)
			}
		}
		for _, is := range listed {
			if !siblings[is.GetID()] || want[is.GetID()] {
				continue
			}
			st.touch(c.Number)
			if err := st.Client.RemoveBlockedBy(ctx, st.Owner, st.Repo, c.Number, is.GetID()); err != nil {
				return fmt.Errorf("item %s: %w", it.ID, childUnblockErr(err, c.Number, is.GetNumber()))
			}
			logr.Infof("%s/%s#%d: no longer blocked by #%d, as item %s of a plan's revision no longer depends on its item", st.Owner, st.Repo, c.Number, is.GetNumber(), it.ID)
		}
	}
	return nil
}

// droppedClose closes the child of each dropped item of items, revision
// rev's of parent's approved plan, with children each item's child, and
// every open issue below it (SPEC §6.4): deepest first, each through the
// App's move to closed, as not planned (Transition), keyed by key, the
// parent's move into planned, but for a task whose PR is open, and each
// open issue above it, which are held instead (droppedOne). plan is the
// revision's plan comment, which each close's and hold's comment links. A
// dropped item whose child isn't found is left alone: childrenRecord
// names it.
func (st *Step) droppedClose(ctx context.Context, parent *github.Snapshot, rev int, plan github.Comment, key state.Key,
	items []planItem, children map[string]childFound) error {
	for _, it := range items {
		if it.Status != planDropped {
			continue
		}
		f, ok := children[it.ID]
		if !ok {
			continue
		}
		d := droppedItem{parent: parent.Number, rev: rev, item: it.ID, root: f.Number, key: key,
			plan: waitLink(st.Owner, st.Repo, parent.Number, plan.ID)}
		seen := map[int]bool{parent.Number: true, f.Number: true}
		if _, err := st.droppedWalk(ctx, d, f.Number, f.ID, seen); err != nil {
			return fmt.Errorf("item %s: %w", it.ID, err)
		}
	}
	return nil
}

// droppedItem is a dropped item whose subtree droppedWalk closes: item
// of plan revision rev of issue parent, whose child is issue root, and
// key, the parent's move into planned, which keys each close. plan links
// the revision's plan comment.
type droppedItem struct {
	parent, rev, root int
	item, plan        string
	key               state.Key
}

// droppedWalk closes issue n, whose ID is id, in d's subtree, and every
// issue below it, post-order: each of its sub-issues' subtrees, in the
// order GitHub lists them, then n itself (droppedOne), which is held in
// place of its close when anything below it was. It reports whether n is
// open still, held so or otherwise. seen holds the issues walked already,
// so a loop of links can't walk one twice. A sub-issue in another repo is
// skipped, with everything below it, as family skips one, as is a pull
// request.
func (st *Step) droppedWalk(ctx context.Context, d droppedItem, n int, id int64, seen map[int]bool) (bool, error) {
	subs, err := st.Client.SubIssues(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return false, fmt.Errorf("below #%d: %w", n, err)
	}
	below := false
	for _, is := range subs {
		m := is.GetNumber()
		if !st.inRepo(is.GetRepositoryURL()) {
			logr.Infof("%s/%s#%d: skipping its sub-issue #%d of %s, which isn't in this repo, and isn't closed with it",
				st.Owner, st.Repo, n, m, is.GetRepositoryURL())
			continue
		}
		if is.IsPullRequest() || seen[m] {
			continue
		}
		seen[m] = true
		held, err := st.droppedWalk(ctx, d, m, is.GetID(), seen)
		if err != nil {
			return false, fmt.Errorf("below #%d: %w", n, err)
		}
		below = below || held
	}
	held, err := st.droppedOne(ctx, d, n, id, below)
	if err != nil {
		return false, fmt.Errorf("drop #%d: %w", n, err)
	}
	return held, nil
}

// droppedOne closes issue n, whose ID is id, in d's subtree, read fresh,
// and removes every dependency link to and from it (droppedUnlink), or
// holds it for a PR, and reports whether it's left open, held so or not:
//   - held for a PR already (droppedHeld), as by a pass that stopped part
//     way after its hold, it gets no write;
//   - open, a task whose PR the poll lists open (Step.PROpenOf), or an
//     issue with one held so below it, below, is held for it (SPEC §6.4):
//     it's reported to Touched, the label rules run on it first, then it
//     moves to held with reason=dropped and at, keyed as its close would
//     be (droppedKey), its comment naming the revision and the PR, or
//     that an issue below it waits for one (droppedHoldText). A held one
//     gets a held → held marker that keeps its first hold's at, origin and
//     round bound (droppedHoldFields), as a hold by hand on a held issue
//     does. It keeps its links until step 2 closes it (droppedPass). A
//     hold the table refuses is a state error that Transition reports once
//     on the issue: it's logged, and the issue is left open, so the issues
//     above it are held too, as they can't close before it;
//   - open otherwise, it's reported to Touched, the label rules run on it first
//     (discCheckLabels), as for any move, then it moves to closed as not
//     planned, keyed by d's key, or apart from a move of it that key
//     carries already (droppedKey), with a comment naming the revision and
//     linking its plan comment. Closing a held one ends its hold. A close
//     the table refuses, as for an issue of a type the pipeline doesn't
//     know, is a state error that Transition reports once on the issue:
//     it's logged, and the issue is left open, with its links, so it
//     can't keep the revision's record from standing;
//   - closed by that move, the App's close keyed by d's key or apart
//     from it (droppedClosedBy), as after a pass that stopped part way,
//     only its links are removed;
//   - closed otherwise, as by someone before the approval, or by the
//     owner after a move of it the key carries, as a new or changed
//     child's, it gets no write: a closed issue is never written.
func (st *Step) droppedOne(ctx context.Context, d droppedItem, n int, id int64, below bool) (bool, error) {
	s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return false, fmt.Errorf("read #%d: %w", n, err)
	}
	dirt, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return false, fmt.Errorf("read #%d: %w", n, err)
	}
	if s.Closed {
		if is := state.Derive(s, st.AppSlug, dirt.Has); !droppedClosedBy(is, d.key) && !droppedClosedBy(is, droppedClosedKey(d.key)) {
			logr.Debugf("%s/%s#%d: closed, but not by revision %d of #%d's plan, which drops %s, so it's left as it is", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
			return false, nil
		}
		if err := st.droppedUnlink(ctx, n, id); err != nil {
			return false, fmt.Errorf("closed #%d: %w", n, err)
		}
		return false, nil
	}
	if _, ok := droppedHeld(state.Derive(s, st.AppSlug, dirt.Has)); ok {
		logr.Debugf("%s/%s#%d: held for a PR already, as revision %d of #%d's plan drops %s", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
		return true, nil
	}
	st.touch(n)
	if err := st.discCheckLabels(ctx, s, st.Trust, dirt); err != nil {
		return false, fmt.Errorf("check #%d: %w", n, err)
	}
	is := state.Derive(s, st.AppSlug, dirt.Has)
	pr, open := 0, false
	if st.PROpenOf != nil {
		pr, open = st.PROpenOf(is)
	}
	if open || below {
		if !open {
			pr = 0
		}
		m := Move{To: state.Held, Trigger: state.TriggerDropped, Key: droppedKey(is, d.key), Fields: droppedHoldFields(is), Text: droppedHoldText(d, n, pr)}
		err := st.Transition(ctx, s, dirt.Has, m)
		if StateError(err) {
			logr.Warnf("%s/%s#%d: left open, not held, though revision %d of #%d's plan drops %s while a PR waits: %v", st.Owner, st.Repo, n, d.rev, d.parent, d.item, err)
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("hold #%d: %w", n, err)
		}
		logr.Infof("%s/%s#%d: held, not closed, as revision %d of #%d's plan drops %s while a PR waits", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
		return true, nil
	}
	m := Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed, Key: droppedKey(is, d.key),
		Text: droppedText(d, n)}
	err = st.Transition(ctx, s, dirt.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: left open, though revision %d of #%d's plan drops %s: %v", st.Owner, st.Repo, n, d.rev, d.parent, d.item, err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("close #%d: %w", n, err)
	}
	logr.Infof("%s/%s#%d: closed as not planned, as revision %d of #%d's plan drops %s", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
	if err := st.droppedUnlink(ctx, n, id); err != nil {
		return false, fmt.Errorf("closed #%d: %w", n, err)
	}
	return false, nil
}

// droppedHoldFields are the fields of the move that holds is for a PR, as
// a revision drops it (SPEC §5, §6.4): reason=dropped and at, the stage it
// was at, with the round bound from before the hold, and, at the
// clarifying after a hold, its origin, as hold writes them. A held issue
// keeps its first hold's at, origin and round bound, as a hold by hand on
// a held issue does (Applier.override).
func droppedHoldFields(is state.Issue) map[string]string {
	fields := map[string]string{"reason": "dropped"}
	if h := is.HeldStack; is.Stage == state.Held {
		fields["at"] = string(h.At)
		if h.Origin != "" {
			fields["origin"] = string(h.Origin)
		}
		return state.HeldStackFields(fields, h.Seen)
	}
	fields["at"] = string(is.Stage)
	if is.AfterHold() {
		// The clarifying after a hold carries its origin forward, a later
		// hold of any kind included (SPEC §6.4).
		fields["origin"] = string(discOrigin(is, is.Stage))
	}
	return state.HeldStackFields(fields, is.Round)
}

// droppedHeld returns the marker of the hold on is for a PR, as a
// revision dropped it (droppedOne), the first if there are two, and
// whether is is held so: one of the holds on it (state.HeldStack) has
// reason=dropped.
func droppedHeld(is state.Issue) (github.Marker, bool) {
	if is.Stage != state.Held {
		return github.Marker{}, false
	}
	i := slices.IndexFunc(is.HeldStack.Holds, droppedHold)
	if i < 0 {
		return github.Marker{}, false
	}
	return is.HeldStack.Holds[i], true
}

// droppedHold reports whether m is the marker of a hold for a PR, as a
// revision dropped its issue: reason=dropped.
func droppedHold(m github.Marker) bool {
	return m.Fields["reason"] == "dropped"
}

// droppedHoldText is the comment of the hold of issue n in d's subtree
// for its PR, pr, or with pr 0 for the issues below it, which Transition
// escapes: the item ID is the planner's. Its first paragraph names the
// revision and the PR, which the digest's item copies (waitDroppedLines),
// and the second says what ends the hold.
func droppedHoldText(d droppedItem, n, pr int) string {
	text := fmt.Sprintf("On hold, not closed, since #%d's plan, rev %d, drops %s", d.parent, d.rev, d.item)
	if n != d.root {
		text += fmt.Sprintf(", #%d, which this issue is below", d.root)
	}
	if pr > 0 {
		return text + fmt.Sprintf(", while its PR #%d is open ([the plan](%s)).\n\n", pr, d.plan) +
			"Once the PR merges, its `Closes` closes this issue, and once it's closed without merging, I close this issue as not planned. " +
			"Or close it yourself. `/resume` doesn't end this hold."
	}
	return text + fmt.Sprintf(", while an issue below it waits for its PR ([the plan](%s)).\n\n", d.plan) +
		"Once nothing below it is open, I close this issue as not planned. `/resume` doesn't end this hold."
}

// droppedKey is the key of the close of an open issue, in state is, in a
// subtree a revision drops, whose moves the revision keys by key (SPEC
// §5): key itself, unless a move other than a close carries it already,
// as a new or changed child's has once someone moved it below a dropped
// one. Transition would take that move for the close and write nothing,
// so the issue would be unlinked but left open. Its close is then keyed
// apart (droppedClosedKey), which no other move carries. A close cut
// short after its comment keeps the key its marker carries, so
// Transition finishes it: key, or the key apart whenever a marker carries
// that, even once an edit has hidden the move that chose it, so no second
// close comment is posted.
func droppedKey(is state.Issue, key state.Key) state.Key {
	apart := droppedClosedKey(key)
	if is.Moved(apart) || is.Moved(key) && !droppedClosedBy(is, key) {
		return apart
	}
	return key
}

// droppedClosedKey is key, the key of a revision's moves, with "-closed"
// after its ID: the key of a dropped subtree's close of an issue that has
// a move keyed by key already, other than a close (droppedKey, SPEC §5).
func droppedClosedKey(key state.Key) state.Key {
	return state.Key{Field: key.Field, ID: key.ID + "-closed"}
}

// droppedClosedBy reports whether a transition marker in is that moves
// the issue to closed carries key: the App's close, keyed by a revision's
// key or apart from it (droppedKey), and not any other move that key
// carries, nor someone's close by hand, which writes no marker.
func droppedClosedBy(is state.Issue, key state.Key) bool {
	return slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
		return m.Kind == "transition" && m.Fields[key.Field] == key.ID && m.Fields["to"] == string(state.Closed)
	})
}

// droppedText is the comment of the close of issue n in d's subtree,
// which Transition escapes: the item ID is the planner's.
func droppedText(d droppedItem, n int) string {
	text := fmt.Sprintf("Closed as not planned, since #%d's plan, rev %d, drops %s", d.parent, d.rev, d.item)
	if n != d.root {
		text += fmt.Sprintf(", #%d, which this issue is below", d.root)
	}
	return text + fmt.Sprintf(" ([the plan](%s)).", d.plan)
}

// droppedUnlink removes every dependency link to and from issue n, whose
// ID is id, so no issue it blocked is left blocked by it (SPEC §6.4): its
// blocked-by links, whatever repo the blocker is in, then the blocked-by
// links of each issue in this repo it blocks. Each listing is read just
// before its removals, so a replay removes none twice, and each issue is
// reported to Touched before its link goes. A link to an issue in another
// repo that n blocks is logged and left, as it's that repo's issue to
// write. A DELETE here would name this repo's issue of that number, which
// has no such link, unless n blocks it as well, when its own link goes
// anyway, or which redirects, for an issue moved from here. GitHub
// answers 404 to it, so every pass would fail at the same point, and the
// revision's record would never stand. A removal GitHub refuses with a
// 422 is a failure no later pass gets past (childUnblockErr).
func (st *Step) droppedUnlink(ctx context.Context, n int, id int64) error {
	blockers, err := st.Client.BlockedBy(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return fmt.Errorf("unlink #%d: %w", n, err)
	}
	for _, b := range blockers {
		st.touch(n)
		if err := st.Client.RemoveBlockedBy(ctx, st.Owner, st.Repo, n, b.GetID()); err != nil {
			return fmt.Errorf("unlink #%d: %w", n, childUnblockErr(err, n, b.GetNumber()))
		}
		logr.Infof("%s/%s#%d: no longer blocked by #%d of %s, as it's closed as not planned", st.Owner, st.Repo, n, b.GetNumber(), b.GetRepositoryURL())
	}
	blocked, err := st.Client.Blocking(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return fmt.Errorf("unlink #%d: %w", n, err)
	}
	for _, b := range blocked {
		if !st.inRepo(b.GetRepositoryURL()) {
			logr.Warnf("%s/%s#%d: closed as not planned, but #%d of %s, in another repo, is still blocked by it", st.Owner, st.Repo, n, b.GetNumber(), b.GetRepositoryURL())
			continue
		}
		st.touch(b.GetNumber())
		if err := st.Client.RemoveBlockedBy(ctx, st.Owner, st.Repo, b.GetNumber(), id); err != nil {
			return fmt.Errorf("unlink #%d: %w", n, childUnblockErr(err, b.GetNumber(), n))
		}
		logr.Infof("%s/%s#%d: no longer blocked by #%d, which is closed as not planned", st.Owner, st.Repo, b.GetNumber(), n)
	}
	return nil
}

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
// (droppedClose). ChildrenVisit posts the revision's record only after
// these, so once it stands no pass reads the children again.

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
// parent's move into planned. plan is the revision's plan comment, which
// each close's comment links. A dropped item whose child isn't found is
// left alone: childrenRecord names it.
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
		if err := st.droppedWalk(ctx, d, f.Number, f.ID, seen); err != nil {
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
// order GitHub lists them, then n itself (droppedOne). seen holds the
// issues walked already, so a loop of links can't walk one twice. A
// sub-issue in another repo is skipped, with everything below it, as
// family skips one, as is a pull request.
func (st *Step) droppedWalk(ctx context.Context, d droppedItem, n int, id int64, seen map[int]bool) error {
	subs, err := st.Client.SubIssues(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return fmt.Errorf("below #%d: %w", n, err)
	}
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
		if err := st.droppedWalk(ctx, d, m, is.GetID(), seen); err != nil {
			return fmt.Errorf("below #%d: %w", n, err)
		}
	}
	return st.droppedOne(ctx, d, n, id)
}

// droppedOne closes issue n, whose ID is id, in d's subtree, read fresh,
// and removes every dependency link to and from it (droppedUnlink):
//   - open, it's reported to Touched, the label rules run on it first
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
func (st *Step) droppedOne(ctx context.Context, d droppedItem, n int, id int64) error {
	s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return fmt.Errorf("read #%d: %w", n, err)
	}
	dirt, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return fmt.Errorf("read #%d: %w", n, err)
	}
	if s.Closed {
		if is := state.Derive(s, st.AppSlug, dirt.Has); !droppedClosedBy(is, d.key) && !droppedClosedBy(is, droppedClosedKey(d.key)) {
			logr.Debugf("%s/%s#%d: closed, but not by revision %d of #%d's plan, which drops %s, so it's left as it is", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
			return nil
		}
		return st.droppedUnlink(ctx, n, id)
	}
	st.touch(n)
	if err := st.discCheckLabels(ctx, s, st.Trust, dirt); err != nil {
		return fmt.Errorf("check #%d: %w", n, err)
	}
	m := Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed, Key: droppedKey(state.Derive(s, st.AppSlug, dirt.Has), d.key),
		Text: droppedText(d, n)}
	err = st.Transition(ctx, s, dirt.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: left open, though revision %d of #%d's plan drops %s: %v", st.Owner, st.Repo, n, d.rev, d.parent, d.item, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close #%d: %w", n, err)
	}
	logr.Infof("%s/%s#%d: closed as not planned, as revision %d of #%d's plan drops %s", st.Owner, st.Repo, n, d.rev, d.parent, d.item)
	return st.droppedUnlink(ctx, n, id)
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

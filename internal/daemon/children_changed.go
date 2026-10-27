package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// This file applies an approved revision's changed items to their
// children (#296; SPEC §6.4): each is rewritten in place from
// the revision and sent back to its first stage. Kept items get no write
// at all, and new ones are filed as any child is (childWrite). Dropped
// items and the changed children's links to their siblings are handled
// in children_dropped.go (#297), which follows these writes, and the
// revision's record follows them all (childrenRecord).

// childrenChanged rewrites the child of each of specs, the changed items
// of revision rev of parent s's approved plan, in creation order
// (childSpecsWhere), with children each item's child, found or filed, and
// numbers each child's number, by item, which a changed human task's
// "Why it blocks" names (childSpec.bodyOf). plan is the plan comment,
// which the move links to, run its run=, which names the rewrite
// (childrenRun), and key the parent's move into planned, event=p<its
// comment ID>, which keys each move as a child's first move is keyed
// (SPEC §5.2). A plan comment that names no run is a failure no later
// pass gets past, which ChildrenVisit finds before any write
// (childrenRun), so run is never "" here. Each changed item's child is in
// children, as ChildrenVisit stops a revision whose kept or changed child
// isn't found before any write (childrenGone), so one that isn't is an
// error, as for a first plan's dependency with no child (childrenLink), and
// nothing is written to it.
func (st *Step) childrenChanged(ctx context.Context, s *github.Snapshot, rev int, plan github.Comment, run string, key state.Key,
	specs []childSpec, children map[string]childFound, numbers map[string]int) error {
	for _, c := range specs {
		f, ok := children[c.Item]
		if !ok {
			return fmt.Errorf("item %s: revision %d changes it, but it has no child", c.Item, rev)
		}
		closed, err := st.childChanged(ctx, s, rev, plan, run, key, c, f.Number, numbers)
		if err != nil {
			return fmt.Errorf("item %s: %w", c.Item, err)
		}
		if closed {
			// Closed since the listing, or by the label rules in this pass,
			// it's closed for the rest of the pass: every later step that
			// skips a closed child, as a changed child's links do
			// (revisedLink), skips it (SPEC §5.2).
			f.Closed = true
			children[c.Item] = f
		}
	}
	return nil
}

// childPlanRun is the run= of the transition marker in plan, the plan
// comment, which the planner's run that wrote the revision posted.
func childPlanRun(plan github.Comment, appSlug string) (string, bool) {
	for _, m := range github.TrustedMarkers(plan.Author, plan.Body, appSlug) {
		if m.Kind == "transition" && m.Fields["run"] != "" {
			return m.Fields["run"], true
		}
	}
	return "", false
}

// childChanged writes what's missing of c's change to child number n,
// under parent s, read fresh, in order, each write checked first:
//   - its title and body, rewritten whole (childRewrite), unless its
//     origin names rev already, or, for an adopted child, its body is the
//     App's rewrite whose origin does (adoptedRewrite);
//   - its type label, on a labels repo, and its scope, priority, model
//     and effort labels, set to c's (childSetLabels);
//   - its move, keyed by key: to c's first stage on the trigger for its
//     class, or for a held child, held → held with that stage as its at
//     and origin, which is no hold of its own (state.HeldStack), with the
//     hold's seen and no reason. Its comment names the revision and links
//     the plan comment, plan.
//
// An adopted child, one a person filed whose origin lives in a clean App
// comment (#372), is rewritten whole like any other: its body becomes the
// App's, from the revision, with none of the person's text, which stays
// in its edit history. Derive still reads no marker from a body a person
// filed, so the rewrite's origin marker, naming rev, goes in the move's
// comment too, which is then the newest clean App comment with one, and
// the comment says why. So it does for a child the App filed whose clean
// App comments carry an older origin (childCommentOrigin), as its repost
// after an edit of the body (originRepost): Derive reads the body's
// marker first, but falls back to the newest clean App comment's once a
// later edit makes the body's markers unread, and originRepost then
// reposts nothing, since an origin is read (SPEC §6.4). Should anyone edit
// an adopted child's move comment, step 1 posts the body's marker again
// (adoptedRepost); should step 1 post it between the rewrite and the move,
// as after a crash, Derive reads rev from it, and the move carries no
// marker. So it does once step 1 there reposts the rewrite's marker for a
// child the App filed (originRepost), after an edit of its body.
//
// The move is the last write, so once it stands the child is done, and a
// later pass reads it and writes nothing. Before it, the child is reported
// to Touched, and the label rules run on it first (discCheckLabels), so a
// stage a trusted user set by hand is recorded as an override first. A
// closed child gets nothing: a revision can't change one (SPEC §11.3), but
// it may close after the approval, or the label rules may close it
// (childCheck). Then closed is true, so childrenChanged marks it closed
// for the rest of the pass. Nor does a story with no parent that isn't
// held, whose move the table lacks: that's a failure no later pass gets
// past (childOrphan).
func (st *Step) childChanged(ctx context.Context, parent *github.Snapshot, rev int, plan github.Comment, run string, key state.Key,
	c childSpec, n int, numbers map[string]int) (closed bool, err error) {
	s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
	if err != nil {
		return false, fmt.Errorf("read #%d: %w", n, err)
	}
	if s.Closed {
		logr.Infof("%s/%s#%d: #%d, the child of item %s of its plan, is closed, so revision %d's change to it isn't applied",
			st.Owner, st.Repo, parent.Number, n, c.Item, rev)
		return true, nil
	}
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return false, fmt.Errorf("read #%d: %w", n, err)
	}
	if is := state.Derive(s, st.AppSlug, d.Has); is.Moved(key) {
		if childStageShown(s, is.Stage) {
			return false, nil
		}
		// The move was cut short after its comment, so its labels are
		// unfinished, and the label rules finish them (SPEC §5.1), or
		// record an owner's change since as an override.
		st.touch(n)
		closed, err := st.childCheck(ctx, s, d)
		if err != nil {
			return false, fmt.Errorf("moved already: %w", err)
		}
		return closed, nil
	}
	if err := childOrphan(s, state.Derive(s, st.AppSlug, d.Has), c); err != nil {
		return false, fmt.Errorf("move #%d: %w", n, err)
	}
	st.touch(n)
	closed, err = st.childCheck(ctx, s, d)
	if err != nil {
		return false, fmt.Errorf("before its change: %w", err)
	}
	if closed {
		// The label rules finished the App's close of it, cut short after
		// its comment (SPEC §5.1, Unfinished writes), as a dropped
		// subtree's close of a child below a dropped one may be once an
		// edit hides the move keyed by key: a closed child gets nothing,
		// and the close is the dropped subtree's to finish (droppedOne).
		// Or they closed it again after an untrusted reopen (SPEC §6.4),
		// and it gets nothing either.
		logr.Infof("%s/%s#%d: #%d, the child of item %s of its plan, is closed now, so revision %d's change to it isn't applied",
			st.Owner, st.Repo, parent.Number, n, c.Item, rev)
		return true, nil
	}
	names := func(o state.Origin) bool { return o.Plan == parent.Number && o.Item == c.Item && o.Rev >= rev }
	if _, o, _ := adoptedRewrite(s, st.AppSlug, d.Has); !names(state.Derive(s, st.AppSlug, d.Has).Origin) && !names(o) {
		body, err := c.bodyOf(numbers)
		if err != nil {
			return false, fmt.Errorf("write #%d's body: %w", n, err)
		}
		if d, err = st.childRewrite(ctx, s, d, run, c.Title, body); err != nil {
			return false, fmt.Errorf("rewrite #%d: %w", n, err)
		}
		logr.Infof("%s/%s#%d: rewrote #%d, the child of item %s of its plan, from revision %d", st.Owner, st.Repo, parent.Number, n, c.Item, rev)
	}
	if err := st.childSetLabels(ctx, s, c); err != nil {
		return false, fmt.Errorf("label #%d: %w", n, err)
	}
	stage, _ := c.Stage.Label()
	text := fmt.Sprintf("Rewritten from #%d's plan, rev %d, as %s, since [the plan](%s) changed it.",
		parent.Number, rev, c.Item, waitLink(st.Owner, st.Repo, parent.Number, plan.ID))
	is := state.Derive(s, st.AppSlug, d.Has)
	var markers []github.Marker
	if !state.IsApp(s.Author, st.AppSlug) && !names(is.Origin) {
		// An adopted child: its body's marker isn't read, so the move's
		// comment carries it, naming rev.
		m, o, ok := adoptedRewrite(s, st.AppSlug, d.Has)
		if !ok || !names(o) {
			return false, fmt.Errorf("#%d, which a person filed, has no rewrite of the App's whose origin names item %s of revision %d, for its move to carry", n, c.Item, rev)
		}
		markers = []github.Marker{m}
		text += " " + adoptedMoveText
	} else if co := childCommentOrigin(s, st.AppSlug, d.Has); state.IsApp(s.Author, st.AppSlug) && co.Plan != 0 && !names(co) {
		// A child the App filed, whose clean App comment carries an older
		// origin, as the App's repost of one an edit took from the body
		// (originRepost) does: a later edit of the body would fall back to
		// it, so the move's comment carries the new one. One that names
		// rev already needs none: step 1 reposted the rewrite's marker
		// between the rewrite and the move, after an edit of the body, so
		// the rewrite isn't made again, as Derive reads rev, and the body
		// has no marker to carry.
		m, ok := state.OriginMarker(s.Body)
		if !ok || !names(is.Origin) {
			return false, fmt.Errorf("#%d has no rewrite of the App's whose origin names item %s of revision %d, for its move to carry", n, c.Item, rev)
		}
		markers = []github.Marker{m}
		text += " " + childRepostMoveText
	}
	var m Move
	if is.Stage == state.Held {
		fields := map[string]string{"at": string(c.Stage), "origin": string(c.Stage)}
		// SPEC §7.2 has no row of its own for it, so it takes /hold's,
		// any open → held: the marker names no trigger, and its lack of a
		// reason is what tells it from a hold.
		m = Move{To: state.Held, Trigger: state.TriggerHold, Key: key, Fields: state.HeldStackFields(fields, is.HeldStack.Seen),
			Text: text + fmt.Sprintf(" It stays on hold, and `/resume` takes it to `%s`.", stage), Markers: markers}
	} else {
		trigger := map[state.Stage]state.Trigger{state.Planning: state.TriggerParentChangedStory,
			state.NeedsHuman: state.TriggerParentChangedHumanTask, state.Ready: state.TriggerParentChangedTask}[c.Stage]
		text += fmt.Sprintf(" It goes back to `%s`", stage)
		if c.Stage == state.Planning {
			text += ", where the planner plans it again"
		}
		m = Move{To: c.Stage, Trigger: trigger, Key: key, Text: text + ".", Markers: markers}
	}
	if err := st.Transition(ctx, s, d.Has, m); err != nil {
		return false, fmt.Errorf("move #%d: %w", n, err)
	}
	logr.Infof("%s/%s#%d: sent #%d, the child of item %s of its plan, back to %s for revision %d, from %s",
		st.Owner, st.Repo, parent.Number, n, c.Item, c.Stage, rev, is.Stage)
	return false, nil
}

// childCheck runs the label rules on s, a changed child read fresh, with
// d its dirty content (discCheckLabels), and reports whether s is closed
// after them: as when they finish the App's close of it, cut short after
// its comment (SPEC §5.1), or close it again after an untrusted reopen
// (SPEC §6.4). The error of the latter, closingErrClosedAgain, says only
// that nothing more is acted on for s this pass, which a closed child
// gets anyway, so it's no error here: the parent's visit goes on without
// s, which it reported to Touched first, so s is read again next pass.
func (st *Step) childCheck(ctx context.Context, s *github.Snapshot, d Dirt) (closed bool, err error) {
	err = st.discCheckLabels(ctx, s, st.Trust, d)
	if errors.Is(err, closingErrClosedAgain) && s.Closed {
		logr.Debugf("%s/%s#%d: closed again in a revision's pass, so the revision skips it: %v", st.Owner, st.Repo, s.Number, err)
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check #%d: %w", s.Number, err)
	}
	return s.Closed, nil
}

// childOrphan is the childrenStuck for s, in state is, the child of c, a
// changed item, when c is a story and s has no parent, as a child a pass
// filed but stopped before linking, or one someone unlinked by hand, and
// isn't held, or nil otherwise. Such a child is a story with no parent
// (state.ClassOwnerStory), which no row of SPEC §7.2 sends back to
// planning on the parent's change, so its move would be a transition
// error each pass, and the parent's visit would fail and read its
// children again every pass. Linking it under the parent first would undo
// a person's unlink, so the parent's state error asks for that instead
// (SPEC §5.2). Once it's linked, a move of the parent, as by /hold and
// /resume, applies the revision to it. A held child's move, held → held,
// fits every class, so it's none. It's found before any write to the
// child, which gets none.
func childOrphan(s *github.Snapshot, is state.Issue, c childSpec) error {
	if c.Stage != state.Planning || s.Parent != 0 || is.Stage == state.Held {
		return nil
	}
	return &childrenStuck{
		why: fmt.Sprintf("#%d, the child of item %s, is a story with no parent, which no transition sends back to `planning` until it's linked under this issue",
			s.Number, inline(c.Item)),
		err: fmt.Errorf("#%d, item %s's child, is a story with no parent at %s, and SPEC §7.2 has no row from there to planning on %s for one",
			s.Number, c.Item, is.Stage, state.TriggerParentChangedStory),
	}
}

// adoptedMoveText is the line in an adopted child's move comment, after
// the revision's, saying why the comment carries its origin marker.
const adoptedMoveText = "It was filed by hand, so this comment carries its `origin` marker, which its description can't: " +
	"I've rewritten the description whole from the plan, and the earlier one stays in its edit history."

// childRepostMoveText is the line in the move comment of a changed child
// the App filed, after the revision's, saying why the comment carries its
// origin marker: an earlier App comment carries an older one.
const childRepostMoveText = "An earlier comment of mine carries this issue's `origin` marker from before, " +
	"so this comment carries the new one, which still counts once the description is edited again."

// childCommentOrigin is the Origin Derive reads for s from its clean App
// comments alone, as it does once the body's markers aren't read (SPEC
// §5): from the App's repost of a marker an edit took from the body
// (originRepost), or from an earlier move's comment that carried one, as
// childChanged's does. It's the zero Origin with none.
func childCommentOrigin(s *github.Snapshot, appSlug string, dirty func(int64) bool) state.Origin {
	return state.Derive(&github.Snapshot{Number: s.Number, Author: s.Author, Comments: s.Comments}, appSlug, dirty).Origin
}

// adoptedRewrite returns the origin marker of s's body, whole, and the
// Origin it gives, while s is an adopted child, one a person filed (#372),
// whose body is the App's full rewrite (state.Rewrote) and isn't dirty:
// the body's text is then all the App's (SPEC §5.1), though Derive reads
// no marker from it, as it reads only the App's bodies. It reads the
// body's markers as Derive would read the App's, from a snapshot of the
// body alone with the App, its last editor, as its author. It's false for
// an issue the App filed, whose body Derive reads itself, and for any
// other body.
func adoptedRewrite(s *github.Snapshot, appSlug string, dirty func(int64) bool) (github.Marker, state.Origin, bool) {
	if state.IsApp(s.Author, appSlug) || dirty(state.BodyID) || !state.Rewrote(github.Edit{By: s.BodyEdit.By, Text: s.Body}, appSlug) {
		return github.Marker{}, state.Origin{}, false
	}
	m, ok := state.OriginMarker(s.Body)
	if !ok {
		return github.Marker{}, state.Origin{}, false
	}
	o := state.Derive(&github.Snapshot{Number: s.Number, Author: s.BodyEdit.By, Body: s.Body}, appSlug, func(int64) bool { return false }).Origin
	return m, o, o.Plan != 0
}

// childStageShown reports whether stage's label is the only stage label
// on s, ignoring case.
func childStageShown(s *github.Snapshot, stage state.Stage) bool {
	want, ok := stage.Label()
	labels := slices.DeleteFunc(slices.Clone(s.Labels), func(l string) bool { return !isStageLabel(l) })
	return ok && len(labels) == 1 && strings.EqualFold(labels[0], want)
}

// childRewrite makes title and body, a child's as childSpec renders them,
// origin marker last, s's title and whole body, in one edit, and returns
// d, s's dirty content, without the body: the App's full rewrite cleans it
// (SPEC §5.1). The body starts with the rewrite marker, run= naming run,
// the planner's run that wrote the revision, and on a dirty body
// replaced=, naming who made it dirty (rewriteLogin), as a spec's does
// (specWriteBody). A title that's the same already isn't sent. s takes
// what GitHub answers, with the App as the body's last editor.
func (st *Step) childRewrite(ctx context.Context, s *github.Snapshot, d Dirt, run, title, body string) (Dirt, error) {
	marker := github.Marker{Kind: state.RewriteKind, Fields: map[string]string{"run": run}}
	if by, dirty := d[state.BodyID]; dirty {
		marker.Fields["replaced"] = rewriteLogin(by)
	}
	f, err := github.FormatMarker(marker)
	if err != nil {
		return d, fmt.Errorf("its rewrite marker: %w", err)
	}
	whole := f + "\n" + body
	req := gogithub.UpdateIssueRequest{Body: &whole}
	if title != s.Title {
		req.Title = &title
	}
	is, _, err := st.Client.Issues.Update(ctx, st.Owner, st.Repo, s.Number, req)
	if err != nil {
		return d, fmt.Errorf("edit its title and body: %w", err)
	}
	s.Title, s.Body = is.GetTitle(), is.GetBody()
	s.BodyEdit = github.Edit{By: github.Author{Login: st.AppSlug + "[bot]", Type: "Bot"}, At: is.GetUpdatedAt().Time}
	d = maps.Clone(d)
	delete(d, state.BodyID)
	return d, nil
}

// childLabelKinds are the label kinds a changed child's labels set
// (childSetLabels), by prefix: the type, on a labels repo, then scope,
// priority, model and effort.
var childLabelKinds = []string{"type:", "scope:", "priority:", github.ModelPrefix, github.EffortPrefix}

// childLabelKindsOf is childLabelKinds for c's child: without the type on
// a repo that uses issue types, where c has an IssueType and no type:*
// label.
func childLabelKindsOf(c childSpec) []string {
	if c.IssueType != "" {
		return childLabelKinds[1:]
	}
	return childLabelKinds
}

// childSetLabels makes c's labels of each of childLabelKinds the only ones
// of their kinds on s, ignoring case: it adds those s lacks, then removes
// the others of those kinds, such as a priority set by /priority since
// the child was filed, or a model the revision no longer names. A kind c
// has none of, as a story's scope, or a model the item doesn't set or a
// reload dropped (childOverrides), is left with none. On a repo that uses
// issue types, the type is the issue's, and its type:* labels are left
// alone. The stage label is the move's.
func (st *Step) childSetLabels(ctx context.Context, s *github.Snapshot, c childSpec) error {
	kinds := childLabelKindsOf(c)
	kindOf := func(l string) bool {
		return slices.ContainsFunc(kinds, func(k string) bool { return strings.HasPrefix(strings.ToLower(l), k) })
	}
	want := slices.DeleteFunc(slices.Clone(c.Labels), func(l string) bool { return !kindOf(l) })
	for _, l := range want {
		if err := st.addLabel(ctx, s, l); err != nil {
			return fmt.Errorf("set %s: %w", l, err)
		}
	}
	for _, l := range slices.Clone(s.Labels) {
		if kindOf(l) && !slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(w, l) }) {
			if err := st.removeLabel(ctx, s, l); err != nil {
				return fmt.Errorf("remove %s: %w", l, err)
			}
		}
	}
	return nil
}

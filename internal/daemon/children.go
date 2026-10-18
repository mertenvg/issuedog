package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// ChildrenVisit creates the children of an approved plan (plan #249, T3):
// the loop calls it after FlagVisit, so the pass that approves a plan, by
// 👍 or /approve, or that skips review, creates its children too. It acts
// only on an open parent at planned, whose move there approved a plan
// revision R with its data (approvedPlanData): the move's comment, ID P,
// posted at T (state.ApprovedPlanMove). A held or closed parent gets
// nothing, and creation carries on once a hold ends back at planned. Until
// T4's record on the parent, it reads and checks every child on every
// visit.
//
// It describes each item of R (childSpecs), finds the children filed
// already (childrenFind), with no search request, then, for each item in
// the order childSpecs gives, plan order with the human tasks last
// (childOrder):
//   - files its child if none is found (CreateChild), with its title, body,
//     labels and issue type. A human task's body names the issues of the
//     items that depend on it, filed before it (childSpec.bodyOf);
//   - links it under the parent if it isn't a sub-issue of it yet
//     (AddSubIssue);
//   - gives it its first move, intake → its first stage on TriggerCreated,
//     keyed event=p<P> (#224's reconcile decision 6), unless it has a
//     transition marker of the App's already (childFirstMove).
//
// Each child it may write to is reported to Touched first, or, for one it
// files, once it has its number, so it skips the rest of the pass and is
// read again on the next. Each write is checked first, so a pass that
// crashed part way runs again with no second child, link or move (SPEC
// §5.2). Any failed read or write is returned, wrapped: Iterate then reads
// the parent again next pass, and the visit starts again from the reads.
func (st *Step) ChildrenVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("children of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	if s.Closed {
		return nil
	}
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	if state.Derive(s, st.AppSlug, d.Has).Stage != state.Planned {
		return nil
	}
	rev, plan, ok, err := approvedPlanData(s, st.AppSlug, d.Has)
	if err != nil {
		return wrap(err)
	}
	move, moved := state.ApprovedPlanMove(s, st.AppSlug, d.Has)
	if !ok || !moved {
		logr.Debugf("%s/%s#%d: at planned with no approved plan revision that has its data, so no children are created", st.Owner, st.Repo, s.Number)
		return nil
	}
	specs, err := childSpecs(s, rev.Rev, plan.Items, st.IssueTypes, st.Notify)
	if err != nil {
		return wrap(err)
	}
	found, err := st.childrenFind(ctx, s, move.CreatedAt)
	if err != nil {
		return wrap(err)
	}
	key := state.Key{Field: "event", ID: "p" + strconv.FormatInt(move.ID, 10)}
	// numbers is each child's number so far, by item, found or filed, for
	// "Why it blocks": the items a human task blocks come before it.
	numbers := map[string]int{}
	for _, c := range specs {
		n, err := st.childWrite(ctx, s, rev.Rev, key, c, found[c.Item], numbers)
		if err != nil {
			return wrap(fmt.Errorf("item %s: %w", c.Item, err))
		}
		numbers[c.Item] = n
	}
	return nil
}

// childFound is a child of a plan item filed already, as a listing gives
// it: its number and ID, and whether it's a sub-issue of the parent.
type childFound struct {
	Number int
	ID     int64
	Linked bool
}

// childrenFind reads the children of parent s filed already: its
// sub-issues, which hold every linked child, open or closed, then the
// repo's issues updated since the move into planned, at since, which also
// hold one filed but not yet linked when a pass stopped. Both lists are
// consistent, unlike search, which lags behind writes (plan #249, "Finding
// a child again, without search"). It returns them by item (childMatch).
func (st *Step) childrenFind(ctx context.Context, s *github.Snapshot, since time.Time) (map[string]childFound, error) {
	subs, err := st.Client.SubIssues(ctx, st.Owner, st.Repo, s.Number)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	listed, err := st.Client.IssuesSince(ctx, st.Owner, st.Repo, since)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	return childMatch(s.Number, st.AppSlug, subs, listed, st.inRepo), nil
}

// childMatch returns the children of parent's plan items among subs, the
// parent's sub-issues, and listed, issues of this repo, by item. A child
// is an issue the App filed whose origin names plan=#<parent>, read by
// Derive from a snapshot of what the listing gives, its author and body,
// whatever its rev (SPEC §5.2): the listing has no edit history, so a body
// someone edited still matches, as long as it keeps the App's marker (plan
// #249, assumption 8). A sub-issue in another repo, as inRepo tells by its
// repository_url, and a pull request are skipped. One that's a sub-issue
// is Linked. Should two issues name the same item, the first wins: the
// sub-issues in link order, then the rest by number, so the one linked, or
// else the one filed first.
func childMatch(parent int, appSlug string, subs, listed []*gogithub.Issue, inRepo func(string) bool) map[string]childFound {
	found := map[string]childFound{}
	seen := map[int]bool{}
	add := func(is *gogithub.Issue, linked bool) {
		n := is.GetNumber()
		if seen[n] || is.IsPullRequest() {
			return
		}
		seen[n] = true
		author := github.Author{Login: is.GetUser().GetLogin(), Type: is.GetUser().GetType()}
		if !state.IsApp(author, appSlug) {
			return
		}
		o := state.Derive(&github.Snapshot{Number: n, Author: author, Body: is.GetBody()}, appSlug, func(int64) bool { return false }).Origin
		if o.Plan != parent {
			return
		}
		if had, ok := found[o.Item]; ok {
			logr.Warnf("#%d and #%d are both the child of item %s of #%d's plan, so #%d is the one kept", had.Number, n, o.Item, parent, had.Number)
			return
		}
		found[o.Item] = childFound{Number: n, ID: is.GetID(), Linked: linked}
	}
	for _, is := range subs {
		if !inRepo(is.GetRepositoryURL()) {
			logr.Debugf("#%d: skipping its sub-issue #%d of %s, which isn't in this repo", parent, is.GetNumber(), is.GetRepositoryURL())
			continue
		}
		add(is, true)
	}
	listed = slices.Clone(listed)
	slices.SortStableFunc(listed, func(a, b *gogithub.Issue) int { return cmp.Compare(a.GetNumber(), b.GetNumber()) })
	for _, is := range listed {
		add(is, false)
	}
	return found
}

// childWrite writes what's missing of c's child, under parent s, in order:
// the issue, its link under s, then its first move, keyed by key, and
// returns its number. f is the child found already, if any (Number 0 for
// none). numbers is the number of each child filed so far, by item, which
// a human task's body names (childSpec.bodyOf).
func (st *Step) childWrite(ctx context.Context, s *github.Snapshot, rev int, key state.Key, c childSpec, f childFound, numbers map[string]int) (int, error) {
	var child *github.Snapshot // a child filed in this pass, as the create answered
	if f.Number == 0 {
		body, err := c.bodyOf(numbers)
		if err != nil {
			return 0, fmt.Errorf("write its child's body: %w", err)
		}
		is, err := st.Client.CreateChild(ctx, st.Owner, st.Repo, github.NewChild{Title: c.Title, Body: body, Labels: c.Labels, Type: c.IssueType})
		if err != nil {
			return 0, fmt.Errorf("create its child: %w", err)
		}
		f = childFound{Number: is.GetNumber(), ID: is.GetID()}
		logr.Infof("%s/%s#%d: filed #%d for item %s of its plan, rev %d", st.Owner, st.Repo, s.Number, f.Number, c.Item, rev)
		child = childCreated(is, s.Number)
	}
	if !f.Linked { // as one filed just now is, which is touched here
		st.touch(f.Number)
		if err := st.Client.AddSubIssue(ctx, st.Owner, st.Repo, s.Number, f.ID); err != nil {
			return 0, fmt.Errorf("link #%d: %w", f.Number, err)
		}
		logr.Infof("%s/%s#%d: linked #%d under it, for item %s of its plan", st.Owner, st.Repo, s.Number, f.Number, c.Item)
	}
	return f.Number, st.childFirstMove(ctx, s.Number, rev, key, c, f.Number, child)
}

// childFirstMove gives child number n its first move (Q1a), intake → c's
// first stage on TriggerCreated, keyed by key, unless a clean transition
// marker of the App's is on it already: its own first move, or an owner's
// stage change recorded as an override. child is the snapshot a create in
// this pass answered with, which nobody else can have changed yet, or nil
// for a child found again: that one is read fresh, and its labels checked
// first (discCheckLabels), so an owner's stage change since is recorded as
// an override, as for any issue, and it keeps the labels it has. One
// found closed, as when someone closed it before its first move, gets
// none.
func (st *Step) childFirstMove(ctx context.Context, parent, rev int, key state.Key, c childSpec, n int, child *github.Snapshot) error {
	dirty := func(int64) bool { return false }
	if child == nil {
		s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
		if err != nil {
			return fmt.Errorf("read #%d: %w", n, err)
		}
		if s.Closed {
			logr.Infof("%s/%s#%d: #%d, the child of item %s of its plan, is closed, so it gets no first move", st.Owner, st.Repo, parent, n, c.Item)
			return nil
		}
		d, err := st.Dirty(ctx, s, st.trustOnceTrust())
		if err != nil {
			return fmt.Errorf("read #%d: %w", n, err)
		}
		if childMoved(s, st.AppSlug, d.Has) {
			return nil
		}
		st.touch(n)
		if err := st.discCheckLabels(ctx, s, st.Trust, d); err != nil {
			return fmt.Errorf("check #%d: %w", n, err)
		}
		child, dirty = s, d.Has
	}
	if childMoved(child, st.AppSlug, dirty) {
		return nil
	}
	stage, _ := c.Stage.Label()
	text := fmt.Sprintf("Created from #%d's plan, rev %d, as %s. It starts at `%s`", parent, rev, c.Item, stage)
	if c.Stage == state.Planning {
		text += ", where the planner writes its task plan"
	}
	m := Move{To: c.Stage, Trigger: state.TriggerCreated, Key: key, Text: text + "."}
	if err := st.Transition(ctx, child, dirty, m); err != nil {
		return fmt.Errorf("first move of #%d: %w", n, err)
	}
	return nil
}

// childMoved reports whether s has a transition marker of the App's that
// counts (state.Derive): one in a clean comment, or a clean body the App
// last edited.
func childMoved(s *github.Snapshot, appSlug string, dirty func(int64) bool) bool {
	return slices.ContainsFunc(state.Derive(s, appSlug, dirty).Markers, func(m github.Marker) bool { return m.Kind == "transition" })
}

// childCreated is the snapshot of a child as CreateChild answered for it,
// under parent: its title, body, author, labels and type, with no comments.
func childCreated(is *gogithub.Issue, parent int) *github.Snapshot {
	s := &github.Snapshot{
		Number: is.GetNumber(), Title: is.GetTitle(), Body: is.GetBody(),
		Author:    github.Author{Login: is.GetUser().GetLogin(), Type: is.GetUser().GetType()},
		CreatedAt: is.GetCreatedAt().Time, UpdatedAt: is.GetUpdatedAt().Time,
		TypeName: is.GetType().GetName(), TypeID: is.GetType().GetID(), TypeFromLabels: is.GetType().GetName() == "",
		Type: github.TypeOf(is), Parent: parent,
	}
	for _, l := range is.Labels {
		s.Labels = append(s.Labels, l.GetName())
	}
	return s
}

// touch tells Touched, if set, of issue n, which this visit may write to.
func (st *Step) touch(n int) {
	if st.Touched != nil {
		st.Touched(n)
	}
}

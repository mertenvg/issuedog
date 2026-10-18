package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/config"
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
// nothing, and creation carries on once a hold ends back at planned. Once
// a clean record of R's children stands on the parent (state.CreatedRecord,
// plan #249, T4), it returns before any read or write: the children are
// all written, so a visit costs nothing.
//
// Until then, it describes each item of R that it files (childSpecs):
// every item of a first plan, and only the new items of a revision
// (#195's T6), and each changed item of a revision, which it rewrites in
// place (childSpecsWhere, #195's T7). A kept item gets no write, and a
// dropped one's child is closed (#195's T8, #297). It finds the children filed already
// (childrenFind), with no search request, then, for each item in creation
// order, which childSpecs gives: plan order with the human tasks last
// (childOrder):
//   - files its child if none is found (CreateChild), with its title, body,
//     labels and issue type. Its model and effort labels are resolved
//     against Models as of this visit, and a warning names the item for
//     each one left off (childOverrides). A human task's body names the
//     issues of the items that depend on it, filed before it
//     (childSpec.bodyOf);
//   - links it under the parent if it isn't a sub-issue of it yet
//     (AddSubIssue);
//   - gives it its first move, intake → its first stage on TriggerCreated,
//     keyed event=p<P> (#224's reconcile decision 6), unless it has a
//     transition marker of the App's already (childFirstMove).
//
// Then it rewrites each changed item's child from R and sends it back to
// its first stage, or for a held one, makes that stage its hold's at and
// origin (childrenChanged).
//
// Then, going through R's items in plan order, not creation order, it
// gives each child the blocked-by links its item's "Depends on" names
// (childrenLink): of a revision, each new item's child. For a revision,
// it then makes each changed child's links to its siblings follow the
// revision (revisedLink), and closes each dropped item's child, with
// every open issue below it, deepest first, and removes their links
// (droppedClose, #297). Last it posts the record on the parent, which
// lists the children in plan order too, a revision's dropped ones left
// out (childrenRecord), so the record stands only once every write
// before it is made.
//
// A kept, changed or dropped item of a revision whose child isn't found,
// as when someone unlinked it from the parent by hand before the
// approval, and it hasn't been updated since, gets no write, and no link
// is made to it: a warning names it, and the record says so, and leaves
// it out. So it can't keep the record from standing, and once the record
// stands no pass looks for it again (#297; #362 would find it).
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
	is := state.Derive(s, st.AppSlug, d.Has)
	if is.Stage != state.Planned {
		return nil
	}
	// The record is checked before the plan's data is read back, which
	// it doesn't need.
	r, planComment, ok := state.ApprovedPlan(s, st.AppSlug, d.Has)
	if ok {
		if _, done := state.CreatedRecord(is, r.Rev); done {
			return nil
		}
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
	var models config.Models // none, if the step has no Models
	if st.Models != nil {
		models = st.Models()
	}
	specs, err := childSpecs(s, rev.Rev, plan.Items, st.IssueTypes, st.Notify, models)
	if err != nil {
		return wrap(err)
	}
	// A revision's changed items, each rewritten in place (#195's T7),
	// are described before any write too, so an item that can't be stops
	// the visit with nothing written.
	changed, err := childSpecsWhere(s, rev.Rev, plan.Items, func(status string) bool { return status == planChanged },
		st.IssueTypes, st.Notify, models)
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
	// children is each child, by item, for its links and the record. A
	// revision's children that aren't filed again (planFiles), such as a
	// kept item a new one depends on, or a changed item a new human task
	// blocks, are known from the start.
	numbers := map[string]int{}
	children := make(map[string]childFound, len(found))
	for item, f := range found {
		numbers[item], children[item] = f.Number, f
	}
	for _, c := range specs {
		f, err := st.childWrite(ctx, s, rev.Rev, key, c, found[c.Item], numbers)
		if err != nil {
			return wrap(fmt.Errorf("item %s: %w", c.Item, err))
		}
		numbers[c.Item], children[c.Item] = f.Number, f
	}
	// Once the new children are filed, so a changed human task's "Why it
	// blocks" can name a new item's issue, the changed children are
	// rewritten and moved (#195's T7).
	if err := st.childrenChanged(ctx, s, rev.Rev, planComment, key, changed, children, numbers); err != nil {
		return wrap(err)
	}
	// plan.Items is plan order, which the links and the record follow,
	// whatever order the children were created in. Of a revision, the new
	// items' children get their links here, and a kept child keeps its
	// links. A dependency on a kept or changed item whose child isn't
	// found is skipped, not an error, so it can't stop the record (#297).
	revised := slices.ContainsFunc(plan.Items, func(it planItem) bool { return it.Status != "" })
	linked := plan.Items
	if revised {
		linked = slices.DeleteFunc(slices.Clone(plan.Items), func(it planItem) bool { return !planFiles(it.Status) })
	}
	if err := st.childrenLink(ctx, linked, children, revised); err != nil {
		return wrap(err)
	}
	if revised {
		// Then a changed child's links to its siblings follow the
		// revision, and each dropped item's child is closed, with every
		// open issue below it, deepest first, and unlinked (#195's
		// assumption 7, #297). The record comes only after them, so once
		// it stands every write of the revision is made.
		if err := st.revisedLink(ctx, plan.Items, children); err != nil {
			return wrap(err)
		}
		if err := st.droppedClose(ctx, s, rev.Rev, planComment, key, plan.Items, children); err != nil {
			return wrap(err)
		}
	}
	if err := st.childrenRecord(ctx, s, rev.Rev, plan.Items, children); err != nil {
		return wrap(err)
	}
	return nil
}

// childFound is a child of a plan item filed already, as a listing gives
// it: its number and ID, whether it's a sub-issue of the parent, and
// whether it's closed, which a revision's links skip (revisedLink).
type childFound struct {
	Number int
	ID     int64
	Linked bool
	Closed bool
}

// childrenFind reads the children of parent s filed already: its
// sub-issues, which hold every linked child, open or closed, then the
// repo's issues updated since the move into planned, at since, which also
// hold one filed but not yet linked when a pass stopped. Both lists are
// consistent, unlike search, which lags behind writes (plan #249, "Finding
// a child again, without search"). It returns them by item (childMatch),
// with the Origin of each sub-issue whose listed body gives none
// (childOrigins), a person's adopted issue's included (#372).
func (st *Step) childrenFind(ctx context.Context, s *github.Snapshot, since time.Time) (map[string]childFound, error) {
	subs, err := st.Client.SubIssues(ctx, st.Owner, st.Repo, s.Number)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	listed, err := st.Client.IssuesSince(ctx, st.Owner, st.Repo, since)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	origins, err := st.childOrigins(ctx, subs)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	return childMatch(s.Number, st.AppSlug, subs, listed, st.inRepo, origins), nil
}

// childOrigins returns the Origin of each of subs, a parent's sub-issues
// in this repo, whose listed body gives none (childListedOrigin), by
// number, as state.Derive reads it from the issue's snapshot, comments
// and dirty content included. That's one of two kinds of sub-issue:
//   - one the App filed, whose origin the App reposted in a comment once
//     the body lost it (originRepost, S5's T2, #291);
//   - one a person filed, whose body never gives an origin, as Derive
//     reads markers only from the App (github.TrustedMarkers), but whose
//     clean App comment may carry one: an adopted issue, such as #195,
//     which #224's plan adopted as S5 (#195's assumption 3, #372). A
//     marker in a dirty App comment, or in a person's comment, gives
//     none, by Derive's rules.
//
// Each such issue costs a snapshot, and the callers read them only before
// the record stands (ChildrenVisit) or while a revision is planned and
// checked (planPrior.read), so an unchanged pass reads none. Only
// sub-issues are read: a child loses its body's marker to an edit after
// it's filed, and it's linked in the pass that files it, so one that isn't
// linked yet was filed just now, and its body is still the App's. An
// adopted issue that isn't a sub-issue of the parent isn't its child.
func (st *Step) childOrigins(ctx context.Context, subs []*gogithub.Issue) (map[int]state.Origin, error) {
	origins := map[int]state.Origin{}
	for _, is := range subs {
		n := is.GetNumber()
		if !st.inRepo(is.GetRepositoryURL()) || is.IsPullRequest() || childListedOrigin(is, st.AppSlug).Plan != 0 {
			continue
		}
		s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
		if err != nil {
			return nil, fmt.Errorf("read #%d's origin: %w", n, err)
		}
		d, err := st.Dirty(ctx, s, st.trustOnceTrust())
		if err != nil {
			return nil, fmt.Errorf("read #%d's origin: %w", n, err)
		}
		if o := state.Derive(s, st.AppSlug, d.Has).Origin; o.Plan != 0 {
			origins[n] = o
		}
	}
	return origins, nil
}

// childListedOrigin is the Origin that Derive reads from is, as a listing
// gives it: from its author and body alone.
func childListedOrigin(is *gogithub.Issue, appSlug string) state.Origin {
	author := github.Author{Login: is.GetUser().GetLogin(), Type: is.GetUser().GetType()}
	return state.Derive(&github.Snapshot{Number: is.GetNumber(), Author: author, Body: is.GetBody()}, appSlug, func(int64) bool { return false }).Origin
}

// childMatch returns the children of parent's plan items among subs, the
// parent's sub-issues, and listed, issues of this repo, by item. A child
// is an issue whose origin names plan=#<parent>, whatever its rev (SPEC
// §5.2). For an issue the App filed, it's read by Derive from a snapshot
// of what the listing gives, its author and body: the listing has no edit
// history, so a body someone edited still matches, as long as it keeps the
// App's marker (plan #249, assumption 8). One whose listed body gives no
// Origin takes it from origins, what its snapshot gives (childOrigins), so
// a child whose origin the App reposted in a comment matches too (#291),
// and so does an issue a person filed, adopted by the plan, whose clean
// App comment carries its origin (#372): its listed body never gives one,
// as Derive reads no marker a person wrote, and only a sub-issue's
// snapshot is read. A sub-issue in another repo, as inRepo tells by its
// repository_url, and a pull request are skipped. One that's a sub-issue
// is Linked. Should two issues name the same item, the first wins: the
// sub-issues in link order, then the rest by number, so the one linked, or
// else the one filed first.
func childMatch(parent int, appSlug string, subs, listed []*gogithub.Issue, inRepo func(string) bool, origins map[int]state.Origin) map[string]childFound {
	found := map[string]childFound{}
	seen := map[int]bool{}
	add := func(is *gogithub.Issue, linked bool) {
		n := is.GetNumber()
		if seen[n] || is.IsPullRequest() {
			return
		}
		seen[n] = true
		// A person's body gives no Origin (github.TrustedMarkers), so a
		// person's issue matches only through origins, by its snapshot.
		o := childListedOrigin(is, appSlug)
		if o.Plan == 0 {
			o = origins[n]
		}
		if o.Plan != parent {
			return
		}
		if had, ok := found[o.Item]; ok {
			logr.Warnf("#%d and #%d are both the child of item %s of #%d's plan, so #%d is the one kept", had.Number, n, o.Item, parent, had.Number)
			return
		}
		found[o.Item] = childFound{Number: n, ID: is.GetID(), Linked: linked, Closed: is.GetState() == "closed"}
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
// returns the child, whose number names it in a later human task's body,
// and which its blocked-by links and the record take. f is the child
// found already, if any (Number 0 for none). numbers is the number of
// each child filed so far, by item, which a human task's body names
// (childSpec.bodyOf).
func (st *Step) childWrite(ctx context.Context, s *github.Snapshot, rev int, key state.Key, c childSpec, f childFound, numbers map[string]int) (childFound, error) {
	var child *github.Snapshot // a child filed in this pass, as the create answered
	if f.Number == 0 {
		body, err := c.bodyOf(numbers)
		if err != nil {
			return childFound{}, fmt.Errorf("write its child's body: %w", err)
		}
		is, err := st.Client.CreateChild(ctx, st.Owner, st.Repo, github.NewChild{Title: c.Title, Body: body, Labels: c.Labels, Type: c.IssueType})
		if err != nil {
			return childFound{}, fmt.Errorf("create its child: %w", err)
		}
		f = childFound{Number: is.GetNumber(), ID: is.GetID()}
		logr.Infof("%s/%s#%d: filed #%d for item %s of its plan, rev %d", st.Owner, st.Repo, s.Number, f.Number, c.Item, rev)
		for _, why := range c.leftOff {
			logr.Warnf("%s/%s#%d: filed #%d for item %s of its plan, rev %d, without the label for %s", st.Owner, st.Repo, s.Number, f.Number, c.Item, rev, why)
		}
		child = childCreated(is, s.Number)
	}
	if !f.Linked { // as one filed just now is, which is touched here
		st.touch(f.Number)
		if err := st.Client.AddSubIssue(ctx, st.Owner, st.Repo, s.Number, f.ID); err != nil {
			return childFound{}, fmt.Errorf("link #%d: %w", f.Number, err)
		}
		logr.Infof("%s/%s#%d: linked #%d under it, for item %s of its plan", st.Owner, st.Repo, s.Number, f.Number, c.Item)
	}
	if err := st.childFirstMove(ctx, s.Number, rev, key, c, f.Number, child); err != nil {
		return childFound{}, err
	}
	return f, nil
}

// childrenLink makes each child in children, by item, blocked by the
// children of the items its item's "Depends on" names, in plan order
// (plan #249, T4). Those name items of the same plan, each once, as S1
// and the planner's schema check, so in a first plan a dependency with no
// child is an error. In a revision, revised, it may name a kept or
// changed item whose child isn't found (ChildrenVisit): that link is
// logged and skipped, and the record names the item (childrenRecord).
// It reads each such child's blocked-by links first (BlockedBy),
// and adds only the ones it lacks, matched by issue ID, so a replay adds
// none twice. It removes none: links someone else added stay. A child it
// adds a link to is reported to Touched first, as the add moves its
// updated_at. A human task, created last, is blocked by what its own item
// depends on, and the children of items that depend on it are blocked by
// it, like any other child's.
func (st *Step) childrenLink(ctx context.Context, items []planItem, children map[string]childFound, revised bool) error {
	for _, it := range items {
		if len(it.DependsOn) == 0 {
			continue
		}
		c := children[it.ID]
		listed, err := st.Client.BlockedBy(ctx, st.Owner, st.Repo, c.Number)
		if err != nil {
			return fmt.Errorf("item %s: %w", it.ID, err)
		}
		has := make(map[int64]bool, len(listed))
		for _, is := range listed {
			has[is.GetID()] = true
		}
		for _, dep := range it.DependsOn {
			b, ok := children[dep]
			if !ok && revised {
				logr.Warnf("%s/%s#%d: item %s of a plan depends on item %s, whose child isn't found, so it isn't blocked by it",
					st.Owner, st.Repo, c.Number, it.ID, dep)
				continue
			}
			if !ok {
				return fmt.Errorf("item %s: it depends on %s, which has no child", it.ID, dep)
			}
			if has[b.ID] {
				continue
			}
			st.touch(c.Number)
			if err := st.Client.AddBlockedBy(ctx, st.Owner, st.Repo, c.Number, b.ID); err != nil {
				return fmt.Errorf("item %s: block #%d by #%d: %w", it.ID, c.Number, b.Number, err)
			}
			logr.Infof("%s/%s#%d: blocked by #%d, as item %s of a plan depends on item %s", st.Owner, st.Repo, c.Number, b.Number, it.ID, dep)
		}
	}
	return nil
}

// childrenRecord posts the record of revision rev's children on parent s
// (plan #249, assumption 7): one App comment listing each item of items
// with its child in children, in plan order, as "- S1 #247: title", then
// the marker created rev=<rev> children=<n>,<n>, which state.CreatedRecord
// reads back. The item IDs and titles are the planner's, so they're
// escaped as text the App didn't write (SPEC §5.1). A title is the
// item's, without the "S1. " a story's issue title may start with, as the
// line names the item already.
//
// For a revision (#297), it lists every child but a dropped item's, a
// kept one by the number it was found under. An item of a revision
// whose child isn't found, kept, changed or dropped, isn't listed: a line
// after the list names it, as one the revision left as it is. With no
// child left to list, the marker has children=none (state.CreatedNone).
func (st *Step) childrenRecord(ctx context.Context, s *github.Snapshot, rev int, items []planItem, children map[string]childFound) error {
	head := "I've created the children of plan rev %d:"
	if slices.ContainsFunc(items, func(it planItem) bool { return it.Status != "" }) {
		head = "I've applied plan rev %d to the children, which are now:"
	}
	lines := []string{fmt.Sprintf(head, rev)}
	nums := make([]string, 0, len(items))
	var missing []string
	for _, it := range items {
		f, ok := children[it.ID]
		if !ok && it.Status != "" {
			missing = append(missing, inline(it.ID))
			continue
		}
		if it.Status == planDropped {
			continue
		}
		title := strings.TrimPrefix(strings.TrimSpace(normalize(it.Title, "")), it.ID+". ")
		lines = append(lines, fmt.Sprintf("- %s #%d: %s", inline(it.ID), f.Number, inline(title)))
		nums = append(nums, strconv.Itoa(f.Number))
	}
	if len(nums) == 0 {
		lines = append(lines, "- none")
	}
	if len(missing) > 0 {
		them := "it as it was"
		if len(missing) > 1 {
			them = "them as they were"
		}
		lines = append(lines, "", fmt.Sprintf("I found no child of %s, so this revision left %s.", list(missing), them))
	}
	listed := strings.Join(nums, ",")
	if listed == "" {
		listed = state.CreatedNone
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "created", Fields: map[string]string{
		"rev": strconv.Itoa(rev), "children": listed}})
	if err != nil {
		return fmt.Errorf("record the children: %w", err)
	}
	if err := st.comment(ctx, s, github.Escape(strings.Join(lines, "\n"))+"\n\n"+marker); err != nil {
		return fmt.Errorf("record the children: %w", err)
	}
	logr.Infof("%s/%s#%d: recorded the children of its plan, rev %d: %s", st.Owner, st.Repo, s.Number, rev, strings.ReplaceAll(listed, ",", ", "))
	for _, id := range missing {
		logr.Warnf("%s/%s#%d: no child of item %s of its plan, rev %d, is found, so the revision left it as it is", st.Owner, st.Repo, s.Number, id, rev)
	}
	return nil
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

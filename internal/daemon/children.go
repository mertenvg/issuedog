package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
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

// ChildrenVisit creates the children of an approved plan (SPEC §5.2):
// the loop calls it after FlagVisit, so the pass that approves a plan, by
// 👍 or /approve, or that skips review, creates its children too. It acts
// only on an open parent at planned, whose move there approved a plan
// revision R with its data (approvedPlanData): the move's comment, ID P,
// posted at T (state.ApprovedPlanMove). A held or closed parent gets
// nothing, and creation carries on once a hold ends back at planned. Once
// a clean record of R's children stands on the parent (state.CreatedRecord,
// SPEC §5.2), it returns before any read or write: the children are
// all written, so a visit costs nothing.
//
// Until then, it describes each item of R that it files (childSpecs):
// every item of a first plan, and only the new items of a revision
// (SPEC §5.2), and each changed item of a revision, which it rewrites in
// place (childSpecsWhere, SPEC §6.4). A kept item gets no write, and a
// dropped one's child is closed (SPEC §6.4, #297). It finds the children filed already
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
//     or for a held one, held → held with that stage as its hold's at and
//     origin, keyed event=p<P> (SPEC §5, §7.2), unless it has a
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
// The children are listed since the plan's first approval (childrenSince),
// as the planner listed them for the revision, so a child someone
// unlinked from the parent by hand is still found. A kept or changed item
// of a revision whose child isn't found is gone, as when it was deleted or
// moved to another repo after the revision was checked, and no later pass
// finds it either: that's a failure no later pass gets past, below, found
// before any write (childrenGone, #362). A dropped item whose child isn't
// found gets no write: a warning names it, and the record says so, and
// leaves it out, so it can't keep the record from standing, and once the
// record stands no pass looks for it again (#297).
//
// Each child it may write to is reported to Touched first, or, for one it
// files, once it has its number, so it skips the rest of the pass and is
// read again on the next. Each write is checked first, so a pass that
// crashed part way runs again with no second child, link or move (SPEC
// §5.2). Any failed read or write is returned, wrapped: Iterate then reads
// the parent again next pass, and the visit starts again from the reads.
//
// But a failure no later pass gets past, as nothing it reads would change
// (childrenStuck), isn't returned: plan data that won't read back, an
// item that can't be written as an issue, a kept or changed item whose
// child is gone, a child GitHub refuses to file with a 422, as for an
// issue type the org doesn't have, a revision's plan comment that names
// no run for its changed children's rewrite (childrenChanged), or a link
// GitHub refuses with a 422, as for a child that has another parent.
// It's reported once on the parent instead, as a state error keyed by
// the comment holding the parent's latest move, the one into planned
// (childrenStuckReport, SPEC §5.3). While that error is reported, the
// visit returns before any read or write, as for a record, so the parent
// waits, at no cost, until it moves, as by /hold and /resume, which gives
// the key a new value, and the visit tries again.
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
	// So is a state error that a failure no later pass gets past was
	// reported with, for the parent's stay at planned.
	errKey := strconv.FormatInt(is.TransitionComment, 10)
	if is.Reported(childrenErrKind, errKey) {
		logr.Debugf("%s/%s#%d: its children wait for it to move, as the state error keyed %s reported why they can't be created", st.Owner, st.Repo, s.Number, errKey)
		return nil
	}
	fail := func(err error) error {
		var stuck *childrenStuck
		if !errors.As(err, &stuck) {
			return wrap(err)
		}
		if err := st.childrenStuckReport(ctx, s, r.Rev, errKey, stuck, wrap(err)); err != nil {
			return wrap(err)
		}
		return nil
	}
	rev, plan, ok, err := approvedPlanData(s, st.AppSlug, d.Has)
	if err != nil {
		return fail(&childrenStuck{why: "its data doesn't read back from the plan comment", err: err})
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
		return fail(&childrenStuck{why: childrenStuckItem, err: err})
	}
	// A revision's changed items, each rewritten in place (SPEC §6.4),
	// are described before any write too, so an item that can't be stops
	// the visit with nothing written.
	changed, err := childSpecsWhere(s, rev.Rev, plan.Items, func(status string) bool { return status == planChanged },
		st.IssueTypes, st.Notify, models)
	if err != nil {
		return fail(&childrenStuck{why: childrenStuckItem, err: err})
	}
	found, err := st.childrenFind(ctx, s, childrenSince(s, st.AppSlug, d.Has, move))
	if err != nil {
		return wrap(err)
	}
	// A kept or changed child that isn't found is gone, so the visit stops
	// with nothing written, and reports it once (#362).
	if err := childrenGone(plan.Items, found); err != nil {
		return fail(err)
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
			return fail(fmt.Errorf("item %s: %w", c.Item, err))
		}
		numbers[c.Item], children[c.Item] = f.Number, f
	}
	// Once the new children are filed, so a changed human task's "Why it
	// blocks" can name a new item's issue, the changed children are
	// rewritten and moved (SPEC §6.4).
	if err := st.childrenChanged(ctx, s, rev.Rev, planComment, key, changed, children, numbers); err != nil {
		return fail(err)
	}
	// plan.Items is plan order, which the links and the record follow,
	// whatever order the children were created in. Of a revision, the new
	// items' children get their links here, and a kept child keeps its
	// links. A kept or changed item's child is found by now (childrenGone),
	// but should a dependency's child not be, it's skipped, not an error,
	// so it can't stop the record (#297).
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
		// open issue below it, deepest first, and unlinked (SPEC §6.4,
		// #297). The record comes only after them, so once
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
// repo's issues updated since the plan's first approval, at since
// (childrenSince), which also hold one filed but not yet linked when a
// pass stopped, and one someone unlinked by hand. Both lists are
// consistent, unlike search, which lags behind writes (SPEC §5.2,
// "Finding a child, without search"). It returns them by item (childMatch),
// with the Origin of each sub-issue whose listed body gives none
// (childOrigins), a person's adopted issue's included (#372), and of each
// issue the App filed whose listed body someone edited
// (childEditedOrigins, #501).
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
	edited, err := st.childEditedOrigins(ctx, s.Number, subs, listed)
	if err != nil {
		return nil, fmt.Errorf("find children: %w", err)
	}
	maps.Copy(origins, edited)
	return childMatch(s.Number, st.AppSlug, subs, listed, st.inRepo, origins), nil
}

// childrenSince is when parent s's plan was first approved: the earliest
// clean comment of the App's holding a move into planned that names a plan
// revision, any of them, or move, the comment holding the latest
// approval's move (state.ApprovedPlanMove), if that's earlier, as when the
// first one's comment is dirty. The children filed already are listed
// since then (childrenFind, SPEC §5.2), not since the latest approval: the
// owner may send the parent back and approve the revision again before the
// next pass, after a child was filed but not linked when a pass stopped,
// and that child's last update is earlier than the new approval. Nor is it
// since the approved revision's first approval: a revision is planned
// against the children listed since its plan's first approval (planPrior),
// and its children are found the same way once it's approved, so a kept
// child of an earlier revision that someone unlinked by hand, and that
// hasn't been updated since, is found by both (#362). A move back from a
// hold names no revision, so it's never the first.
func childrenSince(s *github.Snapshot, appSlug string, dirty func(int64) bool, move github.Comment) time.Time {
	since := move.CreatedAt
	for _, c := range s.Comments {
		if dirty(c.ID) || !c.CreatedAt.Before(since) {
			continue
		}
		if slices.ContainsFunc(github.TrustedMarkers(c.Author, c.Body, appSlug), func(m github.Marker) bool {
			r, err := strconv.Atoi(m.Fields["rev"])
			return m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Planned && err == nil && r > 0
		}) {
			since = c.CreatedAt
		}
	}
	return since
}

// childrenGone is the childrenStuck for the kept and changed items of
// items, a revision's, whose child isn't among found (childrenFind), or
// nil if each has one. The children are listed since the plan's first
// approval (childrenSince), as the planner listed them, so one someone
// unlinked by hand is found: such a child is gone, as when it was deleted
// or moved to another repo after the revision was checked, and no later
// pass finds it either (SPEC §5.2, "A child that isn't found", #362). Its
// why names each item by its ID, the planner's, which the state error
// escapes. A dropped item whose child isn't found isn't one, as the
// revision has nothing to close: the record names it (childrenRecord).
func childrenGone(items []planItem, found map[string]childFound) error {
	var ids, named []string
	for _, it := range items {
		if _, ok := found[it.ID]; !ok && (it.Status == planKept || it.Status == planChanged) {
			ids, named = append(ids, it.ID), append(named, inline(it.ID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	them, were := "it", "it was"
	if len(ids) > 1 {
		them, were = "them", "they were"
	}
	return &childrenStuck{
		why: fmt.Sprintf("no child of %s is found, though the revision keeps or changes %s, as when %s deleted or moved to another repo",
			list(named), them, were),
		err: fmt.Errorf("no child of item %s is among the sub-issues or the issues updated since the plan's first approval", strings.Join(ids, ", ")),
	}
}

// childrenErrKind is the kind of the state error for an approved plan
// whose children can't be created, as a failure no later pass gets past
// stops them (childrenStuck, SPEC §5.3).
const childrenErrKind = "children"

// childrenStuckItem is childrenStuck's why for an item of the plan that
// can't be written as an issue (childSpecs).
const childrenStuckItem = "an item of it can't be written as an issue"

// childrenStuck is a failure creating a plan's children that no later pass
// gets past, as nothing it reads would change: why says what failed, in
// the daemon's own words, for the state error, and err is the cause, which
// may quote the planner's text or GitHub's, so it's only logged (SPEC
// §5.3, What it quotes).
type childrenStuck struct {
	why string
	err error
}

func (e *childrenStuck) Error() string { return e.why + ": " + e.err.Error() }

func (e *childrenStuck) Unwrap() error { return e.err }

// childrenStuckReport posts the state error for stuck on parent s, whose
// approved plan revision is rev, keyed by key, the comment holding the
// parent's latest move. ChildrenVisit calls it only while none was posted
// or acknowledged with that key (state.Issue.Reported). Its why is the
// daemon's text, but an item ID in it is the planner's, so the comment is
// escaped (SPEC §5.1). cause, the full error, is logged as an error as the
// comment is posted, and the comment says the log has it.
func (st *Step) childrenStuckReport(ctx context.Context, s *github.Snapshot, rev int, key string, stuck *childrenStuck, cause error) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": childrenErrKind, "key": key}})
	if err != nil {
		return fmt.Errorf("report why the children can't be created: %w", err)
	}
	text := fmt.Sprintf("I can't create the children of plan rev %d, as %s. "+
		"I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.", rev, stuck.why)
	if err := st.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report why the children can't be created: %w", err)
	}
	logr.Errorf("state error: %v", cause)
	return nil
}

// childRefused reports whether err, from filing a child or linking it
// under its parent, is GitHub's 422, which refuses the write whatever pass
// asks, as for an issue type the org doesn't have, or a child that has
// another parent. A 5xx, or a request that got no answer, isn't: a later
// pass may get past it.
func childRefused(err error) bool {
	var e *gogithub.ErrorResponse
	return errors.As(err, &e) && e.Response != nil && e.Response.StatusCode == http.StatusUnprocessableEntity
}

// childOrigins returns the Origin of each of subs, a parent's sub-issues
// in this repo, whose listed body gives none (childListedOrigin), by
// number, as state.Derive reads it from the issue's snapshot, comments
// and dirty content included. That's one of two kinds of sub-issue:
//   - one the App filed, whose origin the App reposted in a comment once
//     the body lost it (originRepost, SPEC §6.4, #291). Until that repost
//     exists, as when the parent's visit comes before the child's step 1
//     in the pass after an edit removed the marker, the origin is the one
//     the repost would carry (originLost): from a version of the body the
//     App wrote, never one anyone else did, so the child is still found,
//     and not filed twice (#347). Only then is its history read;
//   - one a person filed, whose body never gives an origin, as Derive
//     reads markers only from the App (github.TrustedMarkers), but whose
//     clean App comment may carry one: an adopted issue, such as #195,
//     which #224's plan adopted as one of its stories (SPEC §5, #372). A
//     marker in a dirty App comment, or in a person's comment, gives
//     none, by Derive's rules.
//
// Each such issue costs a snapshot, and one the App filed whose origin
// isn't read costs its body's history too, unless Dirty read it already
// for the same edit, as it does for a trusted user's (histories), or an
// earlier call read it, as originLost keeps it, or found no marker in any
// version of the App's for the same body (originNone). One whose origin is read costs no history beyond
// Dirty's. The callers read them only before
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
			continue
		}
		m, ok, err := st.originLost(ctx, s, d)
		if err != nil {
			return nil, fmt.Errorf("read #%d's origin: %w", n, err)
		}
		if o, read := state.OriginOf(m); ok && read {
			logr.Debugf("%s/%s#%d: its origin, item %s of #%d's plan, is read from its description's history, as no comment carries it yet",
				st.Owner, st.Repo, n, o.Item, o.Plan)
			origins[n] = o
		}
	}
	return origins, nil
}

// childEditedOrigins returns, by number, the Origin of each issue the App
// filed whose listed body gives one (childListedOrigin), but which someone
// may have edited since: each of subs, a parent's sub-issues in this repo,
// and each of listed whose listed body names the parent's plan. A listing
// names no editor, so these bodies' histories are read, all in one query,
// and none is read when there are no such issues, as before a first child
// is filed. An issue never edited has no history: its listed body is the
// App's original post, and it gets no entry. One that was edited takes its
// Origin from the newest version of its body the App wrote that has a
// whole marker, back to the original post (originFromHistory), never from
// one anyone else wrote, as originLost reads it (SPEC §5.2, §6.4). So a
// body that kept the App's marker matches as before, and one whose edit
// pasted a whole marker naming another item, which Derive reads from the
// body until step 1 marks it dirty, still matches its own item (#501):
// matched to the other, it would leave its own item to be filed a second
// time, and the other never filed. One with no such version, as when
// someone deleted the text of each, gets the zero Origin, so it's no
// item's child, as for a body with no marker of the App's (childOrigins).
//
// Each history that reaches the body as listed is kept (histories), so
// the issue's own step 1 in the same pass doesn't read it again when its
// dirty check needs it. Like childOrigins, it's called only before the
// record stands (ChildrenVisit) and while a revision is planned and
// checked (planPrior.read), so an unchanged pass reads none.
func (st *Step) childEditedOrigins(ctx context.Context, parent int, subs, listed []*gogithub.Issue) (map[int]state.Origin, error) {
	var nodes []string
	byNode := map[string]*gogithub.Issue{}
	add := func(is *gogithub.Issue, linked bool) {
		node := is.GetNodeID()
		if _, ok := byNode[node]; ok || node == "" || is.IsPullRequest() {
			return
		}
		if o := childListedOrigin(is, st.AppSlug); o.Plan == 0 || !linked && o.Plan != parent {
			return
		}
		byNode[node] = is
		nodes = append(nodes, node)
	}
	for _, is := range subs {
		if st.inRepo(is.GetRepositoryURL()) {
			add(is, true)
		}
	}
	for _, is := range listed {
		add(is, false)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	read, err := st.Client.History(ctx, nodes)
	if err != nil {
		return nil, fmt.Errorf("read the edit history of the issues filed for #%d's plan: %w", parent, err)
	}
	if st.histories == nil {
		st.histories = histories{}
	}
	origins := map[int]state.Origin{}
	for _, node := range nodes {
		h, is := read[node], byNode[node]
		if len(h) == 0 {
			continue
		}
		n, body := is.GetNumber(), is.GetBody()
		if newest := h[len(h)-1]; len(h) >= 2 && newest.Text == body {
			st.histories[node] = kept{at: newest.At, body: body, edits: h}
		}
		listedOrigin := childListedOrigin(is, st.AppSlug)
		m, ok := originFromHistory(h, st.AppSlug)
		o, whole := state.OriginOf(m)
		switch {
		case !ok || !whole:
			logr.Warnf("%s/%s#%d: its description names item %q of #%d's plan, but no version of it the App wrote has an origin marker, so it's no item's child",
				st.Owner, st.Repo, n, listedOrigin.Item, listedOrigin.Plan)
			o = state.Origin{}
		case o.Plan != listedOrigin.Plan || o.Item != listedOrigin.Item:
			logr.Warnf("%s/%s#%d: its description names item %q of #%d's plan, but the App filed it for item %s of #%d's, so it's matched to that one",
				st.Owner, st.Repo, n, listedOrigin.Item, listedOrigin.Plan, o.Item, o.Plan)
		}
		origins[n] = o
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
// of what the listing gives, its author and body, unless origins has its
// Origin: the listing has no edit history, so an edited body's comes from
// the versions of it the App wrote (childEditedOrigins). So a body someone
// edited still matches, as long as it keeps the App's marker, since
// reusing an edited child is safer than filing a duplicate, but a marker
// someone pasted in, naming another item, counts for nothing, since
// matching by it files one (SPEC §5.2, #501). One whose listed body gives
// no Origin takes it from origins, what its snapshot gives (childOrigins), so
// a child whose origin the App reposted in a comment matches too (#291),
// as does one whose repost isn't posted yet, by its body's history (#347),
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
		// An App body someone edited matches only through origins too, by
		// its history (childEditedOrigins).
		o, ok := origins[n]
		if !ok {
			o = childListedOrigin(is, appSlug)
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
			err = fmt.Errorf("create its child: %w", err)
			if childRefused(err) {
				return childFound{}, &childrenStuck{why: fmt.Sprintf("GitHub refused to file the child of item %s", inline(c.Item)), err: err}
			}
			return childFound{}, err
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
			err = fmt.Errorf("link #%d: %w", f.Number, err)
			if childRefused(err) {
				return childFound{}, &childrenStuck{why: fmt.Sprintf("GitHub refused to link #%d, the child of item %s, under this issue", f.Number, inline(c.Item)), err: err}
			}
			return childFound{}, err
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
// (SPEC §5.2). Those name items of the same plan, each once, as the plan's
// validation (SPEC §11.3) and the planner's schema check, so in a first
// plan a dependency with no child is an error. In a revision, revised, it
// may name a kept or
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
// (SPEC §5, created): one App comment listing each item of items
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

// childFirstMove gives child number n its first move (SPEC §7.2), intake → c's
// first stage on TriggerCreated, keyed by key, unless a clean transition
// marker of the App's that stands for one is on it already (childMoved):
// its own first move, or an owner's stage change recorded as an override.
// child is the snapshot a create in this pass answered with, which nobody
// else can have changed yet, or nil for a child found again: that one is
// read fresh, and its labels checked first (discCheckLabels), so an
// owner's stage change since is recorded as an override, as for any issue,
// and it keeps the labels it has. One found closed, as when someone closed
// it before its first move, gets none.
//
// A first move keyed by key that a crash cut short after its comment, with
// its stage label still to set, as for a child back at intake from a hold,
// is finished by the label rules, as any App write cut short is (SPEC
// §5.1).
//
// One found held, as by a /hold on it, after a hold on the parent moved it
// before its first move, stays held: its first move is held → held, with
// no reason, so it's no hold of its own, and c's first stage as its at and
// origin, which /resume then takes it to, as for a held child that a
// revision changes (childChanged, SPEC §7.2).
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
		// A first move keyed by key whose labels a crash cut short, which a
		// child back at intake from a hold has to change, is finished by
		// the label rules, below.
		if is := state.Derive(s, st.AppSlug, d.Has); childMoved(s, st.AppSlug, d.Has) && (!is.Moved(key) || childStageShown(s, is.Stage)) {
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
	text := fmt.Sprintf("Created from #%d's plan, rev %d, as %s.", parent, rev, c.Item)
	var m Move
	if is := state.Derive(child, st.AppSlug, dirty); is.Stage == state.Held {
		fields := map[string]string{"at": string(c.Stage), "origin": string(c.Stage)}
		// SPEC §7.2 has no row of its own for it, so it takes /hold's,
		// any open → held, as a changed held child's does (childChanged).
		m = Move{To: state.Held, Trigger: state.TriggerHold, Key: key, Fields: state.HeldStackFields(fields, is.HeldStack.Seen),
			Text: text + fmt.Sprintf(" It stays on hold, and `/resume` takes it to `%s`.", stage)}
	} else {
		text += fmt.Sprintf(" It starts at `%s`", stage)
		if c.Stage == state.Planning {
			text += ", where the planner writes its task plan"
		}
		m = Move{To: c.Stage, Trigger: state.TriggerCreated, Key: key, Text: text + "."}
	}
	if err := st.Transition(ctx, child, dirty, m); err != nil {
		return fmt.Errorf("first move of #%d: %w", n, err)
	}
	return nil
}

// childMoved reports whether s has a transition marker of the App's that
// counts (state.Derive), in a clean comment or a clean body the App last
// edited, and stands for its first move: any but a hold's, which carries a
// reason, and a move back from held to intake that isn't an override. A
// hold moves every open issue below the one held, so a child linked but
// not yet moved when a pass stopped may have been held since, and resumed
// back to intake, with no first move (SPEC §5.2). A held → held marker
// with no reason, a held child's first move (childFirstMove), counts. So
// does a held → intake override, with by=, as for a stage a trusted user
// set by hand: a found child that has one gets no first move (SPEC §5.2,
// "A found child keeps its labels").
func childMoved(s *github.Snapshot, appSlug string, dirty func(int64) bool) bool {
	return slices.ContainsFunc(state.Derive(s, appSlug, dirty).Markers, func(m github.Marker) bool {
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		_, reason := m.Fields["reason"]
		_, by := m.Fields["by"]
		return m.Kind == "transition" && !(to == state.Held && reason) && !(from == state.Held && to == state.Intake && !by)
	})
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

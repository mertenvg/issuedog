package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the revised plan (#195, SPEC §11.3): a plan for an
// issue that was planned before, which marks every item kept, changed, new
// or dropped against the children of its approved plan.

// The statuses of a revised plan's items (SPEC §11.3). A first
// plan has none.
const (
	planKept    = "kept"    // a child left as it is
	planChanged = "changed" // a child rewritten in place
	planNew     = "new"     // an item that becomes a new child
	planDropped = "dropped" // a child closed as not planned
)

// planStatuses are the statuses an item of a revision may have.
var planStatuses = []string{planKept, planChanged, planNew, planDropped}

// planFiles reports whether an item of status gets a child filed for it
// once its plan is approved (childSpecs): any item of a first plan, which
// has no status, and a revision's new items. A revision's other items
// have their children already.
func planFiles(status string) bool { return status == "" || status == planNew }

// planBare reports whether an item of status carries its ID, title and
// status only: a kept or a dropped one.
func planBare(status string) bool { return status == planKept || status == planDropped }

// planPrior is what a revision is planned and checked against: the plan
// revision the issue's latest move into planned approved, and its items,
// read back from its plan-data marker (approvedPlanData), and once read
// (planPrior.read), the children that plan's items were filed as, and the
// issue's other sub-issues.
type planPrior struct {
	Rev   int        // the approved plan revision
	Items []planItem // its items, as its plan-data marker keeps them
	move  github.Comment
	since time.Time // the plan's first approval, which its children are listed since (childrenSince)
	// Children are the issues matched to the plan's items by their
	// origin (childMatch), in plan order, then any other item's by
	// number.
	Children []planChild
	// Others are the issue's sub-issues that aren't children of its plan,
	// for context only, and Subs the count of every sub-issue, in this
	// repo or not.
	Others []plannerSub
	Subs   int
}

// planChild is a child of the approved plan, as a listing gives it.
type planChild struct {
	Item   string // the plan item its origin names
	Number int
	Title  string
	// Type is its issue type, as listed, or with none, "" until the
	// planner's Context judges its type:* labels, from Labels, as the label
	// rules do (plannerTrustedSub, #445). Labels are nil for one with an
	// issue type.
	Type   string
	Labels []string
	Stage  state.Stage // while it's open, as its markers set it, once the planner's Context reads it (plannerTrustedSub), or ""
	Closed bool
	// Reason is a closed child's state_reason, such as "completed" or
	// "not_planned", or "".
	Reason string
}

// errNoApprovedPlan is planPriorOf's error for an issue planned before
// that has no approved plan revision with its data.
var errNoApprovedPlan = errors.New("it has no approved plan revision whose data reads back, so it can't be revised")

// planPriorOf returns the planPrior of issue s in state is, for a revision,
// or nil for an issue never planned before (plannedBefore), whose plan is a
// first plan. It reads nothing: the children are read by planPrior.read.
// An issue planned before whose approved revision can't be read back, as
// when no move into planned names one (state.ApprovedPlan), or its comment
// was edited since, wraps errNoApprovedPlan: it isn't planned again, as a
// revision can't be checked without its children's items.
func planPriorOf(s *github.Snapshot, appSlug string, is state.Issue, dirty func(int64) bool) (*planPrior, error) {
	if !plannedBefore(is) {
		return nil, nil
	}
	rev, plan, ok, err := approvedPlanData(s, appSlug, dirty)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoApprovedPlan, err)
	}
	move, moved := state.ApprovedPlanMove(s, appSlug, dirty)
	if !ok || !moved {
		return nil, errNoApprovedPlan
	}
	return &planPrior{Rev: rev.Rev, Items: plan.Items, move: move, since: childrenSince(s, appSlug, dirty, move)}, nil
}

// read reads the children of the plan on issue s, as creating the
// children finds them (childrenFind, SPEC §5.2): its sub-issues, then the repo's issues
// updated since the plan's first approval, two listings, so each child's state
// and close reason come with it. A sub-issue whose listed body gives no
// origin is read on its own (childOrigins): one the App filed, for an
// origin the App reposted in a comment (#291), so it's still the plan's
// child, and one a person filed, for an origin in a clean App comment, so
// an adopted issue is its item's child, never one of Others, and a
// revision keeps, changes or drops it like any child (#372). It fills in
// Children, Others and Subs.
func (pr *planPrior) read(ctx context.Context, st *Step, s *github.Snapshot) error {
	a := st.Applier
	subs, err := a.Client.SubIssues(ctx, a.Owner, a.Repo, s.Number)
	if err != nil {
		return fmt.Errorf("its children: %w", err)
	}
	listed, err := a.Client.IssuesSince(ctx, a.Owner, a.Repo, pr.since)
	if err != nil {
		return fmt.Errorf("its children: %w", err)
	}
	origins, err := st.childOrigins(ctx, subs)
	if err != nil {
		return fmt.Errorf("its children: %w", err)
	}
	found := childMatch(s.Number, a.AppSlug, subs, listed, a.inRepo, origins)
	pr.Children = nil
	byNumber := map[int]*gogithub.Issue{}
	for _, is := range append(slices.Clone(listed), subs...) { // a sub-issue's listing wins
		byNumber[is.GetNumber()] = is
	}
	children := map[int]bool{}
	for item, f := range found {
		is := byNumber[f.Number]
		// Its stage, and a type by label, aren't the listing's to give, as
		// its labels may be untrusted: the planner's Context reads them
		// (plannerTrustedSub).
		c := planChild{Item: item, Number: f.Number, Title: is.GetTitle(),
			Closed: is.GetState() == "closed", Reason: is.GetStateReason()}
		c.Type, c.Labels = plannerListedType(is)
		pr.Children = append(pr.Children, c)
		children[f.Number] = true
	}
	order := map[string]int{}
	for i, it := range pr.Items {
		order[it.ID] = i + 1
	}
	slices.SortFunc(pr.Children, func(x, y planChild) int {
		ox, oy := cmp.Or(order[x.Item], len(order)+1), cmp.Or(order[y.Item], len(order)+1)
		return cmp.Or(cmp.Compare(ox, oy), cmp.Compare(x.Number, y.Number))
	})
	pr.Subs, pr.Others = len(subs), nil
	for _, is := range subs {
		sub := plannerSubOf(a, is)
		if sub.Repo == "" && children[sub.Number] {
			continue
		}
		pr.Others = append(pr.Others, sub)
	}
	return nil
}

// child returns the child of item id, if any.
func (pr *planPrior) child(id string) (planChild, bool) {
	i := slices.IndexFunc(pr.Children, func(c planChild) bool { return c.Item == id })
	if i < 0 {
		return planChild{}, false
	}
	return pr.Children[i], true
}

// keptDeps are the dependencies of each kept item of revision p, which
// carries none of its own: its item's in the approved revision, less any
// that name an item p drops or doesn't list, as those links go (SPEC
// §5.2). The
// cycle check and the link limits count them (SPEC §11.3). For a first
// plan, pr is nil, and there are none.
func (pr *planPrior) keptDeps(p planBody) map[string][]string {
	if pr == nil {
		return nil
	}
	status := map[string]string{}
	for _, it := range p.Items {
		status[it.ID] = it.Status
	}
	deps := map[string][]string{}
	for _, it := range p.Items {
		if it.Status != planKept {
			continue
		}
		i := slices.IndexFunc(pr.Items, func(e planItem) bool { return e.ID == it.ID })
		if i < 0 {
			continue
		}
		for _, d := range pr.Items[i].DependsOn {
			if st, ok := status[d]; ok && st != planDropped && d != it.ID && !slices.Contains(deps[it.ID], d) {
				deps[it.ID] = append(deps[it.ID], d)
			}
		}
	}
	return deps
}

// keptData is plan p's data, as its plan-data marker keeps it (SPEC §5).
// For a first plan, where pr is nil, it's p itself, so the data is as it
// was before revisions. For a revision, each kept item, which carries its
// ID, title and status only, is its approved item's entry in full, read
// from the approved revision's own plan data (approvedPlanData), never
// from the planner's output: its outcome, criteria and task body, with
// its status kept and the dependencies keptDeps gives it, which are its
// links once p is applied. So the next revision reads an item kept twice
// from this revision's data alone: its dependencies count in the cycle
// check and the link limits, and children.md shows its text (SPEC §11.3).
// A kept item with no approved entry, as for a child whose item the
// approved revision doesn't list, stays as it is. p is left as it is.
func (pr *planPrior) keptData(p planBody) planBody {
	if pr == nil {
		return p
	}
	kept := pr.keptDeps(p)
	p.Items = slices.Clone(p.Items)
	for i, it := range p.Items {
		if it.Status != planKept {
			continue
		}
		j := slices.IndexFunc(pr.Items, func(e planItem) bool { return e.ID == it.ID })
		if j < 0 {
			continue
		}
		e := pr.Items[j]
		e.Status, e.DependsOn = planKept, append([]string{}, kept[it.ID]...)
		p.Items[i] = e
	}
	return p
}

// planDepsOf returns each item's dependencies as the links will be once
// plan p is applied: a kept item's from keptDeps, none for a dropped one,
// and the item's own for any other.
func planDepsOf(p planBody, kept map[string][]string) func(planItem) []string {
	return func(it planItem) []string {
		switch it.Status {
		case planKept:
			return kept[it.ID]
		case planDropped:
			return nil
		}
		return it.DependsOn
	}
}

// The rules a revision is checked by beside checkPlan's others (SPEC
// §11.3). Each failure names one.
const (
	ruleStatus      = "status"       // a revision's items each have a status, and a first plan's none
	ruleChildren    = "children"     // every child appears exactly once, kept, changed or dropped
	ruleClosedChild = "closed-child" // a closed child is only ever kept
	ruleNewID       = "new-id"       // a new item's ID is no child's and no earlier revision's
)

// checkStatuses checks p's statuses: none in a first plan, where pr is
// nil, and in a revision, the revision's rules against pr, calling fail for
// each one broken:
//   - status: every item has a status, kept, changed, new or dropped, and
//     a kept or dropped one carries its ID and title only;
//   - children: every child of pr appears exactly once, marked kept,
//     changed or dropped, and only a child's item is;
//   - closed-child: a closed child is only ever kept;
//   - new-id: a new item's ID is no child's, and no item's of the approved
//     revision, which lists every item an earlier revision approved, since
//     each revision lists every child.
//
// The dependencies, the cycle check and the link limits are checkPlan's
// (checkPlanDeps), with the kept items' from pr (keptDeps).
func checkStatuses(p planBody, pr *planPrior, fail func(string, string, ...any)) {
	if pr == nil {
		for _, it := range p.Items {
			if it.Status != "" {
				fail(ruleStatus, "%s has status %q, but this is the issue's first plan, whose items have none", planAt(p, it), it.Status)
			}
		}
		return
	}
	listed := map[string][]string{} // each ID's statuses, in plan order
	for _, it := range p.Items {
		at := planAt(p, it)
		switch {
		case it.Status == "":
			fail(ruleStatus, "%s has no status: every item of a revision is kept, changed, new or dropped", at)
		case !slices.Contains(planStatuses, it.Status):
			fail(ruleStatus, "%s has the unknown status %q, not kept, changed, new or dropped", at, it.Status)
		case planBare(it.Status):
			if extra := planExtraFields(it); len(extra) > 0 {
				fail(ruleStatus, "%s is %s, so it has an ID and a title only, not %s", at, it.Status, strings.Join(extra, ", "))
			}
		}
		listed[it.ID] = append(listed[it.ID], it.Status)
	}
	for _, c := range pr.Children {
		statuses := listed[c.Item]
		switch {
		case len(statuses) == 0:
			fail(ruleChildren, "child #%d, item %s, isn't listed: every child is kept, changed or dropped", c.Number, c.Item)
		case len(statuses) > 1:
			fail(ruleChildren, "child #%d, item %s, is listed %d times, not once", c.Number, c.Item, len(statuses))
		}
		for _, st := range statuses {
			switch {
			case st == planNew:
				fail(ruleChildren, "child #%d, item %s, is marked new: a child is kept, changed or dropped", c.Number, c.Item)
			case c.Closed && (st == planChanged || st == planDropped):
				fail(ruleClosedChild, "child #%d, item %s, is closed, so it can only be kept, not %s: a change is a new item with a new ID", c.Number, c.Item, st)
			}
		}
	}
	earlier := map[string]bool{}
	for _, it := range pr.Items {
		earlier[it.ID] = true
	}
	for _, it := range p.Items {
		c, isChild := pr.child(it.ID)
		switch {
		case it.Status == planNew && isChild:
			fail(ruleNewID, "%s is new, but it's the item of child #%d", it.ID, c.Number)
		case it.Status == planNew && earlier[it.ID]:
			fail(ruleNewID, "%s is new, but it's an item of revision %d, which was approved", it.ID, pr.Rev)
		case slices.Contains([]string{planKept, planChanged, planDropped}, it.Status) && !isChild:
			fail(ruleChildren, "%s is %s, but no child is item %s: an item with no child is new, with a new ID", it.ID, it.Status, it.ID)
		}
	}
}

// planAt names item it of p in a rule's detail: its ID, or its place for
// an ID that isn't one.
func planAt(p planBody, it planItem) string {
	if planItemIDRE.MatchString(it.ID) {
		return it.ID
	}
	return "item " + strconv.Itoa(slices.IndexFunc(p.Items, func(e planItem) bool { return e.ID == it.ID })+1)
}

// planExtraFields names the fields it has beside its ID, title and status.
func planExtraFields(it planItem) []string {
	var extra []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"outcome", it.Outcome != ""}, {"criteria", it.Criteria != nil}, {"depends_on", it.DependsOn != nil},
		{"priority", it.Priority != ""}, {"model", it.Model != ""}, {"effort", it.Effort != ""},
		{"scope", it.Scope != ""}, {"human", it.Human}, {"task", it.Task != nil}, {"human_task", it.HumanTask != nil},
	} {
		if f.set {
			extra = append(extra, f.name)
		}
	}
	return extra
}

// plannerChildrenIntroRevision introduces children.md for a revision.
const plannerChildrenIntroRevision = "This issue was planned before, so your plan is a revision of its approved plan. " +
	"The children are the issues created from that plan, each with its item ID, number, title, type, and stage or close reason. " +
	"List every child exactly once, by its item ID: `kept`, `changed` or `dropped`. A closed child can only be kept. " +
	"An item of the approved revision with no child isn't listed, and its ID isn't used again."

// plannerChildrenFile is children.md for a revision on issue n, after pr
// is read: the children, each with its item ID, number, title, type, and
// stage, as the planner's Context reads them (plannerTrustedSub), or close
// reason; the approved revision's items, as its plan data, fenced; and the other
// sub-issues, as context only. Titles, types and the data are escaped
// (SPEC §5.1).
func plannerChildrenFile(n int, pr *planPrior) (string, error) {
	var lines []string
	for _, c := range pr.Children {
		where := "not in the pipeline"
		switch {
		case c.Closed && c.Reason != "":
			where = "closed as " + github.Escape(strings.ReplaceAll(c.Reason, "_", " "))
		case c.Closed:
			where = "closed"
		case c.Stage != "":
			where = "`stage:" + string(c.Stage) + "`"
		}
		lines = append(lines, fmt.Sprintf("- %s: #%d, %s (type %s, %s)", github.Escape(c.Item), c.Number,
			github.Escape(strings.Join(strings.Fields(c.Title), " ")), github.Escape(cmp.Or(c.Type, "none")), where))
	}
	children := "None: no issue created from the approved plan was found."
	if len(lines) > 0 {
		children = strings.Join(lines, "\n")
	}
	data, err := json.MarshalIndent(pr.Items, "", "  ")
	if err != nil {
		return "", fmt.Errorf("the approved revision's items: %w", err)
	}
	return ivFile("Children of #"+strconv.Itoa(n)+"'s plan", plannerChildrenIntroRevision, []string{
		"## Children of plan revision " + strconv.Itoa(pr.Rev) + "\n\n" + children,
		ivSection(fmt.Sprintf("Items of plan revision %d, as approved, with their task bodies: a kept item keeps these, "+
			"and its dependencies are counted from here", pr.Rev), string(data)),
		"## Other sub-issues\n\nLinked under this issue, but not created from its plan: context only. Never list them as items.\n\n" +
			plannerList(pr.Others, 0),
	}, ""), nil
}

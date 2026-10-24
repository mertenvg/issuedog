package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Revised plans (#195, SPEC §11.3): the revision rules, its rendering and its
// data, against a planPrior built by hand. planner_revision_test.go reads
// one from a fake and drives the planner through the harness.

// revisionPrior is story #124's approved plan, revision 1, the golden story
// plan, and its children: T1 closed as completed, T2 open at ready and T3,
// the human task, open at needs-human. The golden revision is checked
// against it.
func revisionPrior(t *testing.T) *planPrior {
	t.Helper()
	return &planPrior{Rev: 1, Items: goldenPlan(t, "story-plan").Items, Subs: 3, Children: []planChild{
		{Item: "T1", Number: 201, Title: "Record when each quote expires", Type: "task", Closed: true, Reason: "completed"},
		{Item: "T2", Number: 202, Title: "Refuse an expired quote at booking", Type: "task", Stage: state.Ready},
		{Item: "T3", Number: 203, Title: "Set the quote lifetime in production", Type: "task", Stage: state.NeedsHuman},
	}}
}

// revisionFacts are the facts the golden revision is checked against: the
// story #124 at p2, its three children its sub-issues, as plan revision 2.
func revisionFacts(t *testing.T) planFacts {
	f := storyFacts()
	f.Rev, f.SubIssues, f.Prior = 2, 3, revisionPrior(t)
	return f
}

// planTestPlanned is issue n, of type typ, with a parent if parent is set,
// planned before: the spec's approval to planning (101), plan revision 1
// of the golden plan, story-plan for a story, epic-plan for an epic, as
// the planner posts it (102), and its approval by 👍 into planned (103),
// whose body names the revision's hash. Then after, as for planTestIssue,
// from 104 on.
func planTestPlanned(t *testing.T, n int, typ string, parent bool, after ...string) *github.Snapshot {
	t.Helper()
	s := planTestIssue(n, typ, parent, append([]string{planTestApproval, "app: plan", "app: approval"}, after...)...)
	golden := "story-plan"
	if typ == "epic" {
		golden = "epic-plan"
	}
	body, err := planCommentBody(goldenPlan(t, golden), planFacts{Issue: n, Rev: 1, MaxTasks: 10, Priority: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	ms := github.ParseMarkers(body)
	s.Comments[1].Body = body
	s.Comments[2].Body = fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s seen=102 -->",
		ms[len(ms)-1].Fields["body"])
	return s
}

// planPriorOf reads an issue never planned before as a first plan, one
// planned before as a revision of its approved revision, and refuses one
// planned before whose approved revision can't be read back: a move into
// planned that names no revision, a plan comment edited since or dirty,
// or no plan-data in it.
func TestPlanPriorOf(t *testing.T) {
	t.Parallel()
	clean := func(int64) bool { return false }
	of := func(s *github.Snapshot, dirty func(int64) bool) (*planPrior, error) {
		return planPriorOf(s, "issuedog", state.Derive(s, "issuedog", dirty), dirty)
	}
	if pr, err := of(planTestIssue(7, "epic", false, planTestApproval), clean); pr != nil || err != nil {
		t.Errorf("never planned: %+v, %v; want a first plan", pr, err)
	}
	s := planTestPlanned(t, 7, "epic", false, "app: from=planned to=planning event=11 by=owner")
	pr, err := of(s, clean)
	if err != nil || pr == nil || pr.Rev != 1 || len(pr.Items) != 3 || pr.Items[0].ID != "S1" || pr.move.ID != 103 {
		t.Fatalf("planned: %+v, %v; want revision 1's three stories, moved by comment 103", pr, err)
	}
	edited := planTestPlanned(t, 7, "epic", false)
	edited.Comments[1].Body = strings.Replace(edited.Comments[1].Body, "### Plan", "### A plan", 1)
	noData := planTestPlanned(t, 7, "epic", false)
	plain, err := planRevision("### Plan for #7 (rev 1)\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->", 1)
	if err != nil {
		t.Fatal(err)
	}
	ms := github.ParseMarkers(plain)
	noData.Comments[1].Body = plain
	noData.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=" + ms[len(ms)-1].Fields["body"] + " -->"
	for name, tc := range map[string]struct {
		s     *github.Snapshot
		dirty func(int64) bool
	}{
		"a move into planned with no revision": {planTestIssue(7, "epic", false, planTestApproval,
			"app: from=plan-review to=planned cmd=r9 rev=1 body=abcd1234"), clean},
		"the plan comment edited since":      {edited, clean},
		"the plan comment dirty":             {planTestPlanned(t, 7, "epic", false), func(id int64) bool { return id == 102 }},
		"the plan comment with no plan-data": {noData, clean},
	} {
		if pr, err := of(tc.s, tc.dirty); pr != nil || !errors.Is(err, errNoApprovedPlan) {
			t.Errorf("%s: %+v, %v; want errNoApprovedPlan", name, pr, err)
		}
	}
}

// revItem finds item id in o's plan, or fails t.
func revItem(t *testing.T, o *planOutput, id string) *planItem {
	t.Helper()
	for i := range o.Plan.Items {
		if o.Plan.Items[i].ID == id {
			return &o.Plan.Items[i]
		}
	}
	t.Fatalf("the plan has no item %s", id)
	return nil
}

// The golden revision passes: T1 kept though closed, T2 changed, T3
// dropped and T4 new, which depends on the kept T1.
func TestCheckRevisionGolden(t *testing.T) {
	t.Parallel()
	if err := checkPlan(plannerOutput(t, "story-revision"), revisionFacts(t)); err != nil {
		t.Fatal(err)
	}
}

// Each revision rule fails the golden revision broken in only that rule,
// by name, and its detail names the child or item. A first plan with a
// status is refused, and one without passes as before.
func TestCheckRevisionRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, base string
		rules      []string // the rules broken, sorted
		detail     string   // what the error says
		change     func(*testing.T, *planOutput, *planFacts)
	}{
		// children: every child exactly once, kept, changed or dropped
		{"a child omitted", "story-revision", []string{ruleChildren},
			"child #203, item T3, isn't listed: every child is kept, changed or dropped",
			func(_ *testing.T, o *planOutput, _ *planFacts) { o.Plan.Items = slices.Delete(o.Plan.Items, 2, 3) }},
		{"a child listed twice", "story-revision", []string{ruleChildren, ruleItemID},
			"child #203, item T3, is listed 2 times, not once",
			func(t *testing.T, o *planOutput, _ *planFacts) {
				o.Plan.Items = append(o.Plan.Items, *revItem(t, o, "T3"))
			}},
		{"a child marked new", "story-revision", []string{ruleChildren, ruleNewID},
			"child #202, item T2, is marked new: a child is kept, changed or dropped",
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T2").Status = planNew }},
		{"an item kept that isn't a child", "story-revision", []string{ruleChildren},
			"T4 is kept, but no child is item T4: an item with no child is new, with a new ID",
			func(t *testing.T, o *planOutput, _ *planFacts) {
				*revItem(t, o, "T4") = planItem{ID: "T4", Title: "Give the quote lifetime a default", Status: planKept}
				revItem(t, o, "T2").DependsOn = []string{"T1"}
			}},
		{"an approved item with no child, kept", "story-revision", []string{ruleChildren},
			"T3 is kept, but no child is item T3",
			func(t *testing.T, o *planOutput, f *planFacts) {
				f.Prior.Children = f.Prior.Children[:2]
				revItem(t, o, "T3").Status = planKept
			}},

		// closed-child: a closed child is only ever kept
		{"a closed child changed", "story-revision", []string{ruleClosedChild},
			"child #201, item T1, is closed, so it can only be kept, not changed: a change is a new item with a new ID",
			func(t *testing.T, o *planOutput, _ *planFacts) {
				changed := *revItem(t, o, "T4")
				changed.ID, changed.Status, changed.DependsOn = "T1", planChanged, []string{}
				*revItem(t, o, "T1") = changed
				revItem(t, o, "T4").DependsOn = []string{"T1"}
			}},
		{"a closed child dropped", "story-revision", []string{ruleClosedChild, ruleDependsOn},
			"child #201, item T1, is closed, so it can only be kept, not dropped",
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T1").Status = planDropped }},

		// new-id: no child's, and no earlier revision's
		{"a new item reusing a child's ID", "story-revision", []string{ruleChildren, ruleDependsOn, ruleItemID, ruleNewID},
			"T3 is new, but it's the item of child #203",
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T4").ID = "T3" }},
		{"a new item reusing an approved item's ID", "story-revision", []string{ruleNewID},
			"T3 is new, but it's an item of revision 1, which was approved",
			func(t *testing.T, o *planOutput, f *planFacts) {
				f.Prior.Children = f.Prior.Children[:2] // T3's child was never filed
				o.Plan.Items = slices.Delete(o.Plan.Items, 2, 3)
				revItem(t, o, "T4").ID = "T3"
				revItem(t, o, "T2").DependsOn = []string{"T1", "T3"}
			}},

		// status: on every item of a revision, and nothing else on a bare one
		{"an item with no status", "story-revision", []string{ruleStatus},
			"T4 has no status: every item of a revision is kept, changed, new or dropped",
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T4").Status = "" }},
		{"an unknown status", "story-revision", []string{ruleStatus},
			`T4 has the unknown status "renamed", not kept, changed, new or dropped`,
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T4").Status = "renamed" }},
		{"a kept item with fields", "story-revision", []string{ruleStatus},
			"T1 is kept, so it has an ID and a title only, not outcome, criteria, scope",
			func(t *testing.T, o *planOutput, _ *planFacts) {
				it := revItem(t, o, "T1")
				it.Outcome, it.Criteria, it.Scope = "Done.", []string{"Done."}, "booking"
			}},
		{"a dropped item with a body", "story-revision", []string{ruleStatus},
			"T3 is dropped, so it has an ID and a title only, not human, human_task",
			func(t *testing.T, o *planOutput, _ *planFacts) {
				it := revItem(t, o, "T3")
				it.Human, it.HumanTask = true, humanTask(new(plannerOutput(t, "story-plan")))
			}},
		{"a first plan with a status", "story-plan", []string{ruleStatus},
			`T2 has status "changed", but this is the issue's first plan, whose items have none`,
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T2").Status = planChanged }},

		// depends-on: never a dropped item
		{"a dependency on a dropped item", "story-revision", []string{ruleDependsOn},
			"T4 depends on T3, which is dropped",
			func(t *testing.T, o *planOutput, _ *planFacts) { revItem(t, o, "T4").DependsOn = []string{"T1", "T3"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, f := plannerOutput(t, tc.base), storyFacts()
			if tc.base == "story-revision" {
				f = revisionFacts(t)
			}
			tc.change(t, &out, &f)
			err := checkPlan(out, f)
			got := slices.Compact(slices.Sorted(slices.Values(rules(err))))
			if !slices.Equal(got, tc.rules) {
				t.Fatalf("rules %q, want %q (%v)", got, tc.rules, err)
			}
			if !strings.Contains(err.Error(), "rule "+tc.rules[0]+": ") || !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("error %q, want it to name %q", err, tc.detail)
			}
		})
	}
	if err := checkPlan(plannerOutput(t, "story-plan"), storyFacts()); err != nil {
		t.Errorf("a first plan without statuses: %v", err)
	}
}

// Cycles through kept items are found: a kept item has no dependencies of
// its own in the revision, so the cycle check takes them from its approved
// item, less any on an item the revision drops.
func TestCheckRevisionCycles(t *testing.T) {
	t.Parallel()
	open := func(f *planFacts) {
		f.Prior.Children[0].Closed, f.Prior.Children[0].Reason, f.Prior.Children[0].Stage = false, "", state.Ready
	}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, o *planOutput, f *planFacts)
		want   string // the cycle, or "" for none
	}{
		{"T1 changed to depend on the kept T2, which depends on T1", func(t *testing.T, o *planOutput, f *planFacts) {
			open(f)
			changed := *revItem(t, o, "T4")
			changed.ID, changed.Status, changed.DependsOn = "T1", planChanged, []string{"T2"}
			*revItem(t, o, "T1") = changed
			*revItem(t, o, "T2") = planItem{ID: "T2", Title: "Refuse an expired quote at booking", Status: planKept}
			revItem(t, o, "T3").Status = planKept
			revItem(t, o, "T4").DependsOn = []string{}
		}, "T1 → T2 → T1"},
		{"through two kept items, T4 → T2 → T3 → T4", func(t *testing.T, o *planOutput, f *planFacts) {
			// The approved T3 depends on T4 here, and T2 on T1 and T3.
			f.Prior.Items[2].DependsOn = []string{"T4"}
			f.Prior.Items = append(f.Prior.Items, planItem{ID: "T4", Title: "An earlier T4", DependsOn: []string{}})
			f.Prior.Children = append(f.Prior.Children, planChild{Item: "T4", Number: 204, Title: "An earlier T4", Type: "task", Stage: state.Ready})
			*revItem(t, o, "T2") = planItem{ID: "T2", Title: "Refuse an expired quote at booking", Status: planKept}
			revItem(t, o, "T3").Status = planKept
			revItem(t, o, "T4").Status, revItem(t, o, "T4").DependsOn = planChanged, []string{"T2"}
		}, "T2 → T3 → T4 → T2"},
		{"the kept T2's dependency on the dropped T3 is gone", func(t *testing.T, o *planOutput, f *planFacts) {
			f.Prior.Items[2].DependsOn = []string{"T2"} // T3 → T2, with T2 → T3 approved
			*revItem(t, o, "T2") = planItem{ID: "T2", Title: "Refuse an expired quote at booking", Status: planKept}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, f := plannerOutput(t, "story-revision"), revisionFacts(t)
			tc.change(t, &out, &f)
			err := checkPlan(out, f)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkPlan = %v, want no error", err)
				}
				return
			}
			if got := rules(err); !slices.Equal(got, []string{ruleCycle}) || !strings.Contains(err.Error(), "rule cycle: "+tc.want) {
				t.Fatalf("checkPlan = %v (rules %q), want only the cycle %s", err, got, tc.want)
			}
		})
	}
}

// keptStories is a revision of an epic whose approved plan has stories
// S1 to S<n>, each depending on S<n>, all children, kept, plus new items.
func keptStories(t *testing.T, n int, added ...planItem) (planOutput, planFacts) {
	t.Helper()
	prior := &planPrior{Rev: 1}
	p := planBody{Kind: planStories, Why: "Why.", Summary: "Summary.", Order: "In order"}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("S%d", i)
		deps := []string{fmt.Sprintf("S%d", n)}
		if i == n {
			deps = []string{}
		}
		prior.Items = append(prior.Items, planItem{ID: id, Title: "Title", Outcome: "Outcome.", Criteria: []string{"Criterion."}, DependsOn: deps})
		prior.Children = append(prior.Children, planChild{Item: id, Number: 200 + i, Title: "Title", Type: "story", Stage: state.Planning})
		p.Items = append(p.Items, planItem{ID: id, Title: "Title", Status: planKept})
	}
	p.Items = append(p.Items, added...)
	f := epicFacts()
	f.Rev, f.SubIssues, f.Prior = 2, n, prior
	return planOutput{Outcome: outcomePlan, Plan: &p}, f
}

// The link limits count a kept item's dependencies from its approved item:
// S51 blocks the 50 kept stories, so one new story that depends on it too
// is one over. The sub-issue limit counts only the new items, since the
// rest are sub-issues already, and plan.max_tasks the open and new tasks:
// the changed and new ones, and the kept ones whose child is open (SPEC
// §11.3), as the validation line does. A kept task with no child counts,
// and an epic's validation line counts its closed kept stories.
func TestCheckRevisionCounts(t *testing.T) {
	t.Parallel()
	story := func(id string, deps ...string) planItem {
		return planItem{ID: id, Title: "Title", Status: planNew, Outcome: "Outcome.", Criteria: []string{"Criterion."}, DependsOn: append([]string{}, deps...)}
	}
	out, f := keptStories(t, 51)
	if err := checkPlan(out, f); err != nil {
		t.Errorf("51 kept stories, S51 blocking 50: %v", err)
	}
	out, f = keptStories(t, 51, story("S52", "S51"))
	if err := checkPlan(out, f); rules(err) == nil || !strings.Contains(err.Error(), "rule links: S51 blocks 51 items, over 50") {
		t.Errorf("a new story on S51 too = %v, want S51 to block 51", err)
	}
	if got := planValidation(*out.Plan, f.view()); !strings.Contains(got, "52 sub-issues in all, within 100; at most 51 dependency links") {
		t.Errorf("the validation line = %q, want 52 sub-issues and 51 links", got)
	}

	// A kept item's dependency on a dropped item goes with it (SPEC
	// §5.2), so it isn't counted: S1, approved on S2 to S52, keeps 50 once S52 drops.
	out, f = keptStories(t, 52)
	for i := range f.Prior.Items {
		f.Prior.Items[i].DependsOn = []string{}
	}
	for i := 2; i <= 52; i++ {
		f.Prior.Items[0].DependsOn = append(f.Prior.Items[0].DependsOn, fmt.Sprintf("S%d", i))
	}
	out.Plan.Items[51].Status = planDropped
	if err := checkPlan(out, f); err != nil {
		t.Errorf("S1 kept on 50 kept stories and the dropped S52: %v", err)
	}
	out.Plan.Items[51].Status = planKept
	if err := checkPlan(out, f); !strings.Contains(fmt.Sprint(err), "rule links: S1 is blocked by 51 items, over 50") {
		t.Errorf("S1 kept on 51 kept stories = %v, want the links rule", err)
	}

	var added []planItem
	for i := 52; i <= 100; i++ {
		added = append(added, story(fmt.Sprintf("S%d", i)))
	}
	out, f = keptStories(t, 51, added...)
	if err := checkPlan(out, f); err != nil {
		t.Errorf("51 kept and 49 new stories, 100 sub-issues: %v", err)
	}
	out, f = keptStories(t, 51, append(added, story("S101"))...)
	if err := checkPlan(out, f); !strings.Contains(fmt.Sprint(err), "rule sub-issues: 51 sub-issues already and 50 new items make 101, over 100") {
		t.Errorf("51 kept and 50 new stories = %v, want 101 sub-issues", err)
	}

	// The golden revision has 3 tasks that aren't dropped, and 4 items.
	// Its kept T1's child is closed, so 2 are open and new: the changed T2
	// and the new T4. At a plan.max_tasks of 2, it can still grow by T4.
	out, f = plannerOutput(t, "story-revision"), revisionFacts(t)
	f.MaxTasks = 2
	if err := checkPlan(out, f); err != nil {
		t.Errorf("2 open and new tasks and the closed kept T1, within plan.max_tasks of 2: %v", err)
	}
	if got := planValidation(*out.Plan, f.view()); !strings.Contains(got, "Validation (11.3): 2 open and new tasks, within `plan.max_tasks` of 2,") {
		t.Errorf("the validation line = %q, want 2 open and new tasks", got)
	}
	f.MaxTasks = 1
	if err := checkPlan(out, f); !strings.Contains(fmt.Sprint(err),
		"rule max-tasks: 2 open and new tasks, over plan.max_tasks of 1: closed kept tasks don't count") {
		t.Errorf("2 open and new tasks, plan.max_tasks 1 = %v, want max-tasks", err)
	}

	// With T1's child open, the kept T1 counts too.
	out, f = plannerOutput(t, "story-revision"), revisionFacts(t)
	f.Prior.Children[0].Closed, f.Prior.Children[0].Reason, f.Prior.Children[0].Stage = false, "", state.Ready
	f.MaxTasks = 3
	if err := checkPlan(out, f); err != nil {
		t.Errorf("3 open and new tasks, within plan.max_tasks of 3: %v", err)
	}
	if got := planValidation(*out.Plan, f.view()); !strings.Contains(got, "Validation (11.3): 3 open and new tasks, within `plan.max_tasks` of 3,") {
		t.Errorf("the validation line = %q, want 3 open and new tasks", got)
	}
	f.MaxTasks = 2
	if err := checkPlan(out, f); !strings.Contains(fmt.Sprint(err),
		"rule max-tasks: 3 open and new tasks, over plan.max_tasks of 2: closed kept tasks don't count") {
		t.Errorf("3 open and new tasks, plan.max_tasks 2 = %v, want max-tasks", err)
	}

	// A kept task with no child counts, as no closed child is its, though
	// it fails the children rule: with T1's child gone, the kept T1, the
	// changed T2 and the new T4 make 3, here and in the validation line.
	out, f = plannerOutput(t, "story-revision"), revisionFacts(t)
	f.Prior.Children, f.MaxTasks = f.Prior.Children[1:], 2
	err := checkPlan(out, f)
	if !slices.Contains(rules(err), ruleChildren) || !strings.Contains(fmt.Sprint(err),
		"rule max-tasks: 3 open and new tasks, over plan.max_tasks of 2: closed kept tasks don't count") {
		t.Errorf("the kept T1 with no child, plan.max_tasks 2 = %v, want children and max-tasks of 3", err)
	}
	if got := planValidation(*out.Plan, f.view()); !strings.Contains(got, "Validation (11.3): 3 open and new tasks, within `plan.max_tasks` of 2,") {
		t.Errorf("the validation line = %q, want 3 open and new tasks", got)
	}

	// An epic's revision of stories counts every story that isn't
	// dropped in its validation line, a kept one whose child is closed
	// included: max-tasks leaves out closed kept tasks only.
	out, f = keptStories(t, 2)
	f.Prior.Children[0].Closed, f.Prior.Children[0].Reason, f.Prior.Children[0].Stage = true, "completed", ""
	if err := checkPlan(out, f); err != nil {
		t.Errorf("2 kept stories, S1's child closed: %v", err)
	}
	if got := planValidation(*out.Plan, f.view()); !strings.Contains(got, "Validation (11.3): 2 stories, with no tasks; every child listed once,") {
		t.Errorf("the validation line = %q, want 2 stories, the closed kept S1 included", got)
	}
}

// A first plan's data is the plan itself, byte for byte, as it was
// before revisions. A revision's kept item is its approved entry in full,
// read from the approved revision, never from the planner's output, with
// its status kept and only the dependencies it keeps: here T1's on T2,
// not its one on T3, which the revision drops. The changed, new and
// dropped items, and the planner's output, are left as they are.
func TestKeptDataCarriesApprovedEntry(t *testing.T) {
	t.Parallel()
	first := goldenPlan(t, "story-plan")
	got := (*planPrior)(nil).keptData(first)
	gotMarker, err := planDataMarker(got)
	if err != nil {
		t.Fatal(err)
	}
	wantMarker, err := planDataMarker(goldenPlan(t, "story-plan"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, goldenPlan(t, "story-plan")) || gotMarker.Fields["data"] != wantMarker.Fields["data"] {
		t.Errorf("a first plan's data = %+v, want the plan as it is", got)
	}

	pr := revisionPrior(t)
	pr.Items[0].DependsOn = []string{"T3", "T2"}
	p := goldenPlan(t, "story-revision")
	p.Items[0].Outcome = "The planner's own outcome, which a kept item can't have."
	data := pr.keptData(p)
	want := pr.Items[0]
	want.Status, want.DependsOn = planKept, []string{"T2"}
	if !reflect.DeepEqual(data.Items[0], want) {
		t.Errorf("the kept T1's data = %+v, want its approved entry %+v", data.Items[0], want)
	}
	for i := 1; i < len(p.Items); i++ {
		if !reflect.DeepEqual(data.Items[i], p.Items[i]) {
			t.Errorf("%s's data = %+v, want the revision's own %+v", p.Items[i].ID, data.Items[i], p.Items[i])
		}
	}
	if p.Items[0].Outcome != "The planner's own outcome, which a kept item can't have." || p.Items[0].Criteria != nil {
		t.Errorf("the planner's output was changed: %+v", p.Items[0])
	}
}

// On a third plan, an item kept by the second and kept again counts with
// its dependencies from its first plan, which the second plan's data
// carries (planPrior.keptData): the cycle check finds T2 → T3 → T2 once
// T3 is changed to depend on T2, and the link limits count the kept S51's
// 50 blocked stories, so a new story on it is one over. children.md shows
// the twice-kept T2's approved text.
func TestCheckRevisionKeptTwice(t *testing.T) {
	t.Parallel()
	third := func(f planFacts, second planBody) planFacts {
		data := f.Prior.keptData(second)
		f.Rev, f.Prior = 3, &planPrior{Rev: 2, Items: data.Items, Children: f.Prior.Children, Subs: f.Prior.Subs}
		return f
	}
	keptAll := func(p planBody) planBody {
		for i, it := range p.Items {
			p.Items[i] = planItem{ID: it.ID, Title: it.Title, Status: planKept}
		}
		return p
	}

	f := revisionFacts(t)
	second := keptAll(goldenPlan(t, "story-revision"))
	second.Items = second.Items[:3]
	if err := checkPlan(planOutput{Outcome: outcomePlan, Plan: &second}, f); err != nil {
		t.Fatalf("the second plan, keeping all three: %v", err)
	}
	f = third(f, second)
	out := plannerOutput(t, "story-revision")
	t3 := goldenPlan(t, "story-plan").Items[2]
	t3.Status, t3.DependsOn = planChanged, []string{"T2"}
	out.Plan.Items = []planItem{second.Items[0], second.Items[1], t3}
	if err := checkPlan(out, f); !slices.Equal(rules(err), []string{ruleCycle}) || !strings.Contains(err.Error(), "rule cycle: T2 → T3 → T2") {
		t.Errorf("the third plan, T3 changed to depend on the twice-kept T2 = %v, want only the cycle T2 → T3 → T2", err)
	}
	children, err := plannerChildrenFile(124, f.Prior)
	if err != nil {
		t.Fatal(err)
	}
	t2 := goldenPlan(t, "story-plan").Items[1]
	for _, want := range []string{
		"## Items of plan revision 2, as approved",
		`"outcome": "` + t2.Outcome + `"`,
		`"` + t2.Criteria[0] + `"`,
		"\"depends_on\": [\n      \"T1\",\n      \"T3\"\n    ]",
		`"test_plan": `,
	} {
		if !strings.Contains(children, want) {
			t.Errorf("the third plan's children.md lacks\n%s\nin\n%s", want, children)
		}
	}

	stories, f := keptStories(t, 51)
	f = third(f, *stories.Plan)
	out, _ = keptStories(t, 51, planItem{ID: "S52", Title: "Title", Status: planNew, Outcome: "Outcome.", Criteria: []string{"Criterion."},
		DependsOn: []string{"S51"}})
	if err := checkPlan(out, f); !slices.Equal(rules(err), []string{ruleLinks}) || !strings.Contains(err.Error(), "rule links: S51 blocks 51 items, over 50") {
		t.Errorf("the third plan, a new story on S51, which 50 twice-kept stories depend on = %v, want only S51 to block 51", err)
	}
}

// The length rule counts a revision's comment as it's posted, with its
// kept items in full in its data (planPrior.keptData): keeping ten tasks
// whose approved bodies don't fit takes it over the limit, though the
// revision's own items are each an ID, a title and a status.
func TestCheckRevisionLengthCountsKeptData(t *testing.T) {
	t.Parallel()
	approved := bigPlan(t, 10, 8000).Plan
	f := storyFacts()
	f.Rev, f.SubIssues = 2, 10
	f.Prior = &planPrior{Rev: 1, Items: approved.Items, Subs: 10}
	p := planBody{Kind: planTasks, Why: approved.Why, Summary: approved.Summary, Order: "As before"}
	for i, it := range approved.Items {
		f.Prior.Children = append(f.Prior.Children, planChild{Item: it.ID, Number: 201 + i, Title: it.Title, Type: "task", Stage: state.Ready})
		p.Items = append(p.Items, planItem{ID: it.ID, Title: it.Title, Status: planKept})
	}
	m, err := planDataMarker(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(m.Fields["data"]) + textLen(renderPlan(p, f.view())); n > (maxText-textRoom)/2 {
		t.Fatalf("the revision's own text and data are %d characters, so they don't show the kept entries are counted", n)
	}
	err = checkPlan(planOutput{Outcome: outcomePlan, Plan: &p}, f)
	if got := rules(err); !slices.Equal(got, []string{ruleLength}) || !strings.Contains(err.Error(), "the plan comment is") {
		t.Errorf("a revision keeping ten tasks whose approved bodies don't fit: rules %q (%v), want length", got, err)
	}
}

// revisionView is the view the golden revision is rendered with: story
// #124 at p2, with its three children, T1's closed, as plan revision 2.
func revisionView(t *testing.T) planView {
	v := storyView()
	v.Rev, v.SubIssues, v.Revised, v.Earlier, v.Closed = 2, 3, true, goldenPlan(t, "story-plan").Items, []string{"T1"}
	return v
}

// The golden revision renders each item's status in its item line, kept
// and dropped items on that one line alone, and a validation line from the
// revision's counts. Its plan-data marker, as it's posted, keeps the
// statuses, so approval reads them back, and the kept T1 in full
// (planPrior.keptData).
func TestRenderRevisionGolden(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "story-revision")
	got := renderPlan(p, revisionView(t))
	planGolden(t, "story-revision", got)
	checkSafe(t, got)
	if p := filePathRE.FindAllString(got, -1); len(p) > 0 {
		t.Errorf("file paths %q", p)
	}
	for _, w := range []string{
		"### Plan for #124 (rev 2)",
		"**Changes since rev 1.**",
		"**T1. Record when each quote expires** · kept",
		"**T2. Refuse an expired quote at booking, saying when it expired** · changed · scope:booking · p1",
		"**T3. Set the quote lifetime in production** · dropped",
		"**T4. Give the quote lifetime a default** · new · scope:booking · p2",
		"Depends on: T1, T4",
		"Validation (11.3): 2 open and new tasks, within `plan.max_tasks` of 10, each with one scope from the list; " +
			"every child listed once, kept, changed or dropped, and each closed one kept; no dependency cycles; " +
			"4 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.",
		"cc @mertenvg", planApproveLine,
	} {
		if !strings.Contains(got, "\n"+w+"\n") && !strings.HasPrefix(got, w+"\n") && !strings.HasSuffix(got, "\n"+w) {
			t.Errorf("no line %q in\n%s", w, got)
		}
	}
	for _, bare := range []string{"**T1. Record when each quote expires** · kept\n\n", "**T3. Set the quote lifetime in production** · dropped\n\n"} {
		if !strings.Contains(got, bare) {
			t.Errorf("no item %q alone on its line in\n%s", bare, got)
		}
	}

	m, err := planDataMarker(revisionPrior(t).keptData(p))
	if err != nil {
		t.Fatal(err)
	}
	back, ok, err := readPlanData(github.Comment{ID: 9, Author: appAuthor, Body: planComment(t, m)}, "issuedog")
	if err != nil || !ok {
		t.Fatalf("readPlanData = %v, %v", ok, err)
	}
	var statuses []string
	for _, it := range back.Items {
		statuses = append(statuses, it.ID+" "+it.Status)
	}
	if want := []string{"T1 kept", "T2 changed", "T3 dropped", "T4 new"}; !slices.Equal(statuses, want) {
		t.Errorf("the data reads back with %q, want %q", statuses, want)
	}
	data, err := json.MarshalIndent(back, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dataGolden := filepath.Join("testdata", "plan", "story-revision-data.json")
	if *update {
		if err := os.WriteFile(dataGolden, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(dataGolden)
	if err != nil {
		t.Fatal(err)
	}
	if string(data)+"\n" != string(want) {
		t.Errorf("the plan data reads back as\n%s\nwant\n%s", data, want)
	}
}

// A first plan's data has no status, so it reads as it did before
// revisions: the field is left out, not empty.
func TestPlanDataFirstPlanNoStatus(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(goldenPlan(t, "story-plan"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"status"`) {
		t.Errorf("a first plan's data has a status: %s", raw)
	}
}

// The preamble says a first plan has no statuses, and for a revision the
// approved revision, the revision's rules, the new items still free and
// that it always waits for review.
func TestPlannerPreambleRevision(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 124, Type: "story", Parent: 9}
	first := plannerPreamble(planTestPlanner().Daemon.Config, s, 3, nil, nil, nil)
	if !strings.Contains(first, "- This is the issue's first plan, so no item has a `status`.") ||
		!strings.Contains(first, "so a plan has at most 97 items.") || strings.Contains(first, "`kept`") {
		t.Errorf("a first plan's preamble:\n%s", first)
	}
	got := plannerPreamble(planTestPlanner().Daemon.Config, s, 3, revisionPrior(t), nil, nil)
	for _, want := range []string{
		"- This issue has 3 sub-issues, of the 100 GitHub allows, so a revision has at most 97 `new` items.",
		"- This issue was planned before, and revision 1 of its plan is approved, so this plan is a revision of it. " +
			"Every item has a `status`: `kept`, `changed`, `new` or `dropped`.",
		"- Every child in children.md appears exactly once, by its item ID, marked `kept`, `changed` or `dropped`, never `new`. " +
			"A closed child can only be `kept`: to change its work, add a `new` item.",
		"- A `kept` or `dropped` item has its `id`, `title` and `status` only.",
		"- A `new` item's ID is no child's, and no item's of the approved revision.",
		"- `depends_on` may name `kept`, `changed` and `new` items, never a `dropped` one.",
		"- `plan.max_tasks` counts the open and new tasks: the `changed` and `new` ones, and the `kept` ones whose child is open. " +
			"A closed child's `kept` task doesn't count.",
		"A revision always waits for the owner's review, whatever `plan.review` says.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the revision's preamble lacks\n%s\nin\n%s", want, got)
		}
	}
	if strings.Contains(got, "first plan") || !strings.HasSuffix(got, "so keep each task body to what its task needs.") {
		t.Errorf("the revision's preamble says it's a first plan, or doesn't end with the length rule:\n%s", got)
	}
}

// children.md for a revision lists each child with its item ID, number,
// title, type, and stage or close reason, escaped; then the approved
// revision's items, fenced; then the other sub-issues, as context only.
func TestPlannerChildrenFile(t *testing.T) {
	t.Parallel()
	pr := revisionPrior(t)
	pr.Children[2].Title = "Set it <!-- x --> @owner"
	pr.Children = append(pr.Children, planChild{Item: "T9", Number: 209, Title: "Gone", Closed: true, Reason: "not_planned"},
		planChild{Item: "T8", Number: 208, Title: "Closed", Closed: true})
	pr.Others = []plannerSub{{Number: 300, Title: "Filed by hand", State: "open", Type: "bug"}}
	got, err := plannerChildrenFile(124, pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Children of #124's plan\n\n" + plannerChildrenIntroRevision + "\n\n## Children of plan revision 1\n\n" +
			"- T1: #201, Record when each quote expires (type task, closed as completed)\n" +
			"- T2: #202, Refuse an expired quote at booking (type task, `stage:ready`)\n" +
			"- T3: #203, Set it &lt;!-- x --> @owner (type task, `stage:needs-human`)\n" +
			"- T9: #209, Gone (type none, closed as not planned)\n" +
			"- T8: #208, Closed (type none, closed)\n\n",
		"## Items of plan revision 1, as approved, with their task bodies: a kept item keeps these, and its dependencies are counted from here\n\n```text\n[\n  {\n    \"id\": \"T1\",",
		"\"test_plan\": ",
		"## Other sub-issues\n\nLinked under this issue, but not created from its plan: context only. Never list them as items.\n\n" +
			"- #300: Filed by hand (open, type bug, not in the pipeline)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("children.md lacks\n%s\nin\n%s", want, got)
		}
	}
	pr.Children, pr.Others = nil, nil
	if got, err := plannerChildrenFile(124, pr); err != nil || !strings.Contains(got, "None: no issue created from the approved plan was found.") ||
		!strings.HasSuffix(got, "## Other sub-issues\n\nLinked under this issue, but not created from its plan: context only. Never list them as items.\n\nNone.\n") {
		t.Errorf("children.md with no children = %v\n%s", err, got)
	}
}

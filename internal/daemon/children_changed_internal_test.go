package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
)

// A changed child's labels (#195's T7): the type, scope, priority, model
// and effort kinds on a labels repo, and all but the type on a repo that
// uses issue types, where the type is the issue's.
func TestChildLabelKindsOf(t *testing.T) {
	t.Parallel()
	task := childItem(t, "story-plan", "T1")
	for _, tc := range []struct {
		name       string
		issueTypes bool
		want       []string
	}{
		{"labels", false, []string{"type:", "scope:", "priority:", "model:", "effort:"}},
		{"issue types", true, []string{"scope:", "priority:", "model:", "effort:"}},
	} {
		if got := childLabelKindsOf(childOne(t, childParent(124), task, tc.issueTypes)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: kinds %q, want %q", tc.name, got, tc.want)
		}
	}
}

// childSpecsWhere describes only the items its filter takes, in creation
// order, each with its origin naming the revision: here a revision's
// changed items, the human task last, as childOrder files it.
func TestChildSpecsWhereChanged(t *testing.T) {
	t.Parallel()
	items := goldenPlan(t, "story-plan").Items
	items[1].Status, items[2].Status = planChanged, planChanged
	items[0] = planItem{ID: items[0].ID, Title: items[0].Title, Status: planKept}
	specs, err := childSpecsWhere(childParent(124), 3, items, func(s string) bool { return s == planChanged }, false, nil, config.Models{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range specs {
		got = append(got, c.Item)
	}
	if !slices.Equal(got, []string{"T2", "T3"}) {
		t.Fatalf("described %q, want the changed T2, then the human task T3", got)
	}
	_, origin := childSplit(t, "T2", childBody(t, specs[0], nil))
	if origin.Fields["rev"] != "3" || origin.Fields["item"] != "T2" {
		t.Errorf("T2's origin is %v, want revision 3's item T2", origin.Fields)
	}
}

// childPlanRun is the run= of the App's transition marker in the plan
// comment, and none in a comment someone else wrote.
func TestChildPlanRun(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: childSlug + "[bot]", Type: "Bot"}
	body := "### Plan\n\n<!-- issuedog:transition from=planning to=plan-review rev=2 run=p2 -->"
	if run, ok := childPlanRun(github.Comment{Author: app, Body: body}, childSlug); !ok || run != "p2" {
		t.Errorf("the App's plan comment gives run %q, %t; want p2", run, ok)
	}
	if run, ok := childPlanRun(github.Comment{Author: github.Author{Login: "owner", Type: "User"}, Body: body}, childSlug); ok {
		t.Errorf("the owner's copy gives run %q; want none", run)
	}
}

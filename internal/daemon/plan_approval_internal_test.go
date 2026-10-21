package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// approvedPlanSnap is issue #7 with plan comment 2, plan revision 1 from
// planning to to, holding the plan-data marker data (none if ""), and,
// for a move to plan-review, comment 3, the move into planned that
// approves it, with its rev and body.
func approvedPlanSnap(t *testing.T, to state.Stage, data string) *github.Snapshot {
	t.Helper()
	body := "Plan.\n\n<!-- issuedog:transition from=planning to=" + string(to) + " rev=1 run=p1 -->"
	if data != "" {
		body += "\n" + data
	}
	plan, err := planRevision(body, 1)
	if err != nil {
		t.Fatal(err)
	}
	s := &github.Snapshot{Number: 7, Author: appAuthor, Body: "the spec", Comments: []github.Comment{
		{ID: 1, Author: appAuthor, Body: "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r3 -->"},
		{ID: 2, Author: appAuthor, Body: plan},
	}}
	if to == state.PlanReview {
		hash, ok := state.PlanHash(plan)
		if !ok {
			t.Fatalf("no plan comment: %q", plan)
		}
		s.Comments = append(s.Comments, github.Comment{ID: 3, Author: appAuthor,
			Body: "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"})
	}
	return s
}

// approvedPlanData reads the plan's data back from the plan comment the
// move into planned approved, from plan-review or from planning, and
// reports false with no approved comment, or with no data in it. Data
// that won't read back is an error that names the revision and the
// comment (SPEC §7.4).
func TestApprovedPlanData(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "story-plan")
	m, err := planDataMarker(p)
	if err != nil {
		t.Fatal(err)
	}
	data, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	clean := func(int64) bool { return false }
	for _, to := range []state.Stage{state.PlanReview, state.Planned} {
		s := approvedPlanSnap(t, to, data)
		rev, got, ok, err := approvedPlanData(s, "issuedog", clean)
		if err != nil || !ok || rev.Kind != state.PlanRevision || rev.Rev != 1 || rev.Comment != 2 || !reflect.DeepEqual(got, p) {
			t.Errorf("from planning to %s: approvedPlanData = %+v, %+v, %v, %v; want plan revision 1 in comment 2, with the plan",
				to, rev, got, ok, err)
		}
	}

	corrupt := "<!-- issuedog:plan-data v=2 data=" + m.Fields["data"] + " -->"
	noMove := approvedPlanSnap(t, state.PlanReview, data)
	noMove.Comments = noMove.Comments[:2]
	for _, tc := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
	}{
		{"no move into planned", noMove, clean},
		{"no plan-data marker", approvedPlanSnap(t, state.PlanReview, ""), clean},
		{"the approved comment is dirty, though its data won't read back", approvedPlanSnap(t, state.PlanReview, corrupt),
			func(id int64) bool { return id == 2 }},
	} {
		if rev, got, ok, err := approvedPlanData(tc.s, "issuedog", tc.dirty); err != nil || ok || rev != (state.Revision{}) || !reflect.DeepEqual(got, planBody{}) {
			t.Errorf("%s: approvedPlanData = %+v, %+v, %v, %v; want nothing, false, no error", tc.name, rev, got, ok, err)
		}
	}

	for _, to := range []state.Stage{state.PlanReview, state.Planned} {
		rev, got, ok, err := approvedPlanData(approvedPlanSnap(t, to, corrupt), "issuedog", clean)
		switch {
		case err == nil || ok || rev != (state.Revision{}) || !reflect.DeepEqual(got, planBody{}):
			t.Errorf("from planning to %s, data that won't read back: approvedPlanData = %+v, %+v, %v, %v; want nothing, false, an error",
				to, rev, got, ok, err)
		case !strings.Contains(err.Error(), "approved plan revision 1 of #7") || !strings.Contains(err.Error(), "comment 2"):
			t.Errorf("from planning to %s: error %q doesn't name the revision and the comment", to, err)
		}
	}
}

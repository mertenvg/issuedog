package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// approve checks state.PlanApproval itself at plan-review, not only
// through its callers: a plan revision that can't be approved moves
// nothing, and approve makes no request, whatever the key.
func TestApprovePlanChecksItself(t *testing.T) {
	t.Parallel()
	plan, err := planRevision("Plan.\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->", 1)
	if err != nil {
		t.Fatal(err)
	}
	snap := func(more ...github.Comment) *github.Snapshot {
		return &github.Snapshot{Number: 7, Author: appAuthor, Body: "the spec", Comments: append([]github.Comment{
			{ID: 1, Author: appAuthor, Body: "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r3 -->"},
			{ID: 2, Author: appAuthor, Body: plan},
		}, more...)}
	}
	changed := snap()
	changed.Comments[1].Body = strings.Replace(plan, "Plan.", "Plan!", 1)
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		d    Dirt
	}{
		{"hidden", snap(github.Comment{ID: 3, Author: appAuthor, Body: "Noted."}), Dirt{3: "mallory"}},
		{"no clean plan revision", snap(), Dirt{2: "mallory"}},
		{"changed", changed, Dirt{}},
		{"dirty body", snap(), Dirt{state.BodyID: "mallory"}},
	} {
		f := githubtest.New(t, "o/r", "issuedog")
		a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
		for _, key := range []state.Key{state.CmdKey(4), state.ReactionKey(5)} {
			if err := a.approve(context.Background(), tt.s, tt.d, state.PlanReview, key, "owner"); err != nil {
				t.Errorf("%s, by %s: approve = %v, want nil", tt.name, key.ID, err)
			}
		}
		if r := f.Requests(); len(r) != 0 {
			t.Errorf("%s: requests %+v, want none", tt.name, r)
		}
	}
}

// rowClasses names the classes a §7.2 row moves, as the subject of
// inTable's reason: one, several, with a set that's all there worded as
// one, or none (#268).
func TestRowClasses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		from, to state.Stage
		trigger  state.Trigger
		want     string
	}{
		{state.SpecReview, state.Planning, state.TriggerSpecApproved, "only an epic, or a story with no parent,"},
		{state.PlanReview, state.Planned, state.TriggerPlanApproved, "only an epic, or a story,"},
		{state.Planning, state.Planned, state.TriggerPlanNoReview, "only a story"},
		{state.NeedsHuman, state.Closed, state.TriggerHumanTaskDone, "only a human task"},
		{state.Clarifying, state.Ready, state.TriggerTaskRewritten, "only a task from a plan, a task filed by the owner, a bug, " +
			"a bug before `ready`, or a task filed by the owner before `ready`,"},
		{state.Intake, state.Clarifying, state.TriggerTrusted, "only an epic, a story, a task from a plan, a task filed by the owner, " +
			"a human task, a bug, an issue with no type, a bug before `ready`, or a task filed by the owner before `ready`,"},
		{state.PlanReview, state.Closed, state.TriggerPlanApproved, "no issue"},
	} {
		if got := rowClasses(tt.from, tt.to, tt.trigger); got != tt.want {
			t.Errorf("rowClasses(%s, %s, %s) = %q, want %q", tt.from, tt.to, tt.trigger, got, tt.want)
		}
	}
}

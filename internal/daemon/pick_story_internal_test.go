package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// pickBadEpic is epic n, planned into tasks, whose approved plan's data
// doesn't read back.
func pickBadEpic(t *testing.T, n int) *github.Snapshot {
	t.Helper()
	s := doneSnap(t, n, "epic", "story-plan", 12)
	data, err := github.FormatMarker(github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": "not-base64!"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRevision("### Plan\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n"+data, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := state.PlanHash(plan)
	if !ok {
		t.Fatal("no plan hash")
	}
	s.Comments[1].Body = plan
	s.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"
	return s
}

// The pick's candidates (SPEC §7.5, §8.1): a task with no parent, with no
// story; a task in a story, or in an epic planned into tasks, with it; and
// with a Stage at in-progress that isn't a Committer, any task, with no
// story. A task under an epic planned into stories isn't one, and nor is
// one whose parent's plan data doesn't read back, which is warned of once
// across passes, not every pass. It reads logr, so it isn't parallel.
func TestPickCandidate(t *testing.T) {
	story := doneSnap(t, 50, "story", "epic-plan")
	tasks := doneSnap(t, 51, "epic", "story-plan", 12)
	stories := doneSnap(t, 52, "epic", "epic-plan", 12)
	bad := pickBadEpic(t, 53)
	r := &Runs{Daemon: &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r", AppSlug: waitSlug}},
		snaps: map[int]*github.Snapshot{50: story, 51: tasks, 52: stories, 53: bad}}}
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		st     Stage
		task   *github.Snapshot
		story  *github.Snapshot
		picked bool
	}{
		{"no parent", &Implementer{}, &github.Snapshot{Number: 12, Type: "task"}, nil, true},
		{"a story", &Implementer{}, &github.Snapshot{Number: 12, Parent: 50, Type: "task"}, story, true},
		{"an epic planned into tasks", &Implementer{}, &github.Snapshot{Number: 12, Parent: 51, Type: "task"}, tasks, true},
		{"an epic planned into stories", &Implementer{}, &github.Snapshot{Number: 12, Parent: 52, Type: "story"}, nil, false},
		{"a parent the daemon doesn't hold", &Implementer{}, &github.Snapshot{Number: 12, Parent: 99, Type: "task"}, nil, false},
		{"a Stage that isn't a Committer", &Interviewer{}, &github.Snapshot{Number: 12, Parent: 52, Type: "task"}, nil, true},
	} {
		got, ok, err := r.pickCandidate(ctx, tc.st, tc.task)
		if err != nil || ok != tc.picked || got != tc.story {
			t.Errorf("%s: pickCandidate gave %v, %t, %v; want %v, %t", tc.name, got, ok, err, tc.story, tc.picked)
		}
	}
	stop := logtest.Capture(t, logr.W)
	for range 3 {
		got, ok, err := r.pickCandidate(ctx, &Implementer{}, &github.Snapshot{Number: 12, Parent: 53, Type: "task"})
		if err != nil || ok || got != nil {
			t.Errorf("an epic whose plan data doesn't read back: pickCandidate gave %v, %t, %v; want none", got, ok, err)
		}
	}
	logs := stop()
	if got := strings.Count(logs, "o/r#53: no task under it is picked, as it can't tell what its plan is of"); got != 1 {
		t.Errorf("the unreadable plan was warned of %d times; want once, in\n%s", got, logs)
	}
}

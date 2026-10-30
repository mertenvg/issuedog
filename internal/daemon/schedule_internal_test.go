package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// The pick's order (SPEC §8.1): by priority, then the parent's priority,
// then number. A task with no parent, or whose parent no snapshot holds,
// stands in for its own parent. Stories wait for #624, so only this test
// sees the parent's priority decide.
func TestPickLess(t *testing.T) {
	t.Parallel()
	task := func(n int, p string, parent int) *github.Snapshot {
		return &github.Snapshot{Number: n, Labels: []string{"priority:" + p}, Parent: parent}
	}
	snaps := map[int]*github.Snapshot{
		100: task(100, "p0", 0),
		101: task(101, "p3", 0),
		102: task(102, "p2", 0),
	}
	tasks := []*github.Snapshot{
		task(1, "p1", 101), // p1, below a p3 story
		task(2, "p2", 0),   // p2, no parent
		task(3, "p1", 0),   // p1, no parent: its own p1 stands in
		task(4, "p1", 100), // p1, below a p0 story
		task(5, "p1", 102), // p1, below a p2 story
		task(6, "p0", 101), // p0 comes first, whatever its parent
		task(7, "p1", 999), // p1, below a story no snapshot holds: its own p1, before #5
	}
	slices.SortStableFunc(tasks, pickLess(snaps))
	var got []int
	for _, s := range tasks {
		got = append(got, s.Number)
	}
	if want := []int{6, 4, 3, 7, 5, 1, 2}; !slices.Equal(got, want) {
		t.Errorf("the pick's order is %v, want %v", got, want)
	}
}

// A lease that holds a run holds the worker, whatever the stages; an
// ended run's lease, the record of its failed attempts, doesn't (SPEC
// §8.1, §8.2). Nothing at in-progress or verifying, nor an issue closed
// there, holds it either.
func TestPickHeldByLease(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		lease *Lease
		held  bool
	}{
		{name: "no lease"},
		{name: "a run's lease", lease: &Lease{Issue: 9, Stage: state.Clarifying, RunID: "run1"}, held: true},
		{name: "an ended run's", lease: &Lease{Issue: 9, Stage: state.Clarifying, RunID: "run1", Attempt: 1, Failed: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			r := &Runs{Daemon: &Daemon{snaps: map[int]*github.Snapshot{
				2: {Number: 2, Labels: []string{"stage:verifying"}, Closed: true},
				3: {Number: 3, Labels: []string{"stage:pr-open"}},
			}}, Runner: &agent.Runner{StateDir: dir}, Workspace: &workspace.Workspace{RunsDir: t.TempDir()}}
			if tt.lease != nil {
				if err := r.writeLease(*tt.lease); err != nil {
					t.Fatal(err)
				}
			}
			ready := []Ready{{S: &github.Snapshot{Number: 1, Labels: []string{"stage:ready"}}}}
			if held, err := r.pickHeld(context.Background(), ready); err != nil || held != tt.held {
				t.Errorf("pickHeld = %v, %v; want %v", held, err, tt.held)
			}
		})
	}
}

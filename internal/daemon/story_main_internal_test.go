package daemon

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// The merge pass forgets what it remembers of an issue the daemon no
// longer holds, closed or gone, and keeps it for one it holds open, or
// will read again (SPEC §8.1).
func TestStoryMainForgets(t *testing.T) {
	t.Parallel()
	d := &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r"}},
		snaps: map[int]*github.Snapshot{1: {Number: 1, Closed: true, Type: "story"}, 2: {Number: 2, Type: "task"}},
		stale: map[int]bool{3: true}}
	memory := map[int]string{1: "closed", 2: "open", 3: "to read again", 4: "gone"}
	d.storyMainLogged, d.storyMainFiled = maps.Clone(memory), maps.Clone(memory)
	if err := d.storyMainPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]map[int]string{"logged": d.storyMainLogged, "filed": d.storyMainFiled} {
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []int{2, 3}) {
			t.Errorf("after the pass, %s remembers %v; want #2 and #3", name, keys)
		}
	}
}

package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// What conflictMoot judges from the snapshots the daemon holds alone (SPEC
// §7.5, A conflict merged by hand). conflict_moot_test.go runs it end to
// end, on real temporary repos.

// conflictMootTip is a story branch's tip new to the daemon in these tests.
const conflictMootTip = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"

// conflictMootBug is storyFailureBug n, a conflict bug of story 1 for its
// branch's tip failureTestTip and main's failureTestMain.
func conflictMootBug(n int) *github.Snapshot {
	return storyFailureBug(n, func(s *github.Snapshot) {
		s.Body = "Merge it.\n\n<!-- issuedog:origin main=" + failureTestMain + " story=#1 tip=" + failureTestTip + " -->"
	})
}

// A story with no open conflict bug among the snapshots held judges its
// tip at once, with no request and no read of the bare repo: the daemon
// here has neither a client nor a clone, so either would panic. A failure's
// bug that isn't a conflict bug, a closed conflict bug, and another
// story's don't count.
func TestConflictMootNoneOpen(t *testing.T) {
	t.Parallel()
	closed := conflictMootBug(3)
	closed.Closed = true
	// Another story's conflict bug, below that story, with an origin
	// naming it, so it's a conflict bug of its own story (conflictMain),
	// and only the story's filter leaves it out.
	other := conflictMootBug(4)
	other.Parent = 9
	other.Body = strings.Replace(other.Body, "story=#1", "story=#9", 1)
	if (&Daemon{Step: &Step{Applier: &Applier{AppSlug: "issuedog"}}}).conflictMain(other) != failureTestMain {
		t.Fatalf("story #9's conflict bug names no main commit; want %s", failureTestMain)
	}
	d := storyFailureDaemon("", "", storyFailureBug(2, nil), closed, other)
	judge, err := (&Runs{Daemon: d}).conflictMoot(t.Context(), d.snaps[1], conflictMootTip, 0)
	if err != nil || !judge {
		t.Errorf("conflictMoot with no open conflict bug of the story = %t, %v; want true, nil", judge, err)
	}
}

// A conflict bug of the story that the pass applied a saved result to,
// skip, whose snapshot is from before that, holds a tip that holds its
// main commit for the next pass, with no request: the daemon here has no
// client, so one would panic. A tip that lacks its main commit, which the
// bare clone here lacks, isn't held, and doesn't make the bug moot.
func TestConflictMootSkip(t *testing.T) {
	t.Parallel()
	clone, tip := branchRemote(t)
	bug := storyFailureBug(5, func(s *github.Snapshot) {
		s.Body = "Merge it.\n\n<!-- issuedog:origin main=" + tip + " story=#1 tip=" + failureTestTip + " -->"
	})
	d := storyFailureDaemon("", "", bug)
	d.Clone = clone
	r := &Runs{Daemon: d}
	judge, err := r.conflictMoot(t.Context(), d.snaps[1], tip, 5)
	if err != nil || judge {
		t.Errorf("conflictMoot on a tip that holds the main commit of its conflict bug, skip, = %t, %v; want false, nil", judge, err)
	}
	d.snaps[5] = conflictMootBug(5)
	judge, err = r.conflictMoot(t.Context(), d.snaps[1], tip, 5)
	if err != nil || !judge {
		t.Errorf("conflictMoot on a tip that lacks the main commit of its conflict bug, skip, = %t, %v; want true, nil", judge, err)
	}
}

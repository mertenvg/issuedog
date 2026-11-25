package daemon_test

import (
	"strings"
	"testing"
)

// A conflict bug's checks judge its resolution of the conflict's files
// apart, the paths that conflict in its conflicted merge, which the daemon
// reads from that merge in the bare repo, not from the bug's body (SPEC
// §7.5's A conflict, §10.1), on the verdictRigs of conflict_run_test.go.

// A conflict bug whose run resolves a conflict in a source file, story.go,
// which the story added and the main commit it names added too, with no
// test change, passes the checks: rule 5 needs no test for a source file
// among the conflict's files.
func TestConflictRunSourceResolved(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	v.conflictRunReady(map[string]string{"story.go": "package story\n\n// main's\n"})
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n", "story.go": "package story\n\n// main's, then the story's\n"})
	v.pass()
	if v.runs() != 1 || pickStage(f, conflictRunBug) != "stage:verifying" {
		t.Fatalf("the pick's pass ran claude %d times, and left the bug at %s; want once, and verifying", v.runs(), pickStage(f, conflictRunBug))
	}
	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 {
		t.Fatalf("the checks' pass ran the commands %d times; want once", v.checksRan()-ran)
	}
	conflictRunNoFindings(t, f, "the checks' pass")
}

// A conflict bug whose run commits the merge with its conflict markers
// left in f.txt, a file that isn't code, fails rule 7, naming f.txt, which
// the conflicted merge had markers in, though the head holds both commits
// and merges cleanly with the tip.
func TestConflictRunMarkersLeft(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	v.conflictRunReady(nil)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, nil)
	v.passes(2)
	head := v.trimmed("commit.1")
	if got := gitIn(t, v.d.Clone.Dir(), "show", head+":f.txt"); !strings.Contains(got, "<<<<<<< ") {
		t.Fatalf("the head's f.txt =\n%s\nwant the markers git merge left", got)
	}
	findings := conflictRunFindings(t, f)
	for _, want := range []string{"Rule 7", "leaves conflict markers", "- f.txt"} {
		if !strings.Contains(findings, want) {
			t.Errorf("the findings =\n%s\nwant them to hold %q", findings, want)
		}
	}
	for _, not := range []string{"lacks these commits", "doesn't merge cleanly", "Rule 5", "Rule 3"} {
		if strings.Contains(findings, not) {
			t.Errorf("the findings =\n%s\nwant nothing of %q", findings, not)
		}
	}
}

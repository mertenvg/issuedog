package verify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A conflict bug's checks judge the resolution of the conflict's files
// apart (SPEC §7.5, A conflict, §10.1): rule 5 needs no test for a source
// file among them, rule 3 takes main's deletion of a test file among them,
// and rule 7 fails one that still holds conflict markers. These tests
// build the story tip and the main commit on real temporary repos, the
// conflicted merge of the two as the daemon does (repo.ConflictMerge),
// and each head as an implementer's merge of the tip into that main
// commit, resolved.

// conflictBugNumber is the conflict bug whose conflicted merge the tests
// keep.
const conflictBugNumber = 9

// conflictRig is a rulesRepo with a story tip and a main commit cut from
// one base, and their conflicted merge in the bare repo.
type conflictRig struct {
	*rulesRepo
	base, tip, main string
	// merged is the conflicted merge, and conflicts its conflicting paths,
	// as ConflictMerge returns them.
	merged    string
	conflicts []string
}

// newConflictRig commits base, then on it the story's tip, changing
// tipFiles and removing tipRemove, and the main commit, changing mainFiles
// and removing mainRemove, and builds their conflicted merge.
func newConflictRig(t *testing.T, base, tipFiles, mainFiles map[string]string, tipRemove, mainRemove []string) *conflictRig {
	t.Helper()
	c := &conflictRig{rulesRepo: newRulesRepo(t)}
	c.base = c.commit(base)
	c.tip = c.commit(tipFiles, tipRemove...)
	c.checkout(c.base)
	c.main = c.commit(mainFiles, mainRemove...)
	var err error
	c.merged, c.conflicts, err = c.bare.ConflictMerge(context.Background(), conflictBugNumber, c.main, c.tip)
	if err != nil {
		t.Fatalf("ConflictMerge: %v", err)
	}
	return c
}

// resolve is a conflict bug's head: the tip merged into the main commit,
// as git merge leaves it in the work tree, conflict markers included,
// then files written and remove removed, committed as the merge. It
// returns the head's SHA, pushed to the bare repo.
func (c *conflictRig) resolve(files map[string]string, remove ...string) string {
	c.t.Helper()
	c.checkout(c.main)
	out, err := rulesCmd(c.work, "merge", "--quiet", "--no-commit", c.tip).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		c.t.Fatalf("git merge of the tip = %v: %s; want it to stop on the conflicts", err, out)
	}
	head := c.commit(files, remove...)
	if parents := rulesGit(c.t, c.work, "rev-list", "--parents", "-n", "1", head); parents != head+" "+c.main+" "+c.tip {
		c.t.Fatalf("the head and its parents are %q; want the merge of main's %s and the tip %s", parents, c.main, c.tip)
	}
	return head
}

// facts is f as a conflict bug's checks set it for rules 3 to 7: the main
// commit as Baseline, the diff from the conflicted merge, the tip as
// Target, both commits as MustHold, and the conflict's files.
func (c *conflictRig) facts(head string) Facts {
	return Facts{Repo: c.bare, Baseline: c.main, Base: c.merged, Head: head, Target: c.tip,
		MustHold: []string{c.main, c.tip}, Conflicts: c.conflicts, Config: rulesConfig()}
}

// conflictBase is the base the conflict tests' story and main start from:
// rulesBase with a package of its own, whose source and test are pkg/x.go
// and pkg/x_test.go.
func conflictBase() map[string]string {
	base := maps.Clone(rulesBase)
	base["pkg/x.go"] = "package pkg\n"
	base["pkg/x_test.go"] = "package pkg\n"
	return base
}

// Rule 5 passes a conflict bug's resolution that changes only source files
// among the conflict's files, with no test, and fails one that also
// changes a source file outside them, naming that file alone. A test
// change beside it passes, as for any task. With no Conflicts, as for any
// other task, the same source-only resolution fails, naming the
// conflicting file, so the exemption is the conflict bug's alone. The
// story's own source file with no test, story.go, is in the conflicted
// merge, so no head is judged on it.
func TestConflictRule5(t *testing.T) {
	t.Parallel()
	c := newConflictRig(t, conflictBase(),
		map[string]string{"main.go": "package main\n\n// the story's\n", "story.go": "package main\n\n// the story's own\n"},
		map[string]string{"main.go": "package main\n\n// main's\n"}, nil, nil)
	if !slices.Equal(c.conflicts, []string{"main.go"}) {
		t.Fatalf("the conflict's files = %q, want main.go alone", c.conflicts)
	}
	resolved := "package main\n\n// main's\n// the story's\n"

	sourceOnly := c.resolve(map[string]string{"main.go": resolved})
	if got := check(t, Rule5, c.facts(sourceOnly)); got != nil {
		t.Errorf("Rule5 of a source-only resolution = %+v, want nil", got)
	}
	f := c.facts(sourceOnly)
	f.Conflicts = nil
	wantFinding(t, check(t, Rule5, f), 5, "- main.go")

	elsewhere := c.resolve(map[string]string{"main.go": resolved, "pkg/x.go": "package pkg\n\n// untested\n"})
	got := check(t, Rule5, c.facts(elsewhere))
	wantFinding(t, got, 5, "adds or changes no test file", "- pkg/x.go")
	if got != nil && (strings.Contains(got.Detail, "main.go") || strings.Contains(got.Detail, "story.go")) {
		t.Errorf("rule 5's detail =\n%s\nwant pkg/x.go alone, not the conflict's main.go or the story's story.go", got.Detail)
	}

	tested := c.resolve(map[string]string{"main.go": resolved, "pkg/x.go": "package pkg\n\n// tested\n", "pkg/x_test.go": "package pkg\n\n// tested\n"})
	if got := check(t, Rule5, c.facts(tested)); got != nil {
		t.Errorf("Rule5 of a resolution with a tested change elsewhere = %+v, want nil", got)
	}
}

// Rule 3 passes a conflict bug's resolution that deletes a test file among
// the conflict's files that its main commit deleted, while the story
// changed it, and fails one that deletes a conflicting test file main
// kept, or test files outside the conflict, naming those files alone: one
// main has, and one the story added, which main lacks too. With no
// Conflicts, as for any other task, taking main's deletion fails, naming
// the file.
func TestConflictRule3(t *testing.T) {
	t.Parallel()
	c := newConflictRig(t, conflictBase(),
		map[string]string{"pkg/x_test.go": "package pkg\n\n// the story's\n", "main_test.go": "package main\n\n// the story's\n",
			"pkg/story_test.go": "package pkg\n"},
		map[string]string{"main_test.go": "package main\n\n// main's\n"}, nil, []string{"pkg/x_test.go"})
	if !slices.Equal(c.conflicts, []string{"main_test.go", "pkg/x_test.go"}) {
		t.Fatalf("the conflict's files = %q, want main_test.go and pkg/x_test.go", c.conflicts)
	}
	both := "package main\n\n// main's\n// the story's\n"

	mainsDeletion := c.resolve(map[string]string{"main_test.go": both}, "pkg/x_test.go")
	if got := check(t, Rule3, c.facts(mainsDeletion)); got != nil {
		t.Errorf("Rule3 of a resolution taking main's deletion = %+v, want nil", got)
	}
	f := c.facts(mainsDeletion)
	f.Conflicts = nil
	wantFinding(t, check(t, Rule3, f), 3, "deletes test files", "- pkg/x_test.go")

	mainKept := c.resolve(nil, "main_test.go")
	got := check(t, Rule3, c.facts(mainKept))
	wantFinding(t, got, 3, "deletes test files", "- main_test.go")
	if got != nil && strings.Contains(got.Detail, "pkg/x_test.go") {
		t.Errorf("rule 3's detail =\n%s\nwant main_test.go alone, as the head keeps the story's pkg/x_test.go", got.Detail)
	}

	others := c.resolve(map[string]string{"main_test.go": both}, "pkg/x_test.go", "web/app.test.ts", "pkg/story_test.go")
	got = check(t, Rule3, c.facts(others))
	wantFinding(t, got, 3, "deletes test files", "- pkg/story_test.go", "- web/app.test.ts")
	if got != nil && strings.Contains(got.Detail, "pkg/x_test.go") {
		t.Errorf("rule 3's detail =\n%s\nwant pkg/story_test.go and web/app.test.ts alone, not main's deletion of pkg/x_test.go", got.Detail)
	}
}

// Rule 7 fails a conflict bug's head that leaves conflict markers in a
// file among the conflict's files whose blob in the conflicted merge had
// them, naming each such file, past listMax with how many more, and
// passes one that resolves them, or deletes the file. A conflicting file
// whose conflicted blob had none, as the story's side of a file main
// deleted, may hold a marker-like line. With no Conflicts, as for any
// other task, the markers aren't looked for.
func TestConflictRule7Markers(t *testing.T) {
	t.Parallel()
	base, tipFiles, mainFiles := conflictBase(), map[string]string{}, map[string]string{}
	var notes []string
	for i := range listMax + 2 {
		p := fmt.Sprintf("notes/n%02d.md", i)
		notes = append(notes, p)
		base[p], tipFiles[p], mainFiles[p] = "notes\n", "the story's notes\n", "main's notes\n"
	}
	base["docs/old.md"], tipFiles["docs/old.md"] = "old\n", "the story's old\n"
	c := newConflictRig(t, base, tipFiles, mainFiles, nil, []string{"docs/old.md"})
	if want := append([]string{"docs/old.md"}, notes...); !slices.Equal(c.conflicts, want) {
		t.Fatalf("the conflict's files = %q, want %q", c.conflicts, want)
	}
	resolved := map[string]string{}
	for _, p := range notes {
		resolved[p] = "main's notes\nthe story's notes\n"
	}

	left := c.resolve(nil)
	got := check(t, Rule7, c.facts(left))
	wantFinding(t, got, 7, "The head "+left+" leaves conflict markers", "- "+notes[0]+"\n", "- "+notes[listMax-1]+"\n…and 2 more")
	if got != nil && (strings.Contains(got.Detail, "docs/old.md") || strings.Contains(got.Detail, "lacks") || strings.Contains(got.Detail, "merge cleanly")) {
		t.Errorf("rule 7's detail =\n%s\nwant the notes alone, as the head holds both commits and merges cleanly", got.Detail)
	}
	f := c.facts(left)
	f.Conflicts = nil
	if got := check(t, Rule7, f); got != nil {
		t.Errorf("Rule7 with no Conflicts = %+v, want nil", got)
	}

	one := maps.Clone(resolved)
	delete(one, notes[3])
	oneLeft := c.resolve(one)
	got = check(t, Rule7, c.facts(oneLeft))
	wantFinding(t, got, 7, "leaves conflict markers", "- "+notes[3])
	if got != nil && strings.Count(got.Detail, "- notes/") != 1 {
		t.Errorf("rule 7's detail =\n%s\nwant %s alone", got.Detail, notes[3])
	}

	clean := maps.Clone(resolved)
	clean["docs/old.md"] = "the story's old\n=======\n"
	if got := check(t, Rule7, c.facts(c.resolve(clean))); got != nil {
		t.Errorf("Rule7 of a resolution with no markers left = %+v, want nil", got)
	}
	if got := check(t, Rule7, c.facts(c.resolve(nil, notes...))); got != nil {
		t.Errorf("Rule7 of a resolution deleting the marked files = %+v, want nil", got)
	}
}

// Rule 7 names the markers a conflict bug's head leaves in the conflict's
// files even when that head shares no history with the tip, after saying
// so.
func TestConflictRule7NoHistory(t *testing.T) {
	t.Parallel()
	c := newConflictRig(t, conflictBase(), map[string]string{"notes.md": "the story's\n"}, map[string]string{"notes.md": "main's\n"}, nil, nil)
	rulesGit(t, c.work, "checkout", "--quiet", "--orphan", "unrelated")
	head := c.commit(map[string]string{"notes.md": "<<<<<<< HEAD\nmain's\n=======\nthe story's\n>>>>>>> tip\n"})
	wantFinding(t, check(t, Rule7, c.facts(head)), 7, "The head "+head+" shares no history with its target "+c.tip+".",
		"The head "+head+" leaves conflict markers", "- notes.md")
}

// markerLine matches git's conflict marker lines alone: "<<<<<<< " or
// ">>>>>>> " at a line's start, or "=======" as the whole line, with or
// without its line ending.
func TestMarkerLine(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]bool{
		"<<<<<<< HEAD\n":   true,
		"<<<<<<< 0123abc":  true,
		"=======\n":        true,
		"=======\r\n":      true,
		"=======":          true,
		">>>>>>> target\n": true,
		"<<<<<<<\n":        false,
		" <<<<<<< HEAD\n":  false,
		"<<<<<< HEAD\n":    false,
		"========\n":       false,
		"======= x\n":      false,
		" =======\n":       false,
		">>>>>>>>\n":       false,
		"plain\n":          false,
	} {
		if got := markerLine(line); got != want {
			t.Errorf("markerLine(%q) = %v, want %v", line, got, want)
		}
	}
}

// Rules 3 and 7 with Conflicts return an error, saying which rule, and no
// finding, when the files they read for them can't be read, as at a main
// commit or a conflicted merge the bare repo lacks.
func TestConflictRulesReadError(t *testing.T) {
	t.Parallel()
	c := newConflictRig(t, conflictBase(), map[string]string{"pkg/x_test.go": "package pkg\n\n// the story's\n"},
		map[string]string{"README.md": "main's\n"}, nil, []string{"pkg/x_test.go"})
	missing := strings.Repeat("0", len(c.main))
	f := c.facts(c.resolve(nil, "pkg/x_test.go"))
	f.Baseline = missing
	if got, err := Rule3(context.Background(), f); err == nil || !strings.HasPrefix(err.Error(), "rule 3") || got != nil {
		t.Errorf("Rule3 at a missing main commit = %+v, %v; want an error starting %q", got, err, "rule 3")
	}
	f = c.facts(c.resolve(nil))
	f.Base = missing
	if got, err := Rule7(context.Background(), f); err == nil || !strings.HasPrefix(err.Error(), "rule 7") || got != nil {
		t.Errorf("Rule7 at a missing conflicted merge = %+v, %v; want an error starting %q", got, err, "rule 7")
	}
}

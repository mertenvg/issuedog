package daemon_test

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A conflict bug's run starts from the main commit it names, with its
// story branch's tip named as its target to merge, and the checks and the
// verifier judge only its resolution (SPEC §3.4, §7.5's A conflict,
// §10.1, §10.2), on verdictRigs wired as main's, with real temporary
// repos: story 1, whose branch the tests cut and push to in the origin
// itself, as an admin may, and main, which they move as the owner's
// merges do, so step 2's merge of main into the branch conflicts and
// files the conflict bug, #3, which the pick then takes.

// conflictRunClaude is verifierRunClaude, but an implementer run k first
// sources $FAKE/merge.<k>, when the test wrote one (conflictRunMerge), as
// to merge its clone's target and resolve the conflicts, then commits as
// any implementer run of the fake does.
var conflictRunClaude = strings.Replace(verifierRunClaude, `echo "$n" > "run.$n.txt"`,
	`{ [ ! -f "$FAKE/merge.$n" ] || . "$FAKE/merge.$n" || exit 7; } && echo "$n" > "run.$n.txt"`, 1)

// conflictRunBug is the conflict bug's number: story 1 and the digest, #2, are
// the issues before it.
const conflictRunBug = 3

// conflictRunWire has v's fake claude be conflictRunClaude, which a
// restart's wire replaces, and returns v.
func (v *verdictRig) conflictRunWire() *verdictRig {
	v.t.Helper()
	if !strings.Contains(conflictRunClaude, `. "$FAKE/merge.$n"`) {
		v.t.Fatal("conflictRunClaude sources no merge script, as verifierRunClaude changed")
	}
	if err := os.WriteFile(v.path("claude", "claude"), []byte(conflictRunClaude), 0o755); err != nil {
		v.t.Fatal(err)
	}
	return v
}

// conflictRunMerge has v's implementer run k merge its clone's target,
// then write files, each path, in a dir made if it's missing, to its
// content, neither holding a single quote, resolving the conflicts, and
// commit the merge.
func (v *verdictRig) conflictRunMerge(k int, files map[string]string) {
	v.t.Helper()
	script := "git merge --quiet --no-edit target >/dev/null 2>&1\n"
	for _, path := range slices.Sorted(maps.Keys(files)) {
		script += "mkdir -p \"$(dirname '" + path + "')\" && printf '%s' '" + files[path] + "' > '" + path + "' || return 1\n"
	}
	script += "git add -A && git commit --quiet -m 'merge the story branch, resolving the conflicts'\n"
	v.conflictRunScript(k, script)
}

// conflictRunScript has v's implementer run k source script, sh, in its
// clone before its own commit (conflictRunClaude).
func (v *verdictRig) conflictRunScript(k int, script string) {
	v.t.Helper()
	if err := os.WriteFile(v.path("claude", "merge."+strconv.Itoa(k)), []byte(script), 0o644); err != nil {
		v.t.Fatal(err)
	}
}

// conflictRunReady files story 1 on v, cuts its branch, pushes a commit
// of the story's to it, which changes f.txt and adds story.txt and
// story.go, a source file with no test, which the tip's checks don't
// judge, as rule 5 is one task's (SPEC §7.5), and checks that tip as
// success. Then it moves main: each of mainFiles committed in
// turn, each path with its content, then main's own change to f.txt,
// which conflicts. The pass after files the conflict bug for that pair,
// #3, at ready, which its origin names. It returns the tip and main's
// commit.
func (v *verdictRig) conflictRunReady(mainFiles map[string]string) (string, string) {
	v.t.Helper()
	tipStory(v.t, v.f)
	v.passes(2)
	v.tipCut(tipBranch)
	tip := v.tipPush(tipBranch, map[string]string{"f.txt": "the story\n", "story.txt": "the story's change\n", "story.go": "package story\n"})
	v.tipJudged(tip, 1, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")})
	for _, path := range slices.Sorted(maps.Keys(mainFiles)) {
		v.o.commit(path, mainFiles[path])
	}
	main := v.storyMainMoved("f.txt", "main's\n")
	v.pass()
	if n := len(v.f.State().Issues); n != conflictRunBug || !strings.HasSuffix(v.f.State().Issues[n-1].Body, storyMainConflictOrigin(tip, main)) {
		v.t.Fatalf("the fake's newest issue is #%d =\n%+v\nwant the conflict bug, #%d, for main's %s and the tip %s",
			n, v.f.State().Issues[n-1], conflictRunBug, main, tip)
	}
	return tip, main
}

// conflictRunLine is the preamble's line for a conflict bug that names
// main, a main commit, as its start, and the story branch at tip as its
// target (SPEC §12).
func conflictRunLine(main, tip string) string {
	return "- This task is a conflict bug: `main` at `" + main + "` doesn't merge cleanly into the story branch `" + tipBranch + "`. " +
		"Your checkout starts from that `main` commit, or from your earlier runs' work on it, and its `target` branch is `" + tipBranch +
		"` at `" + tip + "`, the story branch's tip."
}

// conflictRunFindings is the comment of the conflict bug's newest move
// verifying → in-progress on findings, failing the test with none.
func conflictRunFindings(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	moves := storyFailureComments(f, conflictRunBug, "<!-- issuedog:transition from=verifying to=in-progress ")
	if len(moves) == 0 || pickStage(f, conflictRunBug) != "stage:in-progress" {
		t.Fatalf("the conflict bug is at %s, with the findings moves %q; want in-progress, by one", pickStage(f, conflictRunBug), moves)
	}
	return moves[len(moves)-1]
}

// conflictRunNoFindings fails the test, naming step, when the conflict
// bug has any move verifying → in-progress on findings, or isn't at
// verifying.
func conflictRunNoFindings(t *testing.T, f *githubtest.Fake, step string) {
	t.Helper()
	if moves := storyFailureComments(f, conflictRunBug, "<!-- issuedog:transition from=verifying to=in-progress "); len(moves) != 0 ||
		pickStage(f, conflictRunBug) != "stage:verifying" {
		t.Errorf("%s left the bug at %s, with the findings moves %q; want verifying, with none", step, pickStage(f, conflictRunBug), moves)
	}
}

// A conflict bug's first attempt starts from the main commit it names,
// with its story branch's tip named as its target, and the lease records
// both, with the tip as its story tip, though that main commit changed a
// protected path since the story's baseline; the preamble names the main
// commit and says to merge the tip (SPEC §3.4, §7.5's A conflict, §12).
// Its run skips the merge, copying the story's change to f.txt by hand,
// so its head lacks the tip, though it merges cleanly with it: the checks
// fail it on rule 7 alone, naming the tip, as rule 2's baseline is that
// main commit, which the head matches. Its retry starts from that head,
// which passes rule 2 against the main commit, so isn't dropped, with the
// tip named as its target again, as its head lacks it. Main moves past
// that commit before the run, changing another protected path, and the
// bug's start and both baselines stay the commit it names, not main's
// newest.
func TestConflictRunSkippedMerge(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	tip, main := v.conflictRunReady(map[string]string{"secrets/key.txt": "main's key\n"})
	v.o.commit("secrets/newer.txt", "a newer main's key\n")
	if newer := v.mainSHA(); newer == main {
		t.Fatalf("main is still at %s; want it moved past the bug's main commit", main)
	}
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunScript(1, "printf 'the story\\n' > f.txt && git add f.txt && git commit --quiet -m 'copy the story by hand'\n")
	v.pass()
	if v.runs() != 1 || pickStage(f, conflictRunBug) != "stage:verifying" {
		t.Fatalf("the pick's pass ran claude %d times, and left the bug at %s; want once, and verifying", v.runs(), pickStage(f, conflictRunBug))
	}
	if l := v.leaseOf("1"); l.Issue != conflictRunBug || l.Start != main || l.Target != tip || l.StoryTip != tip || l.Attempt != 1 {
		t.Errorf("the first attempt's lease = %+v; want it on #%d, attempt 1, from main's %s, with the tip %s as its target and story tip",
			l, conflictRunBug, main, tip)
	}
	if got, refs := v.trimmed("head.1"), v.file("refs.1"); got != main || !strings.Contains(refs, tip+" refs/heads/target") {
		t.Errorf("the first attempt's clone is at %s, with the refs\n%s\nwant main's %s, with the tip %s as target", got, refs, main, tip)
	}
	if sys := v.file("ctx.1/system.md"); !strings.Contains(sys, conflictRunLine(main, tip)) || strings.Contains(sys, "dropped") {
		t.Errorf("the first attempt's system.md =\n%s\nwant it to say\n%s\nand nothing of a dropped head", sys, conflictRunLine(main, tip))
	}
	head := v.trimmed("commit.1")

	v.pass()
	findings := conflictRunFindings(t, f)
	for _, want := range []string{"Rule 7", "The head " + head + " lacks these commits in its history, which it must merge:\n- " + tip} {
		if !strings.Contains(findings, want) {
			t.Errorf("the findings =\n%s\nwant them to hold %q", findings, want)
		}
	}
	if strings.Contains(findings, "Rule 2") || strings.Contains(findings, main+"\n") {
		t.Errorf("the findings =\n%s\nwant rule 7's alone, naming only the tip the head lacks", findings)
	}

	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times after the findings; want twice", v.runs())
	}
	if l := v.leaseOf("2"); l.Start != head || l.Target != tip || l.StoryTip != tip || l.Attempt != 2 {
		t.Errorf("the retry's lease = %+v; want attempt 2, from the head %s, with the tip %s as its target and story tip", l, head, tip)
	}
	if sys := v.file("ctx.2/system.md"); !strings.Contains(sys, conflictRunLine(main, tip)) || strings.Contains(sys, "dropped") {
		t.Errorf("the retry's system.md =\n%s\nwant it to say\n%s\nand nothing of a dropped head", sys, conflictRunLine(main, tip))
	}
}

// A conflict bug whose run merges its story branch's tip into the main
// commit it names, resolving the conflict, passes the checks, though that
// main commit added a test file holding a skip marker: rules 3 to 6 diff
// from the conflicted merge of the two, kept under
// refs/issuedog/conflicts/<n> with both as its parents, so they judge only
// the resolution (SPEC §7.5's A conflict, §10.1): neither the story's
// story.go, a source file with no test, fails rule 5, nor main's test file
// rule 3, so no findings are posted. A restart then runs the checks again,
// as it empties their passes, and posts none either. The verifier's diff.patch is
// that diff: the resolution of f.txt and the run's own file, and nothing
// of the story's or main's changes. Its checks.md names the tip, the main
// commit as rule 2's baseline, and the conflicted merge it starts at
// (SPEC §10.2).
func TestConflictRunResolved(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	mainTest := "package a\n\nfunc TestA(t *testing.T) { " + skipMarker + "\"later\") }\n"
	tip, main := v.conflictRunReady(map[string]string{"a_test.go": mainTest})
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n"})
	v.pass()
	if v.runs() != 1 || pickStage(f, conflictRunBug) != "stage:verifying" {
		t.Fatalf("the pick's pass ran claude %d times, and left the bug at %s; want once, and verifying", v.runs(), pickStage(f, conflictRunBug))
	}
	head := v.trimmed("commit.1")
	if parents := gitIn(t, v.d.Clone.Dir(), "rev-list", "--parents", "-n", "1", head+"^"); parents != head+"^ "+main+" "+tip &&
		!strings.HasSuffix(parents, " "+main+" "+tip) {
		t.Fatalf("the run's head's parent and its parents are %q; want the merge of main's %s and the tip %s", parents, main, tip)
	}

	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 || pickStage(f, conflictRunBug) != "stage:verifying" || v.runs() != 1 {
		t.Fatalf("the checks' pass ran the commands %d times, claude %d times, and left the bug at %s; want once, never, and verifying",
			v.checksRan()-ran, v.runs()-1, pickStage(f, conflictRunBug))
	}
	conflictRunNoFindings(t, f, "the checks' pass")
	conflicted := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.ConflictsRef+strconv.Itoa(conflictRunBug))
	if got := gitIn(t, v.d.Clone.Dir(), "rev-list", "--parents", "-n", "1", conflicted); got != conflicted+" "+main+" "+tip {
		t.Errorf("the conflicted merge and its parents are %q; want main's %s and the tip %s as its parents", got, main, tip)
	}
	if got := gitIn(t, v.d.Clone.Dir(), "show", conflicted+":f.txt"); !strings.Contains(got, "<<<<<<<") {
		t.Errorf("the conflicted merge's f.txt =\n%s\nwant its conflict markers", got)
	}

	v = v.restart().conflictRunWire()
	v.out(2, verifierPass(t, "The resolution keeps both."))
	ran = v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 || v.runs() != 1 {
		t.Fatalf("the restart's pass ran the commands %d times, and claude %d times; want once, and never", v.checksRan()-ran, v.runs()-1)
	}
	conflictRunNoFindings(t, f, "the restart's pass")
	v.pass()
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times after the checks passed again; want twice, the verifier's run", v.runs())
	}
	patch := v.file("ctx.2/diff.patch")
	for _, want := range []string{"f.txt", "+both", "run.1.txt"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the verifier's diff.patch =\n%s\nwant it to hold %q", patch, want)
		}
	}
	for _, not := range []string{"a_test.go", "story.txt", "story.go", "+the story\n", "+main's"} {
		if strings.Contains(patch, not) {
			t.Errorf("the verifier's diff.patch =\n%s\nwant nothing of %q, which the conflicted merge holds", patch, not)
		}
	}
	want := "Rules 2 to 7 passed, against the story branch `" + tipBranch + "` at `" + tip + "`, the tip this conflict bug merges into `main` at `" + main +
		"`: rule 7's target is that tip, and the head holds both commits in its history, and rule 2's baseline is `main` at `" + main + "`. " +
		"diff.patch starts at `" + conflicted + "`, the merge of the two with its conflict markers left in, so it shows only the resolution."
	if checks := v.file("ctx.2/checks.md"); !strings.Contains(checks, want) {
		t.Errorf("the verifier's checks.md =\n%s\nwant it to say\n%s", checks, want)
	}
}

// A conflict bug's resolution that changes a test file the main commit it
// names added, adding a skip marker of its own, fails rule 3 naming only
// that line, not the marker main's commit added, as the diff starts at
// the conflicted merge (SPEC §7.5's A conflict, §10.1).
func TestConflictRunResolutionTest(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	mainTest := "package a\n\nfunc TestA(t *testing.T) { " + skipMarker + "\"later\") }\n"
	v.conflictRunReady(map[string]string{"a_test.go": mainTest})
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n", "a_test.go": mainTest + "\nfunc TestB(t *testing.T) { " + skipMarker + "\"the resolution adds\") }\n"})
	v.passes(2)
	findings := conflictRunFindings(t, f)
	if want := "a_test.go:5 adds " + strconv.Quote(skipMarker); !strings.Contains(findings, want) || strings.Contains(findings, "a_test.go:3") {
		t.Errorf("the findings =\n%s\nwant rule 3's naming %q alone, not main's line 3", findings, want)
	}
	if strings.Contains(findings, "Rule 7") {
		t.Errorf("the findings =\n%s\nwant no rule 7's, as the head merged both", findings)
	}
}

// A conflict bug's head at verifying that holds its story branch's tip but
// not the main commit it names, as no run's head can be, since each holds
// the commit it started from (SPEC §3.2), fails rule 7, naming that main
// commit, though it merges cleanly with the tip: its head must be the
// merge of both (SPEC §7.5's A conflict).
func TestConflictRunLacksMain(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	tip, main := v.conflictRunReady(nil)
	head := v.storyFailureHead(conflictRunBug, tip, map[string]string{"run.txt": "on the tip alone\n"})
	f.RemoveLabel(conflictRunBug, app, "stage:ready")
	f.AddLabel(conflictRunBug, app, "stage:verifying")
	f.AddComment(conflictRunBug, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->")
	v.pass()
	findings := conflictRunFindings(t, f)
	if want := "The head " + head + " lacks these commits in its history, which it must merge:\n- " + main + "\n"; !strings.Contains(findings, want) {
		t.Errorf("the findings =\n%s\nwant them to say %q", findings, want)
	}
}

// A conflict bug's head that fails rule 2, as its run's resolution added a
// protected path, isn't named: its retry starts as a first attempt does,
// from the main commit the bug names, with its story branch's tip named as
// its target, and the preamble says the head was dropped, so the checkout
// starts from that main commit, with the target to merge (SPEC §3.4's A
// head that fails rule 2, §7.5's A conflict, §12).
func TestConflictRunDropped(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	tip, main := v.conflictRunReady(nil)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n", "secrets/key.txt": "a key\n"})
	v.passes(2)
	if findings := conflictRunFindings(t, f); !strings.Contains(findings, "secrets/key.txt") {
		t.Fatalf("the findings =\n%s\nwant rule 2's, naming secrets/key.txt", findings)
	}
	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times after the findings; want twice", v.runs())
	}
	if l := v.leaseOf("2"); l.Start != main || l.Target != tip || l.StoryTip != tip {
		t.Errorf("the retry's lease = %+v; want it from main's %s, with the tip %s as its target and story tip, as the head was dropped", l, main, tip)
	}
	want := "so issuedog dropped it: your checkout starts from `main` at `" + main + "`, with `target` to merge, as a first run's does,"
	if sys := v.file("ctx.2/system.md"); !strings.Contains(sys, want) || !strings.Contains(sys, conflictRunLine(main, tip)) {
		t.Errorf("the retry's system.md =\n%s\nwant it to say\n%s\nand\n%s", sys, conflictRunLine(main, tip), want)
	}
}

// A conflict bug's run whose story branch's tip moved under it, to one
// that passes rule 2, keeps its result (SPEC §8.2), and its head, which
// merged the tip the run named, lacks the new one: the checks fail it on
// rule 7, naming the new tip, and its retry starts from the head with the
// new tip named as its target. While a later tip fails rule 2, the bug
// waits, uncounted, and a pass makes no request but 304s. Once a tip
// passes rule 2 again, the retry starts from the head, naming that tip.
func TestConflictRunTipMoved(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	tip, main := v.conflictRunReady(nil)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n"})
	f.FailWrite(len(f.Writes()) + 4) // the pick writes three: the apply's first write is refused
	writes := len(f.Writes())
	if err := v.d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), "on #3 failed, 1 of 3 times") {
		t.Fatalf("Iterate = %v; want the run's apply failed once", err)
	}
	if w := f.Writes()[writes:]; v.runs() != 1 || len(w) == 0 || !w[len(w)-1].Failed {
		t.Fatalf("the pick's pass ran claude %d times, and wrote %+v; want once, its last write refused", v.runs(), w)
	}
	if l := v.lease(); l.Start != main || l.Target != tip || l.StoryTip != tip {
		t.Fatalf("the kept lease = %+v; want it from main's %s, with the tip %s as its target and story tip", l, main, tip)
	}
	head := v.trimmed("commit.1")

	moved := v.tipPush(tipBranch, map[string]string{"sibling.txt": "a sibling's change\n"})
	v.passes(2)
	findings := conflictRunFindings(t, f)
	if want := "The head " + head + " lacks these commits in its history, which it must merge:\n- " + moved; !strings.Contains(findings, want) {
		t.Errorf("the findings =\n%s\nwant them to say %q", findings, want)
	}
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+strconv.Itoa(conflictRunBug)); got != head {
		t.Errorf("the bug's head is %s; want the run's %s, kept", got, head)
	}

	bad := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	v.passes(3)
	if v.runs() != 1 || pickStage(f, conflictRunBug) != "stage:in-progress" {
		t.Fatalf("on the tip %s, which fails rule 2, claude ran %d times, and the bug is at %s; want once, and in-progress", bad, v.runs(), pickStage(f, conflictRunBug))
	}
	v.quietPass("the conflict bug waits for a tip that passes rule 2")

	fixed := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": ""})
	v.passes(2)
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times once a tip passes rule 2 again; want twice", v.runs())
	}
	if l := v.leaseOf("2"); l.Start != head || l.Target != fixed || l.StoryTip != fixed {
		t.Errorf("the retry's lease = %+v; want it from the head %s, with the newest tip %s as its target and story tip", l, head, fixed)
	}
}

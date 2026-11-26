package daemon_test

import (
	"context"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// An implementer run on a task in a story starts from its story branch's
// checked tip (SPEC §3.2, §3.4, §7.5), and its result is dropped when that
// tip moves under it to one that fails rule 2 (SPEC §8.2), on a
// verdictRig wired as main's, with real temporary repos: story 1, at
// planned, whose branch the test cuts and pushes to in the origin itself,
// and task 2 in it, at in-progress, as the pick would move it there
// (pick_story_test.go tests the pick itself).

// storyRunTask files task 2, in story 1, at in-progress, as the App moved
// it there, with priority:p1 and scope:booking, and returns its number.
func storyRunTask(t testing.TB, f *githubtest.Fake) int {
	t.Helper()
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	if n != 2 {
		t.Fatalf("the task is #%d, want #2", n)
	}
	f.SetParent(n, 1)
	f.AddLabel(n, app, "stage:in-progress")
	f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress run=rpick -->")
	return n
}

// newStoryRunRig is a verdictRig with story 1 and its task 2, whose first
// implementer run commits and returns commits.
func newStoryRunRig(t *testing.T) (*githubtest.Fake, *verdictRig) {
	t.Helper()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	storyRunTask(t, f)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	return f, v
}

// storyRunChecked runs a pass of v, which must check tip, story 1's
// branch's tip, post its issuedog/tip as conclusion, and start no run.
func (v *verdictRig) storyRunChecked(tip, conclusion string) {
	v.t.Helper()
	runs := v.runs()
	v.pass()
	if got := tipLast(v.t, v.f); got.HeadSHA != tip || got.Name != github.TipCheck || got.Conclusion != conclusion || v.runs() != runs {
		v.t.Fatalf("the pass posted %+v, and claude ran %d times; want %s on %s as %s, and no run", got, v.runs()-runs, github.TipCheck, tip, conclusion)
	}
}

// storyRunHead commits files on a branch, task, off base in v's origin's
// work tree, each path to its content, goes back to main, and makes the
// commit task 2's head in v's daemon clone, as an earlier run's handoff
// left it (SPEC §3.2). The origin never gets the branch.
func (v *verdictRig) storyRunHead(base string, files map[string]string) string {
	v.t.Helper()
	w := v.o.work
	gitIn(v.t, w, "checkout", "--quiet", "-B", "task", base)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		full := filepath.Join(w, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			v.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			v.t.Fatal(err)
		}
		gitIn(v.t, w, "add", path)
	}
	gitIn(v.t, w, "commit", "--quiet", "-m", "the task's earlier run")
	head := gitIn(v.t, w, "rev-parse", "HEAD")
	gitIn(v.t, w, "checkout", "--quiet", "main")
	gitIn(v.t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", w, "refs/heads/task:"+repo.TasksRef+"2")
	return head
}

// storyRunTarget is the preamble's line naming the story branch at sha as
// the run's target (SPEC §12).
func storyRunTarget(sha string) string {
	return "- Your checkout's `target` branch is `" + tipBranch + "` at `" + sha + "`, which your starting commit doesn't merge with cleanly, " +
		"or fails verify once merged with, as input.md says. Merge `target` first, resolve any conflicts, and commit the merge."
}

// storyRunDropped is the preamble's line saying the task's head was
// dropped for failing rule 2, so the checkout starts from the story
// branch, before rule 2's finding (SPEC §3.4, §12).
const storyRunDropped = "- The head your earlier runs left on this task fails verify's rule 2, as the finding below says, so issuedog dropped it: " +
	"your checkout starts from `" + tipBranch + "`, as a first run's does, and holds none of their work. Do the task again from here. " +
	"Rule 2's finding on the dropped head:\n\n```text\n"

// An implementer run on a task in a story names its story branch's tip, as
// the pass's sync left it, once that tip's issuedog/tip is success, and
// the lease records it as story_tip (SPEC §3.4, §7.5):
//   - a first attempt, with no head, starts from the tip;
//   - a head that passes rule 2 against the story's baseline, and merges
//     cleanly with the tip, is the start, with no target, even once main
//     has changed a protected path since the story's baseline, as rule 2
//     judges the head against the story's baseline, not main's SHA;
//   - a head whose own diff touches a protected path isn't named: the run
//     starts from the tip, the log says why, naming the story branch, and
//     the preamble says the head was dropped, so the checkout starts from
//     the story branch, with rule 2's finding;
//   - a head that conflicts with the tip is the start, with the tip named
//     as its target: the clone holds it, the lease records it, the
//     preamble names the story branch at the tip, and the log names the
//     conflicting file.
//
// Before the tip is checked, the task gets no run. The run's commit is
// then the task's head, on its start, and the task moves to verifying.
func TestStoryRunStart(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	for _, tt := range []struct {
		name string
		// head is the task's earlier head's files, off the story's tip, or
		// with fromMain, off main, before the tip; nil for no head.
		head     map[string]string
		fromMain bool
		// main is main's changes after the tip, one commit each, so the
		// story's baseline is older than main's SHA.
		main map[string]string
		// fromHead is whether the run starts from the head, else from the
		// tip; target whether it names the tip as its target.
		fromHead, target bool
		logs             []string // what the start logs, at info; nil for nothing of rule 2 or a merge
		// dropped is what system.md says of the dropped head; nil when the
		// head isn't dropped.
		dropped []string
	}{
		{name: "a first attempt"},
		{name: "a retry from the head", head: map[string]string{"quote.txt": "expired\n"}, fromHead: true},
		{name: "a first attempt after main changed a protected path", main: map[string]string{"CLAUDE.md": "main's rules\n"}},
		{name: "a retry from the head after main changed a protected path", head: map[string]string{"quote.txt": "expired\n"},
			main: map[string]string{"CLAUDE.md": "main's rules\n"}, fromHead: true},
		{name: "a head failing rule 2 against the story's baseline", head: map[string]string{"CLAUDE.md": "New rules.\n", "quote.txt": "expired\n"},
			logs:    []string{"fails rule 2, so the run starts from " + tipBranch + "'s", "The task's diff touches protected paths", "CLAUDE.md"},
			dropped: []string{"```text\nThe task's diff touches protected paths", "- CLAUDE.md\n```"}},
		{name: "a conflict with the tip", head: map[string]string{"f.txt": "the head's\n"}, fromMain: true, fromHead: true, target: true,
			logs: []string{"doesn't merge cleanly with " + tipBranch + "'s", "with " + tipBranch + " named as its target", `["f.txt"]`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, v := newStoryRunRig(t)
			// main isn't merged into the story branch, so when main moves,
			// the story's baseline stays older than it, as while that merge
			// waits (SPEC §7.5).
			v.d.StoryMain = false
			main := v.mainSHA()
			v.tipCut(tipBranch)
			tip := v.tipPush(tipBranch, map[string]string{"f.txt": "the story's\n"})
			head := ""
			if tt.head != nil {
				base := tip
				if tt.fromMain {
					base = main
				}
				head = v.storyRunHead(base, tt.head)
			}
			for _, path := range slices.Sorted(maps.Keys(tt.main)) {
				v.o.commit(path, tt.main[path])
			}
			v.storyRunChecked(tip, "success")
			if pickStage(f, 2) != "stage:in-progress" || v.runs() != 0 {
				t.Fatalf("before the tip's check: the task at %s, with %d runs; want it at in-progress, with none", pickStage(f, 2), v.runs())
			}

			stop := logtest.Capture(t, logr.I)
			v.pass()
			logs := stop()
			if v.runs() != 1 {
				t.Fatalf("claude ran %d times once the tip is checked, want once", v.runs())
			}
			wantStart, wantTarget := tip, ""
			if tt.fromHead {
				wantStart = head
			}
			if tt.target {
				wantTarget = tip
			}
			if l := v.leaseOf("1"); l.Start != wantStart || l.Target != wantTarget || l.StoryTip != tip || l.Attempt != 1 {
				t.Errorf("the run's lease names start %q, target %q, story tip %q and attempt %d; want start %q, target %q, story tip %q and attempt 1 (main %s, head %s)",
					l.Start, l.Target, l.StoryTip, l.Attempt, wantStart, wantTarget, tip, main, head)
			}
			if raw := v.file("lease.1"); !strings.Contains(raw, `"story_tip":"`+tip+`"`) {
				t.Errorf("the run's lease is %s; want it to record story_tip %s", raw, tip)
			}
			// The clone's HEAD is the lease's start, and its only ref the
			// lease's target.
			if got := v.trimmed("head.1"); got != wantStart {
				t.Errorf("the run's clone was at %s, want the start %s", got, wantStart)
			}
			wantRefs := ""
			if tt.target {
				wantRefs = tip + " " + workspace.TargetRef
			}
			if got := v.trimmed("refs.1"); got != wantRefs {
				t.Errorf("the run's clone had refs %q; want %q", got, wantRefs)
			}
			system := v.file("ctx.1/system.md")
			if got := strings.Contains(system, storyRunTarget(tip)); got != tt.target || !tt.target && strings.Contains(system, "`target`") {
				t.Errorf("system.md =\n%s\nwant the story branch named as its target: %t", system, tt.target)
			}
			if got := strings.Contains(system, storyRunDropped); got != (tt.dropped != nil) {
				t.Errorf("system.md =\n%s\nwant it to say the head was dropped: %t", system, tt.dropped != nil)
			}
			for _, want := range tt.dropped {
				if !strings.Contains(system, want) {
					t.Errorf("system.md =\n%s\nwant it to say %q", system, want)
				}
			}
			for _, want := range tt.logs {
				if !strings.Contains(logs, want) {
					t.Errorf("the pass logged\n%s\nwant it to say %q", logs, want)
				}
			}
			if tt.logs == nil && (strings.Contains(logs, "rule 2") || strings.Contains(logs, "merge cleanly")) {
				t.Errorf("the pass logged\n%s\nwant nothing of rule 2 or a merge", logs)
			}
			commit := v.trimmed("commit.1")
			if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"2"); got != commit {
				t.Errorf("task 2's head is %s after the run, want the run's commit %s", got, commit)
			}
			if parent := gitIn(t, v.d.Clone.Dir(), "rev-parse", commit+"^"); parent != wantStart {
				t.Errorf("the run's commit %s has parent %s, want the start %s", commit, parent, wantStart)
			}
			if got := pickStage(f, 2); got != "stage:verifying" {
				t.Errorf("the task is at %s, want verifying", got)
			}
		})
	}
}

// While its story branch's tip has no issuedog/tip as success, a task in
// the story gets no run, uncounted: no lease, no state error, and it stays
// at in-progress (SPEC §3.4's A story's waits). With no branch, as the
// task has been in-progress, the story gets its branches state error,
// keyed none, once (SPEC §5.3, §7.5), and the task's wait on it is logged
// once. With a tip not checked yet, and a tip whose check is failure, as
// one that changes a protected path, the log says why once, at info, not
// on every pass, and a pass after makes no request but 304s. Once a
// person's push fixes the tip, and its check is success, the next pass's
// run starts from it, as attempt 1.
func TestStoryRunTipWaits(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	f, v := newStoryRunRig(t)
	stop := logtest.Capture(t, logr.I)
	v.passes(2)
	logs := stop()
	if want := "#2: no implementer run: its story #1's branches state error stands, keyed none"; strings.Count(logs, want) != 1 {
		t.Errorf("two passes with no branch logged\n%s\nwant %q once", logs, want)
	}
	if got, want := errorMarkers(f, 1), []string{"<!-- issuedog:error kind=branches key=none -->"}; !slices.Equal(got, want) {
		t.Errorf("the story's state errors are %q; want %q", got, want)
	}
	v.pass() // the digest and the assignment the error brings, read back
	v.quietPass("no branch")

	v.tipCut(tipBranch)
	bad := v.tipPush(tipBranch, map[string]string{"CLAUDE.md": "The story's own rules.\n"})
	stop = logtest.Capture(t, logr.I)
	v.storyRunChecked(bad, "failure")
	v.passes(2)
	logs = stop()
	for _, want := range []string{
		"#2: no implementer run: its story branch " + tipBranch + "'s tip " + bad + " has no " + github.TipCheck + " as success yet",
		"#2: no implementer run: its story branch " + tipBranch + "'s tip " + bad + " has " + github.TipCheck + " as failure",
	} {
		if strings.Count(logs, want) != 1 {
			t.Errorf("three passes on a tip not checked, then failing, logged\n%s\nwant %q once", logs, want)
		}
	}
	v.quietPass("a failing tip")
	if v.runs() != 0 || pickStage(f, 2) != "stage:in-progress" {
		t.Fatalf("while the tip fails: %d runs, the task at %s; want none, and in-progress", v.runs(), pickStage(f, 2))
	}
	if l := v.lease(); l.RunID != "" || l.Attempt != 0 {
		t.Errorf("the lease is %+v; want none, and no attempt counted", l)
	}
	if errs := errorMarkers(f, 2); len(errs) != 0 {
		t.Errorf("the task's state errors are %q; want none", errs)
	}

	fixed := v.tipPush(tipBranch, map[string]string{"CLAUDE.md": ""})
	v.storyRunChecked(fixed, "success")
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("claude ran %d times once the tip passes, want once", v.runs())
	}
	if l := v.leaseOf("1"); l.Start != fixed || l.StoryTip != fixed || l.Target != "" || l.Attempt != 1 {
		t.Errorf("the run's lease = %+v; want start and story tip %s, no target, attempt 1", l, fixed)
	}
}

// storyRunApplyFails runs a pass of v whose run on task 2 commits, but
// whose result fails to apply, its first write refused, as by GitHub
// failing, so the lease and the fetched commits stay for a later pass,
// and returns the run's commit.
func (v *verdictRig) storyRunApplyFails() string {
	v.t.Helper()
	v.f.FailWrite(len(v.f.Writes()) + 1)
	writes := len(v.f.Writes())
	if err := v.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "on #2 failed, 1 of 3 times") {
		v.t.Fatalf("Iterate = %v; want the run's apply failed once", err)
	}
	if v.runs() != 1 || pickStage(v.f, 2) != "stage:in-progress" {
		v.t.Fatalf("after the failed apply: %d runs, the task at %s; want one, and in-progress", v.runs(), pickStage(v.f, 2))
	}
	if w := v.f.Writes()[writes:]; len(w) != 1 || !w[0].Failed || !strings.HasPrefix(w[0].Path, "/issues/2/") {
		v.t.Fatalf("the pass wrote %+v; want only the apply's first write to task 2, refused", w)
	}
	return v.trimmed("commit.1")
}

// A run's result is dropped, uncounted, when its story branch's tip moved
// under it to one that fails rule 2 (SPEC §8.2's The named target moved):
// the run named the checked tip, and its apply failed, so the lease waits
// while a person pushes a tip that changes a protected path, which the
// next pass's sync shows. That pass drops the result, writing nothing, and
// moves no task head, and the task waits at in-progress, as its tip check
// posts failure. A fresh daemon reads that check, and starts no run while
// it stands, and its pass after makes no request but 304s. Once a push
// fixes the tip, and its check is success, the next run starts from it, as
// attempt 1, with no head.
func TestStoryRunTipMovedFails(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	f, v := newStoryRunRig(t)
	tip := v.tipCut(tipBranch)
	v.storyRunChecked(tip, "success")
	v.storyRunApplyFails()
	l := v.lease()
	if l.StoryTip != tip || l.Start != tip {
		t.Fatalf("the kept lease = %+v; want start and story tip %s", l, tip)
	}

	bad := v.tipPush(tipBranch, map[string]string{".github/workflows/x.yml": "on: push\n"})
	writes := len(f.Writes())
	stop := logtest.Capture(t, logr.I)
	v.pass()
	logs := stop()
	if want := "dropping the result of run " + l.RunID + ", uncounted: the story branch's tip moved to " + bad + ", which fails rule 2"; !strings.Contains(logs, want) ||
		!strings.Contains(logs, ".github/workflows/x.yml") {
		t.Errorf("the pass logged\n%s\nwant %q, naming the protected path", logs, want)
	}
	// The tip fails rule 2, so its human task is filed under the story
	// before its check (SPEC §7.5): the issue, its link, its first move.
	if got, want := writesOf(f)[writes:], []string{"POST /issues", "POST /issues/1/sub_issues", "POST /issues/4/comments", "POST /check-runs"}; !slices.Equal(got, want) {
		t.Errorf("the pass wrote %q; want only the human task's filing and the new tip's check, %q", got, want)
	}
	if got := tipLast(t, f); got.HeadSHA != bad || got.Conclusion != "failure" {
		t.Errorf("the last check is %+v; want failure on the new tip %s", got, bad)
	}
	if pickStage(f, 2) != "stage:in-progress" || v.runs() != 1 {
		t.Errorf("after the drop: the task at %s, with %d runs; want in-progress, and still one", pickStage(f, 2), v.runs())
	}
	if refs := v.bareRefs(); strings.Contains(refs, repo.TasksRef+"2") {
		t.Errorf("task 2 has a head after the drop; want none: a dropped result moves nothing")
	}
	if l := v.lease(); l.RunID != "" || l.Attempt != 0 {
		t.Errorf("the lease is %+v; want none, and no attempt counted", l)
	}

	v = v.restart()
	from := len(f.Requests())
	v.pass()
	if got := tipReads(f, from, bad); got != 1 || v.runs() != 1 {
		t.Errorf("the restart's first pass read the tip's checks %d times, with %d runs; want once, and still one", got, v.runs())
	}
	v.pass() // reads what the restart's first pass wrote for the human task, as the digest's
	v.quietPass("after the restart")

	fixed := v.tipPush(tipBranch, map[string]string{".github/workflows/x.yml": ""})
	v.storyRunChecked(fixed, "success")
	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times once the tip passes, want twice", v.runs())
	}
	if l := v.leaseOf("2"); l.Start != fixed || l.StoryTip != fixed || l.Target != "" || l.Attempt != 1 {
		t.Errorf("the next run's lease = %+v; want start and story tip %s, no target, attempt 1", l, fixed)
	}
	if got := pickStage(f, 2); got != "stage:verifying" {
		t.Errorf("the task is at %s, want verifying", got)
	}
}

// A run's result is kept when its story branch's tip moved under it to
// one that passes rule 2 against its own baseline, as a sibling's change
// does, though main has changed a protected path since: the next pass's
// apply hands off the run's commit, whose parent is the tip the run
// named, and moves the task to verifying.
func TestStoryRunTipMovedPasses(t *testing.T) {
	t.Parallel()
	f, v := newStoryRunRig(t)
	tip := v.tipCut(tipBranch)
	v.storyRunChecked(tip, "success")
	commit := v.storyRunApplyFails()

	moved := v.tipPush(tipBranch, map[string]string{"sibling.txt": "a sibling's change\n"})
	v.o.commit("CLAUDE.md", "main's rules\n")
	v.pass()
	if got := pickStage(f, 2); got != "stage:verifying" {
		t.Errorf("the task is at %s after the tip moved to %s, which passes rule 2; want verifying", got, moved)
	}
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"2"); got != commit {
		t.Errorf("task 2's head is %s, want the run's commit %s", got, commit)
	}
	if parent := gitIn(t, v.d.Clone.Dir(), "rev-parse", commit+"^"); parent != tip {
		t.Errorf("the run's commit has parent %s, want the tip it named, %s", parent, tip)
	}
	if v.runs() != 1 {
		t.Errorf("claude ran %d times, want once", v.runs())
	}
}

// A task in a story is judged again on the issue read again before its run
// (SPEC §3.4's A story's waits): when that read shows it moved under
// another parent, an epic with no plan, which has no story branch, no run
// starts, uncounted, on the tip step 1's snapshot named, and none starts
// on the passes after, which read the new parent.
func TestStoryRunParentChanged(t *testing.T) {
	t.Parallel()
	f, v := newStoryRunRig(t)
	epic := fileAt(f, "planned", "An epic with no plan.", "type:epic", "priority:p1")
	tip := v.tipCut(tipBranch)
	v.storyRunChecked(tip, "success")
	var moved atomic.Bool
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/2" }, func() {
		f.SetParent(2, epic)
		moved.Store(true)
	})
	v.passes(3)
	if !moved.Load() {
		t.Fatal("no pass read task 2 again before a run")
	}
	if v.runs() != 0 || pickStage(f, 2) != "stage:in-progress" {
		t.Errorf("with the task moved under #%d: %d runs, the task at %s; want none, and in-progress", epic, v.runs(), pickStage(f, 2))
	}
	if l := v.lease(); l.RunID != "" || l.Attempt != 0 {
		t.Errorf("the lease is %+v; want none, and no attempt counted", l)
	}
}

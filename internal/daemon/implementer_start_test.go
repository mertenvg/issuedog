package daemon_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the start an implementer run on a task with no parent gets
// from its head (SPEC §3.2, §3.4): the head only once it passes rule 2 of
// SPEC §10.1, with main's commit as its baseline, else main's commit, and
// main's commit named as its target when the head doesn't merge cleanly
// with it. Each case is a whole run against the fakes, on real temporary
// repos, as TestImplementerWholeRun is.

// implementerStartTarget is the preamble's line naming main at sha as the
// run's target (SPEC §12).
func implementerStartTarget(sha string) string {
	return "- Your checkout's `target` branch is `main` at `" + sha + "`, which your starting commit doesn't merge with cleanly. " +
		"Merge `target` first, resolve the conflicts, and commit the merge."
}

// implementerStartDropped is the preamble's line saying the task's head
// was dropped for failing rule 2, before rule 2's finding (SPEC §3.4, §12).
const implementerStartDropped = "- The head your earlier runs left on this task fails verify's rule 2, as the finding below says, so issuedog dropped it: " +
	"your checkout starts from `main`, as a first run's does, and holds none of their work. Do the task again from here. " +
	"Rule 2's finding on the dropped head:\n\n```text\n"

// startHead commits files on a branch, task, off main's tip in o's work
// tree, or with orphan, on one that shares no history with main, holding
// main's files and files, then goes back to main and returns the commit.
// The origin never gets the branch: fetchStartHead takes it into the
// daemon's clone.
func startHead(t *testing.T, o *origin, files map[string]string, orphan bool) string {
	t.Helper()
	if orphan {
		gitIn(t, o.work, "checkout", "--quiet", "--orphan", "task", "main")
	} else {
		gitIn(t, o.work, "checkout", "--quiet", "-b", "task", "main")
	}
	for path, content := range files {
		full := filepath.Join(o.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, o.work, "add", path)
	}
	gitIn(t, o.work, "commit", "--quiet", "-m", "the task's earlier run")
	head := gitIn(t, o.work, "rev-parse", "HEAD")
	gitIn(t, o.work, "checkout", "--quiet", "main")
	return head
}

// fetchStartHead makes head task 1's head in g's daemon clone, as an
// earlier run's handoff left it (SPEC §3.2).
func fetchStartHead(t *testing.T, g *rig, o *origin, head string) {
	t.Helper()
	gitIn(t, g.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", o.work, "refs/heads/task:"+repo.TasksRef+"1")
	if got := gitIn(t, g.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != head {
		t.Fatalf("task 1's head in the daemon's clone is %s, want %s", got, head)
	}
}

// newStartRig is a rig wired as main's, on a real origin whose main holds
// the implementer's config and prompt, a CODEOWNERS trusting
// wholeRunOwner, and f.txt, and a fake GitHub where wholeRunOwner is an
// admin. Its fake claude's first run commits, and returns commits.
func newStartRig(t *testing.T) (*githubtest.Fake, *origin, *rig) {
	t.Helper()
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+wholeRunOwner+"\n")
	o.commit(config.Path, implementerRunConfig)
	o.commit(agent.PromptDir+"/implementer.md", implementerRunPrompt)
	o.commit("f.txt", "the base's\n")
	g := newRig(t, f, o, t.TempDir())
	cfg, err := config.Parse([]byte(implementerRunConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(implementerRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	if err := os.WriteFile(g.path("claude", "out.1"), []byte(ivResult(t, implementerOutput(t, "commits"))), 0o644); err != nil {
		t.Fatal(err)
	}
	return f, o, g
}

// requestLines are reqs as "<method> <path>?<query>", one each.
func requestLines(reqs []githubtest.Request) []string {
	lines := make([]string, len(reqs))
	for i, r := range reqs {
		lines[i] = r.Method + " " + r.Path + "?" + r.Query
	}
	return lines
}

// An implementer run on a task with no parent starts from the task's head
// only when the head passes rule 2, with main's commit as its baseline,
// and names main's commit as its target only when the head doesn't merge
// cleanly with it (SPEC §3.4):
//   - a head whose own diff touched a protected path, one behind a
//     protected path main changed since, and one with no history in common
//     with main, so with no merge base, aren't named: the run starts from
//     main's commit, as a first attempt does, with no target, and the log
//     says why, naming the path, as the preamble does, which says the head
//     was dropped, with rule 2's finding. The run's commit then replaces
//     the head;
//   - a head that conflicts with main is the start, with main named as the
//     target: the clone holds it at refs/heads/target, the lease records
//     it, the preamble says to merge it first, and the log names the
//     conflicting file;
//   - a clean head, and a task with no head, name no target.
//
// Naming the start reads only the bare repo, so every case's pass makes
// the requests the task with no head makes, in the same order.
func TestImplementerStart(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	requests := map[string][]string{} // each case's pass's, by its name
	for _, tt := range []struct {
		name   string
		head   map[string]string // the head's own change, off main; nil for no head
		orphan bool              // whether the head shares no history with main
		main   map[string]string // main's changes after the head, one commit each
		// fromHead is whether the run starts from the head, else from main;
		// target whether it names main as its target.
		fromHead, target bool
		logs             []string // what the start logs, at info; nil for nothing
		// dropped is what system.md says of the dropped head; nil when the
		// head isn't dropped.
		dropped []string
	}{
		{name: "a head that touched a protected path", head: map[string]string{".github/x": "x\n", "a.txt": "a\n"},
			logs:    []string{"fails rule 2, so the run starts from main's", "The task's diff touches protected paths", ".github/x"},
			dropped: []string{"```text\nThe task's diff touches protected paths", "- .github/x\n```"}},
		{name: "a head behind a protected path main changed since", head: map[string]string{"a.txt": "a\n"},
			main:    map[string]string{"CLAUDE.md": "New rules.\n"},
			logs:    []string{"fails rule 2, so the run starts from main's", "Protected paths differ from the baseline", "CLAUDE.md"},
			dropped: []string{"```text\nProtected paths differ from the baseline", "- CLAUDE.md\n```"}},
		{name: "a head with no history in common with main", head: map[string]string{"a.txt": "a\n"}, orphan: true,
			logs:    []string{"fails rule 2, so the run starts from main's", "not exactly one merge base"},
			dropped: []string{"```text\nThe head doesn't have exactly one merge base with main's", "not exactly one merge base"}},
		{name: "a conflicting head", head: map[string]string{"f.txt": "the head's\n"}, main: map[string]string{"f.txt": "main's\n"},
			fromHead: true, target: true, logs: []string{"doesn't merge cleanly with main's", `["f.txt"]`}},
		{name: "a clean head", head: map[string]string{"a.txt": "a\n"}, main: map[string]string{"b.txt": "b\n"}, fromHead: true},
		{name: "no head", main: map[string]string{"b.txt": "b\n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, o, g := newStartRig(t)
			head := ""
			if tt.head != nil {
				head = startHead(t, o, tt.head, tt.orphan)
			}
			for _, path := range slices.Sorted(maps.Keys(tt.main)) {
				o.commit(path, tt.main[path])
			}
			main := gitIn(t, o.work, "rev-parse", "main")
			if head != "" {
				fetchStartHead(t, g, o, head)
			}
			n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.",
				"type:task", "priority:p1", "scope:booking")
			f.AddLabel(n, wholeRunOwner, "stage:in-progress")

			from := len(f.Requests())
			stop := logtest.Capture(t, logr.I)
			g.pass()
			logs := stop()
			if g.runs() != 1 {
				t.Fatalf("claude ran %d times, want once", g.runs())
			}
			w := &wholeRun{rig: g, n: n}
			files := w.context(1, state.InProgress) // the clone's HEAD is the lease's start, its only ref the lease's target
			wantStart, wantTarget := main, ""
			if tt.fromHead {
				wantStart = head
			}
			if tt.target {
				wantTarget = main
			}
			if l := g.leaseOf("1"); l.Start != wantStart || l.Target != wantTarget {
				t.Errorf("the run's lease names start %q and target %q; want start %q and target %q (main %s, head %s)",
					l.Start, l.Target, wantStart, wantTarget, main, head)
			}
			system := files["system.md"]
			if got := strings.Contains(system, implementerStartTarget(main)); got != tt.target || !tt.target && strings.Contains(system, "`target`") {
				t.Errorf("system.md =\n%s\nwant main named as its target: %t", system, tt.target)
			}
			if got := strings.Contains(system, implementerStartDropped); got != (tt.dropped != nil) {
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
				t.Errorf("the pass logged\n%s\nwant nothing of rule 2 or a merge with main", logs)
			}
			// The run's commit is the task's head now, on the start, so a
			// head that wasn't named is dropped.
			commit := g.trimmed("commit.1")
			if got := gitIn(t, g.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != commit {
				t.Errorf("task 1's head is %s after the run, want the run's commit %s", got, commit)
			}
			if parent := gitIn(t, g.d.Clone.Dir(), "rev-parse", commit+"^"); parent != wantStart {
				t.Errorf("the run's commit %s has parent %s, want the start %s", commit, parent, wantStart)
			}
			if labels := ivLabels(f, n); !slices.Contains(labels, "stage:verifying") {
				t.Errorf("the task's labels are %v, want stage:verifying", labels)
			}
			requests[tt.name] = requestLines(f.Requests()[from:])
		})
	}
	// Naming the start costs no request: every pass made the requests the
	// pass with no head made.
	want := requests["no head"]
	if len(want) == 0 {
		t.Fatal("the pass with no head made no request")
	}
	for name, got := range requests {
		if !slices.Equal(got, want) {
			t.Errorf("%s: the pass made the requests\n%s\nwant those of the pass with no head\n%s",
				name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// dropObject removes the loose object sha from g's daemon clone, as a
// broken bare repo might lack it, so any read of it fails.
func dropObject(t *testing.T, g *rig, sha string) {
	t.Helper()
	path := filepath.Join(g.d.Clone.Dir(), "objects", sha[:2], sha[2:])
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the loose object %s: %v", sha, err)
	}
}

// A read that fails while naming an implementer run's start, other than
// BaseOf's finding no single merge base, which fails rule 2, is a setup
// error (SPEC §3.4): reading the head's base, checking it against rule 2,
// or merging it with main. The pass returns it, naming the read; no run
// starts, and none is counted, so with the head gone the next pass's run
// is attempt 1; no state error is posted, and no ref moves. Each
// case breaks one read of a whole pass, on real temporary repos: a task
// whose ref names a tree, not a commit, so merge-base fails on it; a head
// whose tree is missing, so rule 2's diff fails; and a head whose own
// f.txt is missing while main changed f.txt too, so rule 2, which reads
// only trees, passes, but the merge with main fails.
func TestImplementerStartReadFails(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// head makes task 1's head in g's daemon clone, with one read of it
		// broken.
		head func(t *testing.T, g *rig, o *origin)
		err  string // the read that fails, as the pass's error names it
	}{
		{name: "a task ref that names a tree", head: func(t *testing.T, g *rig, o *origin) {
			gitIn(t, g.d.Clone.Dir(), "update-ref", repo.TasksRef+"1", gitIn(t, g.d.Clone.Dir(), "rev-parse", "main^{tree}"))
		}, err: "read the head's base"},
		{name: "a head whose tree is missing", head: func(t *testing.T, g *rig, o *origin) {
			head := startHead(t, o, map[string]string{"a.txt": "the task's own a\n"}, false)
			fetchStartHead(t, g, o, head)
			dropObject(t, g, gitIn(t, o.work, "rev-parse", head+"^{tree}"))
		}, err: "check rule 2"},
		{name: "a head whose changed file is missing", head: func(t *testing.T, g *rig, o *origin) {
			head := startHead(t, o, map[string]string{"f.txt": "the task's own f\n"}, false)
			o.commit("f.txt", "main's f\n")
			if err := g.d.Clone.Sync(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			fetchStartHead(t, g, o, head)
			dropObject(t, g, gitIn(t, o.work, "rev-parse", head+":f.txt"))
		}, err: "merge with main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, o, g := newStartRig(t)
			tt.head(t, g, o)
			n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.",
				"type:task", "priority:p1", "scope:booking")
			f.AddLabel(n, wholeRunOwner, "stage:in-progress")
			refs := g.bareRefs()

			err := g.d.Iterate(context.Background())
			if err == nil || !strings.Contains(err.Error(), "name the run's start") || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Iterate = %v, want the setup error naming the run's start: %s", err, tt.err)
			}
			if g.runs() != 0 {
				t.Errorf("claude ran %d times, want never", g.runs())
			}
			if l := g.lease(); l.RunID != "" || l.Failed || l.Attempt != 0 {
				t.Errorf("the lease is %+v; want none, and no attempt counted", l)
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v; want no lease, result, bundle or clone", left)
			}
			if errs := errorMarkers(f, n); len(errs) != 0 {
				t.Errorf("the issue's state errors are %q; want none", errs)
			}
			if labels := ivLabels(f, n); !slices.Contains(labels, "stage:in-progress") {
				t.Errorf("the task's labels are %v, want stage:in-progress", labels)
			}
			if got := g.bareRefs(); got != refs {
				t.Errorf("the bare repo's refs are\n%s\nafter the pass, want them as before\n%s", got, refs)
			}

			// With the head gone, the next pass's run starts from main, as
			// attempt 1, so the failed read counted no attempt.
			gitIn(t, g.d.Clone.Dir(), "update-ref", "-d", repo.TasksRef+"1")
			g.pass()
			if g.runs() != 1 {
				t.Fatalf("claude ran %d times once the head is gone, want once", g.runs())
			}
			main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
			if l := g.leaseOf("1"); l.Attempt != 1 || l.Start != main || l.Target != "" {
				t.Errorf("the run's lease names attempt %d, start %q and target %q; want attempt 1 from main's %s, with no target",
					l.Attempt, l.Start, l.Target, main)
			}
		})
	}
}

package daemon_test

import (
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
//     says why, naming the path. The run's commit then replaces the head;
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
	}{
		{name: "a head that touched a protected path", head: map[string]string{".github/x": "x\n", "a.txt": "a\n"},
			logs: []string{"fails rule 2, so the run starts from main's", "The task's diff touches protected paths", ".github/x"}},
		{name: "a head behind a protected path main changed since", head: map[string]string{"a.txt": "a\n"},
			main: map[string]string{"CLAUDE.md": "New rules.\n"},
			logs: []string{"fails rule 2, so the run starts from main's", "Protected paths differ from the baseline", "CLAUDE.md"}},
		{name: "a head with no history in common with main", head: map[string]string{"a.txt": "a\n"}, orphan: true,
			logs: []string{"fails rule 2, so the run starts from main's", "not exactly one merge base"}},
		{name: "a conflicting head", head: map[string]string{"f.txt": "the head's\n"}, main: map[string]string{"f.txt": "main's\n"},
			fromHead: true, target: true, logs: []string{"doesn't merge cleanly with main's", `["f.txt"]`}},
		{name: "a clean head", head: map[string]string{"a.txt": "a\n"}, main: map[string]string{"b.txt": "b\n"}, fromHead: true},
		{name: "no head", main: map[string]string{"b.txt": "b\n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
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

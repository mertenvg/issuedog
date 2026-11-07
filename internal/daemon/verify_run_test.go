package daemon_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/verify"
)

// verifyRunConfig is implementerRunConfig with rule 5's globs: a .txt
// file is source, and a test is a *_test.go file, so the implementer's
// run.<n>.txt, with no test beside it, fails rule 5.
const verifyRunConfig = "types: labels\nnotify: [" + wholeRunOwner + "]\nscopes: [booking]\n" +
	"pr: {title_types: [feat, fix, docs]}\n" +
	"verify: {commands: [\"true\"], source_globs: [\"**/*.txt\"], test_globs: [\"**/*_test.go\"], protected_paths: [secrets/**]}\n"

// A whole run against the fakes, with the daemon built as startDaemon
// builds it: the implementer commits a change with no test, and moves the
// task to verifying. The next pass runs the checks on its head, which
// fail rule 5, so the task moves back to in-progress on findings, keyed
// by the comment holding its move to verifying, naming the head. The pass
// after runs the implementer again, from that head (SPEC §3.2, §10.1).
func TestVerifyWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+wholeRunOwner+"\n")
	o.commit(config.Path, verifyRunConfig)
	o.commit(agent.PromptDir+"/implementer.md", implementerRunPrompt)
	g := newRig(t, f, o, t.TempDir())
	cfg, err := config.Parse([]byte(verifyRunConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(implementerRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	g.r.Commands = &verify.Commands{Workspace: g.r.Workspace, Proxy: "http://127.0.0.1:3128", Timeout: func() time.Duration { return time.Minute }}
	for _, k := range []string{"1", "2"} {
		if err := os.WriteFile(g.path("claude", "out."+k), []byte(ivResult(t, implementerOutput(t, "commits"))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.",
		"type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	w := &wholeRun{rig: g, n: n}

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	head := g.trimmed("commit.1")
	moved := w.comments()
	move := moved[len(moved)-1]
	if ms := github.ParseMarkers(move.Body); len(ms) != 1 || ms[0].Fields["to"] != "verifying" || ms[0].Fields["head"] != head {
		t.Fatalf("the task's newest comment =\n%s\nwant the move to verifying, naming head %s", move.Body, head)
	}

	g.pass()
	if g.runs() != 1 {
		t.Errorf("claude ran %d times after the checks, want once: the checks run no claude", g.runs())
	}
	cs := w.comments()
	last := cs[len(cs)-1]
	wantMarker := findingsAttemptMarker(move.ID, head, 1, move.ID)
	if last.Author.Login != app ||
		!strings.HasPrefix(last.Body, "Verify failed on attempt 1 of 3, at `"+head+"`, checked against `main` at `"+main+"`.\n\n### Rule 5: tests alongside source\n\n") ||
		!strings.Contains(last.Body, "- run.1.txt\n") || !strings.HasSuffix(last.Body, wantMarker) {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's findings on head %s, rule 5 naming run.1.txt, ending with %s",
			last.Author.Login, last.Body, head, wantMarker)
	}
	if labels := ivLabels(f, n); !slices.Contains(labels, "stage:in-progress") || slices.Contains(labels, "stage:verifying") {
		t.Errorf("the task's labels are %v after the findings, want stage:in-progress in place of stage:verifying", labels)
	}

	g.pass()
	if g.runs() != 2 {
		t.Fatalf("claude ran %d times after the findings move, want twice", g.runs())
	}
	if l := g.leaseOf("2"); l.Start != head || l.Target != "" {
		t.Errorf("the next run's lease names start %q and target %q; want the task's head %s, and none", l.Start, l.Target, head)
	}
	if got := gitIn(t, g.d.Clone.Dir(), "rev-parse", g.trimmed("commit.2")+"^"); got != head {
		t.Errorf("the next run's commit has parent %s, want the task's head %s", got, head)
	}
	if l := g.leaseOf("2"); l.RunID == "run2" {
		t.Errorf("the next run's ID is %s, which the checks' checkout took; want a new one", l.RunID)
	}
}

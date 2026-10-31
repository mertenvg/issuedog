package daemon_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is a task's round of attempts (SPEC §8.3): the implementer's
// failed runs and its failed verifies count in one round, which the verify
// loop's own moves don't end, each retry gets the latest findings at one
// effort level higher, and the round's count lives on attempt= on those
// moves, so a restart reads it back.

// roundConfig is .issuedog/config.yaml on main for a task's round: the
// implementer starts at effort low, raised up to max_effort, over
// max_attempts attempts, and verify's one command always fails, with no
// output, so every head gets findings. The fake claude's run.<n>.txt
// passes every other rule.
func roundConfig(maxAttempts int, maxEffort string) string {
	return fmt.Sprintf("types: labels\nnotify: [owner]\nloop:\n  max_attempts: %d\n"+
		"stages:\n  effort: {implementer: low}\n  max_effort: %s\nverify:\n  commands: [\"false\"]\n", maxAttempts, maxEffort)
}

// roundRig is restartRig on a new origin with config cfg, whose fake
// claude copies each run's context files, commits in its clone, and
// prints out.<n> for run n, each of outs in turn, with the checks' commands
// run as the daemon, as tests can't switch to agent.
func roundRig(t *testing.T, f *githubtest.Fake, cfg string, outs ...string) *rig {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, cfg)
	g := restartRig(t, f, o, t.TempDir(), nil, implementerOutput(t, "commits"))
	g.r.Commands = &verify.Commands{Workspace: g.r.Workspace, Proxy: "http://127.0.0.1:3128", Timeout: func() time.Duration { return time.Minute }}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(implementerRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, out := range outs {
		if err := os.WriteFile(g.path("claude", "out."+strconv.Itoa(i+1)), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// roundEffort is the --effort claude run k got, or "" for none.
func (g *rig) roundEffort(k int) string {
	g.t.Helper()
	return g.flags(k)["--effort"]
}

// roundInput is input.md as claude run k had it.
func (g *rig) roundInput(k int) string {
	g.t.Helper()
	return g.file(filepath.Join("ctx."+strconv.Itoa(k), "input.md"))
}

// roundFindingsInput is input.md's start for findings of attempt, the
// text of the comment that holds them, markers left out, fenced as a
// findings comment's fences need.
func roundFindingsInput(attempt int, findings string) string {
	return "# Input for this run\n\n" +
		"What's new since the implementer last ran, or since the task came to in-progress. " +
		"Verify's findings on the task's head, if any, come first: fix every one.\n\n" +
		"## The findings of verify attempt " + strconv.Itoa(attempt) + ", which this run must fix\n\n" +
		"````text\n" + findings + "\n````"
}

// A round mixing a failed run and findings: run 1 fails, run 2 commits,
// and its move to verifying carries attempt=2; the checks fail its head
// and the move back carries attempt=2 too, its comment naming attempt 2
// of 3. Run 3 is attempt 3, at one effort level higher each time (low,
// medium, high), and its input.md holds the findings verbatim, labelled
// with their attempt. Run 3 fails at the cap, so the task gets the failed
// runs' hand-off at in-progress, saying the round's attempts failed and
// that the last was a run that failed, and why. An unchanged pass after
// makes no request but conditional GETs answered 304. /done starts a new
// round: run 4 is attempt 1 again, at effort low, with no findings.
func TestVerifyRoundMixesRunsAndFindings(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	commits := ivResult(t, implementerOutput(t, "commits"))
	g := roundRig(t, f, roundConfig(3, "high"), failOut, commits, failOut, commits)
	key := restartKey(t, f, n)

	g.pass()
	if l := g.leaseOf("1"); g.runs() != 1 || l.Attempt != 1 || l.AttemptKey != key || pickStage(f, n) != "stage:in-progress" {
		t.Fatalf("after pass 1: %d runs, run 1 leased %+v, the task at %s; want one, attempt 1 of %s, at in-progress", g.runs(), l, pickStage(f, n), key)
	}
	g.pass()
	head := g.trimmed("commit.2")
	cs := implementerComments(t, f, n)
	moved := cs[len(cs)-1]
	if l := g.leaseOf("2"); g.runs() != 2 || l.Attempt != 2 || l.AttemptKey != key ||
		!strings.HasSuffix(moved.Body, fmt.Sprintf("<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=%s effort=medium head=%s seen=%d -->", l.RunID, head, cs[len(cs)-2].ID)) {
		t.Fatalf("after pass 2: %d runs, run 2 leased %+v, the newest comment\n%s\nwant two, attempt 2 of %s, moving to verifying with attempt=2 and head %s",
			g.runs(), l, moved.Body, key, head)
	}

	g.pass()
	cs = implementerComments(t, f, n)
	findings := cs[len(cs)-1]
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	text := "Verify failed on attempt 2 of 3, at `" + head + "`, checked against `main` at `" + main + "`.\n\n" +
		"### Rule 1: verify.commands\n\nThis command failed, exit status 1:\n\n```text\nfalse\n```\n\nIt printed nothing.\n\n" +
		"So the task is back at in-progress, for the implementer to fix."
	if want := text + "\n\n" + findingsAttemptMarker(moved.ID, head, 2, moved.ID); findings.Body != want || pickStage(f, n) != "stage:in-progress" {
		t.Fatalf("after the checks, the newest comment =\n%s\nwith the task at %s; want\n%s\nat in-progress", findings.Body, pickStage(f, n), want)
	}

	g.pass()
	if l := g.leaseOf("3"); g.runs() != 3 || l.Attempt != 3 || l.AttemptKey != key || l.Start != head {
		t.Errorf("after the findings: %d runs, run 3 leased %+v; want three, attempt 3 of %s, from the head %s", g.runs(), l, key, head)
	}
	for k, want := range []string{"low", "medium", "high"} {
		if got := g.roundEffort(k + 1); got != want {
			t.Errorf("run %d's --effort = %q, want %q", k+1, got, want)
		}
	}
	if got, want := g.roundInput(3), roundFindingsInput(2, text)+"\n"; got != want {
		t.Errorf("run 3's input.md =\n%s\nwant\n%s", got, want)
	}
	cs = implementerComments(t, f, n)
	handoff := cs[len(cs)-1]
	if want := "This task's 3 attempts in this round failed, so I've stopped running the implementer here.\n\n" +
		"The last attempt, a run, failed:\n\n"; !strings.HasPrefix(handoff.Body, want) ||
		!strings.HasSuffix(handoff.Body, "<!-- issuedog:transition from=in-progress to=needs-human event=a"+key+" -->") ||
		pickStage(f, n) != "stage:needs-human" {
		t.Errorf("after run 3 failed, the newest comment =\n%s\nwith the task at %s; want the cap's hand-off, starting\n%s\nat needs-human",
			handoff.Body, pickStage(f, n), want)
	}

	g.pass()
	from, writes := len(f.Requests()), len(f.Writes())
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass after the hand-off sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes || g.runs() != 3 {
		t.Errorf("an unchanged pass after the hand-off wrote %+v, with %d runs; want nothing, and 3", w[writes:], g.runs())
	}

	f.AddComment(n, "owner", "/done")
	g.pass()
	l := g.leaseOf("4")
	if g.runs() != 4 || l.Attempt != 1 || l.AttemptKey == key || g.roundEffort(4) != "low" {
		t.Errorf("after /done: %d runs, run 4 leased %+v with effort %q; want four, attempt 1 of a new round, not %s, at low",
			g.runs(), l, g.roundEffort(4), key)
	}
	if got := g.roundInput(4); !strings.HasSuffix(got, "\n\nnone: first run\n") || strings.Contains(got, "findings of verify") {
		t.Errorf("after /done, run 4's input.md =\n%s\nwant no findings, and none: first run", got)
	}
}

// roundTask files task 1 at in-progress, as the pick's move left it, then
// the implementer's move to verifying, with attempt=, of the head head,
// then, if during is set, a comment by owner posted during the checks,
// then the move back on findings, with the move's attempt=, and seen=,
// the move to verifying's comment, as the checks' step 1 read it. It
// returns the findings' text, without the marker.
func roundTask(f *githubtest.Fake, attempt int, head, during string) string {
	n := implementerTask(f)
	moved := f.AddComment(n, app, fmt.Sprintf("The implementer committed.\n\n"+
		"<!-- issuedog:transition from=in-progress to=verifying attempt=%d run=r%d head=%s -->", attempt, attempt, head))
	if during != "" {
		f.AddComment(n, "owner", during)
	}
	text := fmt.Sprintf("Verify failed on attempt %d of 4, at `%s`, checked against `main` at `abc`.\n\n"+
		"### Rule 1: verify.commands\n\nThis command failed, exit status 1:\n\n```text\nfalse\n```\n\nIt printed nothing.\n\n"+
		"So the task is back at in-progress, for the implementer to fix.", attempt, head)
	f.AddComment(n, app, text+"\n\n"+findingsAttemptMarker(moved, head, attempt, moved))
	return text
}

// A restart between attempts reads the round's count back from the
// attempt= of the findings move, with no memory of the runs before it: a
// fresh daemon's run on a task whose findings name attempt 2 is attempt 3,
// at effort high, and its input.md holds the findings. The failed runs
// since come from the lease, as before: with an ended lease of attempt 3,
// the run is attempt 4, at xhigh. A comment posted during the checks,
// above the findings move's seen, or after the findings move, is no input
// at in-progress, where step 1 gives none: it starts no new count, and
// input.md has only the findings, while conversation.md has the comment,
// as it has every trusted comment.
func TestVerifyRoundRestart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		ended   int    // the attempt of an ended lease's failed run, if any
		during  string // owner's comment during the checks, if any
		after   string // owner's comment after the findings move, if any
		attempt int    // the run's attempt
		effort  string // the run's --effort
	}{
		{name: "findings last", attempt: 3, effort: "high"},
		{name: "a failed run since", ended: 3, attempt: 4, effort: "xhigh"},
		{name: "a comment during the checks", during: "Keep the error text short.", attempt: 3, effort: "high"},
		{name: "a comment after the findings", after: "Also log the expiry.", attempt: 3, effort: "high"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			text := roundTask(f, 2, "abc", tt.during)
			if tt.after != "" {
				f.AddComment(1, "owner", tt.after)
			}
			g := roundRig(t, f, roundConfig(4, "xhigh"), ivResult(t, implementerOutput(t, "commits")))
			if tt.ended > 0 {
				restartEnded(t, f, g.root, "ended", tt.ended)
			}
			g.pass()
			if l := g.leaseOf("1"); g.runs() != 1 || l.Attempt != tt.attempt || g.roundEffort(1) != tt.effort {
				t.Errorf("%d runs, the run leased %+v with effort %q; want one, attempt %d, at %s",
					g.runs(), l, g.roundEffort(1), tt.attempt, tt.effort)
			}
			if got, want := g.roundInput(1), roundFindingsInput(2, text)+"\n"; got != want {
				t.Errorf("input.md =\n%s\nwant\n%s", got, want)
			}
			conversation := g.file(filepath.Join("ctx.1", "conversation.md"))
			for _, c := range implementerComments(t, f, 1) {
				want := fmt.Sprintf("## Comment %d by owner", c.ID)
				if c.Author.Login == "owner" && !strings.Contains(conversation, want) {
					t.Errorf("conversation.md =\n%s\nwant it to hold %s", conversation, want)
				}
			}
		})
	}
}

// Findings on the round's last attempt send the task back to in-progress,
// as the checks do on any attempt, and then the round has no attempt
// left: the next pass makes the cap's hand-off in place of a run, saying
// the round's attempts failed, and nothing of why, as no run failed last.
func TestVerifyRoundFindingsAtCap(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	roundTask(f, 4, "abc", "")
	g := roundRig(t, f, roundConfig(4, "xhigh"), ivResult(t, implementerOutput(t, "commits")))
	key := restartKey(t, f, 1)

	g.pass()
	cs := implementerComments(t, f, 1)
	want := "This task's 4 attempts in this round failed, so I've stopped running the implementer here.\n\n**Goal:**"
	if g.runs() != 0 || !strings.HasPrefix(cs[len(cs)-1].Body, want) ||
		!strings.HasSuffix(cs[len(cs)-1].Body, "<!-- issuedog:transition from=in-progress to=needs-human event=a"+key+" -->") {
		t.Errorf("%d runs, the newest comment =\n%s\nwant none, and the cap's hand-off keyed by %s, starting\n%s", g.runs(), cs[len(cs)-1].Body, key, want)
	}
}

// roundVerifying files task 1 at verifying, as the implementer's move by
// run r2 left it, with attempt=2 and head head, and returns the move's
// comment ID.
func roundVerifying(f *githubtest.Fake, head string) int64 {
	n := implementerTask(f)
	f.RemoveLabel(n, app, "stage:in-progress")
	f.AddLabel(n, app, "stage:verifying")
	return f.AddComment(n, app, "The implementer committed.\n\n"+
		"<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head="+head+" -->")
}

// The findings move with attempt=, a crash at each of its writes, then a
// restart: the replay ends with the same writes (SPEC §5.2), and the move
// carries the attempt= of the move into verifying it checked, its comment
// naming it.
func TestVerifyRoundFindingsReplay(t *testing.T) {
	t.Parallel()
	cfg := verifyConfig("false")
	o := verifyOrigin(t, cfg)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	gitIn(t, o.work, "push", "--quiet", o.url, "task")
	parsed, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	moves := map[*githubtest.Fake]int64{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		moves[f] = roundVerifying(f, head)
		return f
	}, func(f *githubtest.Fake) error {
		g := verifyRig(t, f, o)
		g.d.Config = parsed
		return g.work(1)
	})
	for f, move := range moves {
		cs := implementerComments(t, f, 1)
		body := cs[len(cs)-1].Body
		if !strings.HasPrefix(body, "Verify failed on attempt 2 of 3, at `"+head+"`") || !strings.HasSuffix(body, findingsAttemptMarker(move, head, 2, move)) {
			t.Errorf("after the replay, the newest comment =\n%s\nwant the findings on attempt 2 of 3, ending with %s", body, findingsAttemptMarker(move, head, 2, move))
		}
	}
}

// A move back to in-progress that isn't the verify loop's own, as the
// owner's by a stage label, starts a new round, as any move does (SPEC
// §8.3): the run after it is attempt 1, whatever the findings before it
// counted, and its input.md has no findings.
func TestVerifyRoundOwnerMoveStartsRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	roundTask(f, 2, "abc", "")
	f.AddComment(1, app, "owner set `stage:verifying` by hand.\n\n<!-- issuedog:transition from=in-progress to=verifying by=owner event=901 -->")
	f.AddComment(1, app, "owner set `stage:in-progress` by hand.\n\n<!-- issuedog:transition from=verifying to=in-progress by=owner event=902 -->")
	g := roundRig(t, f, roundConfig(4, "xhigh"), ivResult(t, implementerOutput(t, "commits")))

	g.pass()
	if l := g.leaseOf("1"); g.runs() != 1 || l.Attempt != 1 || l.AttemptKey != "in-progress-"+strconv.FormatInt(implementerComments(t, f, 1)[4].ID, 10) {
		t.Errorf("%d runs, the run leased %+v; want one, attempt 1 of the round the owner's move started", g.runs(), l)
	}
	if got := g.roundInput(1); strings.Contains(got, "findings of verify") {
		t.Errorf("input.md =\n%s\nwant no findings, as the owner's move started a new round", got)
	}
}

// The round's count is read from the issue as step 1 read it, so it adds
// no request: the run on a task with findings reads nothing of the issue
// but what a run on a task without them reads.
func TestVerifyRoundCostsNoRequest(t *testing.T) {
	t.Parallel()
	reads := map[bool]int{}
	for _, findings := range []bool{false, true} {
		f := newFake(t)
		if findings {
			roundTask(f, 2, "abc", "")
		} else {
			implementerTask(f)
		}
		g := roundRig(t, f, roundConfig(4, "xhigh"), ivResult(t, implementerOutput(t, "commits")))
		g.pass()
		if g.runs() != 1 {
			t.Fatalf("findings %t: %d runs, want one", findings, g.runs())
		}
		reads[findings] = len(f.Requests())
	}
	if reads[true] != reads[false] {
		t.Errorf("a pass that runs the implementer made %d requests with findings, and %d without; want the same", reads[true], reads[false])
	}
}

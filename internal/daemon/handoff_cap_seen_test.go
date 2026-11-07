package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the caps' hand-offs' seen= and counts (SPEC §5, §8.3,
// §9.3): each cap's move to needs-human records seen=, the newest comment
// when the last run or the checks started, so a comment posted during
// them is above the hand-off's round bound; a /done posted before the
// hand-off's comment doesn't end it; and after a loop.max_attempts
// lowered mid-round, the hand-off counts the round's attempts as they
// were, and says why the last failed only when a run did.

// earlyDoneReply is the App's reply to a /done in comment cmd, posted
// before the hand-off it would finish.
func earlyDoneReply(cmd int64) string {
	return "> /done\n\nI didn't apply this command: it was posted before the hand-off it would finish. " +
		"Once you've done what the hand-off asks, reply `/done` again or react 👍 to it.\n\n" +
		"<!-- issuedog:reply reason=rejected cmd=" + strconv.FormatInt(cmd, 10) + " -->"
}

// earlyDoneTask is newFake with issue 1 a task the owner filed, at
// needs-human after a hand-off posted after owner's /done: the App's move
// before, comment 1001, the /done, 1002, posted during the run or the
// checks that led to the hand-off, then the hand-off, 1003.
func earlyDoneTask(before, handoff string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue("owner", "Refuse an expired quote", "Refuse it.")
		f.SetType(1, "task")
		f.AddLabel(1, app, "stage:needs-human")
		f.AddComment(1, app, before)
		doneBy("owner", 1002)(t, f)
		if id := f.AddComment(1, app, handoff); id != 1003 {
			t.Fatalf("the hand-off's comment ID = %d, want 1003", id)
		}
		return f
	}
}

// A trusted /done posted before the task's latest hand-off comment, as
// during the run or the checks whose failure made it, doesn't end the
// hand-off, for the implementer's own, the attempt cap's and the verify
// cap's alike: the App 👎s it and replies once, saying so, and the task
// stays at needs-human. A replay of that pass makes the same writes. A
// /done posted after the reply ends it.
func TestHandoffDoneBeforeHandoff(t *testing.T) {
	t.Parallel()
	toProgress := "<!-- issuedog:transition from=ready to=in-progress event=p5 -->"
	for _, tt := range []struct {
		name, before, handoff string
	}{
		{"the implementer's hand-off", toProgress,
			handbackComment + "\n\n<!-- issuedog:transition from=in-progress to=needs-human run=run1 seen=1001 -->"},
		{"the attempt cap's hand-off", toProgress, restartCapComment("in-progress-1001", 1001)},
		{"the verify cap's hand-off", "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head=abc -->",
			verifyCapText(2, 2, "abc", "def") + "\n\n" + verifyCapMarker(1001, 1001)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := newOrigin(t)
			setup := earlyDoneTask(tt.before, tt.handoff)
			githubtest.Replay(t, setup, handbackPass(t, o))

			f := setup(t)
			d := handbackDaemon(t, f, o)
			iterate(t, d)
			is := f.State().Issues[0]
			if got := closingComments(f, 1, 3); !slices.Equal(is.Labels, []string{"stage:needs-human"}) || !slices.Equal(got, []string{earlyDoneReply(1002)}) {
				t.Errorf("labels %q, comments after the hand-off\n%q\nwant needs-human, and only\n%q", is.Labels, got, earlyDoneReply(1002))
			}
			if r := is.Comments[1].Reactions; !slices.Equal(r, []string{app + " -1"}) {
				t.Errorf("the early /done's reactions = %q, want the App's 👎", r)
			}

			later := f.AddComment(1, "owner", "/done")
			iterate(t, d)
			is = f.State().Issues[0]
			want := fmt.Sprintf("<!-- issuedog:transition from=needs-human to=ready cmd=%d seen=%d -->", later, later)
			if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"stage:ready"}) || !strings.HasSuffix(last, want) {
				t.Errorf("after a /done posted since, labels %q, newest comment\n%s\nwant ready, on a move ending %s", is.Labels, last, want)
			}
		})
	}
}

// A /done posted before the hand-off, which the App 👎'd but whose reply
// was lost, then edited after the 👎: the reply posted again says the
// edit is ignored, and asks for a new comment, as one posted now, after
// the hand-off, would apply.
func TestHandoffDoneBeforeHandoffLostReply(t *testing.T) {
	t.Parallel()
	f := earlyDoneTask("<!-- issuedog:transition from=ready to=in-progress event=p5 -->",
		handbackComment+"\n\n<!-- issuedog:transition from=in-progress to=needs-human run=run1 seen=1001 -->")(t)
	f.AddReaction(1002, app, "-1")
	f.EditComment(1002, "owner", "/done\n\nThe secret is in.")
	iterate(t, handbackDaemon(t, f, newOrigin(t)))
	want := "> /done\n\nI didn't act on this comment, and my reply saying why was lost. " +
		"It was edited after I rejected it, so I've ignored the edit. Post a new comment to try again.\n\n" +
		"<!-- issuedog:reply reason=rejected cmd=1002 -->"
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 3); !slices.Equal(is.Labels, []string{"stage:needs-human"}) || !slices.Equal(got, []string{want}) {
		t.Errorf("labels %q, comments after the hand-off\n%q\nwant needs-human, and only\n%q", is.Labels, got, want)
	}
}

// capRoundBound is the round bound of issue n as its markers set it
// (state.Issue.Round): comments above it are input after its latest move.
func capRoundBound(t *testing.T, f *githubtest.Fake, n int) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return state.Derive(s, slug, func(int64) bool { return false }).Round
}

// A plain comment and a /done the owner posts during the checks of the
// round's last attempt: the verify cap's move records seen=, the move to
// verifying, the newest comment when the checks started, so both are
// above the hand-off's round bound, as after the findings move. On the
// next pass, the /done, posted before the hand-off, is rejected, and the
// task waits at needs-human; a /done posted after the hand-off ends it.
func TestVerifyCapDuringChecks(t *testing.T) {
	t.Parallel()
	cfg := verifyConfig("false") + "loop:\n  max_attempts: 2\nnotify: [owner]\n"
	o := verifyOrigin(t, cfg)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	gitIn(t, o.work, "push", "--quiet", o.url, "task")
	main := gitIn(t, o.work, "rev-parse", "main")
	parsed, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	move := roundVerifying(f, head)
	g := verifyRig(t, f, o)
	g.d.Config = parsed
	g.d.Step.Commands["done"] = daemon.DoneCommand()

	ctx := context.Background()
	s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	in, err := g.d.Step.Run(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// The next read of the issue, or of its comments, is the one before
	// the move, after the checks.
	var plain, done int64
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && strings.HasPrefix(r.Path+"/", "/issues/1/")
	}, func() {
		plain = f.AddComment(1, "owner", "Keep the error text short.")
		done = f.AddComment(1, "owner", "/done")
	})
	if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}}); err != nil {
		t.Fatal(err)
	}
	cs := implementerComments(t, f, 1)
	if got, want := cs[len(cs)-1].Body, verifyCapText(2, 2, head, main)+"\n\n"+verifyCapMarker(move, move); got != want || pickStage(f, 1) != "stage:needs-human" {
		t.Fatalf("after the checks, the newest comment =\n%s\nwith the task at %s; want\n%s\nat needs-human", got, pickStage(f, 1), want)
	}
	if b := capRoundBound(t, f, 1); plain == 0 || b >= plain || b >= done {
		t.Errorf("the round bound is %d, the comment %d and the /done %d; want the bound below both", b, plain, done)
	}

	g.pass()
	is := f.State().Issues[0]
	if got := closingComments(f, 1, len(cs)); !slices.Equal(got, []string{earlyDoneReply(done)}) || pickStage(f, 1) != "stage:needs-human" {
		t.Errorf("after the next pass, the new comments are\n%q\nwith the task at %s; want only\n%q\nat needs-human", got, pickStage(f, 1), earlyDoneReply(done))
	}
	for _, c := range is.Comments {
		if c.Body == "Keep the error text short." && len(c.Reactions) != 0 {
			t.Errorf("the plain comment's reactions = %q, want none", c.Reactions)
		}
	}

	later := f.AddComment(1, "owner", "/done")
	g.pass()
	want := fmt.Sprintf("<!-- issuedog:transition from=needs-human to=ready cmd=%d seen=%d -->", later, later)
	if !slices.ContainsFunc(implementerComments(t, f, 1), func(c github.Comment) bool { return strings.HasSuffix(c.Body, want) }) {
		t.Errorf("after a /done posted after the hand-off, no comment ends %s", want)
	}
}

// A plain comment and a /done the owner posts during the round's last
// run, which fails: the attempt cap's move records seen=, the newest
// comment when that run started, so both are above the hand-off's round
// bound. On the next pass, the /done, posted before the hand-off, is
// rejected, and the task gets no run; a 👍 on the hand-off ends it.
func TestImplementerAttemptCapDuringRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := restartCapRig(t, f, digestNotifyOrigin(t), t.TempDir())
	key := restartKey(t, f, n)
	im := duringImplementer{Implementer: &daemon.Implementer{Daemon: g.d}}
	g.r.Stages[state.InProgress] = im
	for range 2 {
		g.pass()
	}
	var plain, done int64
	im.during = func() {
		plain = f.AddComment(n, "owner", "Keep the error text short.")
		done = f.AddComment(n, "owner", "/done")
	}
	g.r.Stages[state.InProgress] = im
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	cs := implementerComments(t, f, n)
	if want := restartCapComment(key, cs[0].ID); g.runs() != 3 || cs[len(cs)-1].Body != want || pickStage(f, n) != "stage:needs-human" {
		t.Fatalf("after %d runs, the newest comment =\n%s\nwith the task at %s; want 3, and\n%s\nat needs-human", g.runs(), cs[len(cs)-1].Body, pickStage(f, n), want)
	}
	if b := capRoundBound(t, f, n); plain == 0 || b >= plain || b >= done {
		t.Errorf("the round bound is %d, the comment %d and the /done %d; want the bound below both", b, plain, done)
	}

	g.r.Stages[state.InProgress] = &daemon.Implementer{Daemon: g.d}
	g.pass()
	if got := closingComments(f, n, len(cs)); !slices.Equal(got, []string{earlyDoneReply(done)}) || pickStage(f, n) != "stage:needs-human" || g.runs() != 3 {
		t.Errorf("after the next pass, the new comments are\n%q\nwith the task at %s, after %d runs; want only\n%q\nat needs-human, after 3",
			got, pickStage(f, n), g.runs(), earlyDoneReply(done))
	}

	f.AddReaction(cs[len(cs)-1].ID, "owner", "+1")
	g.pass()
	if l := g.leaseOf("4"); g.runs() != 4 || l.Attempt != 1 || l.AttemptKey == key {
		t.Errorf("after the 👍, %d runs, the 4th leased %+v; want run 4, attempt 1 of a new round, not %s", g.runs(), l, key)
	}
}

// verifyingAttempt files task 1 at verifying, as the implementer's move
// by run r<attempt> left it, with attempt= and head head.
func verifyingAttempt(f *githubtest.Fake, head string, attempt int) {
	n := implementerTask(f)
	f.RemoveLabel(n, app, "stage:in-progress")
	f.AddLabel(n, app, "stage:verifying")
	f.AddComment(n, app, fmt.Sprintf("The implementer committed.\n\n"+
		"<!-- issuedog:transition from=in-progress to=verifying attempt=%d run=r%d head=%s -->", attempt, attempt, head))
}

// Findings on attempt 3 after loop.max_attempts is lowered from 3 to 2:
// the verify cap's hand-off counts the round's 3 attempts, above findings
// on attempt 3 of 2, not 2 attempts.
func TestVerifyCapLoweredLimit(t *testing.T) {
	t.Parallel()
	cfg := verifyConfig("false") + "loop:\n  max_attempts: 2\nnotify: [owner]\n"
	o := verifyOrigin(t, cfg)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	gitIn(t, o.work, "push", "--quiet", o.url, "task")
	main := gitIn(t, o.work, "rev-parse", "main")
	parsed, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	verifyingAttempt(f, head, 3)
	g := verifyRig(t, f, o)
	g.d.Config = parsed
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	cs := implementerComments(t, f, 1)
	want := "This task's 3 attempts in this round failed, so I've stopped running the implementer here.\n\n" +
		"Verify failed on attempt 3 of 2, at `" + head + "`, checked against `main` at `" + main + "`.\n\n"
	if got := cs[len(cs)-1].Body; !strings.HasPrefix(got, want) || pickStage(f, 1) != "stage:needs-human" {
		t.Errorf("the newest comment =\n%s\nwith the task at %s; want the verify cap's hand-off, starting\n%s\nat needs-human", got, pickStage(f, 1), want)
	}
}

// The attempt cap's hand-off made in place of a run, after findings
// below loop.max_attempts that was lowered since: it counts the round's
// attempts as the findings name them, even above the limit, and says
// nothing of why, as the last attempt was the findings, even when a run
// failed earlier in the round.
func TestImplementerAttemptCapLoweredLimit(t *testing.T) {
	t.Parallel()
	commits := ivResult(t, implementerOutput(t, "commits"))
	for _, tt := range []struct {
		name     string
		outs     []string // the runs' outputs, each run's in turn
		attempts int      // the attempts the hand-off counts
	}{
		{"findings on attempt 3 of 4", []string{commits, commits, commits}, 3},
		{"findings on attempt 2 of 4, after a failed run", []string{failOut, commits}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := implementerTask(f)
			g := roundRig(t, f, roundConfig(4, "xhigh"), tt.outs...)
			for range len(tt.outs) {
				g.pass()
				if pickStage(f, n) == "stage:verifying" {
					g.pass()
				}
			}
			cs := implementerComments(t, f, n)
			if want := fmt.Sprintf("Verify failed on attempt %d of 4,", tt.attempts); !strings.HasPrefix(cs[len(cs)-1].Body, want) || pickStage(f, n) != "stage:in-progress" {
				t.Fatalf("the newest comment =\n%s\nwith the task at %s; want findings starting %q, at in-progress", cs[len(cs)-1].Body, pickStage(f, n), want)
			}
			// Work doesn't load the config from main, as a pass does.
			g.d.Config.Loop.MaxAttempts = 2
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			cs = implementerComments(t, f, n)
			want := fmt.Sprintf("This task's %d attempts in this round failed, so I've stopped running the implementer here.\n\n**Goal:**", tt.attempts)
			if got := cs[len(cs)-1].Body; g.runs() != len(tt.outs) || !strings.HasPrefix(got, want) || pickStage(f, n) != "stage:needs-human" {
				t.Errorf("after %d runs, the newest comment =\n%s\nwith the task at %s; want %d, and the cap's hand-off starting\n%s\nat needs-human",
					g.runs(), got, pickStage(f, n), len(tt.outs), want)
			}
		})
	}
}

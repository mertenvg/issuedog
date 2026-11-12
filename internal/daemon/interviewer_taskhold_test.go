package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the interviewer at the clarifying after a discussion hold
// on a task, a bug or a human task (SPEC §6.4, §7.2, §9.1): it runs there
// from any stage the task was held at, and leaves it as it was, rewrites
// its body and sends it to ready, or a human task's to needs-human. A
// task from a plan keeps its origin marker, wherever it was read from,
// and a human task its human=1 and check.

// The origin markers of #2's plan items: a task, and human tasks with a
// file check and with a plain-text "Done when", as the App writes them
// (github.FormatMarker), so a kept marker reads the same.
const (
	taskHoldPlan       = "<!-- issuedog:origin plan=#2 rev=1 item=T1 -->"
	taskHoldHumanCheck = "<!-- issuedog:origin plan=#2 rev=1 item=T2 check=file contains=QUOTE_TTL=24h human=1 value=deploy/production.env -->"
	taskHoldHumanPlain = "<!-- issuedog:origin plan=#2 rev=1 item=T3 human=1 -->"
)

// taskHoldFiled files issue 1 on f as kind, at stage by the App's moves,
// then epic #2, the parent of every kind but "owner" and "bug", and holds
// issue 1 for mallory's comment, which the owner's 👍 resolves in a pass
// of step 1 (resolvePass): issue 1 is at the clarifying after the hold,
// with origin stage, and the writes are forgotten. The kinds:
//   - "owner": the owner's task, by its type:task label;
//   - "bug": the owner's bug, by its type:bug label;
//   - "plan": a task the App filed from #2's plan, with scope:booking,
//     its origin marker in its body;
//   - "adopted": the owner's task, adopted as #2's plan's item, its origin
//     marker in an App comment, not the body, which the owner wrote;
//   - "human": a human task the App filed from #2's plan, with
//     scope:booking and a file check;
//   - "plain": the same, with a plain-text "Done when".
//
// At needs-human, a task's move there is a hand-off (handbackPosted), and
// a human task's its first move. It returns the move out of the hold.
func taskHoldFiled(tb testing.TB, f *githubtest.Fake, kind, stage string) int64 {
	tb.Helper()
	var n int
	switch kind {
	case "owner", "bug":
		n = f.AddIssue("owner", "Quotes", "Refuse an expired quote.")
		f.AddLabel(n, "owner", "type:"+map[string]string{"owner": "task", "bug": "bug"}[kind])
	case "adopted":
		n = f.AddIssue("owner", "Quotes", "Refuse an expired quote.")
		f.AddLabel(n, "owner", "type:task")
	case "plan":
		n = f.AddIssue(app, "Quotes", "Refuse an expired quote.\n\n"+taskHoldPlan)
	case "human":
		n = f.AddIssue(app, "Quotes", "Set the quote lifetime.\n\n"+taskHoldHumanCheck)
	case "plain":
		n = f.AddIssue(app, "Quotes", "Set the quote lifetime.\n\n"+taskHoldHumanPlain)
	default:
		tb.Fatalf("no kind %q", kind)
	}
	if n != 1 {
		tb.Fatalf("the task is #%d, want #1", n)
	}
	if epic := f.AddIssue("owner", "Epic", "The epic."); kind != "owner" && kind != "bug" {
		f.SetParent(n, epic)
	}
	if kind == "plan" || kind == "human" || kind == "plain" {
		f.AddLabel(n, app, "type:task")
		f.AddLabel(n, app, "scope:booking")
	}
	if kind == "adopted" {
		f.AddComment(n, app, "Adopted as T1.\n\n"+taskHoldPlan)
	}
	f.AddLabel(n, app, "stage:"+stage)
	switch {
	case kind == "human" || kind == "plain":
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=needs-human event=p5 -->")
	case stage == "needs-human":
		f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress event=s3 -->")
		f.AddComment(n, app, handbackPosted("im1"))
	case stage == "verifying":
		f.AddComment(n, app, "<!-- issuedog:transition from=in-progress to=verifying run=im1 head=abc attempt=1 -->")
	default:
		f.AddComment(n, app, "<!-- issuedog:transition from=ready to="+stage+" event=s3 -->")
	}
	m := f.AddComment(n, "mallory", ivHoldMallory)
	if err := resolvePass(tb, n)(f); err != nil {
		tb.Fatal(err)
	}
	f.AddReaction(m, "owner", "+1")
	if err := resolvePass(tb, n)(f); err != nil {
		tb.Fatal(err)
	}
	if body := newestComment(f, n); !strings.Contains(body, "from=held to=clarifying at="+stage+" origin="+stage) {
		tb.Fatalf("newest comment %q; want the end of the hold from %s", body, stage)
	}
	f.ForgetWrites()
	return discRunFind(tb, f, n, "from=held to=clarifying")
}

// taskHoldComment is the ID of the newest comment on issue 1 whose body is
// body, read with a fresh client.
func taskHoldComment(tb testing.TB, f *githubtest.Fake, body string) int64 {
	tb.Helper()
	s, err := f.Client(tb).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		tb.Fatal(err)
	}
	for i := len(s.Comments) - 1; i >= 0; i-- {
		if s.Comments[i].Body == body {
			return s.Comments[i].ID
		}
	}
	tb.Fatalf("issue 1 has no comment %q", body)
	return 0
}

// taskHoldRig is ivHoldRig on a rig of f, with booking among config's
// scopes, whose fake claude prints out.
func taskHoldRig(t *testing.T, f *githubtest.Fake, out map[string]any) *rig {
	t.Helper()
	g := ivHoldNew(t, f, out)
	g.d.Config.Scopes = []string{"booking", "cli"}
	return g
}

// taskHoldMoved is the App's comment holding run's move from the
// clarifying after a hold from origin to to, having seen seen, with text.
func taskHoldMoved(text, to, origin, run string, seen int64) string {
	return fmt.Sprintf("%s\n\n<!-- issuedog:transition from=clarifying to=%s origin=%s run=%s effort=high model=opus seen=%d -->", text, to, origin, run, seen)
}

// A task held from ready, in-progress, verifying or needs-human, a bug or
// one from a plan, comes back to clarifying when its discussion ends,
// where the interviewer runs at once, in the same pass. Its task outcome
// rewrites the body as an 11.2 task, the App's full rewrite, sets the
// scope, and moves it clarifying → ready on task-rewritten, with origin
// (SPEC §6.4, §7.2). A plan's task keeps its origin marker whole as the
// body's last line, and an adopted one, whose marker is in an App comment,
// gets it too, as a plan revision writes one (SPEC §6.4), and still reads
// it from the comment. The class stays as it was. The pass after writes
// nothing.
func TestTaskHoldRewrite(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind, stage string
		parent      string // the header's parent
		marker      string // the body's last line, the kept origin marker, if any
		class       state.Class
	}{
		{"owner", "ready", "none", "", state.ClassOwnerTask},
		{"owner", "in-progress", "none", "", state.ClassOwnerTask},
		{"bug", "verifying", "none", "", state.ClassBug},
		{"owner", "needs-human", "none", "", state.ClassOwnerTask},
		{"plan", "ready", "#2", taskHoldPlan, state.ClassTask},
		{"plan", "needs-human", "#2", taskHoldPlan, state.ClassTask},
		{"adopted", "verifying", "#2", taskHoldPlan, state.ClassTask},
	} {
		t.Run(tt.kind+" at "+tt.stage, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			back := taskHoldFiled(t, f, tt.kind, tt.stage)
			g := taskHoldRig(t, f, taskOut(t))
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			want := taskBody(t, "run1", "", tt.parent, "p2")
			if tt.marker != "" {
				want += "\n\n" + tt.marker
			}
			if got := f.State().Issues[0].Body; got != want {
				t.Errorf("body =\n%s\nwant\n%s", got, want)
			}
			if got, want := newestComment(f, 1), taskHoldMoved(taskMovedText, "ready", tt.stage, "run1", back); got != want {
				t.Errorf("the move =\n%s\nwant\n%s", got, want)
			}
			typ := "type:task"
			if tt.kind == "bug" {
				typ = "type:bug"
			}
			if got := ivLabels(f, 1); !slices.Equal(got, []string{"scope:booking", "stage:ready", typ}) {
				t.Errorf("labels %q, want scope:booking, stage:ready and %s", got, typ)
			}
			is := ivInput(t, g.d, 1).Issue
			if class, err := state.ClassOf(is.Item); err != nil || class != tt.class || is.Stage != state.Ready {
				t.Errorf("class %s, %v, at %s; want %s, at ready", class, err, is.Stage, tt.class)
			}
			if tt.marker != "" && (is.Origin.Plan != 2 || is.Origin.Item != "T1") {
				t.Errorf("origin %+v, want #2's T1, kept", is.Origin)
			}
			if tt.kind == "adopted" && is.Origin.Comment == state.BodyID {
				t.Errorf("origin read from the body the owner filed, want from the App's comment")
			}
			writes := len(f.Writes())
			if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %q and %d runs; want none and 1", err, writeLines(f, writes), g.runs())
			}
		})
	}
}

// The unchanged outcome sends the task back to the stage it was held at,
// with origin, and writes no body: a task held at in-progress, whose run
// the hold dropped, goes to ready instead (SPEC §6.4, §7.2), and a human
// task back to needs-human. Back at needs-human, the comment says what
// still stands, the hand-off or the human task, and ends in the cc line,
// as the task waits on the owner again (SPEC §9.1). The pass after writes
// nothing and runs nothing.
func TestTaskHoldUnchanged(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind, stage, to, text string
	}{
		{"owner", "ready", "ready", "Nothing in the discussion during the hold changes this issue, so it goes back to `ready`."},
		{"owner", "in-progress", "ready", "Nothing in the discussion during the hold changes this issue. " +
			"The hold from `in-progress` dropped the task's run, so it goes back to `ready`."},
		{"plan", "verifying", "verifying", "Nothing in the discussion during the hold changes this issue, so it goes back to `verifying`."},
		{"owner", "needs-human", "needs-human", "Nothing in the discussion during the hold changes this issue, so it goes back to `needs-human`.\n\n" +
			"The hand-off from before the hold still stands: when it's done, reply `/done` on this issue or react 👍 to the hand-off.\n\ncc @owner"},
		{"human", "needs-human", "needs-human", "Nothing in the discussion during the hold changes this issue, so it goes back to `needs-human`.\n\n" +
			"When it's done, reply `/done` or react 👍 to this issue.\n\ncc @owner"},
	} {
		t.Run(tt.kind+" at "+tt.stage, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			back := taskHoldFiled(t, f, tt.kind, tt.stage)
			body := f.State().Issues[0].Body
			g := taskHoldRig(t, f, map[string]any{"outcome": "unchanged"})
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if got, want := newestComment(f, 1), taskHoldMoved(tt.text, tt.to, tt.stage, "run1", back); got != want {
				t.Errorf("the move =\n%s\nwant\n%s", got, want)
			}
			if st := ivStage(t, g.d, 1); st != state.Stage(tt.to) || f.State().Issues[0].Body != body {
				t.Errorf("at %s, body %q; want %s, with the body as it was", st, f.State().Issues[0].Body, tt.to)
			}
			writes := len(f.Writes())
			if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %q and %d runs; want none and 1", err, writeLines(f, writes), g.runs())
			}
		})
	}
}

// Back at needs-human by the interviewer's unchanged, the hand-off from
// before the hold is still the latest (SPEC §6.4, §9.3), so a trusted
// /done, or a trusted 👍 on the hand-off, sends the task to ready, keyed
// as usual, and both are safe to replay.
func TestTaskHoldHandoffStands(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	for name, tt := range map[string]struct {
		more func(tb testing.TB, f *githubtest.Fake)
		key  func(t *testing.T, f *githubtest.Fake) string
	}{
		"/done": {func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/done") }, func(t *testing.T, f *githubtest.Fake) string {
			return fmt.Sprintf("cmd=%d", taskHoldComment(t, f, "/done"))
		}},
		"a 👍 on the hand-off": {func(tb testing.TB, f *githubtest.Fake) {
			f.AddReaction(discRunFind(tb, f, 1, "from=in-progress to=needs-human run=im1"), "owner", "+1")
		}, func(*testing.T, *githubtest.Fake) string { return "cmd=r" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// setup files the task held at needs-human, and applies the
			// interviewer's unchanged, back to needs-human, then runs more.
			setup := func(tb testing.TB) *githubtest.Fake {
				f := newFake(tb)
				taskHoldFiled(tb, f, "owner", "needs-human")
				g := taskHoldRig(t, f, map[string]any{"outcome": "unchanged"})
				if err := g.work(1); err != nil {
					tb.Fatal(err)
				}
				if c := newestComment(f, 1); !strings.Contains(c, "from=clarifying to=needs-human origin=needs-human run=run1") {
					tb.Fatalf("newest comment %q; want unchanged, back to needs-human", c)
				}
				tt.more(tb, f)
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, handbackPass(t, o))
			f := setup(t)
			if err := handbackPass(t, o)(f); err != nil {
				t.Fatal(err)
			}
			if got, key := newestComment(f, 1), tt.key(t, f); !strings.Contains(got, "from=needs-human to=ready "+key) {
				t.Errorf("newest comment %q; want the task back at ready, keyed %s", got, key)
			}
			if l := ivLabels(f, 1); !slices.Contains(l, "stage:ready") {
				t.Errorf("labels %q, want stage:ready", l)
			}
		})
	}
}

// taskHoldHumanFields are the golden human output's fields as a human
// task's body has them, Why it blocks naming why, and Done when done.
func taskHoldHumanFields(why, done string) string {
	return "**Goal:** Give the booking service its quote lifetime in production.\n" +
		"**Why it blocks:** " + why + " can't proceed until production has a quote lifetime, because the discussion moved it out of the code " +
		"and the service won't start without one\n" +
		"**Steps:**\n" +
		"1. Open the production service's settings in the hosting console.\n" +
		"2. Add the environment variable `QUOTE_TTL` with the value `24h`, and save.\n" +
		"3. Reply `/done` on this issue.\n\n" +
		"**Links:**\n" +
		"- https://console.hosting.example.com/services/booking/settings\n\n" +
		"**Done when:** " + done + "\n" +
		"**Estimated time:** 5 min"
}

// A human task held at needs-human gets the interviewer's human outcome at
// the clarifying after the hold: the App rewrites its body in the SPEC §9.3
// template, with its parent line, its fields, Why it blocks naming the
// open issues it blocks, or its parent, the cc line and its origin marker
// whole, human=1 and the check included, and moves it clarifying →
// needs-human on human-task-rewritten, its comment ending in the cc line.
// With a check, Done when is the daemon's sentence on the check, not the
// agent's text; with plain text, the agent's. It's still a human task with
// the same check, and the pass after writes nothing.
func TestTaskHoldHuman(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind, marker, why, done string
		blocked                 bool
		check                   state.Check
	}{
		{"human", taskHoldHumanCheck, "#3", "The file `deploy/production.env` on `main` contains `QUOTE_TTL=24h`. " +
			"I check this when you reply `/done` or react 👍 to this issue.", true,
			state.Check{Kind: state.CheckFile, Value: "deploy/production.env", Contains: "QUOTE_TTL=24h"}},
		{"plain", taskHoldHumanPlain, "#2", "The production service's settings list QUOTE_TTL=24h.", false, state.Check{}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			back := taskHoldFiled(t, f, tt.kind, "needs-human")
			if tt.blocked {
				// #3 waits on the human task, and #4, closed, did.
				f.SetBlockedBy(f.AddIssue("owner", "Waits", ""), 1)
				f.SetBlockedBy(f.AddIssue("owner", "Waited", ""), 1)
				f.Close(4)
				f.ForgetWrites()
			}
			g := taskHoldRig(t, f, wholeRunOutput(t, "human"))
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			want := "<!-- issuedog:rewrite run=run1 -->\n**Parent:** #2   **Scope:** scope:booking   **Priority:** p2\n\n" +
				taskHoldHumanFields(tt.why, tt.done) + "\n\ncc @owner\n\n" + tt.marker
			if got := f.State().Issues[0].Body; got != want {
				t.Errorf("body =\n%s\nwant\n%s", got, want)
			}
			text := "I've rewritten this human task from the discussion, in the description above, so it's back at `needs-human`. " +
				"When it's done, reply `/done` or react 👍 to this issue.\n\ncc @owner"
			if got, want := newestComment(f, 1), taskHoldMoved(text, "needs-human", "needs-human", "run1", back); got != want {
				t.Errorf("the move =\n%s\nwant\n%s", got, want)
			}
			is := ivInput(t, g.d, 1).Issue
			if class, err := state.ClassOf(is.Item); err != nil || class != state.ClassHumanTask || is.Origin.Check != tt.check || is.Stage != state.NeedsHuman {
				t.Errorf("class %s, %v, check %+v, at %s; want a human task, check %+v, at needs-human", class, err, is.Origin.Check, is.Stage, tt.check)
			}
			writes := len(f.Writes())
			if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %q and %d runs; want none and 1", err, writeLines(f, writes), g.runs())
			}
		})
	}
}

// The interviewer's context after a hold on a task offers questions, task
// and unchanged, with the hold line saying what each does and the task's
// typing line, and on a human task, human in place of task, with its line
// on the template, the origin marker and Done when, and prompt.md asks for
// the human task's body (SPEC §12).
func TestTaskHoldContext(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind, stage string
		want        []string
		prompt      string
	}{
		{"plan", "in-progress", []string{
			"- The outcomes you may return here: `questions`, `task` and `unchanged`. Any other outcome fails the run.",
			"Before the hold it was at `in-progress`. If nothing in the discussion changes the task, return `unchanged`, " +
				"and it goes to `ready`, as the hold dropped its run.",
			"- This issue is a bug or a task already, so leave `type` out, and never return `typed`.",
			"- A task's scope is one of: `booking`, `cli`."}, "the task's body"},
		{"human", "needs-human", []string{
			"- The outcomes you may return here: `questions`, `human` and `unchanged`. Any other outcome fails the run.",
			"If the discussion changes the task's body, return `human`, its whole body, and it goes back to `needs-human`.",
			"- This issue is a human task, so it gets no spec: if the discussion changes the owner's action, return `human`",
			"If the task has a check, its **Done when** stays issuedog's sentence on the check",
			"- This issue is a bug or a task already, so leave `type` out, and never return `typed`."}, "the human task's body"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			taskHoldFiled(t, f, tt.kind, tt.stage)
			d := ivDaemon(t, f, newOrigin(t))
			d.Config.Scopes = []string{"booking", "cli"}
			files := ivContext(t, d, 1)
			preamble, _, _ := strings.Cut(files["system.md"], "\n# Interviewer\n")
			for _, w := range tt.want {
				if !strings.Contains(preamble, w) {
					t.Errorf("the preamble lacks %q:\n%s", w, preamble)
				}
			}
			if !strings.Contains(files["prompt.md"], "or "+tt.prompt+" once nothing that blocks it is open") || !strings.Contains(files["issue.md"], "## After a hold") {
				t.Errorf("prompt.md %q, issue.md\n%s\nwant %s, and the hold's section", files["prompt.md"], files["issue.md"], tt.prompt)
			}
		})
	}
}

// A task, a bug or a human task the interviewer asked a round of at the
// clarifying after a hold waits for its answer: a pass makes no request
// for it, a conditional read of the poll at most, answered 304, and starts
// no run. Every request is counted, whatever its answer. An answer then
// starts the next run.
func TestTaskHoldRoundQuiet(t *testing.T) {
	t.Parallel()
	for kind, stage := range map[string]string{"plan": "ready", "bug": "verifying", "human": "needs-human"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			taskHoldFiled(t, f, kind, stage)
			g := taskHoldRig(t, f, ivRound(1, ""))
			g.pass()
			if c := newestComment(f, 1); g.runs() != 1 || !strings.Contains(c, "from=clarifying to=clarifying origin="+stage+" round=1 run=run1") {
				t.Fatalf("after %d runs, newest comment %q; want one run, its round", g.runs(), c)
			}
			g.pass() // reads back the App's own writes
			from := len(f.Requests())
			g.pass()
			reqs := f.Requests()[from:]
			for _, r := range reqs {
				if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified || strings.HasPrefix(r.Path, "/issues/1") {
					t.Errorf("an unchanged pass sent %+v; want only the poll's conditional reads, answered 304", r)
				}
			}
			if len(reqs) == 0 || g.runs() != 1 {
				t.Errorf("an unchanged pass sent %d requests and ran %d times; want the poll's, and one run", len(reqs), g.runs())
			}
			f.AddComment(1, "owner", "1a")
			g.claude(ivResult(t, map[string]any{"outcome": "unchanged"}), 0)
			g.pass()
			if st := ivStage(t, g.d, 1); g.runs() != 2 || st != state.Stage(stage) {
				t.Errorf("after the answer, %d runs, at %s; want two, back at %s", g.runs(), st, stage)
			}
		})
	}
}

// Each write of the task's, the human task's and unchanged's apply after a
// hold is safe to replay after a crash at any of them (SPEC §5.2): the
// scope, the body and the move, then the labels.
func TestTaskHoldReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, kind, stage string
		out               func(t *testing.T) map[string]any
	}{
		{"task, a plan's", "plan", "verifying", taskOut},
		{"task, adopted", "adopted", "ready", taskOut},
		{"human, with a check", "human", "needs-human", func(t *testing.T) map[string]any { return wholeRunOutput(t, "human") }},
		{"unchanged, from in-progress", "owner", "in-progress", func(*testing.T) map[string]any { return map[string]any{"outcome": "unchanged"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) { taskHoldFiled(tb, f, tt.kind, tt.stage) }, func(g *rig) {
				ivHoldRig(g)
				g.d.Config.Scopes = []string{"booking", "cli"}
				g.claude(ivResult(t, tt.out(t)), 0)
			})
		})
	}
}

// A crash after each write, then a fresh daemon: it writes only what's
// left, never a write twice (SPEC §5.2). After the scope, the body is
// written; after the body, it isn't again, though the header would now
// name priority p0, which the owner set since; after the move, only its
// labels are finished, though the owner edited the body and switched the
// scope since, so neither is written again (the !moved guard).
func TestTaskHoldCrashThenFreshDaemon(t *testing.T) {
	t.Parallel()
	failPatch := func(r githubtest.Request) bool { return r.Method == "PATCH" && r.Path == "/issues/1" }
	failComment := func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/issues/1/comments" }
	failDelete := func(r githubtest.Request) bool {
		return r.Method == "DELETE" && r.Path == "/issues/1/labels/stage:clarifying"
	}
	priority := func(f *githubtest.Fake) { f.AddLabel(1, "owner", "priority:p0") }
	editAndRescope := func(f *githubtest.Fake) {
		f.EditBody(1, "owner", "The owner's own words.")
		f.RemoveLabel(1, "owner", "scope:booking")
		f.AddLabel(1, "owner", "scope:cli")
	}
	for _, tt := range []struct {
		name, kind, stage string
		out               func(t *testing.T) map[string]any
		fail              func(githubtest.Request) bool
		between           func(f *githubtest.Fake)
		after             []string
		body              string // the body after, if not the run's
		labels            []string
	}{
		{"task, after the scope", "owner", "ready", taskOut, failPatch, nil,
			[]string{"PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"},
			"", []string{"scope:booking", "stage:ready", "type:task"}},
		{"task, after the body", "plan", "ready", taskOut, failComment, priority,
			[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"},
			"", []string{"priority:p0", "scope:booking", "stage:ready", "type:task"}},
		{"task, after the move", "owner", "ready", taskOut, failDelete, editAndRescope,
			[]string{"DELETE /issues/1/labels/stage:clarifying"},
			"The owner's own words.", []string{"scope:cli", "stage:ready", "type:task"}},
		{"human, after the body", "human", "needs-human", func(t *testing.T) map[string]any { return wholeRunOutput(t, "human") }, failComment, priority,
			[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"},
			"", []string{"priority:p0", "scope:booking", "stage:needs-human", "type:task"}},
		{"human, after the move", "human", "needs-human", func(t *testing.T) map[string]any { return wholeRunOutput(t, "human") }, failDelete, editAndRescope,
			// The owner's edit hid the body's origin marker, so step 1
			// reposts it (SPEC §6.4), and the task stays a human task.
			[]string{"DELETE /issues/1/labels/stage:clarifying", "POST /issues/1/comments"},
			"The owner's own words.", []string{"scope:cli", "stage:needs-human", "type:task"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			taskHoldFiled(t, f, tt.kind, tt.stage)
			o, root := newOrigin(t), t.TempDir()
			fresh := func() *rig {
				g := newRig(t, f, o, root)
				ivHoldRig(g)
				g.d.Config.Scopes = []string{"booking", "cli"}
				return g
			}
			g := fresh()
			g.claude(ivResult(t, tt.out(t)), 0)
			f.AnswerOnce(tt.fail, 500)
			if err := g.work(1); err == nil {
				t.Fatal("work = nil, want the crash")
			}
			// The run's move is the only comment keyed by run1.
			moved := func() int { return strings.Count(fmt.Sprint(f.State().Issues[0].Comments), " run=run1 ") }
			written := f.State().Issues[0].Body
			if !strings.HasPrefix(written, "<!-- issuedog:rewrite run=run1 -->") {
				written = "" // the crash came before the body
			}
			if tt.between != nil {
				tt.between(f)
			}
			g = fresh()
			writes := len(f.Writes())
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if got := writeLines(f, writes); !slices.Equal(got, tt.after) {
				t.Errorf("the fresh daemon's writes %q, want %q", got, tt.after)
			}
			body := f.State().Issues[0].Body
			switch {
			case tt.body != "" && body != tt.body:
				t.Errorf("body =\n%s\nwant the owner's edit\n%s", body, tt.body)
			case tt.body == "" && written != "" && body != written:
				t.Errorf("body =\n%s\nwant the one written before the crash\n%s", body, written)
			case tt.body == "" && !strings.HasPrefix(body, "<!-- issuedog:rewrite run=run1 -->"):
				t.Errorf("body =\n%s\nwant run1's", body)
			}
			if n := moved(); n != 1 || g.runs() != 1 {
				t.Errorf("%d moves after %d runs; want one move, and one run", n, g.runs())
			}
			if got := ivLabels(f, 1); !slices.Equal(got, tt.labels) {
				t.Errorf("labels %q, want %q", got, tt.labels)
			}
			if is := ivInput(t, g.d, 1).Issue; tt.kind == "human" && (!is.Item.Human || is.Origin.Item != "T2") {
				t.Errorf("origin %+v; want the human task's T2 still read", is.Origin)
			}
		})
	}
}

// A human task that blocks no open issue makes no read of the issues it
// blocks as the interviewer's human outcome rewrites it after a hold
// (ivHumanBlocks): its snapshot says it blocks none.
func TestTaskHoldHumanNoBlockingRead(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	taskHoldFiled(t, f, "plain", "needs-human")
	g := taskHoldRig(t, f, wholeRunOutput(t, "human"))
	from := len(f.Requests())
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if st := ivStage(t, g.d, 1); st != state.NeedsHuman {
		t.Fatalf("at %s, want needs-human, rewritten", st)
	}
	for _, r := range f.Requests()[from:] {
		if strings.HasSuffix(r.Path, "/dependencies/blocking") {
			t.Errorf("the rewrite sent %+v; want no read of what a human task that blocks nothing blocks", r)
		}
	}
}

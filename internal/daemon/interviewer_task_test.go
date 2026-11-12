package daemon_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the interviewer's task (SPEC §9.1, §11.2): a bug, or a task
// the owner filed, gets interviewer runs at clarifying until nothing
// blocks it, and its task outcome rewrites the body as an 11.2 task, sets
// its scope:* label and moves it to ready, keyed by the run.

// taskOut is internal/agent's golden task output, decoded, whose scope is
// booking.
func taskOut(t *testing.T) map[string]any {
	t.Helper()
	return wholeRunOutput(t, "task")
}

// taskSet sets the task's field to value in out, a taskOut, and returns
// out.
func taskSet(t testing.TB, out map[string]any, field string, value any) map[string]any {
	t.Helper()
	task, ok := out["task"].(map[string]any)
	if !ok {
		t.Fatalf("no task in %v", out)
	}
	task[field] = value
	return out
}

// taskRendered is taskOut's body as the App writes it, with its header
// naming parent and pri, and no rewrite marker: testdata/interview/task.md,
// which TestRenderTask renders, with its header's parent and priority.
func taskRendered(t testing.TB, parent, pri string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "interview", "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	return strings.Replace(text, "**Parent:** none   **Scope:** scope:booking   **Priority:** p2",
		"**Parent:** "+parent+"   **Scope:** scope:booking   **Priority:** "+pri, 1)
}

// taskBody is the whole body run writes: its rewrite marker, with
// replaced=<replaced> unless it's "", then taskRendered.
func taskBody(t testing.TB, run, replaced, parent, pri string) string {
	t.Helper()
	marker := "<!-- issuedog:rewrite run=" + run
	if replaced != "" {
		marker += " replaced=" + replaced
	}
	return marker + " -->\n" + taskRendered(t, parent, pri)
}

// taskMovedText is the text of a task's move to ready (SPEC §9.1).
const taskMovedText = "I've written this issue up as a task, in the description above, so it's `ready` for the implementer."

// taskMoved is the App's comment on run's move to ready, with
// rig.interviewer's settings, having seen seen, and fields, if any, before
// run=.
func taskMoved(fields, run string, seen int64) string {
	if fields != "" {
		fields += " "
	}
	return fmt.Sprintf("%s\n\n<!-- issuedog:transition from=clarifying to=ready %srun=%s effort=high model=opus seen=%d -->", taskMovedText, fields, run, seen)
}

// taskRig is ivRig with booking among config's scopes.
func taskRig(t *testing.T, f *githubtest.Fake) *rig {
	t.Helper()
	g := ivRig(t, f)
	g.d.Config.Scopes = []string{"booking", "cli"}
	return g
}

// writeLines are the fake's writes from from on, as "<method> <path>".
func writeLines(f *githubtest.Fake, from int) []string {
	var lines []string
	for _, w := range f.Writes()[from:] {
		lines = append(lines, w.Method+" "+w.Path)
	}
	return lines
}

// The task outcome on a bug or a task the owner filed rewrites the body
// whole, the App's full rewrite, with the 11.2 header naming the parent,
// or none, the scope and the priority, p2 with none (SPEC §11.2). First it
// sets scope:booking, removing any other scope:* label, and last it moves
// the issue clarifying → ready, keyed by the run, with no cc line. The
// body is clean after. The pass after writes nothing, and runs nothing, as
// the interviewer doesn't run on a task at ready.
func TestTaskWritten(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		typ         string
		parent      bool
		labels      []string
		header, pri string
		wantLabels  []string
		wantWrites  []string
	}{
		"a bug with no parent or priority": {"bug", false, nil, "none", "p2",
			[]string{"scope:booking", "stage:ready", "type:bug"},
			[]string{"POST /issues/1/labels", "PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"}},
		"the owner's task with a parent, a priority and another scope": {"task", true, []string{"priority:p1", "scope:cli"}, "#2", "p1",
			[]string{"priority:p1", "scope:booking", "stage:ready", "type:task"},
			[]string{"POST /issues/1/labels", "DELETE /issues/1/labels/scope:cli", "PATCH /issues/1", "POST /issues/1/comments",
				"POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"}},
		"a bug with its scope already, in another case": {"bug", false, []string{"scope:Booking"}, "none", "p2",
			[]string{"scope:Booking", "stage:ready", "type:bug"},
			[]string{"PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, tt.typ, tt.parent, "from=intake to=clarifying event=5")
			for _, l := range tt.labels {
				f.AddLabel(n, "owner", l)
			}
			g := taskRig(t, f)
			g.claude(ivResult(t, taskOut(t)), 0)
			writes := len(f.Writes())
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if got := writeLines(f, writes); !slices.Equal(got, tt.wantWrites) {
				t.Errorf("writes %q, want %q", got, tt.wantWrites)
			}
			if got, want := f.State().Issues[n-1].Body, taskBody(t, "run1", "", tt.header, tt.pri); got != want {
				t.Errorf("body =\n%s\nwant\n%s", got, want)
			}
			if got := ivLabels(f, n); !slices.Equal(got, tt.wantLabels) {
				t.Errorf("labels %v, want %v", got, tt.wantLabels)
			}
			if got, want := newestComment(f, n), taskMoved("", "run1", entry); got != want {
				t.Errorf("the move =\n%s\nwant\n%s", got, want)
			}
			if title := f.State().Issues[n-1].Title; title != "Idea" {
				t.Errorf("title %q, want the issue's own, Idea", title)
			}
			d, err := g.d.Step.Dirty(context.Background(), g.snapshot(n), g.d.Step.Trust)
			if err != nil || d.Has(state.BodyID) {
				t.Errorf("Dirty = %v, %v; want the body clean", d, err)
			}
			if st := ivStage(t, g.d, n); st != state.Ready {
				t.Errorf("at %s, want ready", st)
			}
			writes = len(f.Writes())
			f.AddComment(n, "owner", "Thanks.")
			if err := g.work(n); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %q and %d runs; want none and 1", err, writeLines(f, writes), g.runs())
			}
		})
	}
}

// An untyped issue the interviewer types a bug gets its run at once, on
// the typed move, with no input (SPEC §9.1), and that run's task moves it
// to ready, its comment naming the typed move as seen.
func TestTaskAfterTyped(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, _ := ivFiled(f, "", false, "from=intake to=clarifying event=5")
	g := taskRig(t, f)
	g.claude(ivResult(t, ivTyped("bug")), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	typed := specNewest(t, f, n)
	g.claude(ivResult(t, taskOut(t)), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	if got, want := newestComment(f, n), taskMoved("", "run2", typed); got != want || g.runs() != 2 {
		t.Errorf("after %d runs, the newest comment =\n%s\nwant two runs and\n%s", g.runs(), got, want)
	}
	if got, want := f.State().Issues[n-1].Body, taskBody(t, "run2", "", "none", "p2"); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if got := ivLabels(f, n); !slices.Equal(got, []string{"scope:booking", "stage:ready", "type:bug"}) {
		t.Errorf("labels %v, want scope:booking, stage:ready and type:bug", got)
	}
}

// The interviewer's context on a bug offers questions and task only, and
// lists the scopes a task may have, as the planner's does (SPEC §12), and
// prompt.md asks for the task's body. On an untyped issue it offers typed,
// never task, so an untyped bug is typed first.
func TestTaskContext(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		typ       string
		want, not []string
	}{
		"a bug": {"bug", []string{
			"- The outcomes you may return here: `questions` and `task`. Any other outcome fails the run.",
			"- A task's scope is one of: `booking`, `cli`. Any other scope is a question to the owner, never a guess.",
			"- This issue is a bug or a task, so it gets no spec: once nothing open blocks it, return `task`, its body,",
			"- This issue is a bug or a task already, so leave `type` out, and never return `typed`."},
			[]string{"so you may change it", "The owner set"}},
		"an untyped issue": {"", []string{"- The outcomes you may return here: `questions`, `spec` and `typed`. Any other outcome fails the run."},
			[]string{"A task's scope", "`task`"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, _ := ivFiled(f, tt.typ, false, "from=intake to=clarifying event=5")
			d := ivDaemon(t, f, newOrigin(t))
			d.Config.Scopes = []string{"booking", "cli"}
			files := ivContext(t, d, n)
			// The preamble, before the shipped prompt, which covers task.
			preamble, _, ok := strings.Cut(files["system.md"], "\n# Interviewer\n")
			if !ok {
				t.Fatalf("system.md has no prompt after its preamble:\n%s", files["system.md"])
			}
			for _, w := range tt.want {
				if !strings.Contains(preamble, w) {
					t.Errorf("the preamble lacks %q:\n%s", w, preamble)
				}
			}
			for _, w := range tt.not {
				if strings.Contains(preamble, w) {
					t.Errorf("the preamble has %q:\n%s", w, preamble)
				}
			}
			if task := strings.Contains(files["prompt.md"], "or the task's body once nothing that blocks it is open"); task != (tt.typ != "") {
				t.Errorf("prompt.md = %q; want it to ask for the task's body %t", files["prompt.md"], tt.typ != "")
			}
		})
	}
}

// On a body mallory made dirty, the task is the App's full rewrite, its
// marker naming her, so the body is clean again (SPEC §5.1, §11.2), and
// the move's text first says whose edit the task replaced.
func TestTaskDirtyBody(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, entry := ivFiled(f, "bug", false, "from=intake to=clarifying event=5")
	f.EditBody(n, "mallory", "the bug, as mallory sees it")
	g := taskRig(t, f)
	g.claude(ivResult(t, taskOut(t)), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	if got, want := f.State().Issues[n-1].Body, taskBody(t, "run1", "mallory", "none", "p2"); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	want := "This task replaces the whole issue description, with the edit by `mallory` that wasn't approved, " +
		"so the description is clean again.\n\n" + taskMoved("", "run1", entry)
	if got := newestComment(f, n); got != want {
		t.Errorf("the move =\n%s\nwant\n%s", got, want)
	}
	d, err := g.d.Step.Dirty(context.Background(), g.snapshot(n), g.d.Step.Trust)
	if err != nil || d.Has(state.BodyID) {
		t.Errorf("Dirty = %v, %v; want the body clean", d, err)
	}
}

// Each broken rule of a task (SPEC §9.1's Checks) is a failed attempt,
// with nothing written: a scope not on the list, or with none listed, a
// body over the length limit, a field that doesn't go with task, a
// section the schema refuses, and task on an issue it isn't offered on,
// an epic or an untyped issue.
func TestTaskCheckFailsAttempt(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		typ    string
		scopes []string
		out    func(t *testing.T) map[string]any
	}{
		"a scope not on the list": {"bug", []string{"cli"}, taskOut},
		"no scopes listed":        {"bug", nil, taskOut},
		"a body over the limit": {"bug", []string{"booking"}, func(t *testing.T) map[string]any {
			return taskSet(t, taskOut(t), "change", strings.Repeat("x", 65536))
		}},
		"a type with the task": {"bug", []string{"booking"}, func(t *testing.T) map[string]any {
			out := taskOut(t)
			out["type"] = map[string]any{"value": "bug", "reason": ivReason}
			return out
		}},
		"a two-line criterion": {"bug", []string{"booking"}, func(t *testing.T) map[string]any {
			return taskSet(t, taskOut(t), "acceptance_criteria", []string{"a\nb"})
		}},
		"no criteria": {"task", []string{"booking"}, func(t *testing.T) map[string]any {
			return taskSet(t, taskOut(t), "acceptance_criteria", []string{})
		}},
		"a blank context": {"task", []string{"booking"}, func(t *testing.T) map[string]any {
			return taskSet(t, taskOut(t), "context", " ")
		}},
		"task on an epic":          {"epic", []string{"booking"}, taskOut},
		"task on an untyped issue": {"", []string{"booking"}, taskOut},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, _ := ivFiled(f, tt.typ, false, "from=intake to=clarifying event=5")
			g := taskRig(t, f)
			g.d.Config.Scopes = tt.scopes
			g.claude(ivResult(t, tt.out(t)), 0)
			before := f.State()
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if after := f.State(); !reflect.DeepEqual(after.Issues, before.Issues) {
				t.Errorf("the issue changed:\n%+v\nwant\n%+v", after.Issues, before.Issues)
			}
			if l := g.lease(); g.runs() != 1 || !l.Failed || l.Attempt != 1 || l.ApplyFailures != 0 {
				t.Errorf("after %d runs, lease %+v; want one failed attempt", g.runs(), l)
			}
		})
	}
}

// The task's writes, the scope label, the body, the move and its labels,
// are safe to replay after a crash at any of them (SPEC §5.2): a body the
// run wrote isn't edited again, and a written move only finishes its
// labels. Also on a dirty body, on a scope label to replace, and at the
// clarifying after a hold from clarifying, whose move is on
// task-rewritten, with origin.
func TestTaskReplay(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(f *githubtest.Fake){
		"a bug": func(f *githubtest.Fake) { ivFiled(f, "bug", false, "from=intake to=clarifying event=5") },
		"a dirty body": func(f *githubtest.Fake) {
			f.EditBody(fst(ivFiled(f, "bug", false, "from=intake to=clarifying event=5")), "mallory", "edited")
		},
		"a scope to clear": func(f *githubtest.Fake) {
			f.AddLabel(fst(ivFiled(f, "task", false, "from=intake to=clarifying event=5")), "owner", "scope:cli")
		},
		"after a hold from clarifying": func(f *githubtest.Fake) {
			ivFiled(f, "bug", false, "from=held to=clarifying at=clarifying origin=clarifying event=h9 seen=7")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) { setup(f) }, func(g *rig) {
				g.interviewer()
				g.d.Config.Scopes = []string{"booking", "cli"}
				g.claude(ivResult(t, taskOut(t)), 0)
			})
		})
	}
}

// fst is the first of a pair, such as ivFiled's issue number.
func fst[A, B any](a A, _ B) A { return a }

// At the clarifying after a hold from clarifying, the task's move is on
// task-rewritten and carries origin (SPEC §6.4, §7.2).
func TestTaskAfterHold(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, entry := ivFiled(f, "bug", false, "from=held to=clarifying at=clarifying origin=clarifying event=h9 seen=7")
	g := taskRig(t, f)
	g.claude(ivResult(t, taskOut(t)), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	if got, want := newestComment(f, n), taskMoved("origin=clarifying", "run1", entry); got != want {
		t.Errorf("the move =\n%s\nwant\n%s", got, want)
	}
	if st := ivStage(t, g.d, n); st != state.Ready {
		t.Errorf("at %s, want ready", st)
	}
}

// A crash after the body, before the move, then a fresh daemon: the body
// isn't edited again, though its header would change, as the owner set a
// priority since, and the move is posted, naming no one. A crash after
// the move, before its labels, then a fresh daemon: only the labels are
// finished, with no body edit, scope write or comment (SPEC §5.2).
func TestTaskCrashThenFreshDaemon(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		fail  func(githubtest.Request) bool
		after []string // the fresh daemon's writes
	}{
		"after the body": {func(r githubtest.Request) bool { return r.Method == "POST" && strings.HasSuffix(r.Path, "/comments") },
			[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying"}},
		"after the move": {func(r githubtest.Request) bool {
			return r.Method == "DELETE" && strings.HasSuffix(r.Path, "/labels/stage:clarifying")
		},
			[]string{"DELETE /issues/1/labels/stage:clarifying"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, "bug", false, "from=intake to=clarifying event=5")
			o, root := newOrigin(t), t.TempDir()
			fresh := func() *rig {
				g := newRig(t, f, o, root)
				g.interviewer()
				g.d.Config.Scopes = []string{"booking"}
				return g
			}
			g := fresh()
			g.claude(ivResult(t, taskOut(t)), 0)
			f.AnswerOnce(tt.fail, 500)
			if err := g.work(n); err == nil {
				t.Fatal("work = nil, want the crash")
			}
			f.AddLabel(n, "owner", "priority:p0")
			g = fresh()
			writes := len(f.Writes())
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if got := writeLines(f, writes); !slices.Equal(got, tt.after) {
				t.Errorf("the fresh daemon's writes %q, want %q", got, tt.after)
			}
			if got, want := f.State().Issues[n-1].Body, taskBody(t, "run1", "", "none", "p2"); got != want {
				t.Errorf("body =\n%s\nwant run1's, as written before the crash,\n%s", got, want)
			}
			if got, want := newestComment(f, n), taskMoved("", "run1", entry); got != want || g.runs() != 1 {
				t.Errorf("after %d runs, the move =\n%s\nwant the one before the crash, no more, and\n%s", g.runs(), got, want)
			}
			if got := ivLabels(f, n); !slices.Equal(got, []string{"priority:p0", "scope:booking", "stage:ready", "type:bug"}) {
				t.Errorf("labels %v, want the move's", got)
			}
		})
	}
}

// A task waiting at clarifying on the implementer's or the verifier's
// round gets no interviewer run, answered or not: it has been at ready, and
// its answer sends it back there in step 1 (SPEC §7.2, §9.1).
func TestTaskRoundsAreNotTheInterviewers(t *testing.T) {
	t.Parallel()
	for name, from := range map[string]string{"the implementer's": "in-progress", "the verifier's": "verifying"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := fileAt(f, from, "Refuse an expired quote.", "priority:p1", "scope:booking")
			f.SetType(n, "Task")
			f.AddComment(n, app, strings.Replace(taskAnswerRound, "from=in-progress", "from="+from, 1))
			f.AddLabel(n, app, "stage:clarifying")
			f.RemoveLabel(n, app, "stage:"+from)
			g := taskRig(t, f)
			g.claude(ivResult(t, taskOut(t)), 0)
			g.pass()
			if g.runs() != 0 || ivStage(t, g.d, n) != state.Clarifying {
				t.Errorf("unanswered: %d runs, at %s; want none, at clarifying", g.runs(), ivStage(t, g.d, n))
			}
			first := f.AddComment(n, "owner", "1a")
			g.pass()
			want := fmt.Sprintf("Round 1 has an answer, so the task goes back to `ready`. The next run gets it.\n\n"+
				"<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", first, first)
			if got := newestComment(f, n); got != want || g.runs() != 0 {
				t.Errorf("answered: %d runs, the newest comment =\n%s\nwant none, and\n%s", g.runs(), got, want)
			}
		})
	}
}

// A bug or a task that waited at clarifying before the interviewer ran on
// one, as the move into the pipeline or a typed move said it would, gets
// its run on the next pass, with no input (SPEC §9.1).
func TestTaskWaitedBefore(t *testing.T) {
	t.Parallel()
	const waits = "Bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3), so I won't ask anything more here."
	for name, setup := range map[string]func(f *githubtest.Fake) int{
		"filed a task": func(f *githubtest.Fake) int {
			n := f.AddIssue("owner", "Idea", "the idea", "type:task")
			f.AddLabel(n, app, "stage:clarifying")
			f.AddComment(n, app, "owner opened this issue. It's a task: bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3)."+
				"\n\n<!-- issuedog:transition from=intake to=clarifying event=5 -->")
			return n
		},
		"typed a bug": func(f *githubtest.Fake) int {
			n, entry := ivFiled(f, "", false, "from=intake to=clarifying event=5")
			f.AddLabel(n, app, "type:bug")
			f.AddComment(n, app, fmt.Sprintf("%s\n\n%s\n\n<!-- issuedog:transition from=clarifying to=clarifying run=iv1 seen=%d -->", ivTypeLine("bug"), waits, entry))
			return n
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := setup(f)
			g := taskRig(t, f)
			g.claude(ivResult(t, taskOut(t)), 0)
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if g.runs() != 1 || ivStage(t, g.d, n) != state.Ready {
				t.Errorf("%d runs, at %s; want one, at ready", g.runs(), ivStage(t, g.d, n))
			}
		})
	}
}

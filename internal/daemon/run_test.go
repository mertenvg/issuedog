package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// fakeClaude counts its runs in $FAKE/count, keeps each prompt and the lease
// it ran under as $FAKE/prompt.<n> and $FAKE/lease.<n>, prints $FAKE/out and
// exits with $FAKE/exit, if there is one.
const fakeClaude = `#!/bin/sh
n=$(($(cat "$FAKE/count" 2>/dev/null || echo 0) + 1))
echo "$n" > "$FAKE/count"
printf '%s' "$2" > "$FAKE/prompt.$n"
cp "$STATE/lease.json" "$FAKE/lease.$n"
cat "$FAKE/out"
exit "$(cat "$FAKE/exit" 2>/dev/null || echo 0)"
`

// Results the fake claude prints.
const (
	okOut      = `{"type":"result","subtype":"success","is_error":false,"result":"Done.","structured_output":{"text":"The spec <!-- issuedog:transition to=planning -->"}}`
	failOut    = `{"type":"result","subtype":"error_max_turns","is_error":true,"result":""}`
	invalidOut = `{"type":"result","subtype":"success","is_error":false,"result":"You've hit your usage limit","structured_output":{"text":5}}`
)

// limitOut is a usage-limit failure that resets at reset.
func limitOut(subtype string, reset time.Time) string {
	return fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":true,"result":"Claude usage limit reached|%d"}`, subtype, reset.Unix())
}

// fakeStage moves the issue to spec-review with its output's text, or with
// moreQuestions, stays at clarifying. Its Context gives the inputs' bodies
// as the prompt, and calls during, if set, which happens after the pre-run
// read: during the run.
type fakeStage struct {
	pending       func(daemon.Input) bool // nil: when the newest comment isn't the App's
	during        func()
	moreQuestions bool
}

func (*fakeStage) Agent() string { return "interviewer" }

func (fs *fakeStage) Pending(in daemon.Input) bool {
	if fs.pending != nil {
		return fs.pending(in)
	}
	return in.S.Comments[len(in.S.Comments)-1].Author.Login != app
}

func (fs *fakeStage) Context(in daemon.Input) (map[string][]byte, error) {
	if fs.during != nil {
		fs.during()
	}
	var prompt []string
	for _, c := range in.Inputs {
		prompt = append(prompt, c.Body)
	}
	return map[string][]byte{"prompt.md": []byte(strings.Join(prompt, "\n")), "system.md": []byte("system")}, nil
}

func (*fakeStage) Schema() []byte {
	return []byte(`{"type":"object","required":["text"],"additionalProperties":false,"properties":{"text":{"type":"string"}}}`)
}

func (fs *fakeStage) Apply(ctx context.Context, a *daemon.Applier, r daemon.Result) error {
	var out struct{ Text string }
	if err := json.Unmarshal(r.Output, &out); err != nil {
		return err
	}
	m := r.Move(state.SpecReview, state.TriggerSpecWritten)
	if fs.moreQuestions {
		m = r.Move(state.Clarifying, state.TriggerMoreQuestions)
	}
	m.Text = out.Text
	return a.Transition(ctx, r.S, r.Dirt.Has, m)
}

// rig is a Runs on a fresh daemon of f, with the fakeStage at clarifying, a
// fake claude, and its state, clones and context files under root. Its
// run IDs are run1, run2 and so on, and its clock reads now.
type rig struct {
	t     *testing.T
	f     *githubtest.Fake
	d     *daemon.Daemon
	r     *daemon.Runs
	stage *fakeStage
	root  string
	now   time.Time
}

func newRig(t *testing.T, f *githubtest.Fake, o *origin, root string) *rig {
	t.Helper()
	g := &rig{t: t, f: f, root: root, now: githubtest.Start, stage: &fakeStage{}}
	for _, dir := range []string{"state", "runs", "ctx", "claude"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := g.path("claude", "claude")
	if err := os.WriteFile(script, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	gate, err := agent.NewGate(config.Defaults().UsageLimit.Pattern)
	if err != nil {
		t.Fatal(err)
	}
	g.d = newDaemon(t, f, o, nil)
	g.d.Step.Trust = trusted{"owner": true}
	ids := 0
	g.r = &daemon.Runs{Daemon: g.d, Stages: map[state.Stage]daemon.Stage{state.Clarifying: g.stage},
		Runner:    &agent.Runner{Claude: script, StateDir: g.path("state")},
		Workspace: &workspace.Workspace{Bare: o.url, RunsDir: g.path("runs"), ContextDir: g.path("ctx")},
		Gate:      gate,
		Env:       []string{"FAKE=" + g.path("claude"), "STATE=" + g.path("state"), "PATH=/usr/bin:/bin"},
		Now:       func() time.Time { return g.now },
		NewID:     func() string { ids++; return "run" + strconv.Itoa(ids) },
	}
	g.d.Work = g.r.Work
	return g
}

func (g *rig) path(elem ...string) string { return filepath.Join(append([]string{g.root}, elem...)...) }

// claude sets what the fake claude prints and its exit code.
func (g *rig) claude(out string, exit int) {
	g.t.Helper()
	for name, data := range map[string]string{"out": out, "exit": strconv.Itoa(exit)} {
		if err := os.WriteFile(g.path("claude", name), []byte(data), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
}

// runs is how many times the fake claude ran.
func (g *rig) runs() int {
	data, err := os.ReadFile(g.path("claude", "count"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		g.t.Fatal(err)
	}
	return n
}

// file reads a file of the fake claude's, such as prompt.1.
func (g *rig) file(name string) string {
	g.t.Helper()
	data, err := os.ReadFile(g.path("claude", name))
	if err != nil {
		g.t.Fatal(err)
	}
	return string(data)
}

// work reads issue n, runs step 1 on it, and gives it to Work, as a pass
// of the loop does.
func (g *rig) work(n int) error {
	ctx := context.Background()
	s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", n)
	if err != nil {
		return err
	}
	in, err := g.d.Step.Run(ctx, s)
	if err != nil {
		return err
	}
	return g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}})
}

// leftovers lists what's left of runs: the lease, results and clones.
func (g *rig) leftovers() []string {
	var left []string
	for _, dir := range []string{"state", "state/runs", "runs", "ctx"} {
		entries, err := os.ReadDir(g.path(dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || dir == "runs" || dir == "ctx" {
				left = append(left, dir+"/"+e.Name())
			}
		}
	}
	return slices.DeleteFunc(left, func(p string) bool { return p == "ctx/.agent.gitconfig" })
}

// interview files an epic at clarifying, as the App moved it there, with
// owner's answer 1a, which the App 👍'd. It returns the answer's ID.
func interview(f *githubtest.Fake) int64 {
	n := f.AddIssue("owner", "Idea", "the idea")
	f.SetType(n, "Epic")
	f.AddLabel(n, app, "stage:clarifying")
	f.AddComment(n, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
	id := f.AddComment(n, "owner", "1a")
	f.AddReaction(id, app, "+1")
	return id
}

// specWritten is the App's comment applying a run's result.
func specWritten(run string, seen int64) string { return applied("spec-review", run, seen) }

// applied is the App's comment applying a run's result with a move to.
func applied(to, run string, seen int64) string {
	return fmt.Sprintf("The spec &lt;!-- issuedog:transition to=planning -->\n\n"+
		"<!-- issuedog:transition from=clarifying to=%s run=%s seen=%d -->", to, run, seen)
}

// saveRun leaves the files of run id on issue 1 at clarifying under root,
// as a daemon that stopped during it would: the lease, the result, if out
// isn't empty, and a clone.
func saveRun(t testing.TB, root, id, out string, seen int64) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.Clarifying, RunID: id, Clone: filepath.Join(root, "runs", id), Seen: seen})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"state/lease.json": string(l), "runs/" + id + "/f.txt": "x"}
	if out != "" {
		files["state/runs/"+id+".json"] = out
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// replayRuns is Replay with a rig per fake, kept for its passes, as the
// state volume outlives a crash. setup may save a run with saveRun.
func replayRuns(t *testing.T, setup func(t testing.TB, f *githubtest.Fake, root string), more func(g *rig)) {
	t.Helper()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		setup(tb, f, roots[f])
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, o, roots[f])
		if more != nil {
			more(g)
		}
		return g.work(1)
	})
}

func TestRunStageAppliesResult(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	if last := is.Comments[len(is.Comments)-1].Body; last != specWritten("run1", answer) || !slices.Equal(is.Labels, []string{"stage:spec-review"}) {
		t.Errorf("newest comment %q and labels %v; want the spec written by run1, escaped, and spec-review", last, is.Labels)
	}
	if g.runs() != 1 || g.file("prompt.1") != "1a" {
		t.Errorf("claude ran %d times, the first on %q; want once, on the answer", g.runs(), g.file("prompt.1"))
	}
	var l daemon.Lease
	if err := json.Unmarshal([]byte(g.file("lease.1")), &l); err != nil {
		t.Fatal(err)
	}
	if l.Issue != 1 || l.Stage != state.Clarifying || l.RunID != "run1" || l.Clone != g.path("runs", "run1") ||
		!l.Started.Equal(githubtest.Start) || l.Seen != answer {
		t.Errorf("the lease during the run = %+v; want issue 1 at clarifying, run1, its clone, started now, seen the answer", l)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v after the run; want no lease, result or clone", left)
	}
	// Nothing is pending now, and nothing is applied again.
	writes := len(f.Writes())
	if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
		t.Errorf("a second Work = %v, with %d writes and %d runs; want none", err, len(f.Writes())-writes, g.runs())
	}
}

func TestRunStageApplyReplay(t *testing.T) {
	replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) { interview(f) }, func(g *rig) { g.claude(okOut, 0) })
}

// A lease with a valid result, left by a restart, is applied once, without
// running claude, even if the daemon stops again before its files go. The
// issue gets no run in that pass, as its snapshot predates the result,
// even when the move keeps it at clarifying.
func TestResumeAppliesSavedResultOnce(t *testing.T) {
	for _, to := range []string{"spec-review", "clarifying"} {
		t.Run(to, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			saveRun(t, root, "saved", okOut, answer)
			rig := func() *rig {
				g := newRig(t, f, newOrigin(t), root)
				g.stage.moreQuestions = to == "clarifying"
				g.claude(failOut, 1)
				return g
			}
			g := rig()
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			comments := f.State().Issues[0].Comments
			if last := comments[len(comments)-1].Body; last != applied(to, "saved", answer) || g.runs() != 0 {
				t.Errorf("newest comment %q, after %d runs; want the saved result applied with no run", last, g.runs())
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v; want the lease, result and clone gone", left)
			}
			writes := len(f.Writes())
			saveRun(t, root, "saved", okOut, answer)
			for range 2 {
				if err := rig().work(1); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.Writes()) != writes || len(g.leftovers()) != 0 || g.runs() != 0 {
				t.Errorf("resuming an applied run wrote %+v, left %v and ran %d times; want nothing", f.Writes()[writes:], g.leftovers(), g.runs())
			}
		})
	}
}

func TestResumeReplay(t *testing.T) {
	replayRuns(t, func(t testing.TB, f *githubtest.Fake, root string) {
		saveRun(t, root, "saved", okOut, interview(f))
	}, func(g *rig) { g.claude(failOut, 1) })
}

// A lease with no valid result deletes the clone, and the stage runs again.
func TestResumeWithNoResultRunsAgain(t *testing.T) {
	for name, out := range map[string]string{"none": "", "invalid": invalidOut} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			saveRun(t, root, "lost", out, answer)
			g := newRig(t, f, newOrigin(t), root)
			g.claude(okOut, 0)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			comments := f.State().Issues[0].Comments
			if last := comments[len(comments)-1].Body; last != specWritten("run1", answer) || g.runs() != 1 {
				t.Errorf("newest comment %q, after %d runs; want one new run, applied", last, g.runs())
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v; want the old run's clone and lease gone", left)
			}
		})
	}
}

// A held or closed issue's lease and clone are dropped, its result unused.
func TestResumeDropsHeldOrClosed(t *testing.T) {
	for name, change := range map[string]func(f *githubtest.Fake){
		"closed": func(f *githubtest.Fake) { f.Close(1) },
		"held": func(f *githubtest.Fake) {
			f.AddComment(1, app, "<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner event=9 -->")
			f.AddLabel(1, app, "stage:held")
			f.RemoveLabel(1, app, "stage:clarifying")
		},
	} {
		for _, out := range []string{okOut, ""} {
			t.Run(fmt.Sprintf("%s, result %t", name, out != ""), func(t *testing.T) {
				f := newFake(t)
				answer := interview(f)
				change(f)
				root := t.TempDir()
				saveRun(t, root, "saved", out, answer)
				g := newRig(t, f, newOrigin(t), root)
				g.claude(okOut, 0)
				writes := len(f.Writes())
				if err := g.r.Work(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				if len(f.Writes()) != writes || g.runs() != 0 || len(g.leftovers()) != 0 {
					t.Errorf("wrote %+v, ran %d times, and left %v; want the lease dropped and nothing else", f.Writes()[writes:], g.runs(), g.leftovers())
				}
			})
		}
	}
}

// A usage limit closes the gate, whatever the exit code: stage work waits
// while step 1 keeps running, and it isn't an attempt, so with
// max_attempts 1 the stage runs again once the gate opens.
func TestUsageLimitGate(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run("exit "+strconv.Itoa(exit), func(t *testing.T) {
			f := newFake(t)
			interview(f)
			o := newOrigin(t)
			o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 1\n")
			g := newRig(t, f, o, t.TempDir())
			reset := githubtest.Start.Add(time.Hour)
			g.claude(limitOut("error_during_execution", reset), exit)
			iterate(t, g.d)
			if g.runs() != 1 || g.r.Gate.Open(g.now) || !g.r.Gate.OpenAt().Equal(reset) {
				t.Fatalf("after %d runs the gate opens at %s; want one run, and the gate shut until %s", g.runs(), g.r.Gate.OpenAt(), reset)
			}
			f.AddComment(1, "owner", "/priority p1")
			iterate(t, g.d)
			is := f.State().Issues[0]
			if g.runs() != 1 || !slices.Contains(is.Labels, "priority:p1") {
				t.Errorf("while the gate is shut: %d runs and labels %v; want no run and /priority p1 applied", g.runs(), is.Labels)
			}
			g.now = reset
			g.claude(okOut, 0)
			iterate(t, g.d)
			is = f.State().Issues[0]
			if last := is.Comments[len(is.Comments)-1].Body; g.runs() != 2 || !strings.HasPrefix(last, "The spec") {
				t.Errorf("after the reset: %d runs, newest comment %q; want a second run, applied, and no state error", g.runs(), last)
			}
		})
	}
}

// Only a failed run is matched against usage limits: a successful one whose
// text names a limit closes no gate, valid or not, and an invalid one is
// an attempt.
func TestLimitTextInSuccessfulRun(t *testing.T) {
	for name, tt := range map[string]struct {
		out   string
		cap   bool // an attempt, which with max_attempts 1 is the last
		moved bool
	}{
		"invalid": {out: invalidOut, cap: true},
		"valid":   {out: strings.Replace(okOut, `"Done."`, `"You've hit your usage limit"`, 1), moved: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			g.claude(tt.out, 0)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			last := is.Comments[len(is.Comments)-1].Body
			if !g.r.Gate.Open(g.now) || strings.Contains(last, "kind=attempts") != tt.cap || strings.HasPrefix(last, "The spec") != tt.moved {
				t.Errorf("gate open %t, newest comment %q; want the gate open, the attempt cap %t, the move %t", g.r.Gate.Open(g.now), last, tt.cap, tt.moved)
			}
		})
	}
}

// A stage that fails max_attempts times in a row gets one state error and
// no more runs, counted in memory over one snapshot, and read back from
// GitHub after a restart, until new input starts a new count.
func TestAttemptCap(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	o := newOrigin(t)
	root := t.TempDir()
	always := func(daemon.Input) bool { return true }
	g := newRig(t, f, o, root)
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = always
	g.claude(failOut, 1)
	ctx := context.Background()
	s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	ready := []daemon.Ready{{S: s, Inputs: s.Comments[1:2]}}
	errorMarkers := func() []string {
		var keys []string
		for _, c := range f.State().Issues[0].Comments {
			if i := strings.Index(c.Body, "<!-- issuedog:error"); i >= 0 {
				keys = append(keys, c.Body[i:])
			}
		}
		return keys
	}
	for range 4 {
		if err := g.r.Work(ctx, ready); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{fmt.Sprintf("<!-- issuedog:error kind=attempts key=clarifying-%d -->", answer)}
	if g.runs() != 2 || !reflect.DeepEqual(errorMarkers(), want) {
		t.Errorf("after 4 passes: %d runs, errors %q; want 2 runs and %q", g.runs(), errorMarkers(), want)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want failed runs cleaned up", left)
	}
	restarted := newRig(t, f, o, root)
	restarted.d.Config.Loop.MaxAttempts = 2
	restarted.stage.pending = always
	if err := restarted.work(1); err != nil || g.runs() != 2 {
		t.Errorf("after a restart: %v, %d runs; want no run", err, g.runs())
	}
	f.AddReaction(f.AddComment(1, "owner", "1b, I think"), app, "+1")
	g.claude(okOut, 0)
	if err := restarted.work(1); err != nil || g.runs() != 3 || len(errorMarkers()) != 1 {
		t.Errorf("after a new answer: %v, %d runs, errors %q; want a third run and no new error", err, g.runs(), errorMarkers())
	}
}

func TestAttemptCapReplay(t *testing.T) {
	replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) { interview(f) }, func(g *rig) {
		g.d.Config.Loop.MaxAttempts = 1
		g.claude(failOut, 1)
	})
}

// The issue is read again, and CheckLabels run, before a result is
// applied: the owner's move to held during the run wins.
func TestOwnerMoveDuringRunWins(t *testing.T) {
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() { f.AddLabel(1, "owner", "stage:held") }
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	last := is.Comments[len(is.Comments)-1].Body
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !strings.Contains(last, "to=held at=clarifying reason=owner by=owner") ||
		strings.Contains(last, "run=run1") || g.runs() != 1 {
		t.Errorf("labels %v, newest comment %q; want the owner's hold recorded and kept, and run1's result dropped", is.Labels, last)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want the dropped run cleaned up", left)
	}
}

// An answer edited during a run drops its result, and the stage runs again
// on the edit.
func TestAnswerEditedDuringRun(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() {
		g.stage.during = nil
		f.EditComment(answer, "owner", "1b")
	}
	iterate(t, g.d)
	comments := f.State().Issues[0].Comments
	if g.runs() != 1 || strings.Contains(comments[len(comments)-1].Body, "run=run1") {
		t.Fatalf("after %d runs the newest comment is %q; want run1's result dropped", g.runs(), comments[len(comments)-1].Body)
	}
	iterate(t, g.d)
	comments = f.State().Issues[0].Comments
	if last := comments[len(comments)-1].Body; g.runs() != 2 || g.file("prompt.1") != "1a" || g.file("prompt.2") != "1b" || last != specWritten("run2", answer) {
		t.Errorf("runs saw %q then %q, and the newest comment is %q; want 1a, then 1b applied by run2", g.file("prompt.1"), g.file("prompt.2"), last)
	}
}

// countTrust counts each login's checks.
type countTrust struct {
	trusted
	checks map[string]int
}

func (c countTrust) Trusted(ctx context.Context, login string) (bool, error) {
	c.checks[login]++
	return c.trusted.Trusted(ctx, login)
}

// A run uses only inputs whose author is trusted when it starts, checking
// each author once.
func TestRunChecksTrustAtStart(t *testing.T) {
	f := newFake(t)
	interview(f)
	for _, body := range []string{"x's first", "x's second"} {
		f.AddReaction(f.AddComment(1, "x", body), app, "+1")
	}
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Step.Trust = trusted{"owner": true, "x": true}
	ctx := context.Background()
	s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	in, err := g.d.Step.Run(ctx, s)
	if err != nil || len(in) != 3 {
		t.Fatalf("step 1 = %d inputs, %v; want the 3 answers", len(in), err)
	}
	ct := countTrust{trusted{"owner": true}, map[string]int{}}
	g.d.Step.Trust = ct
	g.claude(okOut, 0)
	if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}}); err != nil {
		t.Fatal(err)
	}
	if g.file("prompt.1") != "1a" || !reflect.DeepEqual(ct.checks, map[string]int{"owner": 1, "x": 1}) {
		t.Errorf("the run saw %q, after trust checks %v; want only owner's answer, each author checked once", g.file("prompt.1"), ct.checks)
	}
}

// An issue deleted while it's held and quiet is found gone by the read
// before a run, so the daemon drops it within two passes.
func TestRunFindsIssueGone(t *testing.T) {
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	pending := false
	g.stage.pending = func(daemon.Input) bool { return pending }
	var work [][]int
	g.d.Work = func(ctx context.Context, ready []daemon.Ready) error {
		ns := []int{}
		for _, r := range ready {
			ns = append(ns, r.S.Number)
		}
		work = append(work, ns)
		return g.r.Work(ctx, ready)
	}
	iterate(t, g.d)
	f.DeleteIssue()
	pending = true
	for range 2 {
		iterate(t, g.d)
	}
	// The run's 404 and the next pass's are two strikes: it's dropped.
	from := len(f.Requests())
	iterate(t, g.d)
	if !reflect.DeepEqual(work, [][]int{{1}, {1}, {}, {}}) || g.runs() != 0 {
		t.Errorf("Work got %v, with %d runs; want the deleted issue once more, then never, and no run", work, g.runs())
	}
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read deleted issue 1 again: %+v", r)
	}
}

// Work runs one issue a pass, the highest priority first.
func TestWorkPicksByPriority(t *testing.T) {
	f := newFake(t)
	interview(f)
	interview(f)
	f.AddLabel(2, app, "priority:p1")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	ctx := context.Background()
	var ready []daemon.Ready
	for n := 1; n <= 2; n++ {
		s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		ready = append(ready, daemon.Ready{S: s, Inputs: s.Comments[1:2]})
	}
	if err := g.r.Work(ctx, ready); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues
	if g.runs() != 1 || !slices.Contains(is[1].Labels, "stage:spec-review") || !slices.Contains(is[0].Labels, "stage:clarifying") {
		t.Errorf("%d runs, labels %v and %v; want one run, on issue 2", g.runs(), is[0].Labels, is[1].Labels)
	}
}

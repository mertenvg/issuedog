package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// checkStage is a fakeStage with a Check that counts its calls and fails
// with err, if set.
type checkStage struct {
	*fakeStage
	err    error
	checks int
}

func (c *checkStage) Check(context.Context, daemon.Result) error {
	c.checks++
	return c.err
}

// triageStage is a fakeStage for Runs.Unlabeled, as the triage agent: its
// Apply admits the issue, a move from intake to clarifying.
type triageStage struct{ *fakeStage }

func (ts triageStage) Apply(ctx context.Context, a *daemon.Applier, r daemon.Result) error {
	m := r.Move(state.Clarifying, state.TriggerTrusted)
	m.Text = "Admitted."
	return a.Transition(ctx, r.S, r.Dirt.Has, m)
}

// triage sets g's Unlabeled stage, the triage agent, with work pending on
// every issue it's asked about, and returns it.
func (g *rig) triage() triageStage {
	ts := triageStage{&fakeStage{agent: "triage", pending: always}}
	g.r.Unlabeled = ts
	return ts
}

// triageAdmitted is the App's comment applying triage run's result, on an issue
// with no comments before it, so it has no seen.
func triageAdmitted(run string) string {
	return fmt.Sprintf("Admitted.\n\n<!-- issuedog:transition from=intake to=clarifying run=%s -->", run)
}

// commentBodies lists the bodies of issue n's comments.
func commentBodies(f *githubtest.Fake, n int) []string {
	var bodies []string
	for _, c := range f.State().Issues[n-1].Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// marked reports whether a comment on issue n names run.
func marked(f *githubtest.Fake, n int, run string) bool {
	return slices.ContainsFunc(f.State().Issues[n-1].Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, "run="+run+" ") })
}

// leaseIssues lists the issue each of the fake claude's runs leased.
func (g *rig) leaseIssues() []int {
	g.t.Helper()
	var ns []int
	for i := 1; i <= g.runs(); i++ {
		var l daemon.Lease
		if err := json.Unmarshal([]byte(g.file("lease."+strconv.Itoa(i))), &l); err != nil {
			g.t.Fatal(err)
		}
		ns = append(ns, l.Issue)
	}
	return ns
}

// saveIntake leaves the files of the Unlabeled run id on issue 1 under
// root, as saveRun does, with its lease at intake.
func saveIntake(t testing.TB, root, id, out string) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.Intake, RunID: id, Clone: filepath.Join(root, "runs", id),
		AttemptKey: "intake-0", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"state/lease.json": string(l), "runs/" + id + "/f.txt": "x", "state/runs/" + id + ".json": out}
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

// resumeWrites is Replay over a pass that only resumes the lease setup
// saved, with nothing else pending, after more sets the rig up. The pass
// must write nothing, and leave a lease only as the record of attempts
// failed in a row, if attempts isn't 0.
func resumeWrites(t *testing.T, setup func(t testing.TB, f *githubtest.Fake, root string), more func(g *rig), attempts int) {
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
		g.stage.pending = func(daemon.Input) bool { return false }
		g.claude(okOut, 0)
		if more != nil {
			more(g)
		}
		writes := len(f.Writes())
		if err := g.r.Work(context.Background(), nil); err != nil {
			return err
		}
		if w := f.Writes(); len(w) != writes {
			return fmt.Errorf("the pass wrote %+v; want nothing", w[writes:])
		}
		if l := g.lease(); l.RunID != "" && (!l.Failed || l.Attempt != attempts) || l.RunID == "" && attempts != 0 {
			return fmt.Errorf("the pass left the lease %+v; want %d failed attempts recorded", l, attempts)
		}
		return nil
	})
}

// Pending gets the issue's dirt and state, read before it's asked.
func TestInputHasDirtAndIssue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	f.EditBody(1, "mallory", "the idea, rewritten")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	var got []daemon.Input
	g.stage.pending = func(in daemon.Input) bool { got = append(got, in); return false }
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Dirt.Has(state.BodyID) || got[0].Issue.Stage != state.Clarifying || got[0].Issue.Round == 0 {
		t.Errorf("Pending got %+v; want one call, with the dirty body and the issue at clarifying, in its round", got)
	}
}

// A Checker's error writes nothing and is one failed attempt, and after
// loop.max_attempts of them the attempts state error is posted.
func TestCheckerErrorIsFailedAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = always
	cs := &checkStage{fakeStage: g.stage, err: errors.New("8 questions, but the limit is 7")}
	g.r.Stages[state.Clarifying] = cs
	g.claude(okOut, 0)
	writes := len(f.Writes())
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if l := g.lease(); len(f.Writes()) != writes || cs.checks != 1 || !l.Failed || l.Attempt != 1 || l.ApplyFailures != 0 {
		t.Fatalf("after one run: writes %+v, %d checks, lease %+v; want no write, one check, and one failed attempt", f.Writes()[writes:], cs.checks, l)
	}
	for range 2 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{capError(answer)}; g.runs() != 2 || cs.checks != 2 || !reflect.DeepEqual(errorMarkers(f, 1), want) {
		t.Errorf("after 3 passes: %d runs, %d checks, errors %q; want 2 runs, both checked, and %q", g.runs(), cs.checks, errorMarkers(f, 1), want)
	}
}

func TestCheckerErrorWritesNothing(t *testing.T) {
	t.Parallel()
	resumeWrites(t, func(t testing.TB, f *githubtest.Fake, root string) {
		saveRun(t, root, "saved", okOut, interview(f))
	}, func(g *rig) {
		g.r.Stages[state.Clarifying] = &checkStage{fakeStage: g.stage, err: errors.New("bad")}
	}, 1)
}

// A saved result whose move is written isn't checked again on resume, but
// only finished. One whose move isn't written is checked.
func TestCheckerSkipsWrittenMove(t *testing.T) {
	t.Parallel()
	for _, written := range []bool{true, false} {
		t.Run(fmt.Sprintf("written %t", written), func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			if written {
				f.AddComment(1, app, specWritten("saved", answer)) // the labels aren't set yet
			}
			root := t.TempDir()
			saveRun(t, root, "saved", okOut, answer)
			g := newRig(t, f, newOrigin(t), root)
			cs := &checkStage{fakeStage: g.stage, err: errors.New("bad")}
			g.r.Stages[state.Clarifying] = cs
			if err := g.r.Work(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if done := slices.Equal(is.Labels, []string{"stage:spec-review"}); cs.checks == 1 == written || done != written {
				t.Errorf("%d checks, labels %v; want a check only for a move not written, and a written one finished", cs.checks, is.Labels)
			}
			if l := g.lease(); l.Failed != !written {
				t.Errorf("lease %+v; want a failed attempt only for a checked result", l)
			}
		})
	}
}

// An owner's body edit during the run drops the result, with no attempt
// counted, and the stage runs again.
func TestBodyEditDuringRunDrops(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 1\n")
	g := newRig(t, f, o, t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() {
		g.stage.during = nil
		f.EditBody(1, "owner", "the idea, edited")
	}
	g.pass()
	if newest := newestComment(f, 1); g.runs() != 1 || strings.Contains(newest, "run=run1") || len(g.leftovers()) != 0 || g.lease().RunID != "" {
		t.Fatalf("after %d runs: newest comment %q, left %v; want run1 dropped, uncounted, with nothing left", g.runs(), newest, g.leftovers())
	}
	g.pass()
	if g.runs() != 2 || !specApplied(f, 1, "run2", answer) || len(errorMarkers(f, 1)) != 0 {
		t.Errorf("%d runs, errors %q; want run2 applied, and no attempt cap", g.runs(), errorMarkers(f, 1))
	}
}

// On resume, a body edit since the run read it drops the result, unless
// it's the App's, such as one the result made before a crash.
func TestBodyEditOnResume(t *testing.T) {
	t.Parallel()
	for _, editor := range []string{app, "owner"} {
		t.Run(editor, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			saveRun(t, root, "saved", okOut, answer)
			f.EditBody(1, editor, "the spec")
			g := newRig(t, f, newOrigin(t), root)
			if err := g.r.Work(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if specApplied(f, 1, "saved", answer) != (editor == app) || g.lease().RunID != "" {
				t.Errorf("applied %t, lease %+v; want the result applied only after the App's edit, and the lease gone", specApplied(f, 1, "saved", answer), g.lease())
			}
		})
	}
}

func TestBodyEditDropWritesNothing(t *testing.T) {
	t.Parallel()
	resumeWrites(t, func(t testing.TB, f *githubtest.Fake, root string) {
		saveRun(t, root, "saved", okOut, interview(f))
		f.EditBody(1, "owner", "the idea, edited")
	}, nil, 0)
}

// A trusted comment posted during the run drops the result, and the next
// run has it as input. An untrusted comment, the App's, a command and a
// dirty comment don't, when the untrusted user has no write access: a
// write collaborator's holds the issue (TestDiscDuringRunDrops).
func TestCommentDuringRun(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		post func(f *githubtest.Fake)
		drop bool
	}{
		"trusted":   {post: func(f *githubtest.Fake) { f.AddComment(1, "owner", "1b too") }, drop: true},
		"untrusted": {post: func(f *githubtest.Fake) { f.AddComment(1, "eve", "1c, surely") }},
		"the App":   {post: func(f *githubtest.Fake) { f.AddComment(1, app, "A note.") }},
		"command":   {post: func(f *githubtest.Fake) { f.AddComment(1, "owner", "/priority p1") }},
		"dirty": {post: func(f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, "owner", "1b too"), "eve", "1c")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.claude(okOut, 0)
			g.stage.during = func() {
				g.stage.during = nil
				tt.post(f)
			}
			g.pass()
			if specApplied(f, 1, "run1", answer) == tt.drop || g.lease().RunID != "" {
				t.Fatalf("run1 applied %t, lease %+v; want it dropped %t, and the lease gone", specApplied(f, 1, "run1", answer), g.lease(), tt.drop)
			}
			if !tt.drop {
				return
			}
			g.pass()
			if is := f.State().Issues[0]; g.runs() != 2 || g.file("prompt.2") != "1a\n1b too" || !slices.Contains(is.Labels, "stage:spec-review") {
				t.Errorf("run 2 of %d saw %q, labels %v; want it to see both answers and be applied", g.runs(), g.file("prompt.2"), is.Labels)
			}
		})
	}
}

func TestNewInputDropWritesNothing(t *testing.T) {
	t.Parallel()
	resumeWrites(t, func(t testing.TB, f *githubtest.Fake, root string) {
		saveRun(t, root, "saved", okOut, interview(f))
		f.AddReaction(f.AddComment(1, "owner", "1b too"), app, "+1")
	}, nil, 0)
}

// After a failed apply, step 1 👍s an answer, and the resumed apply drops
// the result rather than lose it: the next run has it.
func TestAnswerAfterFailedApply(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.applyErr = errors.New("502 bad gateway")
	if err := g.d.Iterate(context.Background()); err == nil || g.lease().ApplyFailures != 1 {
		t.Fatalf("the first pass = %v, lease %+v; want the apply error, its lease kept", err, g.lease())
	}
	g.stage.applyErr = nil
	id := f.AddComment(1, "owner", "1b too")
	g.pass()
	is := f.State().Issues[0]
	if !slices.Contains(is.Comments[len(is.Comments)-1].Reactions, app+" +1") || specApplied(f, 1, "run1", id) || g.lease().RunID != "" || g.runs() != 1 {
		t.Fatalf("comments %+v, lease %+v, %d runs; want the answer 👍'd, run1 dropped, and no run yet", is.Comments, g.lease(), g.runs())
	}
	g.pass()
	if g.runs() != 2 || g.file("prompt.2") != "1a\n1b too" || !specApplied(f, 1, "run2", id) {
		t.Errorf("run 2 of %d saw %q; want both answers, applied", g.runs(), g.file("prompt.2"))
	}
}

// The Unlabeled stage runs only in a pass where no registered stage ran,
// on the newest issue with no stage label first, one a pass.
func TestUnlabeledRunsWhenNoStageRan(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	f.AddIssue("owner", "Older", "no labels")
	f.AddIssue("mallory", "Newer", "no labels")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	for range 4 {
		g.pass()
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{1, 3, 2}) {
		t.Errorf("the runs leased issues %v; want 1, the interviewer's, then 3 and 2, newest first", got)
	}
	is := f.State().Issues
	if !specApplied(f, 1, "run1", answer) || !slices.Equal(is[2].Labels, []string{"stage:clarifying"}) || !slices.Equal(is[1].Labels, []string{"stage:clarifying"}) {
		t.Errorf("labels %v, %v and %v; want 1 at spec-review, then 3 and 2 admitted", is[0].Labels, is[1].Labels, is[2].Labels)
	}
	if got := commentBodies(f, 3); !slices.Equal(got, []string{triageAdmitted("run2")}) {
		t.Errorf("#3's comments = %q, want only %q", got, triageAdmitted("run2"))
	}
	var l daemon.Lease
	if err := json.Unmarshal([]byte(g.file("lease.2")), &l); err != nil {
		t.Fatal(err)
	}
	if l.Stage != state.Intake || l.AttemptKey != "intake-0" {
		t.Errorf("the Unlabeled run's lease = %+v; want it at intake", l)
	}
}

// A pass where a registered stage ran starts no Unlabeled run, even with
// Unlabeled work pending.
func TestUnlabeledOneRunPerPass(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	f.AddIssue("owner", "Idea", "no labels")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{1}) || len(commentBodies(f, 2)) != 0 {
		t.Errorf("the first pass ran on %v, and #2 has comments %q; want only the interviewer's run, on #1", got, commentBodies(f, 2))
	}
}

// An issue a trusted user approved with no stage label is moved in by step
// 1 (IntakeVisit), before Work, so it's no Unlabeled work.
func TestUnlabeledLeavesApprovedToIntake(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("mallory", "Idea", "no labels")
	f.AddLabel(1, "owner", "approved")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	g.pass()
	if is := f.State().Issues[0]; g.runs() != 0 || !slices.Equal(is.Labels, []string{"approved", "stage:clarifying"}) {
		t.Errorf("%d runs, labels %v; want IntakeVisit's move and no triage run", g.runs(), is.Labels)
	}
}

// The issue a resume just handled gets no Unlabeled run in that pass, as
// its snapshot predates the resume, even when the result was dropped.
func TestUnlabeledSkipsResumedIssue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	f.AddComment(1, "owner", "More detail.") // after the run's seen, so its result is dropped
	root := t.TempDir()
	saveIntake(t, root, "saved", okOut)
	g := newRig(t, f, newOrigin(t), root)
	g.triage()
	g.claude(okOut, 0)
	if err := g.r.Work(context.Background(), g.ready(1)); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 0 || g.lease().RunID != "" || marked(f, 1, "saved") {
		t.Fatalf("%d runs, lease %+v; want the saved result dropped, and no run in that pass", g.runs(), g.lease())
	}
	if err := g.r.Work(context.Background(), g.ready(1)); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 1 || !marked(f, 1, "run1") {
		t.Errorf("%d runs in the next pass; want one, applied", g.runs())
	}
}

// A stage label that a rename puts on during an Unlabeled run holds the
// apply back, then, once it's left out, drops the result: it's no
// Unlabeled work while it's there.
func TestUnlabeledDropsOnLeftOutStageLabel(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	f.AddLabel(1, "mallory", "foo") // unguarded, so it stands
	f.Advance(time.Hour)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	ts := g.triage()
	ts.during = func() { f.RenameLabel("foo", "stage:planning") }
	g.claude(okOut, 0)
	g.pass()
	if l := g.lease(); l.RunID != "run1" || l.Failed {
		t.Fatalf("lease %+v during the hold; want run1's kept", l)
	}
	f.Advance(github.HoldBound)
	g.pass()
	if g.runs() != 1 || marked(f, 1, "run1") || g.lease().RunID != "" {
		t.Errorf("%d runs, lease %+v, comments %q; want run1 dropped once the label is left out, and no other run", g.runs(), g.lease(), commentBodies(f, 1))
	}
}

// loginErrTrust fails to check one login.
type loginErrTrust struct {
	trusted
	login string
}

func (e loginErrTrust) Trusted(ctx context.Context, login string) (bool, error) {
	if login == e.login {
		return false, errors.New("503 trust is down")
	}
	return e.trusted.Trusted(ctx, login)
}

// A failure to check whether a new commenter is trusted is a failure to
// apply: the lease stays, counting it, and nothing is written.
func TestNewInputTrustErrorIsApplyFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() {
		g.stage.during = nil
		f.AddComment(1, "flaky", "hello")
		g.d.Step.Trust = loginErrTrust{trusted{"owner": true}, "flaky"}
	}
	err := g.work(1)
	if l := g.lease(); err == nil || !strings.Contains(err.Error(), "503") || l.ApplyFailures != 1 || l.Failed || specApplied(f, 1, "run1", answer) {
		t.Errorf("Work = %v, lease %+v; want the trust error, as one failed apply, with the lease kept and nothing applied", err, l)
	}
}

func TestNewInputTrustErrorWritesNothing(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		saveRun(tb, roots[f], "saved", okOut, interview(f))
		f.AddComment(1, "flaky", "hello")
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, o, roots[f])
		g.d.Step.Trust = loginErrTrust{trusted{"owner": true}, "flaky"}
		writes := len(f.Writes())
		err := g.r.Work(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "503") {
			return fmt.Errorf("Work = %v; want the trust error", err)
		}
		if w := f.Writes(); len(w) != writes {
			return fmt.Errorf("the pass wrote %+v; want nothing", w[writes:])
		}
		if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures == 0 {
			return fmt.Errorf("the pass left the lease %+v; want it kept, counting the failed apply", l)
		}
		return nil
	})
}

// No Unlabeled run starts while the usage gate is closed.
func TestUnlabeledWaitsForTheGate(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	reset := g.now.Add(time.Hour)
	if !g.r.Gate.Observe(g.now, true, fmt.Sprintf("Claude usage limit reached|%d", reset.Unix())) {
		t.Fatal("the gate didn't close")
	}
	g.pass()
	if g.runs() != 0 {
		t.Fatalf("%d runs while the gate is closed; want none", g.runs())
	}
	g.now = reset
	g.pass()
	if got := commentBodies(f, 1); g.runs() != 1 || !slices.Equal(got, []string{triageAdmitted("run1")}) {
		t.Errorf("%d runs once it opened, comments %q; want one, applied", g.runs(), got)
	}
}

// A stage label a trusted user adds during an Unlabeled run drops its
// result, whether CheckLabels records it with a marker or not.
func TestUnlabeledDropsOnStageLabel(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"stage:clarifying", "stage:intake"} {
		t.Run(label, func(t *testing.T) {
			f := newFake(t)
			f.AddIssue("owner", "Idea", "no labels")
			g := newRig(t, f, newOrigin(t), t.TempDir())
			ts := g.triage()
			ts.during = func() { f.AddLabel(1, "owner", label) }
			g.claude(okOut, 0)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if g.runs() != 1 || marked(f, 1, "run1") || !slices.Equal(is.Labels, []string{label}) || g.lease().RunID != "" {
				t.Errorf("%d runs, labels %v, comments %+v, lease %+v; want run1 dropped, uncounted, and the owner's label kept", g.runs(), is.Labels, is.Comments, g.lease())
			}
		})
	}
}

func TestUnlabeledDropWritesNothing(t *testing.T) {
	t.Parallel()
	resumeWrites(t, func(t testing.TB, f *githubtest.Fake, root string) {
		f.AddIssue("owner", "Idea", "no labels")
		f.AddLabel(1, "owner", "stage:intake")
		saveIntake(t, root, "saved", okOut)
	}, func(g *rig) { g.triage() }, 0)
}

// A saved Unlabeled result is applied on resume, through Unlabeled, whose
// lease records intake, even after a crash part way.
func TestUnlabeledResume(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	root := t.TempDir()
	saveIntake(t, root, "saved", okOut)
	g := newRig(t, f, newOrigin(t), root)
	g.triage()
	if err := g.r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if is := f.State().Issues[0]; !slices.Equal(commentBodies(f, 1), []string{triageAdmitted("saved")}) || !slices.Equal(is.Labels, []string{"stage:clarifying"}) || len(g.leftovers()) != 0 {
		t.Errorf("labels %v, comments %+v, left %v; want the saved result applied, and nothing left", is.Labels, is.Comments, g.leftovers())
	}
}

func TestUnlabeledResumeReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.AddIssue("owner", "Idea", "no labels")
		roots[f] = t.TempDir()
		saveIntake(tb, roots[f], "saved", okOut)
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, o, roots[f])
		g.triage()
		g.claude(failOut, 1)
		return g.work(1)
	})
}

// Its runs get the interviewer's model, effort, turns and timeout.
func TestUnlabeledGetsInterviewerSettings(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	f.AddIssue("owner", "Idea", "no labels")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.allow()
	c := g.d.Config
	c.Stages.Model = config.PerStage[string]{Interviewer: "haiku", Planner: "opus", Implementer: "opus", Verifier: "opus"}
	c.Stages.Effort = config.PerStage[string]{Interviewer: "low", Planner: "max", Implementer: "max", Verifier: "max"}
	c.Stages.MaxTurns.Interviewer = 7
	g.triage()
	g.claude(okOut, 0)
	if err := g.work(2); err != nil {
		t.Fatal(err)
	}
	g.checkFlags(1, "haiku", "low", "")
	if got := g.flags(1)["--max-turns"]; got != "7" {
		t.Errorf("--max-turns = %q, want the interviewer's 7", got)
	}
	c.Stages.Timeouts.Interviewer = 200 * time.Millisecond
	// claude sleeps 120s, so a run that outlives the bound wasn't killed at
	// the timeout. The bound is generous so a loaded machine doesn't fail it.
	if err := os.WriteFile(g.path("claude", "sleep"), []byte("120"), 0o644); err != nil {
		t.Fatal(err)
	}
	const bound = time.Minute
	start := time.Now()
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if took, l := time.Since(start), g.lease(); took > bound || !l.Failed || l.Attempt != 1 || l.Issue != 1 {
		t.Errorf("the run took %s, lease %+v; want it killed at the interviewer's timeout, a failed attempt", took, l)
	}
}

// An issue with a stage label left out, or held back, is no Unlabeled
// work, until the label is removed.
func TestUnlabeledSkipsHeldLabels(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	f.AddLabel(1, "mallory", "foo") // unguarded, so it stands
	f.Advance(time.Hour)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	f.RenameLabel("foo", "stage:planning")
	for _, wait := range []time.Duration{0, github.HoldBound, time.Minute} {
		f.Advance(wait)
		g.pass()
		if g.runs() != 0 {
			t.Fatalf("%v on: %d runs, labels %v; want none while the stage label is held back or left out", wait, g.runs(), f.State().Issues[0].Labels)
		}
	}
	f.Advance(github.UnexplainedWait)
	g.pass()
	g.pass()
	if is := f.State().Issues[0]; g.runs() != 1 || !slices.Equal(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("%d runs, labels %v once the label is removed; want one run, applied", g.runs(), is.Labels)
	}
}

// An issue at a stage with no Stage, or with a transition marker but no
// stage label, or with a stage label set since step 1 read it, gets no
// Unlabeled run.
func TestUnlabeledSkipsPipelineIssues(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "planning", "a plan")
	f.AddIssue("owner", "Moved", "no labels")
	f.AddComment(2, app, "<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
	f.AddIssue("owner", "Labeled late", "no labels")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.triage()
	g.claude(okOut, 0)
	var ready []daemon.Ready
	for n := 1; n <= 3; n++ {
		ready = append(ready, daemon.Ready{S: g.snapshot(n)}) // no step 1, which would set #2's label
	}
	f.AddLabel(3, "owner", "stage:intake")
	writes := len(f.Writes())
	if err := g.r.Work(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 0 || len(f.Writes()) != writes {
		t.Errorf("%d runs, writes %+v; want none", g.runs(), f.Writes()[writes:])
	}
}

// The Unlabeled stage is capped like any other: after loop.max_attempts
// failed runs the issue gets the attempts state error, and the next issue
// gets its turn. An ended lease's count carries over.
func TestUnlabeledAttemptCap(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Older", "no labels")
	f.AddIssue("owner", "Newer", "no labels")
	root := t.TempDir()
	l, err := json.Marshal(daemon.Lease{Issue: 2, Stage: state.Intake, RunID: "ended", Clone: filepath.Join(root, "runs", "ended"),
		AttemptKey: "intake-0", Attempt: 1, Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "lease.json"), l, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newRig(t, f, newOrigin(t), root)
	g.d.Config.Loop.MaxAttempts = 2
	g.triage()
	g.claude(failOut, 1)
	for range 2 {
		if err := g.r.Work(context.Background(), g.ready(1, 2)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"<!-- issuedog:error kind=attempts key=intake-0 -->"}
	if got := g.leaseIssues(); !slices.Equal(got, []int{2, 1}) || !reflect.DeepEqual(errorMarkers(f, 2), want) || len(errorMarkers(f, 1)) != 0 {
		t.Errorf("the runs leased %v, errors on #2 %q and #1 %q; want #2's second attempt capped, then #1's turn", got, errorMarkers(f, 2), errorMarkers(f, 1))
	}
}

// Finding Unlabeled work reads nothing more: an unchanged pass with an
// issue the Unlabeled stage has no work for, whose edited body needs a
// history read for its dirt, sends only conditional GETs answered 304.
func TestUnlabeledUnchangedPass(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Idea", "no labels")
	f.EditBody(1, "owner", "no labels, edited")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	ts := g.triage()
	asked := 0
	ts.pending = func(daemon.Input) bool { asked++; return false }
	g.pass()
	histories, from := f.HistoryQueries(), len(f.Requests())
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if asked != 2 || histories == 0 || f.HistoryQueries() != histories || len(f.Writes()) != 0 || g.runs() != 0 {
		t.Errorf("Pending asked %d times, %d history queries then %d more, writes %+v, %d runs; want it asked each pass, and nothing else",
			asked, histories, f.HistoryQueries()-histories, f.Writes(), g.runs())
	}
}

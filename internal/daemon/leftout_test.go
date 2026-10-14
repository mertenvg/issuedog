package daemon_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// priorityP1 is a Stage's pending: when /priority p1 is among the comments.
func priorityP1(in daemon.Input) bool {
	return slices.ContainsFunc(in.S.Comments, func(c github.Comment) bool { return c.Body == "/priority p1" })
}

// watchRig makes g's Work note the labels it gets issue 1 with, each pass
// it gets it, before running the stage work.
func watchRig(g *rig) *[][]string {
	var seen [][]string
	g.d.Work = func(ctx context.Context, rs []daemon.Ready) error {
		for _, r := range rs {
			if r.S.Number == 1 {
				seen = append(seen, slices.Clone(r.S.Labels))
			}
		}
		return g.r.Work(ctx, rs)
	}
	return &seen
}

// A label left out stays out when the App writes a label in step 1, here
// for /priority, and Work runs in the same pass: the run's settings, state
// errors, stage and labels come from the labels without it.
func TestLeftOutLabelStaysOutAfterAWrite(t *testing.T) {
	tests := []struct {
		name, to string
		setup    func(f *githubtest.Fake)
		check    func(t *testing.T, f *githubtest.Fake, g *rig)
	}{
		{name: "effort", to: "effort:max", check: func(t *testing.T, _ *githubtest.Fake, g *rig) {
			if got := g.flags(1)["--effort"]; got == "max" {
				t.Errorf("the run's --effort = %q, from the left-out effort:max", got)
			}
		}},
		{name: "model", to: "model:haiku",
			setup: func(f *githubtest.Fake) { f.AddLabel(1, app, "model:sonnet") },
			check: func(t *testing.T, _ *githubtest.Fake, g *rig) { g.checkFlags(1, "sonnet", "", "") }},
		{name: "stage", to: "stage:held"},
		{name: "priority", to: "priority:p0", check: func(t *testing.T, f *githubtest.Fake, _ *rig) {
			if labels := f.State().Issues[0].Labels; !slices.Contains(labels, "priority:p1") {
				t.Errorf("labels %q; want priority:p1 added", labels)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			f.AddLabel(1, "mallory", "foo")
			if tt.setup != nil {
				tt.setup(f)
			}
			f.Advance(time.Hour)
			o := newOrigin(t)
			o.commit(config.Path, "types: labels\nmodels:\n  allowed: [opus, sonnet, haiku]\n")
			g := newRig(t, f, o, t.TempDir())
			g.claude(okOut, 0)
			g.stage.pending = priorityP1
			seen := watchRig(g)
			f.RenameLabel("foo", tt.to)
			g.pass() // held back
			f.Advance(github.HoldBound)
			g.pass() // left out, with nothing pending
			f.AddComment(1, "owner", "/priority p1")
			g.pass()
			if g.runs() != 1 || len(errorMarkers(f, 1)) != 0 {
				t.Fatalf("%d runs, errors %q; want one run, and no error", g.runs(), errorMarkers(f, 1))
			}
			for _, labels := range *seen {
				if slices.Contains(labels, tt.to) {
					t.Errorf("Work got issue 1 with the left-out %s: %q", tt.to, labels)
				}
			}
			if tt.check != nil {
				tt.check(t, f, g)
			}
		})
	}
}

// A type label left out gives the issue no type, also after a label write
// in the same pass.
func TestLeftOutTypeStaysOutAfterAWrite(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "do it")
	f.AddLabel(1, "mallory", "foo")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	var got []*github.Snapshot
	d.Work = func(_ context.Context, rs []daemon.Ready) error {
		for _, r := range rs {
			got = append(got, r.S)
		}
		return nil
	}
	iterate(t, d)
	f.RenameLabel("foo", "Type:Epic")
	f.AddComment(1, "owner", "poke")
	f.Advance(time.Minute)
	iterate(t, d) // held back
	f.AddComment(1, "owner", "/priority p1")
	f.Advance(time.Minute)
	iterate(t, d)
	s := got[len(got)-1]
	if s.Type != "" || slices.Contains(s.Labels, "Type:Epic") || !slices.Contains(s.Labels, "priority:p1") {
		t.Errorf("Work got labels %q, type %q; want priority:p1, Type:Epic left out, and no type", s.Labels, s.Type)
	}
}

// A stage label for another stage, left out and renamed every four minutes,
// doesn't stop the issue's runs: the read just before a run leaves it out
// too.
func TestLeftOutStageLetsTheIssueRun(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "do it")
	f.AddLabel(1, "mallory", "foo")
	f.AddComment(1, "owner", "go")
	f.Advance(time.Hour)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.pending = func(daemon.Input) bool { return true }
	g.r.Stages = map[state.Stage]daemon.Stage{state.Ready: g.stage}
	f.RenameLabel("foo", "stage:held")
	g.pass() // held back
	f.Advance(time.Minute)
	f.RenameLabel("stage:held", "stage:in-progress")
	g.pass()
	// The fake stage asks for a move the table lacks from ready, so a
	// transition error follows the run; no other is due.
	bad := slices.ContainsFunc(errorMarkers(f, 1), func(m string) bool { return !strings.Contains(m, "kind=transition") })
	if g.runs() != 1 || bad {
		t.Errorf("%d runs, errors %q, labels %q; want the run, with the stage label left out", g.runs(), errorMarkers(f, 1), f.State().Issues[0].Labels)
	}
}

// settingDaemon is a daemon on f whose step 1 has the setting commands,
// with opus and sonnet allowed, and mem as its memory of unexplained labels.
func settingDaemon(t *testing.T, f *githubtest.Fake, o *origin, mem *github.Unexplained) *daemon.Daemon {
	d := newDaemon(t, f, o, nil)
	d.Step.Unexplained = mem
	allowed := models(t, "[opus, sonnet]")
	maps.Copy(d.Step.Commands, daemon.SettingCommands(d.Step.CurrentTrust(), func() config.Models { return allowed }))
	return d
}

// The App's write of a label that's on the issue but left out, as for
// /model opus just after a rename onto model:opus, makes the App's own
// event: the label stays past the wait, and no revert is posted. It's safe
// to replay.
func TestAppWriteOfLeftOutLabelStands(t *testing.T) {
	for _, tt := range []struct{ rename, cmd string }{{"model:opus", "/model opus"}, {"priority:p1", "/priority p1"}} {
		t.Run(tt.cmd, func(t *testing.T) {
			o := newOrigin(t)
			setup := func(tb testing.TB) *githubtest.Fake {
				f := newFake(tb)
				fileAt(f, "ready", "do it")
				f.AddLabel(1, "mallory", "foo")
				f.Advance(time.Hour)
				f.RenameLabel("foo", tt.rename)
				d := settingDaemon(t, f, o, memoryOf(f))
				for range 2 { // held back, then left out
					if err := d.Iterate(context.Background()); err != nil {
						tb.Fatal(err)
					}
					f.Advance(time.Minute)
				}
				if w := f.Writes(); len(w) != 0 {
					tb.Fatalf("writes %+v before the command, want none", w)
				}
				f.AddComment(1, "owner", tt.cmd)
				return f
			}
			githubtest.Replay(t, setup, func(f *githubtest.Fake) error {
				return settingDaemon(t, f, o, memoryOf(f)).Iterate(context.Background())
			})
			f := setup(t)
			d := settingDaemon(t, f, o, memoryOf(f))
			for range 8 {
				iterate(t, d)
				f.Advance(time.Minute)
			}
			is := f.State().Issues[0]
			var reacts []string
			for _, c := range is.Comments {
				if c.Body == tt.cmd {
					reacts = c.Reactions
				}
				if strings.Contains(c.Body, "issuedog:revert") {
					t.Errorf("a revert comment: %q", c.Body)
				}
			}
			if !slices.Contains(reacts, app+" +1") || !slices.Contains(is.Labels, tt.rename) {
				t.Errorf("reactions %q, labels %q eight minutes on; want %s applied and %s kept", reacts, is.Labels, tt.cmd, tt.rename)
			}
		})
	}
}

// A new issue the owner files with labels, a stage label among them, whose
// events are a second or two late, is held back once, then is clean: no
// revert or state error, and its labels kept.
func TestIntakeLagIsClean(t *testing.T) {
	f := newFake(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	seen := watchWork(d)
	iterate(t, d)
	f.LagEvents(2 * time.Second)
	n := f.AddIssue("owner", "Bug", "broken")
	f.AddLabel(n, "owner", "type:bug")
	f.AddLabel(n, "owner", "priority:p1")
	f.AddLabel(n, "owner", "stage:intake")
	iterate(t, d)
	if got := (*seen)[1]; got != nil {
		t.Errorf("Work got the issue before its label events: %q", got)
	}
	for range 2 {
		f.Advance(time.Minute)
		iterate(t, d)
	}
	is := f.State().Issues[0]
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:revert") || strings.Contains(c.Body, "issuedog:error") {
			t.Errorf("a revert or state error: %q", c.Body)
		}
	}
	if want := []string{"priority:p1", "stage:intake", "type:bug"}; !slices.Equal(is.Labels, want) {
		t.Errorf("labels %q, want %q", is.Labels, want)
	}
}

// An issue with a label left out isn't read again until the label is due
// for removal: then it is, and the label is removed.
func TestLeftOutIssueReadOnlyWhenDue(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "do it")
	f.AddLabel(1, "mallory", "foo")
	fileAt(f, "ready", "other")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameLabel("foo", "scope:x")
	f.AddComment(1, "owner", "poke")
	iterate(t, d) // held back, and first seen
	reads := func() int {
		n := 0
		for _, r := range f.Requests() {
			if r.Method == "GET" && (r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/timeline")) {
				n++
			}
		}
		return n
	}
	f.Advance(time.Minute)
	iterate(t, d) // left out
	before := reads()
	for range 3 {
		f.Advance(time.Minute)
		iterate(t, d)
	}
	if n := reads() - before; n != 0 {
		t.Errorf("issue 1 read %d times while its label wasn't due, want none", n)
	}
	f.Advance(time.Minute) // five minutes since it was first seen
	iterate(t, d)
	if reads() == before || slices.Contains(f.State().Issues[0].Labels, "scope:x") {
		t.Errorf("labels %q once due; want issue 1 read again, and scope:x removed", f.State().Issues[0].Labels)
	}
}

// A stage label for another stage, left out, doesn't stop the attempt cap's
// state error, which the issue read again would take for a move on.
func TestLeftOutStageKeepsCapError(t *testing.T) {
	f, g, answer := heldRig(t, 1)
	g.claude(failOut, 1)
	g.stage.pending = priorityP1
	f.RenameLabel("foo", "stage:held")
	g.pass() // held back
	f.Advance(github.HoldBound)
	g.pass()
	f.AddComment(1, "owner", "/priority p1")
	g.pass()
	if want := []string{capError(answer)}; g.runs() != 1 || !slices.Equal(errorMarkers(f, 1), want) {
		t.Errorf("%d runs, errors %q; want one failed run, and %q", g.runs(), errorMarkers(f, 1), want)
	}
}

// A stage:held label left out on a sub-issue isn't a hold /resume would
// end there.
func TestLeftOutHoldIsNoHoldForResume(t *testing.T) {
	f := chain(t) // 1 > 2 > 3
	f.AddLabel(3, "mallory", "foo")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameLabel("foo", "stage:held")
	f.AddComment(3, "mallory", "poke")
	iterate(t, d) // held back
	f.Advance(time.Minute)
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	var reply string
	for _, c := range f.State().Issues[0].Comments {
		if strings.Contains(c.Body, "issuedog:reply reason=rejected") {
			reply = c.Body
		}
	}
	if !strings.Contains(reply, "nothing here or in its open sub-issues is on an owner hold") {
		t.Errorf("reply %q; want /resume rejected as there's no hold", reply)
	}
}

package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// usageOut is okOut with modelUsage keyed by models.
func usageOut(t testing.TB, models ...string) string {
	t.Helper()
	usage := map[string]any{}
	for _, m := range models {
		usage[m] = map[string]int{"inputTokens": 1}
	}
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(okOut, `"result":"Done.",`, `"result":"Done.","modelUsage":`+string(data)+",", 1)
}

// allow puts opus, sonnet and haiku on g's models.allowed.
func (g *rig) allow() {
	g.d.Config.Models.Allowed = []config.Model{{Name: "opus"}, {Name: "sonnet"}, {Name: "haiku"}}
}

// flags returns the flags the fake claude's run n got, each with the
// argument after it, or "" if that's another flag.
func (g *rig) flags(n int) map[string]string {
	g.t.Helper()
	args := strings.Split(g.file("args."+strconv.Itoa(n)), "\n")
	flags := map[string]string{}
	for i, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		flags[a] = ""
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			flags[a] = args[i+1]
		}
	}
	return flags
}

// checkFlags checks the model, effort and fallback flags of run n, where
// "" is no flag.
func (g *rig) checkFlags(n int, model, effort, fallback string) {
	g.t.Helper()
	flags := g.flags(n)
	for flag, want := range map[string]string{"--model": model, "--effort": effort, "--fallback-model": fallback} {
		got, ok := flags[flag]
		if want == "" && ok || want != "" && got != want {
			g.t.Errorf("run %d: %s = %q (given %t); want %q, or no flag for \"\"", n, flag, got, ok, want)
		}
	}
}

// newestBody is the body of issue 1's newest comment.
func newestBody(f *githubtest.Fake) string {
	comments := f.State().Issues[0].Comments
	return comments[len(comments)-1].Body
}

// A run gets its stage's defaults from config, and none when they're unset.
func TestRunGetsStageDefaults(t *testing.T) {
	for name, tt := range map[string]struct {
		model, effort string
		fallback      []string
	}{
		"set":   {model: "sonnet", effort: "high", fallback: []string{"haiku"}},
		"unset": {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.allow()
			g.d.Config.Stages.Model.Interviewer = tt.model
			g.d.Config.Stages.Effort.Interviewer = tt.effort
			g.d.Config.Stages.FallbackModel.Interviewer = tt.fallback
			g.claude(okOut, 0)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if g.runs() != 1 {
				t.Fatalf("%d runs; want 1", g.runs())
			}
			g.checkFlags(1, tt.model, tt.effort, strings.Join(tt.fallback, ","))
			var l daemon.Lease
			if err := json.Unmarshal([]byte(g.file("lease.1")), &l); err != nil {
				t.Fatal(err)
			}
			if l.Model != tt.model || l.Effort != tt.effort || strings.Join(l.FallbackModels, ",") != strings.Join(tt.fallback, ",") {
				t.Errorf("the lease during the run has %q, %q, %q; want %q, %q, %q", l.Model, l.Effort, l.FallbackModels, tt.model, tt.effort, tt.fallback)
			}
		})
	}
}

// model:opus applies to the interviewer's run, but not to the verifier's
// on the same issue, which uses its own default.
func TestModelLabelSkipsVerifier(t *testing.T) {
	f := newFake(t)
	interview(f)
	f.AddLabel(1, "owner", "model:opus")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.allow()
	g.d.Config.Stages.Model.Verifier = "haiku"
	g.stage.moreQuestions = true // stays at clarifying, for the verifier
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	g.stage.agent = "verifier"
	f.AddReaction(f.AddComment(1, "owner", "1b"), app, "+1")
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 2 {
		t.Fatalf("%d runs; want 2", g.runs())
	}
	g.checkFlags(1, "opus", "", "")
	g.checkFlags(2, "haiku", "", "")
}

// Each implementer attempt after the first runs one effort level higher.
func TestRetryRaisesEffort(t *testing.T) {
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 3
	g.d.Config.Stages.Effort.Implementer = "medium"
	g.stage.agent = "implementer"
	g.stage.pending = func(daemon.Input) bool { return true }
	g.claude(failOut, 1)
	ctx := context.Background()
	s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: s.Comments[1:2]}}); err != nil {
			t.Fatal(err)
		}
	}
	if g.runs() != 3 {
		t.Fatalf("%d runs; want 3", g.runs())
	}
	for n, want := range []string{"medium", "high", "xhigh"} {
		g.checkFlags(n+1, "", want, "")
	}
}

// A label changed during a run changes only the next run's flags: the run
// and its marker keep the settings it started with.
func TestLabelChangedDuringRun(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	f.AddLabel(1, "owner", "model:opus")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.allow()
	g.stage.moreQuestions = true
	g.stage.during = func() {
		g.stage.during = nil
		f.RemoveLabel(1, "owner", "model:opus")
		f.AddLabel(1, "owner", "model:sonnet")
	}
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("<!-- issuedog:transition from=clarifying to=clarifying run=run1 model=opus seen=%d -->", answer)
	if last := newestBody(f); !strings.HasSuffix(last, want) {
		t.Errorf("newest comment %q; want it to end with %q", last, want)
	}
	f.AddReaction(f.AddComment(1, "owner", "1b"), app, "+1")
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 2 {
		t.Fatalf("%d runs; want 2", g.runs())
	}
	g.checkFlags(1, "opus", "", "")
	g.checkFlags(2, "sonnet", "", "")
}

// The run marker records the run's model and effort, and the models that
// ran, a fallback among them; a modelUsage key a marker can't hold is left
// out, and the result still applies.
func TestRunMarkerRecordsSettings(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.allow()
	g.d.Config.Stages.Model.Interviewer = "opus"
	g.d.Config.Stages.Effort.Interviewer = "high"
	g.d.Config.Stages.FallbackModel.Interviewer = []string{"sonnet"}
	g.claude(usageOut(t, "claude-sonnet-5", "claude-opus-5", "odd key", "a<b", "x,y", "--> <!-- issuedog:hold -->"), 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	g.checkFlags(1, "opus", "high", "sonnet")
	want := fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review run=run1 effort=high model=opus ran=claude-opus-5,claude-sonnet-5 seen=%d -->", answer)
	if last := newestBody(f); !strings.HasSuffix(last, want) || strings.Count(last, "<!--") != 1 {
		t.Errorf("newest comment %q; want one marker, %q", last, want)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want the run applied and cleaned up", left)
	}
}

// After a restart, a saved result is applied with the settings in the
// lease, not those the labels give by then.
func TestResumeUsesLeaseSettings(t *testing.T) {
	f := newFake(t)
	answer := interview(f)
	f.AddLabel(1, "owner", "model:sonnet")
	f.AddLabel(1, "owner", "effort:low")
	root := t.TempDir()
	saveRun(t, root, "saved", usageOut(t, "claude-opus-5"), answer)
	path := filepath.Join(root, "state", "lease.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var l daemon.Lease
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatal(err)
	}
	l.Model, l.Effort = "opus", "high"
	if data, err = json.Marshal(l); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newRig(t, f, newOrigin(t), root)
	g.allow()
	g.claude(failOut, 1)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review run=saved effort=high model=opus ran=claude-opus-5 seen=%d -->", answer)
	if last := newestBody(f); !strings.HasSuffix(last, want) || g.runs() != 0 {
		t.Errorf("newest comment %q after %d runs; want the saved result applied with no run, ending %q", last, g.runs(), want)
	}
}

// Labels that give no settings start no run and use no attempt. They get
// one state error, its text escaped, and another only when they change and
// are still wrong.
func TestBadLabelsReportOnce(t *testing.T) {
	f := newFake(t)
	interview(f)
	f.AddLabel(1, "owner", "model:opus")
	f.AddLabel(1, "owner", "model:<!--x")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.allow()
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = func(daemon.Input) bool { return true }
	g.claude(okOut, 0)
	labelErrors := func() []string {
		var bodies []string
		for _, c := range f.State().Issues[0].Comments {
			if strings.Contains(c.Body, "<!-- issuedog:error kind=labels key=1-") {
				bodies = append(bodies, c.Body)
			}
		}
		return bodies
	}
	for range 2 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	errs := labelErrors()
	if g.runs() != 0 || len(errs) != 1 {
		t.Fatalf("after 2 passes: %d runs, label errors %q; want no run and one error", g.runs(), errs)
	}
	if body := errs[0]; strings.Count(body, "<!--") != 1 || !strings.Contains(body, `"model:&lt;!--x"`) || !strings.Contains(body, "interviewer") {
		t.Errorf("the error says %q; want the interviewer's run named, the label quoted and escaped, and only the marker unescaped", body)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease", left)
	}

	f.RemoveLabel(1, "owner", "model:<!--x")
	f.AddLabel(1, "owner", "effort:extreme")
	for range 2 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if errs := labelErrors(); g.runs() != 0 || len(errs) != 2 || !strings.Contains(errs[1], `"effort:extreme" is not one of`) {
		t.Fatalf("after another bad set: %d runs, label errors %q; want no run and a second error naming effort:extreme", g.runs(), errs)
	}

	f.RemoveLabel(1, "owner", "effort:extreme")
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 1 || !strings.HasPrefix(newestBody(f), "The spec") {
		t.Errorf("after the fix: %d runs, newest comment %q; want a run, applied, though max_attempts is 1", g.runs(), newestBody(f))
	}
	g.checkFlags(1, "opus", "", "")
}

func TestBadLabelsReplay(t *testing.T) {
	replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) {
		interview(f)
		f.AddLabel(1, "owner", "model:opus")
		f.AddLabel(1, "owner", "model:sonnet")
	}, func(g *rig) {
		g.allow()
		g.stage.pending = func(daemon.Input) bool { return true }
		g.claude(okOut, 0)
	})
}

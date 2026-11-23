package daemon_test

import (
	"context"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// A story branch's tip gets its issuedog/tip check (SPEC §7.5, §8.1 step
// 4, §10.4), on a verdictRig wired as main's, with real temporary repos:
// story 1, at planned, whose branch the test cuts or pushes to in the
// origin itself, as nothing in the daemon cuts one yet.

// tipBranch is story 1's branch, from its title, tipStoryTitle.
const tipBranch = "issuedog/1-quote-expiry"

// tipStoryTitle is story 1's title.
const tipStoryTitle = "Quote expiry"

// tipStory files issue 1 on f, a story at planned with priority:p1, as the
// App moved it there.
func tipStory(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	if n := tipStoryAt(f, tipStoryTitle, "planned", "type:story"); n != 1 {
		t.Fatalf("the story is #%d, want #1", n)
	}
}

// tipStoryAt files an issue titled title at stage, as the App moved it
// there, with priority:p1 and labels, and returns its number.
func tipStoryAt(f *githubtest.Fake, title, stage string, labels ...string) int {
	n := f.AddIssue("owner", title, "The story.", append([]string{"priority:p1"}, labels...)...)
	f.AddLabel(n, app, "stage:"+stage)
	f.AddComment(n, app, "<!-- issuedog:transition from=plan-review to="+stage+" run=rp -->")
	return n
}

// tipCut cuts branch from main in v's origin, as the App's pick will, and
// returns its tip, main's SHA.
func (v *verdictRig) tipCut(branch string) string {
	v.t.Helper()
	gitIn(v.t, v.o.work, "push", "--quiet", v.o.url, "main:refs/heads/"+branch)
	return v.mainSHA()
}

// tipPush commits files on branch in v's origin, as a person's push does,
// each path to its content, or deleted for "", and returns the new tip.
func (v *verdictRig) tipPush(branch string, files map[string]string) string {
	v.t.Helper()
	w := v.o.work
	gitIn(v.t, w, "fetch", "--quiet", v.o.url, "refs/heads/"+branch)
	gitIn(v.t, w, "checkout", "--quiet", "-B", "story", "FETCH_HEAD")
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if files[path] == "" {
			gitIn(v.t, w, "rm", "--quiet", path)
			continue
		}
		full := filepath.Join(w, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			v.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			v.t.Fatal(err)
		}
		gitIn(v.t, w, "add", path)
	}
	gitIn(v.t, w, "commit", "--quiet", "-m", "a person's push")
	tip := gitIn(v.t, w, "rev-parse", "HEAD")
	gitIn(v.t, w, "push", "--quiet", v.o.url, "story:refs/heads/"+branch)
	gitIn(v.t, w, "checkout", "--quiet", "main")
	return tip
}

// tipCheck is the App's issuedog/tip on tip, with conclusion, naming
// branch, as State has it.
func tipCheck(tip, branch, conclusion string) githubtest.CheckRunState {
	return githubtest.CheckRunState{SHA: tip, Name: github.TipCheck, Conclusion: conclusion, AppID: githubtest.AppID, ExternalID: "branch=" + branch}
}

// tipReads counts the requests since from that read tip's check runs.
func tipReads(f *githubtest.Fake, from int, tip string) int {
	return prMainReads(f, from, tip)
}

// tipLast is the last check run the App posted on f, as the POST sent it.
func tipLast(t *testing.T, f *githubtest.Fake) prMoveCheck {
	t.Helper()
	checks := prMoveChecks(t, f)
	if len(checks) == 0 {
		t.Fatal("the App posted no check run")
	}
	return checks[len(checks)-1]
}

// tipQuiet runs an unchanged pass of v, which must make no request but
// 304s, write nothing, run no command, and make no request for a commit's
// check runs.
func (v *verdictRig) tipQuiet(step string) {
	v.t.Helper()
	from := len(v.f.Requests())
	v.quietPass(step)
	for _, r := range v.f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/commits/") {
			v.t.Errorf("%s: the unchanged pass sent %+v; want nothing for a tip", step, r)
		}
	}
}

// tipJudged runs a pass of v, which must check tip, story 1's branch's tip,
// running the commands commands times, and leave the check runs want.
func (v *verdictRig) tipJudged(tip string, commands int, want []githubtest.CheckRunState) {
	v.t.Helper()
	ran := v.checksRan()
	v.pass()
	if got := v.f.State().CheckRuns; !slices.Equal(got, want) || v.checksRan() != ran+commands {
		v.t.Fatalf("the pass on the tip %s left the check runs %+v, and ran the commands %d times; want %+v, and %d", tip, got, v.checksRan()-ran, want, commands)
	}
}

// A story with no branch gets no check, and its passes make no request for
// one. Once its branch is cut from main, the next pass checks the new tip,
// main's commit: the commands run once there, and issuedog/tip is posted
// on it as success, naming the branch in its external_id, titled for the
// tip, its summary naming the tip, the branch, the story and its baseline,
// with each command's result, linking the story, and no issuedog/verify.
// The passes after make no request for it and run no command. A fresh
// daemon reads the tip's checks once, finds the one naming the branch, and
// runs nothing, and its passes after are quiet too.
func TestTipCheckCut(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.passes(2)
	v.tipQuiet("no branch")

	tip := v.tipCut(tipBranch)
	v.tipJudged(tip, 1, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")})
	last := tipLast(t, f)
	summary := "issuedog checked `" + tip + "`, the tip of `" + tipBranch + "` for story #1, against the story's baseline, `main` at `" + tip + "`. " +
		"The checks passed.\n\n| Check | Result |\n|---|---|\n| `echo ran >> '" + v.ran + "'` | pass |\n| `echo every test passed` | pass |\n| Rules 2, 3 and 7 | pass |"
	if last.Name != github.TipCheck || last.Status != "completed" || last.Output.Title != "issuedog checked this story branch's tip" ||
		last.Output.Summary != summary || last.DetailsURL != "https://github.com/o/r/issues/1" {
		t.Errorf("the check = %+v; want %s, completed, titled for the tip, with the summary\n%s\nand a link to the story", last, github.TipCheck, summary)
	}
	v.tipQuiet("after the check")
	v.tipQuiet("again")

	v = v.restart()
	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	if got := tipReads(f, from, tip); got != 1 || v.checksRan() != ran || len(f.State().CheckRuns) != 1 {
		t.Errorf("the restart's first pass read the tip's checks %d times, ran the commands %d times, and left the checks %+v; want once, none, and one",
			got, v.checksRan()-ran, f.State().CheckRuns)
	}
	v.tipQuiet("after the restart")
}

// A tip a person pushed to the branch after its cut was checked is checked
// the same way: the next pass runs the commands once on it, and posts
// success there, naming the branch, and the cut's check stays.
func TestTipCheckPersonPush(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	want := []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")}
	v.tipJudged(cut, 1, want)
	tip := v.tipPush(tipBranch, map[string]string{"person.txt": "a person's change\n"})
	want = append(want, tipCheck(tip, tipBranch, "success"))
	slices.SortStableFunc(want, func(a, b githubtest.CheckRunState) int { return strings.Compare(a.SHA, b.SHA) })
	v.tipJudged(tip, 1, want)
	if last := tipLast(t, f); !strings.Contains(last.Output.Summary, "against the story's baseline, `main` at `"+cut+"`") {
		t.Errorf("the check's summary =\n%s\nwant it to name the baseline %s", last.Output.Summary, cut)
	}
	v.tipQuiet("after the push's check")
}

// tipFailed runs a pass of v on tip, story 1's branch's tip, which must
// post issuedog/tip as failure there, naming the branch, titled for a
// failure, with a summary starting as one does and holding each of
// sections, the commands having run commands times.
func (v *verdictRig) tipFailed(cut, tip string, commands int, sections ...string) {
	v.t.Helper()
	want := []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success"), tipCheck(tip, tipBranch, "failure")}
	slices.SortStableFunc(want, func(a, b githubtest.CheckRunState) int { return strings.Compare(a.SHA, b.SHA) })
	v.tipJudged(tip, commands, want)
	last := tipLast(v.t, v.f)
	intro := "issuedog checked `" + tip + "`, the tip of `" + tipBranch + "` for story #1, against the story's baseline, `main` at `" + cut + "`. It fails its checks."
	if last.Output.Title != "This story branch's tip fails its checks" || !strings.HasPrefix(last.Output.Summary, intro) {
		v.t.Errorf("the check = %+v; want it titled for a failure, its summary starting\n%s", last, intro)
	}
	for _, s := range sections {
		if !strings.Contains(last.Output.Summary, s) {
			v.t.Errorf("the check's summary =\n%s\nwant it to hold\n%s", last.Output.Summary, s)
		}
	}
	v.tipQuiet("after the failure")
}

// A tip whose command fails: the commands run once, every one, and
// issuedog/tip is failure, giving the failed command.
func TestTipCheckFailingCommand(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	v.tipFailed(cut, tip, 1, "This command failed, exit status 1:\n\n```text\ntest ! -f breaks.txt\n```")
}

// A tip that changes a protected path fails rule 2 against the story's
// baseline, and that stops the checks: no command runs, and issuedog/tip
// is failure, naming the path under rule 2.
func TestTipCheckRule2(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	tip := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	v.tipFailed(cut, tip, 0, "### Rule 2: protected paths", "secrets/key.txt")
}

// A tip that deletes a test file fails rule 3, on the diff from the
// story's baseline: the commands run, and issuedog/tip is failure, naming
// the file under rule 3.
func TestTipCheckDeletedTest(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.o.commit("quote_test.go", "package quote\n")
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	tip := v.tipPush(tipBranch, map[string]string{"quote_test.go": ""})
	v.tipFailed(cut, tip, 1, "### Rule 3: test deletions and skip markers", "quote_test.go")
}

// A tip that doesn't merge cleanly with main, which changed the same file
// since the branch was cut, fails rule 7 against main: the commands run,
// and issuedog/tip is failure, naming main and the conflicting file under
// rule 7.
func TestTipCheckConflict(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	main := v.prMainCommit("f.txt", "main's own text\n")
	tip := v.tipPush(tipBranch, map[string]string{"f.txt": "the story's text\n"})
	v.tipFailed(cut, tip, 1, "### Rule 7: merge with the target", "doesn't merge cleanly with its target "+main, "- f.txt")
}

// A tip with no merge base with main, as an orphan pushed to the branch,
// has no baseline, so it fails rule 2, as the checks at verifying judge a
// head with none: no command runs, and issuedog/tip is failure, saying so.
func TestTipCheckNoBaseline(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.passes(2)
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "--orphan", "orphan")
	gitIn(t, w, "commit", "--quiet", "-m", "an orphan")
	tip := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", v.o.url, "orphan:refs/heads/"+tipBranch)
	gitIn(t, w, "checkout", "--quiet", "-f", "main")
	v.tipJudged(tip, 0, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "failure")})
	last := tipLast(t, f)
	intro := "issuedog checked `" + tip + "`, the tip of `" + tipBranch + "` for story #1, which has no one merge base with `main`, so no baseline. It fails its checks."
	if !strings.HasPrefix(last.Output.Summary, intro) || !strings.Contains(last.Output.Summary, "### Rule 2: protected paths") ||
		!strings.Contains(last.Output.Summary, "so the story has no baseline") {
		t.Errorf("the check's summary =\n%s\nwant it to start\n%s\nand give rule 2's finding", last.Output.Summary, intro)
	}
	v.tipQuiet("after the failure")
}

// Only the App's issuedog/tip naming the branch counts for its tip (SPEC
// §7.5): with one naming another branch, as on a commit that's the tip of
// two, another App's issuedog/tip, or the App's issuedog/verify, such as a
// task's, on the tip, the next pass checks the tip, running the commands
// once, and posts its own issuedog/tip naming the branch. The passes after
// are quiet.
func TestTipCheckOtherChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		check func(t *testing.T, f *githubtest.Fake, tip string) githubtest.CheckRunState
	}{
		{"another branch's", func(t *testing.T, f *githubtest.Fake, tip string) githubtest.CheckRunState {
			if _, err := f.Client(t).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
				Title: "t", Summary: "s", ExternalID: "branch=issuedog/9-another-story"}); err != nil {
				t.Fatal(err)
			}
			return tipCheck(tip, "issuedog/9-another-story", "success")
		}},
		{"another App's", func(_ *testing.T, f *githubtest.Fake, tip string) githubtest.CheckRunState {
			f.SetCheckRun(tip, github.TipCheck, "success", githubtest.AppID+1)
			return githubtest.CheckRunState{SHA: tip, Name: github.TipCheck, Conclusion: "success", AppID: githubtest.AppID + 1}
		}},
		{"the App's issuedog/verify", func(_ *testing.T, f *githubtest.Fake, tip string) githubtest.CheckRunState {
			f.SetCheckRun(tip, github.VerifyCheck, "success", githubtest.AppID)
			return githubtest.CheckRunState{SHA: tip, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			tipStory(t, f)
			v.passes(2)
			tip := v.tipCut(tipBranch)
			other := tt.check(t, f, tip)
			f.ForgetWrites()
			v.tipJudged(tip, 1, []githubtest.CheckRunState{other, tipCheck(tip, tipBranch, "success")})
			v.tipQuiet("after the check")
		})
	}
}

// A crash just after the post, a write GitHub made whose answer was lost,
// then a fresh daemon: its first pass reads the tip's checks once, finds
// the one naming the branch, runs no command, and posts no second check.
func TestTipCheckCrash(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.passes(2)
	tip := v.tipCut(tipBranch)
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" },
		func() { f.FailWriteAfter(len(f.Writes()) + 1) })
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose answer was lost gave no error")
	}
	want := []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) {
		t.Fatalf("after the lost answer, the check runs = %+v; want %+v", got, want)
	}
	v = v.restart()
	from, ran := len(f.Requests()), v.checksRan()
	v.passes(2)
	if got := f.State().CheckRuns; !slices.Equal(got, want) || v.checksRan() != ran || tipReads(f, from, tip) != 1 {
		t.Errorf("after the restart, the check runs = %+v, the commands run %d times, the tip's checks read %d times; want %+v, none, and once",
			got, v.checksRan()-ran, tipReads(f, from, tip), want)
	}
	v.tipQuiet("after the restart")
}

// The tip checks run in step 4 after task work at in-progress and before
// the checks at verifying, one tip a pass, so their commands never overlap
// a claude run: with a task at in-progress and two stories' branches cut,
// the first pass runs the implementer and checks no tip, the second checks
// story 1's tip alone, the third story 2's, and only the fourth runs the
// task's checks at verifying.
func TestTipCheckOrder(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	second := tipStoryAt(f, "Quote refunds", "planned", "type:story")
	task := f.AddIssue(wholeRunOwner, "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(task, wholeRunOwner, "stage:in-progress")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	tip := v.tipCut(tipBranch)
	v.tipCut("issuedog/2-quote-refunds")

	v.pass()
	if v.runs() != 1 || len(f.State().CheckRuns) != 0 || pickStage(f, task) != "stage:verifying" {
		t.Fatalf("after the first pass, claude ran %d times, the checks are %+v, the task at %s; want once, none, and verifying",
			v.runs(), f.State().CheckRuns, pickStage(f, task))
	}
	first := []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")}
	v.tipJudged(tip, 1, first)
	both := []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success"), tipCheck(tip, "issuedog/"+strconv.Itoa(second)+"-quote-refunds", "success")}
	v.tipJudged(tip, 1, both)
	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 || !slices.Equal(f.State().CheckRuns, both) || v.runs() != 1 {
		t.Errorf("the fourth pass ran the commands %d times, left the checks %+v, and claude ran %d times; want the task's checks once, %+v, and once",
			v.checksRan()-ran, f.State().CheckRuns, v.runs(), both)
	}
}

// Nothing but an open story's, or an open epic's planned into tasks, not
// held, tip is checked: a held story, a closed one, and an epic with no
// approved plan, each with a branch by its prefix, get no check and run no
// command, and their passes make no request for a tip.
func TestTipCheckNotAStory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(f *githubtest.Fake)
	}{
		{"a held story", func(f *githubtest.Fake) { tipStoryAt(f, tipStoryTitle, "held", "type:story") }},
		{"a closed story", func(f *githubtest.Fake) { f.CloseBy(tipStoryAt(f, tipStoryTitle, "planned", "type:story"), "owner") }},
		{"an epic with no plan", func(f *githubtest.Fake) { tipStoryAt(f, tipStoryTitle, "planned", "type:epic") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			tt.setup(f)
			v.tipCut(tipBranch)
			ran := v.checksRan()
			v.passes(2)
			if got := f.State().CheckRuns; len(got) != 0 || v.checksRan() != ran {
				t.Errorf("the check runs = %+v, the commands run %d times; want none, and none", got, v.checksRan()-ran)
			}
			v.tipQuiet("no story")
		})
	}
}

// Replay passes for the check (SPEC §5.2): each pass is a fresh daemon's,
// with its own bare clone, and the digest and assignment off, so its only
// write is the check.
func TestTipCheckReplay(t *testing.T) {
	t.Parallel()
	base := newVerdictRig(t, newFake(t), 3)
	tip := base.tipCut(tipBranch)
	roots := map[*githubtest.Fake]string{}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		tipStory(tb, f)
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}
	pass := func(f *githubtest.Fake) error {
		v := &verdictRig{rig: newRig(t, f, base.o, roots[f]), o: base.o, cfg: base.cfg, ran: base.ran}
		v.wire()
		v.d.Digest, v.d.Assign = false, false
		return v.d.Iterate(context.Background())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if got, want := writesOf(clean), []string{"POST /check-runs"}; !slices.Equal(got, want) {
		t.Errorf("the clean pass wrote %q; want %q", got, want)
	}
	if got, want := clean.State().CheckRuns, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")}; !slices.Equal(got, want) {
		t.Errorf("the clean pass left the check runs %+v; want %+v", got, want)
	}
	githubtest.Replay(t, setup, pass)
}

// Two branches with the story's prefix: the daemon can't tell which is
// its branch, so neither tip is checked, no command runs, the passes make
// no request for a tip, and that's warned of once, not every pass. It
// reads logr, so it isn't parallel.
func TestTipCheckTwoBranches(t *testing.T) {
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.tipCut(tipBranch)
	v.tipCut("issuedog/1-another-slug")
	stop := logtest.Capture(t, logr.W)
	ran := v.checksRan()
	v.passes(3)
	logs := stop()
	if got := f.State().CheckRuns; len(got) != 0 || v.checksRan() != ran {
		t.Errorf("the check runs = %+v, the commands run %d times; want none, and none", got, v.checksRan()-ran)
	}
	if got := strings.Count(logs, "o/r#1: its branch's tip isn't checked, as it can't tell which is its branch"); got != 1 {
		t.Errorf("the two branches were warned of %d times; want once, in\n%s", got, logs)
	}
	v.tipQuiet("two branches")
}

// A tip that's only behind main passes rule 2, even once main changed a
// protected path since the branch was cut: its baseline is the story's,
// the merge base of the tip and main, not main as it is now (SPEC §10.1).
func TestTipCheckBehindMain(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	v.prMainCommit("secrets/key.txt", "main's own key\n")
	tip := v.tipPush(tipBranch, map[string]string{"person.txt": "a person's change\n"})
	want := []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success"), tipCheck(tip, tipBranch, "success")}
	slices.SortStableFunc(want, func(a, b githubtest.CheckRunState) int { return strings.Compare(a.SHA, b.SHA) })
	v.tipJudged(tip, 1, want)
}

// The post is checked first, just before it's made, so it never makes a
// second check naming the branch: here the App's success naming the
// branch lands on the tip while its commands run, as a replay of the post
// would leave it, and the pass posts nothing, and the tip counts as judged,
// so the passes after are quiet.
func TestTipCheckPostedMeanwhile(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.passes(2)
	tip := v.tipCut(tipBranch)
	reads := 0
	f.BeforeRequest(func(r githubtest.Request) bool {
		if r.Method == http.MethodGet && r.Path == "/commits/"+tip+"/check-runs" {
			reads++
		}
		return reads == 2
	}, func() {
		if _, err := f.Client(t).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + tipBranch}); err != nil {
			t.Error(err)
		}
	})
	writes := len(f.Writes())
	v.tipJudged(tip, 1, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")})
	if got := writesOf(f)[writes:]; len(got) != 1 {
		t.Errorf("the pass and its hook wrote %q; want the hook's check alone", got)
	}
	v.tipQuiet("after the check")
}

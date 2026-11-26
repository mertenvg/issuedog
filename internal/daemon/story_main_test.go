package daemon_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// When main moves, step 2 merges it into each open story branch whose tip
// passed its checks, one branch a pass (SPEC §7.5, When main moves; §8.1),
// on verdictRigs wired as main's, with real temporary repos, whose
// branches and main the tests push to in the origin itself, as an admin
// and the owner's merges into main may. The merge commit has the tip and
// main as its parents, by the App's bot, and is pushed leased at the tip.
// Step 4 then checks the new tip as any push the App didn't make, and a
// merge that conflicts files one conflict bug under the story.

// storyMainMoved commits path, with content, on main in v's origin, as a
// merge into main does, and returns main's new commit.
func (v *verdictRig) storyMainMoved(path, content string) string {
	v.t.Helper()
	v.o.commit(path, content)
	return v.mainSHA()
}

// storyMainMerged fails unless branch in v's origin is at the App's merge
// of main, a main commit, into tip: a merge commit with tip and main as its
// parents, in that order, by the App's bot as author and committer, dated
// as main was committed, with the message "Merge `main` at <main> into
// <branch>" (SPEC §3.2). It returns the merge commit.
func (v *verdictRig) storyMainMerged(branch, tip, main string) string {
	v.t.Helper()
	merge := v.originTip(branch)
	if got := gitIn(v.t, v.o.url, "rev-list", "--parents", "-n", "1", merge); got != merge+" "+tip+" "+main {
		v.t.Fatalf("%s's tip and its parents are %q; want a merge commit with the parents %s and %s", branch, got, tip, main)
	}
	bot := app + " <" + app + "@users.noreply.github.com>"
	want := strings.Join([]string{bot, bot, gitIn(v.t, v.o.url, "log", "-1", "--format=%cI", main), "Merge `main` at " + main + " into " + branch}, "\n")
	if got := gitIn(v.t, v.o.url, "log", "-1", "--format=%an <%ae>%n%cn <%ce>%n%aI%n%B", merge); got != want {
		v.t.Errorf("the merge commit %s =\n%s\nwant\n%s", merge, got, want)
	}
	return merge
}

// storyMainNone fails unless branch in v's origin is still at tip, and
// the daemon's clone holds no merge of main for story n (SPEC §3.2): no
// merge was built, and nothing was pushed.
func (v *verdictRig) storyMainNone(n int, branch, tip, why string) {
	v.t.Helper()
	built := gitIn(v.t, v.d.Clone.Dir(), "for-each-ref", "--format=%(objectname)", repo.StoryMergesRef+strconv.Itoa(n))
	if got := v.originTip(branch); got != tip || built != "" {
		v.t.Errorf("%s: %s is at %s, with the merge %q built; want it left at %s, and none built", why, branch, got, built, tip)
	}
}

// storyMainReady files story 1, cuts its branch, pushes a commit of the
// story's to it, story.txt, and runs the pass that checks that tip as
// success, after two that settle the story, on v. It returns the tip.
func (v *verdictRig) storyMainReady() string {
	v.t.Helper()
	tipStory(v.t, v.f)
	v.passes(2)
	v.tipCut(tipBranch)
	tip := v.tipPush(tipBranch, map[string]string{"story.txt": "the story's change\n"})
	v.tipJudged(tip, 1, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")})
	return tip
}

// A story branch whose tip passed its checks gets main merged in once main
// moves: an unchanged pass before makes no request and pushes nothing, and
// the pass after main moves pushes the App's merge commit to the branch,
// leased at the tip, then checks the new tip in step 4 as success, running
// the commands once. The passes after are quiet and push nothing, and so
// is one after a fresh daemon's first pass, which reads the new tip's
// check once, and runs nothing.
func TestStoryMainMerge(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tip := v.storyMainReady()
	v.tipQuiet("main unchanged")
	v.storyMainNone(1, tipBranch, tip, "main unchanged")
	main := v.storyMainMoved("main.txt", "main moved\n")

	ran := v.checksRan()
	v.pass()
	merge := v.storyMainMerged(tipBranch, tip, main)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(merge, tipBranch, "success")) || v.checksRan() != ran+1 {
		t.Fatalf("the merge's pass left the checks %+v, and ran the commands %d times; want success on the merge %s, once", got, v.checksRan()-ran, merge)
	}
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.StoryMergesRef+"1"); got != merge {
		t.Errorf("the daemon's clone keeps the merge %s; want %s", got, merge)
	}
	v.tipQuiet("after the merge's check")
	v.tipQuiet("again")

	v = v.restart()
	from := len(f.Requests())
	v.pass()
	if got := tipReads(f, from, merge); got != 1 || v.checksRan() != ran+1 || v.originTip(tipBranch) != merge {
		t.Errorf("a fresh daemon's first pass read the merge's checks %d times, ran the commands %d times, and left the branch at %s; want once, never, and %s",
			got, v.checksRan()-ran-1, v.originTip(tipBranch), merge)
	}
	v.tipQuiet("after the restart")
	if got := v.originTip(tipBranch); got != merge {
		t.Errorf("the branch is at %s; want the merge %s, with nothing more pushed", got, merge)
	}
}

// A tip not checked yet gets no main: the pass after main moves checks
// the tip in step 4, after step 2 has passed it over, and the pass after
// merges main into it.
func TestStoryMainUnchecked(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	v.passes(2)
	cut := v.tipCut(tipBranch)
	main := v.storyMainMoved("main.txt", "main moved\n")
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	v.storyMainNone(1, tipBranch, cut, "the pass that checks the tip")
	v.pass()
	v.storyMainMerged(tipBranch, cut, main)
}

// The App merges a task's PR into the story branch before it merges main
// in (SPEC §8.1's step 2): on the pass after main moves, a task whose PR
// is ready to merge is merged onto the tip, and main isn't merged into a
// tip the App merged onto. The pass after carries the check over to the
// squash commit, then merges main into it, and step 4 checks the merge.
func TestStoryMainTaskFirst(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	main := v.storyMainMoved("main.txt", "main moved\n")
	v.pass()
	squash := v.storyMerged(pr, tip)
	v.storyMainNone(2, storyVerifyBranch, squash, "the PR's merge's pass")
	ran := v.checksRan()
	v.pass()
	merge := v.storyMainMerged(storyVerifyBranch, squash, main)
	got := f.State().CheckRuns
	if !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || !slices.Contains(got, tipCheck(merge, storyVerifyBranch, "success")) ||
		v.checksRan() != ran+1 {
		t.Errorf("the pass after the PR's merge left the checks %+v, and ran the commands %d times; want success carried to %s, and checked on the merge %s, once",
			got, v.checksRan()-ran, squash, merge)
	}
}

// A task's open PR into the story branch is checked again on its merge
// with the new tip main's merge makes, and merged onto it (SPEC §7.5,
// §10.4): GitHub refuses the PR's merge once, so it's open when main
// moves, and that pass merges main in and checks the new tip as success.
// The pass after checks the PR's head on its merge with that tip, and the
// one after merges the PR onto it, then the one after that checks the
// squash commit afresh and closes the task. Then the passes are quiet.
func TestStoryMainTaskPR(t *testing.T) {
	t.Parallel()
	f, v, tip, head, pr := storyMergeReady(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPut && r.Path == storyMergePath(pr) }, http.StatusConflict)
	main := v.storyMainMoved("main.txt", "main moved\n")
	ran := v.checksRan()
	v.pass()
	merge := v.storyMainMerged(storyVerifyBranch, tip, main)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(merge, storyVerifyBranch, "success")) || v.checksRan() != ran+1 ||
		f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the merge's pass left the checks %+v, ran the commands %d times, the PR %s, and the task at %s; want success on %s, once, open, and pr-open",
			got, v.checksRan()-ran, f.State().PRs[0].State, pickStage(f, 1), merge)
	}
	v.storyTipPassed(1, pr, head, merge)
	squash := v.storyTipMergedOnto(pr, head, merge)
	v.storyTipMergedChecked(1, pr, squash, "success")
	v.passes(2)
	v.quietPass("after the task's close")
	if got := v.originTip(storyVerifyBranch); got != squash {
		t.Errorf("the story branch is at %s; want the squash commit %s, as main has nothing new", got, squash)
	}
}

// A branch gets no main while its story is held, its tip failed its
// checks, a failure's bug of its story is open, or two branches have its
// prefix (SPEC §7.5, §8.1): the passes after main moves build no merge,
// push nothing, and once they've read what they wrote, make no request.
// Once that ends, the next pass merges main in: on the hold's /resume,
// the bug's close, and the second branch's delete. A failing tip stays
// failing, so nothing is merged into it.
func TestStoryMainWaits(t *testing.T) {
	t.Parallel()
	const other = "issuedog/1-another-slug"
	for _, tt := range []struct {
		name string
		// wait makes story 1 wait, on v, whose branch's tip, tip, is
		// checked as success, and returns the tip it waits on.
		wait func(t *testing.T, v *verdictRig, tip string) string
		// end ends the wait, or is nil for one that doesn't end.
		end func(t *testing.T, v *verdictRig)
	}{
		{"a held story", func(t *testing.T, v *verdictRig, tip string) string {
			v.f.AddComment(1, "owner", "/hold")
			v.passes(2)
			if got := pickStage(v.f, 1); got != "stage:held" {
				t.Fatalf("the story is at %s; want held", got)
			}
			return tip
		}, func(_ *testing.T, v *verdictRig) { v.f.AddComment(1, "owner", "/resume") }},
		{"a failing tip", func(t *testing.T, v *verdictRig, tip string) string {
			failing := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
			v.pass()
			if !slices.Contains(v.f.State().CheckRuns, tipCheck(failing, tipBranch, "failure")) {
				t.Fatalf("the checks are %+v; want failure on %s", v.f.State().CheckRuns, failing)
			}
			return failing
		}, nil},
		{"an open failure's bug", func(t *testing.T, v *verdictRig, tip string) string {
			storyFailureBugAt(v.f, 1, strings.Repeat("e", 40), "ready")
			return tip
		}, func(_ *testing.T, v *verdictRig) { v.f.CloseBy(len(v.f.State().Issues), "owner") }}, // the bug, the newest issue
		{"two branches", func(t *testing.T, v *verdictRig, tip string) string {
			v.branchesAt(other, tip)
			v.pass()
			if got := branchesErrs(v.f, 1); len(got) != 1 {
				t.Fatalf("the story's branches errors are %q; want one", got)
			}
			return tip
		}, func(_ *testing.T, v *verdictRig) { v.branchesDrop(other) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRigWith(t, f, 3, "", storyFailureBreaks).pickOff()
			tip := tt.wait(t, v, v.storyMainReady())
			v.passes(2)
			main := v.storyMainMoved("main.txt", "main moved\n")
			v.passes(2)
			v.quietPass("waiting with main moved")
			v.storyMainNone(1, tipBranch, tip, "while it waits")
			if tt.name == "two branches" && v.originTip(other) != tip {
				t.Errorf("%s is at %s; want it left at %s", other, v.originTip(other), tip)
			}
			if tt.end == nil {
				return
			}
			tt.end(t, v)
			for range 3 {
				if v.originTip(tipBranch) != tip {
					break
				}
				v.pass()
			}
			v.storyMainMerged(tipBranch, tip, main)
		})
	}
}

// A /hold on the story's epic posted as main moves holds the merge from
// that very pass (SPEC §6.4): the command holds the story too, and touches
// it, so it's read again before step 2 judges it, and no merge is built or
// pushed then, and none on the passes after, with the story held.
func TestStoryMainHoldPosted(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tip := v.storyMainReady()
	epic := tipStoryAt(f, "Quotes", "planned", "type:epic")
	f.SetParent(1, epic)
	v.passes(2)
	v.storyMainMoved("main.txt", "main moved\n")
	f.AddComment(epic, "owner", "/hold")
	v.pass()
	v.storyMainNone(1, tipBranch, tip, "the hold's pass")
	v.passes(2)
	if got := pickStage(f, 1); got != "stage:held" {
		t.Fatalf("the story is at %s; want held", got)
	}
	v.storyMainNone(1, tipBranch, tip, "with the story held")
}

// While a failure's bug of the story is open, the passes after main moves
// log once that main isn't merged in, and why, naming the bug (SPEC §7.5).
// It reads logr, so it isn't parallel.
func TestStoryMainFailureLogged(t *testing.T) {
	f := newFake(t)
	v := newVerdictRig(t, f, 3).pickOff()
	tip := v.storyMainReady()
	bug := storyFailureBugAt(f, 1, strings.Repeat("e", 40), "ready")
	v.storyMainMoved("main.txt", "main moved\n")
	stop := logtest.Capture(t, logr.I)
	v.passes(4)
	logs := stop()
	want := "o/r#1: main isn't merged into its branch " + tipBranch + ", as a failure on it is open, as its bug #" + strconv.Itoa(bug) + " is"
	if got := strings.Count(logs, want); got != 1 {
		t.Errorf("the wait was logged %d times; want once, in\n%s", got, logs)
	}
	v.storyMainNone(1, tipBranch, tip, "with the bug open")
}

// A tip that has main already, as when a person merged main into the
// branch by hand, gets no merge of the App's (SPEC §7.5): the pass after
// the person's push checks the new tip, and the passes after build no
// merge, push nothing, and are quiet.
func TestStoryMainHasMain(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.storyMainReady()
	v.storyMainMoved("main.txt", "main moved\n")
	w := v.o.work
	gitIn(t, w, "fetch", "--quiet", v.o.url, "refs/heads/"+tipBranch)
	gitIn(t, w, "checkout", "--quiet", "-B", "story", "FETCH_HEAD")
	gitIn(t, w, "merge", "--quiet", "--no-edit", "main")
	merged := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", v.o.url, "story:refs/heads/"+tipBranch)
	gitIn(t, w, "checkout", "--quiet", "main")
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(merged, tipBranch, "success")) {
		t.Fatalf("the person's merge's pass left the checks %+v; want success on %s", got, merged)
	}
	v.tipQuiet("the tip has main")
	v.tipQuiet("again")
	v.storyMainNone(1, tipBranch, merged, "with main in the tip")
}

// A person's push to the story branch between the pass's sync and the
// merge's push makes the lease stale (SPEC §7.5): nothing is pushed, the
// branch keeps the person's tip, and that's logged. The next pass's sync
// shows the person's tip, which step 4 checks first, and the pass after
// merges main into it. It reads logr, so it isn't parallel.
func TestStoryMainStalePush(t *testing.T) {
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tip := v.storyMainReady()
	main := v.storyMainMoved("main.txt", "main moved\n")
	var person string
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues" }, func() {
		person = v.tipPush(tipBranch, map[string]string{"person.txt": "a person's change\n"})
	})
	stop := logtest.Capture(t, logr.I)
	v.pass()
	logs := stop()
	if person == "" || v.originTip(tipBranch) != person {
		t.Fatalf("the stale push's pass left the branch at %s; want the person's %q", v.originTip(tipBranch), person)
	}
	merge := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.StoryMergesRef+"1")
	want := "o/r#1: the merge " + merge + " of main's " + main + " isn't pushed to its branch " + tipBranch + ", as the branch moved from " + tip + " since the sync"
	if !strings.Contains(logs, want) {
		t.Errorf("the logs =\n%s\nwant them to hold\n%s", logs, want)
	}
	v.pass()
	if !slices.Contains(f.State().CheckRuns, tipCheck(person, tipBranch, "success")) {
		t.Fatalf("the pass after the stale push left the checks %+v; want success on the person's %s", f.State().CheckRuns, person)
	}
	if got := v.originTip(tipBranch); got != person {
		t.Fatalf("the pass that checks the person's tip left the branch at %s; want %s, with nothing pushed", got, person)
	}
	v.pass()
	v.storyMainMerged(tipBranch, person, main)
}

// One branch a pass gets main, by story priority, then number (SPEC
// §8.1): story 2, at p0, gets it first, and story 1, at p1, on the pass
// after.
func TestStoryMainTwoStories(t *testing.T) {
	t.Parallel()
	const second = "issuedog/2-refunds"
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	if n := f.AddIssue("owner", "Refunds", "The story.", "priority:p0", "type:story"); n != 2 {
		t.Fatalf("the second story is #%d, want #2", n)
	}
	f.AddLabel(2, app, "stage:planned")
	f.AddComment(2, app, "<!-- issuedog:transition from=plan-review to=planned run=rp -->")
	v.passes(2)
	cut := v.tipCut(tipBranch)
	v.tipCut(second)
	v.passes(2)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(cut, tipBranch, "success")) || !slices.Contains(got, tipCheck(cut, second, "success")) {
		t.Fatalf("the checks are %+v; want success on both branches' tips", got)
	}
	main := v.storyMainMoved("main.txt", "main moved\n")
	v.pass()
	v.storyMainMerged(second, cut, main)
	v.storyMainNone(1, tipBranch, cut, "story 2's pass")
	v.pass()
	v.storyMainMerged(tipBranch, cut, main)
}

// A crash after the build, before the push, then a restart: the fresh
// daemon's first pass reads the tip's check, and the one after builds the
// same merge commit again, and pushes it. A crash after the push, then a
// restart: the fresh daemon's first pass checks the new tip, and the
// passes after build no second merge (SPEC §5.2).
func TestStoryMainCrash(t *testing.T) {
	t.Parallel()
	for _, pushed := range []bool{false, true} {
		t.Run("pushed "+strconv.FormatBool(pushed), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			tip := v.storyMainReady()
			main := v.storyMainMoved("main.txt", "main moved\n")
			gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", v.o.url, "+refs/heads/main:refs/heads/main")
			built, conflicts, err := v.d.Clone.MainMerge(context.Background(), 1, tipBranch, tip, main, app, app+"@users.noreply.github.com")
			if err != nil || len(conflicts) != 0 {
				t.Fatalf("the build = %s, %q, %v; want a merge", built, conflicts, err)
			}
			if pushed {
				gitIn(t, v.d.Clone.Dir(), "push", "--quiet", v.o.url, built+":refs/heads/"+tipBranch)
			}
			v = v.restart()
			ran := v.checksRan()
			v.pass()
			if pushed {
				if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(built, tipBranch, "success")) || v.checksRan() != ran+1 {
					t.Fatalf("the restart's pass left the checks %+v, and ran the commands %d times; want success on %s, once", got, v.checksRan()-ran, built)
				}
				v.tipQuiet("after the pushed merge's check")
				if got := v.storyMainMerged(tipBranch, tip, main); got != built {
					t.Errorf("the branch is at %s; want %s, pushed before the crash", got, built)
				}
				return
			}
			if got := v.originTip(tipBranch); got != tip {
				t.Fatalf("the restart's pass left the branch at %s; want it at %s till the tip's check is read", got, tip)
			}
			v.pass()
			if got := v.storyMainMerged(tipBranch, tip, main); got != built {
				t.Errorf("the merge pushed is %s; want %s, built before the crash", got, built)
			}
		})
	}
}

// The merge of main, a crash at each write, then a restart (SPEC §5.2):
// each pass is a fresh daemon on a fresh clone, whose first iteration
// reads the tip's check, and whose second merges main in and checks the
// new tip, posting its check, the one write. A crash there, before or
// after GitHub takes the check, leaves the merge pushed, and the next
// fresh daemon checks it once. Every run ends with the branch at the one
// merge commit, and one check on it. The origin is shared, so each setup
// puts the story branch back at its tip.
func TestStoryMainReplay(t *testing.T) {
	t.Parallel()
	base := newVerdictRig(t, newFake(t), 3)
	base.tipCut(tipBranch)
	tip := base.tipPush(tipBranch, map[string]string{"story.txt": "the story's change\n"})
	main := base.storyMainMoved("main.txt", "main moved\n")
	roots := map[*githubtest.Fake]string{}
	setup := func(tb testing.TB) *githubtest.Fake {
		gitIn(tb, base.o.url, "update-ref", "refs/heads/"+tipBranch, tip)
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		tipStory(tb, f)
		if _, err := f.Client(tb).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + tipBranch}); err != nil {
			tb.Fatal(err)
		}
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}
	pass := storyMainReplayPass(t, base, roots)
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	merge := base.storyMainMerged(tipBranch, tip, main)
	if got := writesOf(clean); !slices.Equal(got, []string{"POST /check-runs"}) {
		t.Errorf("the clean pass wrote %q; want the new tip's check alone", got)
	}
	if got := clean.State().CheckRuns; !slices.Contains(got, tipCheck(merge, tipBranch, "success")) {
		t.Errorf("the clean pass left the checks %+v; want success on the merge %s", got, merge)
	}
	githubtest.Replay(t, setup, pass)
}

// storyMainReplayPass is a Replay pass on base's origin: a fresh daemon on
// a fresh clone, in roots[f], that picks nothing and keeps no digest or
// assignees, whose two iterations read the tip's check, then merge main.
func storyMainReplayPass(t *testing.T, base *verdictRig, roots map[*githubtest.Fake]string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		v := &verdictRig{rig: newRig(t, f, base.o, roots[f]), o: base.o, cfg: base.cfg, ran: base.ran, noPick: true}
		v.wire()
		v.d.Digest, v.d.Assign = false, false
		for range 2 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	}
}

// storyMainConflictReady files story 1 on v, cuts its branch, pushes a
// commit of the story's to it that changes f.txt, checks that tip as
// success, then moves main with its own change to f.txt, which conflicts.
// It returns the tip and main's commit.
func (v *verdictRig) storyMainConflictReady() (string, string) {
	v.t.Helper()
	tipStory(v.t, v.f)
	v.passes(2)
	v.tipCut(tipBranch)
	tip := v.tipPush(tipBranch, map[string]string{"f.txt": "the story's\n"})
	v.tipJudged(tip, 1, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "success")})
	return tip, v.storyMainMoved("f.txt", "main's\n")
}

// storyMainConflictOrigin is the origin marker of the conflict bug filed
// for main's commit main, which doesn't merge into story 1's branch's tip
// tip (SPEC §5).
func storyMainConflictOrigin(tip, main string) string {
	return "<!-- issuedog:origin main=" + main + " story=#1 tip=" + tip + " -->"
}

// A merge of main that conflicts pushes nothing, and files one conflict
// bug under the story (SPEC §7.5, A conflict): the App's, a sub-issue of
// the story, with type:bug, the story's priority and stage:ready, titled
// for main's commit and the tip, its body naming both and the conflicting
// file, ending in its origin with main=, and its first move intake →
// ready, keyed event=t<SHA> by the tip. The passes after file nothing
// more, build no merge and push nothing, and once they've read what it
// wrote, make no request. Nor does a fresh daemon file a second one.
func TestStoryMainConflict(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).pickOff()
	tip, main := v.storyMainConflictReady()
	from := len(f.Writes())
	v.pass()
	v.storyMainNone(1, tipBranch, tip, "the conflict's pass")
	n := len(f.State().Issues)
	is := f.State().Issues[n-1]
	if is.Author != app || is.Closed || is.Parent != 1 || !slices.Equal(f.State().Issues[0].SubIssues, []int{n}) ||
		!slices.Equal(is.Labels, []string{"priority:p1", "stage:ready", "type:bug"}) {
		t.Fatalf("#%d is by %s, closed %t, below #%d, the story's sub-issues %v, labelled %v; want the App's open bug, the story's one sub-issue, at ready",
			n, is.Author, is.Closed, is.Parent, f.State().Issues[0].SubIssues, is.Labels)
	}
	if want := "Merge main at " + main[:7] + " into story #1's branch at " + tip[:7]; is.Title != want {
		t.Errorf("#%d's title = %q; want %q", n, is.Title, want)
	}
	for _, text := range []string{"Merging `main` at `" + main + "` into story #1's branch `" + tipBranch + "`, at its tip `" + tip + "`, conflicts in these files:\n\n- `f.txt`",
		"Starting from `main` at `" + main + "`, merge the tip `" + tip + "`"} {
		if !strings.Contains(is.Body, text) {
			t.Errorf("#%d's body =\n%s\nwant it to hold\n%s", n, is.Body, text)
		}
	}
	if !strings.HasSuffix(is.Body, "\n\n"+storyMainConflictOrigin(tip, main)) {
		t.Errorf("#%d's body =\n%s\nwant it to end in its origin", n, is.Body)
	}
	move := "Filed for story #1, whose branch `" + tipBranch + "` at `" + tip + "` doesn't merge `main` at `" + main + "` cleanly. It starts at `stage:ready`.\n\n" +
		tipFailureMove("ready", tip)
	if len(is.Comments) != 1 || is.Comments[0].Author != app || is.Comments[0].Body != move {
		t.Errorf("#%d's comments = %+v; want the App's first move alone,\n%s", n, is.Comments, move)
	}
	want := []string{"POST /issues", "POST /issues/1/sub_issues", "POST /issues/" + strconv.Itoa(n) + "/comments"}
	if got := writesOf(f)[from:]; len(got) < 3 || !slices.Equal(got[:3], want) || slices.Contains(got[3:], "POST /issues") {
		t.Errorf("the conflict's pass wrote %q; want it to start %q, and file nothing more", got, want)
	}

	v.passes(2)
	v.quietPass("after the conflict's filing")
	v = v.restart()
	v.passes(2)
	v.quietPass("after the restart")
	if got := len(f.State().Issues); got != n {
		t.Errorf("the fake has %d issues; want %d, with no second bug", got, n)
	}
	v.storyMainNone(1, tipBranch, tip, "after the filing")
}

// A conflict bug someone closed, with the tip and main unchanged, files
// nothing more, and once the passes have read the close, the conflict
// costs no request, not even a conditional read of the story's
// sub-issues to find the bug again: the pair's conflict is remembered as
// filed.
func TestStoryMainConflictClosed(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).pickOff()
	tip, _ := v.storyMainConflictReady()
	v.pass()
	n := len(f.State().Issues)
	f.CloseBy(n, "owner")
	v.passes(3)
	from := len(f.Requests())
	v.quietPass("the conflict bug closed")
	if got := branchesSubReads(f, from, 1); got != 0 {
		t.Errorf("the pass read the story's sub-issues %d times; want none", got)
	}
	if got := len(f.State().Issues); got != n {
		t.Errorf("the fake has %d issues; want %d, with no second bug", got, n)
	}
	v.storyMainNone(1, tipBranch, tip, "with the bug closed")
}

// GitHub's 422 on the conflict bug's filing, as for a story with 100
// sub-issues, is warned of once, pushes nothing, and isn't an error: the
// passes after file nothing, and once they've read what the pass wrote,
// make no request, as the pair's conflict is remembered as filed. It reads
// logr, so it isn't parallel.
func TestStoryMainConflictRefused(t *testing.T) {
	f := newFake(t)
	v := newVerdictRig(t, f, 3).pickOff()
	tip, main := v.storyMainConflictReady()
	issues := len(f.State().Issues)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues" }, http.StatusUnprocessableEntity)
	stop := logtest.Capture(t, logr.W)
	v.passes(3)
	v.quietPass("the refused conflict")
	logs := stop()
	want := "o/r#1: main's " + main + " conflicts with its branch " + tipBranch + " at " + tip + ", in 1 files, but no conflict bug is filed"
	if got := strings.Count(logs, want); got != 1 {
		t.Errorf("the refusal was warned of %d times; want once, in\n%s", got, logs)
	}
	if got := len(f.State().Issues); got != issues {
		t.Errorf("the fake has %d issues; want %d, with no bug filed", got, issues)
	}
	v.storyMainNone(1, tipBranch, tip, "after the refusal")
}

// The filing of a conflict bug, a crash at each write, then a restart
// (SPEC §5.2): each pass is a fresh daemon on a fresh clone, whose first
// iteration reads the tip's check, and whose second merges main in, finds
// the conflict and files it: the issue, its link under the story, then
// its first move. A crash once the bug is linked leaves the story with an
// open failure, so the next daemon merges main again only to finish that
// filing. Every run ends with the one bug, linked and moved once.
func TestStoryMainConflictReplay(t *testing.T) {
	t.Parallel()
	base := newVerdictRig(t, newFake(t), 3)
	base.tipCut(tipBranch)
	tip := base.tipPush(tipBranch, map[string]string{"f.txt": "the story's\n"})
	main := base.storyMainMoved("f.txt", "main's\n")
	roots := map[*githubtest.Fake]string{}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		tipStory(tb, f)
		if _, err := f.Client(tb).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + tipBranch}); err != nil {
			tb.Fatal(err)
		}
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}
	pass := storyMainReplayPass(t, base, roots)
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if got, want := writesOf(clean), []string{"POST /issues", "POST /issues/1/sub_issues", "POST /issues/2/comments"}; !slices.Equal(got, want) {
		t.Errorf("the clean pass wrote %q; want %q", got, want)
	}
	if is := clean.State().Issues[1]; !strings.HasSuffix(is.Body, storyMainConflictOrigin(tip, main)) || is.Parent != 1 {
		t.Errorf("the clean pass filed %+v; want the conflict bug below #1", is)
	}
	githubtest.Replay(t, setup, pass)
	if got := base.originTip(tipBranch); got != tip {
		t.Errorf("the branch is at %s; want it left at %s", got, tip)
	}
}

// A merge of main whose new tip fails its checks, while the story's other
// task, #2, holds the worker at in-progress (SPEC §7.5, §8.1's One task
// at a time): the merge lands, step 4 checks the new tip as failure, as
// main brought breaks.txt, and files a bug for it. #2 steps aside for the
// bug, which is picked from the failing tip, runs, passes its checks and
// its verifier, and is merged onto it. The squash commit is checked afresh
// as success, and only once the bug is closed does #2 run, from there.
// Then the passes are quiet, with main merged in already.
func TestStoryMainFailingMerge(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt || ls run.*.txt"`)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	storyRunTask(t, f)
	v.pass()
	if !slices.Contains(f.State().CheckRuns, tipCheck(cut, tipBranch, "success")) || v.runs() != 0 {
		t.Fatalf("the cut's pass left the checks %+v, and ran claude %d times; want success on %s, and no run", f.State().CheckRuns, v.runs(), cut)
	}
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	pass := verifierOutput(t, "pass")
	pass["follow_ups"] = []any{}
	v.out(2, ivResult(t, pass))
	v.out(3, ivResult(t, implementerOutput(t, "question")))
	main := v.storyMainMoved("breaks.txt", "main breaks the story's commands\n")

	v.pass()
	merge := v.storyMainMerged(tipBranch, cut, main)
	if !slices.Contains(f.State().CheckRuns, tipCheck(merge, tipBranch, "failure")) || v.runs() != 0 {
		t.Fatalf("the merge's pass left the checks %+v, and ran claude %d times; want failure on the merge %s, and no run", f.State().CheckRuns, v.runs(), merge)
	}
	bug := storyFailureNewest(t, f, 1, merge)
	var order []int
	for range 20 {
		if v.runs() >= 3 {
			break
		}
		before := v.runs()
		v.pass()
		for k := before + 1; k <= v.runs(); k++ {
			l := v.leaseOf(strconv.Itoa(k))
			order = append(order, l.Issue)
			if l.Issue == 2 && !f.State().Issues[bug-1].Closed {
				t.Errorf("#2's run %d came while the failure's bug #%d is open", k, bug)
			}
		}
	}
	if !slices.Equal(order, []int{bug, bug, 2}) {
		t.Fatalf("the runs were on %v; want the bug #%d's implementer and verifier, then #2's", order, bug)
	}
	prs := f.State().PRs
	if len(prs) != 1 || !prs[0].Merged || prs[0].MergedBy != app || prs[0].Base != tipBranch || !f.State().Issues[bug-1].Closed {
		t.Fatalf("the PRs = %+v, the bug closed %t; want the bug's, merged into %s by the App, and the bug closed", prs, f.State().Issues[bug-1].Closed, tipBranch)
	}
	squash := v.originTip(tipBranch)
	if got := gitIn(t, v.o.url, "rev-list", "--parents", "-n", "1", squash); got != squash+" "+merge {
		t.Errorf("the story branch's tip and its parents are %q; want the squash commit on the merge %s", got, merge)
	}
	if !slices.Contains(f.State().CheckRuns, tipCheck(squash, tipBranch, "success")) {
		t.Errorf("the checks are %+v; want success on the squash commit %s", f.State().CheckRuns, squash)
	}
	if l := v.leaseOf("3"); l.StoryTip != squash {
		t.Errorf("#2's run's lease = %+v; want it named the squash commit %s as its story tip", l, squash)
	}
	if pickStage(f, 2) != "stage:clarifying" {
		t.Fatalf("#2 is at %s; want clarifying, on its run's question", pickStage(f, 2))
	}
	v.passes(2)
	v.quietPass("#2 at clarifying, with main merged in")
	if got := v.originTip(tipBranch); got != squash {
		t.Errorf("the branch is at %s; want the squash commit %s, with no second merge", got, squash)
	}
}

package daemon_test

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// The pick of a task in a story (SPEC §3.4, §7.5, §8.1), on a verdictRig
// wired as main's, with real temporary repos: for the story's first task
// the pick cuts the story's branch from main, and picks nothing that pass;
// step 4 checks the new tip on the next; and the pass after picks the
// task, whose run starts from that tip. A task in a story waits, uncounted,
// while its own blocker is open, while its story's tip has no success,
// and while its story has an open blocker.

// pickStoryTask files a task titled title in story 1, at ready, as its
// plan's approval moved it there, with priority:p1 and scope:booking, and
// returns its number.
func pickStoryTask(f *githubtest.Fake, title string) int {
	n := f.AddIssue("owner", title, "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.SetParent(n, 1)
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	return n
}

// newPickStoryRig is a verdictRig with story 1 and its task 2 at ready,
// whose first implementer run commits.
func newPickStoryRig(t *testing.T) (*githubtest.Fake, *verdictRig) {
	t.Helper()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	if n := pickStoryTask(f, "Refuse an expired quote"); n != 2 {
		t.Fatalf("the task is #%d, want #2", n)
	}
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	return f, v
}

// pickStoryBranches are the issuedog/ branches of f's git remote, each
// with its tip.
func pickStoryBranches(f *githubtest.Fake) []githubtest.BranchState {
	return f.State().Branches
}

// pickStoryCut runs a pass of v, which must cut branch, and only it, from
// main, write nothing to task n or its story, issue 1, or any check run,
// start no run and leave n at ready, and return the tip, main's SHA.
func (v *verdictRig) pickStoryCut(branch string, n int) string {
	v.t.Helper()
	main := v.mainSHA()
	writes, runs := len(v.f.Writes()), v.runs()
	v.pass()
	want := []githubtest.BranchState{{Name: branch, SHA: main}}
	if got := pickStoryBranches(v.f); !slices.Equal(got, want) || v.runs() != runs || pickStage(v.f, n) != "stage:ready" {
		v.t.Fatalf("the cut's pass left the branches %+v, ran claude %d times, and #%d at %s; want %+v, no run, and ready",
			got, v.runs()-runs, n, pickStage(v.f, n), want)
	}
	for _, w := range v.f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/"+strconv.Itoa(n)+"/") || strings.HasPrefix(w.Path, "/issues/1/") || strings.Contains(w.Path, "check-runs") {
			v.t.Errorf("the cut's pass wrote %+v; want nothing to #%d, its story or a check", w, n)
		}
	}
	return main
}

// pickStoryChecked runs a pass of v, which must check tip, branch's tip,
// posting its issuedog/tip as conclusion, naming the branch, push nothing,
// and start no run, leaving task n at ready.
func (v *verdictRig) pickStoryChecked(branch, tip, conclusion string, n int) {
	v.t.Helper()
	branches, runs := pickStoryBranches(v.f), v.runs()
	v.pass()
	if got := tipLast(v.t, v.f); got.HeadSHA != tip || got.Name != github.TipCheck || got.Conclusion != conclusion ||
		!slices.Contains(v.f.State().CheckRuns, tipCheck(tip, branch, conclusion)) {
		v.t.Fatalf("the pass posted %+v, leaving the checks %+v; want %s on %s as %s, naming %s", got, v.f.State().CheckRuns, github.TipCheck, tip, conclusion, branch)
	}
	if got := pickStoryBranches(v.f); !slices.Equal(got, branches) || v.runs() != runs || pickStage(v.f, n) != "stage:ready" {
		v.t.Fatalf("the check's pass left the branches %+v, ran claude %d times, and #%d at %s; want %+v, no run, and ready",
			got, v.runs()-runs, n, pickStage(v.f, n), branches)
	}
}

// pickStoryPicked runs a pass of v, which must pick task n, from tip, its
// story branch's checked tip, as run k: the task moves to in-progress, and
// the run, named in its lease with tip as its start and story tip, moves
// it on to verifying. It pushes nothing.
func (v *verdictRig) pickStoryPicked(n int, tip, k string) {
	v.t.Helper()
	branches, runs := pickStoryBranches(v.f), v.runs()
	v.pass()
	if v.runs() != runs+1 || pickStage(v.f, n) != "stage:verifying" {
		v.t.Fatalf("the pick's pass ran claude %d times, and left #%d at %s; want once, and verifying", v.runs()-runs, n, pickStage(v.f, n))
	}
	if l := v.leaseOf(k); l.Issue != n || l.Start != tip || l.StoryTip != tip || l.Target != "" || l.Attempt != 1 {
		v.t.Errorf("the run's lease = %+v; want it on #%d, from the story tip %s, which it names, with no target, on attempt 1", l, n, tip)
	}
	if got := implementerComments(v.t, v.f, n); !slices.ContainsFunc(got, func(c github.Comment) bool {
		return strings.HasPrefix(c.Body, pickStoryMoveText) && strings.Contains(c.Body, "from=ready to=in-progress")
	}) {
		v.t.Errorf("#%d's comments are %+v; want the pick's move among them", n, got)
	}
	if got := pickStoryBranches(v.f); !slices.Equal(got, branches) {
		v.t.Errorf("the pick's pass left the branches %+v; want %+v", got, branches)
	}
}

// pickStoryMoveText is the start of the pick's comment.
const pickStoryMoveText = "I've picked this task, so the implementer starts on it now."

// pickStoryReads counts the blocked_by reads in f's requests from from on,
// by the issue read.
func pickStoryReads(f *githubtest.Fake, from int) map[int]int {
	reads := map[int]int{}
	for _, r := range f.Requests()[from:] {
		rest, ok := strings.CutSuffix(r.Path, "/dependencies/blocked_by")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(rest, "/issues/"))
		if err != nil {
			panic("a blocked_by read of no issue: " + r.Path)
		}
		reads[n]++
	}
	return reads
}

// A story's first task, from a plan, its story a story or an epic planned
// into tasks, end to end (SPEC §7.5, §8.1): the plan's tasks are created,
// and the pass after cuts the story's branch, issuedog/1-quotes-expire,
// from main, for its first task, #2, and picks nothing, writing nothing on
// GitHub. The next pass checks the new tip, main's commit, and posts its
// issuedog/tip as success, naming the branch, and pushes nothing, so the
// second pass cuts nothing. The one after picks #2, which moves to
// in-progress, and its run starts from that tip, which its lease names:
// three passes from the cut to the pick, the cut's included. #3, which #2
// and the human task #4 block, isn't picked, and nothing cuts a second
// branch.
func TestPickStoryFirstTask(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"story", "epic"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			doneParent(t, f, typ, storyTasks)
			v.out(1, ivResult(t, implementerOutput(t, "commits")))
			v.pass()
			if got := f.State().Issues[0].SubIssues; !slices.Equal(got, []int{2, 3, 4}) || len(pickStoryBranches(f)) != 0 {
				t.Fatalf("the parent's sub-issues are %v, the branches %+v; want its tasks, 2 to 4, and none yet", got, pickStoryBranches(f))
			}
			const branch = "issuedog/1-quotes-expire"
			tip := v.pickStoryCut(branch, 2)
			v.pickStoryChecked(branch, tip, "success", 2)
			v.pickStoryPicked(2, tip, "1")
			if got := pickStage(f, 3); got != "stage:ready" {
				t.Errorf("#3 is at %s; want it left at ready, as #2 blocks it", got)
			}
		})
	}
}

// The cut ends the pick for its pass (SPEC §8.1): with the story's task
// first in the pick's order, and a task with no parent after it, the pass
// that cuts the branch picks neither, the next checks the tip, and the one
// after picks the story's task, and the other waits for the worker.
func TestPickStoryCutEndsPick(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	alone := pickReady(f, "p2")
	f.AddLabel(alone, "owner", "scope:booking")
	tip := v.pickStoryCut(tipBranch, 2)
	if got := pickStage(f, alone); got != "stage:ready" {
		t.Fatalf("after the cut's pass, the task with no parent is at %s; want ready, as the cut ends the pick", got)
	}
	v.pickStoryChecked(tipBranch, tip, "success", 2)
	v.pickStoryPicked(2, tip, "1")
	if got := pickStage(f, alone); got != "stage:ready" {
		t.Errorf("after the pick, the task with no parent is at %s; want ready", got)
	}
}

// A second task in the story waits while the first, which blocks it, is
// open, and once that closes it's picked, and its run starts from the
// story branch's checked tip, with no second cut (SPEC §8.1): the first
// is picked, then held, which releases the worker, and the second still
// isn't picked, as the search finds it blocked. Once the first closes,
// the second is.
func TestPickStorySecondTask(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	second := pickStoryTask(f, "Show when a quote expires")
	f.SetBlockedBy(second, 2)
	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	tip := v.pickStoryCut(tipBranch, 2)
	v.pickStoryChecked(tipBranch, tip, "success", 2)
	v.pickStoryPicked(2, tip, "1")

	f.AddComment(2, "owner", "/hold")
	from := len(f.Requests())
	v.passes(2)
	if pickStage(f, 2) != "stage:held" || pickStage(f, second) != "stage:ready" || v.runs() != 1 {
		t.Fatalf("with the first held, it's at %s, the second at %s, with %d runs; want held, ready, and one", pickStage(f, 2), pickStage(f, second), v.runs())
	}
	if got := pickStoryReads(f, from); len(got) != 0 {
		t.Errorf("with the first held, the passes read blocked_by %v; want none, as the search finds the second blocked", got)
	}

	f.CloseBy(2, "owner")
	from = len(f.Requests())
	v.pass()
	if !f.State().Issues[1].Closed || pickStage(f, second) != "stage:verifying" || v.runs() != 2 {
		t.Fatalf("once the first closed, it's closed %t, the second at %s, with %d runs; want closed, verifying, and two",
			f.State().Issues[1].Closed, pickStage(f, second), v.runs())
	}
	if got, want := pickStoryReads(f, from), map[int]int{1: 1, second: 1}; !maps.Equal(got, want) {
		t.Errorf("the pick's pass read blocked_by %v; want %v, the second's and its story's", got, want)
	}
	if l := v.leaseOf("2"); l.Issue != second || l.Start != tip || l.StoryTip != tip {
		t.Errorf("the second run's lease = %+v; want it on #%d, from the story tip %s", l, second, tip)
	}
	if got, want := pickStoryBranches(f), []githubtest.BranchState{{Name: tipBranch, SHA: tip}}; !slices.Equal(got, want) {
		t.Errorf("the branches are %+v; want %+v alone", got, want)
	}
}

// A story another blocks gets no cut and no pick (SPEC §7.5, §8.1): the
// pass reads the task's blocked_by and the story's, once each, and finds
// the story blocked, so it cuts nothing, and the passes after read neither
// again, as the story found blocked isn't read again until the next
// search, so they're quiet. Once the blocking story closes, the poll lists
// it, the search is made again, both are read again, and the branch is
// cut, then checked, and the task picked.
func TestPickStoryBlocked(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	blocker := tipStoryAt(f, "Quote refunds", "planned", "type:story")
	f.SetBlockedBy(1, blocker)
	from := len(f.Requests())
	v.pass()
	if got, want := pickStoryReads(f, from), map[int]int{1: 1, 2: 1}; !maps.Equal(got, want) || len(pickStoryBranches(f)) != 0 || pickStage(f, 2) != "stage:ready" {
		t.Fatalf("the first pass read blocked_by %v, left the branches %+v, and the task at %s; want %v, none, and ready",
			got, pickStoryBranches(f), pickStage(f, 2), want)
	}
	v.pass() // the pass after the start's first, whose reads may still miss the ETag cache
	from = len(f.Requests())
	v.quietPass("the story blocked")
	if got := pickStoryReads(f, from); len(got) != 0 || len(pickStoryBranches(f)) != 0 {
		t.Errorf("an unchanged pass read blocked_by %v, and left the branches %+v; want neither read, and none", got, pickStoryBranches(f))
	}

	f.CloseBy(blocker, "owner")
	from = len(f.Requests())
	tip := v.pickStoryCut(tipBranch, 2)
	if got, want := pickStoryReads(f, from), map[int]int{1: 1, 2: 1}; !maps.Equal(got, want) {
		t.Errorf("the cut's pass read blocked_by %v; want %v", got, want)
	}
	v.pickStoryChecked(tipBranch, tip, "success", 2)
	v.pickStoryPicked(2, tip, "1")
}

// A task's own open blocker has it wait as before, and its story's
// blockers aren't read, nor its story's branch cut: the search lags here,
// so the task's blocked_by is read, and found blocked, and read no more
// until the next search.
func TestPickStoryOwnBlocker(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	other := tipStoryAt(f, "Something first", "planned", "type:story")
	f.FreezeSearch()
	f.SetBlockedBy(2, other)
	from := len(f.Requests())
	v.pass()
	if got, want := pickStoryReads(f, from), map[int]int{2: 1}; !maps.Equal(got, want) || len(pickStoryBranches(f)) != 0 || pickStage(f, 2) != "stage:ready" {
		t.Fatalf("the pass read blocked_by %v, left the branches %+v, and the task at %s; want %v, none, and ready",
			got, pickStoryBranches(f), pickStage(f, 2), want)
	}
	v.pass() // the pass after the start's first, whose poll lists the digest it wrote, so it searches again
	from = len(f.Requests())
	v.quietPass("blocked")
	if got := pickStoryReads(f, from); len(got) != 0 || len(pickStoryBranches(f)) != 0 {
		t.Errorf("an unchanged pass read blocked_by %v, and left the branches %+v; want neither read, and none", got, pickStoryBranches(f))
	}
}

// A failing tip, here main's commit, whose commands fail as main holds
// breaks.txt, has the task wait, uncounted, with no request and no
// blocked_by read, and that's logged once, not every pass, though the
// passes run step 4's tip checks too (SPEC §8.1). The failure files a bug
// under the story, #4, as the digest is #3, which the owner closes, so the
// tip's check alone holds the task here: an open bug's pick is
// story_failure_test.go's. Once a person's push gives the branch a tip
// that passes, it's checked, and the task picked from it. It reads logr,
// so it isn't parallel.
func TestPickStoryFailingTip(t *testing.T) {
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	v.o.commit("breaks.txt", "breaks the commands\n")
	tipStory(t, f)
	pickStoryTask(f, "Refuse an expired quote")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	cut := v.pickStoryCut(tipBranch, 2)
	v.pickStoryChecked(tipBranch, cut, "failure", 2)
	if bug := f.State().Issues[3]; !slices.Contains(bug.Labels, "type:bug") || bug.Parent != 1 {
		t.Fatalf("#4 = %+v; want the bug the failing tip filed under the story", bug)
	}
	f.CloseBy(4, "owner")

	stop := logtest.Capture(t, logr.I)
	from := len(f.Requests())
	v.passes(2) // the close, then the digest's change it brings, read back
	v.quietPass("the tip failing")
	v.quietPass("again")
	logs := stop()
	wait := "o/r#1: no task in it is picked, as its branch " + tipBranch + "'s tip " + cut + " has " + github.TipCheck + " as failure"
	if got := strings.Count(logs, wait); got != 1 {
		t.Errorf("the wait was logged %d times; want once, in\n%s", got, logs)
	}
	if got := pickStoryReads(f, from); len(got) != 0 || pickStage(f, 2) != "stage:ready" || v.runs() != 0 {
		t.Errorf("the passes read blocked_by %v, left the task at %s, and ran claude %d times; want no read, ready, and no run", got, pickStage(f, 2), v.runs())
	}

	tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": ""})
	v.pickStoryChecked(tipBranch, tip, "success", 2)
	v.pickStoryPicked(2, tip, "1")
}

// A branch an admin cut first, with the story's prefix but another slug,
// is the story's: the pick cuts none, the tip is checked, and the task is
// picked from it.
func TestPickStoryAdminBranch(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	const branch = "issuedog/1-cut-by-an-admin"
	tip := v.tipCut(branch)
	v.pickStoryChecked(branch, tip, "success", 2)
	v.pickStoryPicked(2, tip, "1")
	if got, want := pickStoryBranches(f), []githubtest.BranchState{{Name: branch, SHA: tip}}; !slices.Equal(got, want) {
		t.Errorf("the branches are %+v; want the admin's alone, %+v", got, want)
	}
}

// Two branches with the story's prefix: the daemon can't tell which is
// its branch, so it cuts none, picks nothing, and reads no blocked_by for
// it, and that's warned of once, not every pass, as the tip checks' own
// warning on the story is, on the same passes: neither one's makes the
// other's repeat. It reads logr, so it isn't parallel.
func TestPickStoryTwoBranches(t *testing.T) {
	f, v := newPickStoryRig(t)
	v.tipCut(tipBranch)
	v.tipCut("issuedog/1-another-slug")
	branches := pickStoryBranches(f)
	stop := logtest.Capture(t, logr.W)
	from := len(f.Requests())
	v.passes(4)
	logs := stop()
	for _, want := range []string{
		"o/r#1: no task in it is picked, and no branch is cut for it, as it can't tell which is its branch",
		"o/r#1: its branch's tip isn't checked, as it can't tell which is its branch",
	} {
		if got := strings.Count(logs, want); got != 1 {
			t.Errorf("%q was warned %d times; want once, in\n%s", want, got, logs)
		}
	}
	if got := pickStoryReads(f, from); len(got) != 0 || !slices.Equal(pickStoryBranches(f), branches) || pickStage(f, 2) != "stage:ready" || v.runs() != 0 {
		t.Errorf("the passes read blocked_by %v, left the branches %+v, the task at %s, and ran claude %d times; want no read, %+v, ready, and no run",
			got, pickStoryBranches(f), pickStage(f, 2), v.runs(), branches)
	}
	v.quietPass("two branches")
}

// A crash just after the cut's push, then a fresh daemon, on a bare clone
// that never saw the push: its sync shows the branch, found by its prefix,
// so it cuts no second one. Its first pass checks the tip, and its second
// picks the task. A fresh daemon after the tip's check reads that check
// once, and picks the task on the same pass.
func TestPickStoryRestart(t *testing.T) {
	t.Parallel()
	t.Run("after the push", func(t *testing.T) {
		t.Parallel()
		f, v := newPickStoryRig(t)
		tip := v.pickStoryCut(tipBranch, 2)
		v = &verdictRig{rig: newRig(t, f, v.o, t.TempDir()), o: v.o, cfg: v.cfg, ran: v.ran}
		v.wire()
		v.out(1, ivResult(t, implementerOutput(t, "commits")))
		v.pickStoryChecked(tipBranch, tip, "success", 2)
		v.pickStoryPicked(2, tip, "1")
	})
	t.Run("after the check", func(t *testing.T) {
		t.Parallel()
		f, v := newPickStoryRig(t)
		tip := v.pickStoryCut(tipBranch, 2)
		v.pickStoryChecked(tipBranch, tip, "success", 2)
		v = v.restart()
		v.out(1, ivResult(t, implementerOutput(t, "commits")))
		from := len(f.Requests())
		checks := len(f.State().CheckRuns)
		v.pickStoryPicked(2, tip, "1")
		if got := tipReads(f, from, tip); got != 1 || len(f.State().CheckRuns) != checks {
			t.Errorf("the restart's pass read the tip's checks %d times, and left %d checks; want once, and %d", got, len(f.State().CheckRuns), checks)
		}
	})
}

// A push refused as stale, as an admin cut the branch, at another commit,
// after the pass's sync, changes nothing: the task stays at ready, the
// branch is the admin's, and the next pass's sync shows it, so its tip is
// checked, and the pass after picks the task from it.
func TestPickStoryStalePush(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "admin", "main")
	gitIn(t, w, "commit", "--quiet", "--allow-empty", "-m", "an admin's cut")
	admin := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "checkout", "--quiet", "main")
	cut := false
	f.BeforeRequest(func(r githubtest.Request) bool { return !cut && r.Path == "/issues/1/dependencies/blocked_by" }, func() {
		cut = true
		gitIn(t, w, "push", "--quiet", v.o.url, "admin:refs/heads/"+tipBranch)
	})
	comments := len(f.State().Issues[1].Comments)
	v.pass()
	want := []githubtest.BranchState{{Name: tipBranch, SHA: admin}}
	if got := pickStoryBranches(f); !cut || !slices.Equal(got, want) || len(f.State().Issues[1].Comments) != comments || pickStage(f, 2) != "stage:ready" || v.runs() != 0 {
		t.Fatalf("the stale push's pass left the branches %+v, the task at %s with %d comments, and ran claude %d times; want the admin's, %+v, ready with %d, and no run",
			got, pickStage(f, 2), len(f.State().Issues[1].Comments), v.runs(), want, comments)
	}
	v.pickStoryChecked(tipBranch, admin, "success", 2)
	v.pickStoryPicked(2, admin, "1")
}

// The pick of a task in a story, then its run's move to verifying, a
// failed write at each of their writes, then the next pass: the replay
// ends with the same writes (SPEC §5.2). The story's branch is cut and its
// tip checked first, so the first pass reads the tip's check, then picks
// the task. Each fake keeps its daemon, as the loop does after a pass's
// error: a fresh daemon's first pass runs task work at in-progress before
// step 4 reads the tip's check, so a task the move took there waits a pass
// for its run (SPEC §3.4), which TestPickStoryRestart covers.
func TestPickStoryReplay(t *testing.T) {
	t.Parallel()
	_, base := newPickStoryRig(t)
	tip := base.tipCut(tipBranch)
	roots, rigs := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*verdictRig{}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		tipStory(tb, f)
		pickStoryTask(f, "Refuse an expired quote")
		if _, err := f.Client(tb).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + tipBranch}); err != nil {
			tb.Fatal(err)
		}
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}
	pass := func(f *githubtest.Fake) error {
		v := rigs[f]
		if v == nil {
			v = &verdictRig{rig: newRig(t, f, base.o, roots[f]), o: base.o, cfg: base.cfg, ran: base.ran}
			v.wire()
			v.d.Digest, v.d.Assign = false, false
			v.out(1, ivResult(t, implementerOutput(t, "commits")))
			// The commit's dates are fixed, so each replay's commit, and the
			// comment naming it, is the clean run's.
			v.r.Env = append(v.r.Env, "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
			rigs[f] = v
		}
		return v.d.Iterate(context.Background())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if got := pickStage(clean, 2); got != "stage:verifying" || !slices.ContainsFunc(clean.State().Issues[1].Comments, func(c githubtest.CommentState) bool {
		return strings.HasPrefix(c.Body, pickStoryMoveText)
	}) {
		t.Fatalf("the clean pass left the task at %s, with comments %+v; want it at verifying, picked", got, clean.State().Issues[1].Comments)
	}
	githubtest.Replay(t, setup, pass)
}

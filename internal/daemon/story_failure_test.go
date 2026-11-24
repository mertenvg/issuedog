package daemon_test

import (
	"context"
	"maps"
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
	"github.com/mertenvg/issuedog/internal/repo"
)

// A story's tasks wait while a failure on its branch is open, and the
// failure's bug goes first (SPEC §3.4's A story's waits, §7.5, §8.1), on
// verdictRigs wired as main's, with real temporary repos, whose branches
// the tests cut and push to in the origin itself, as an admin may. A
// failure is open while the branch's tip's issuedog/tip is failure, or a
// failure's bug of the story is open, and the bug takes a tip step 4
// judged that passes rule 2, at its pick, its run, its checks and its
// merge, which carries no check over.

// storyFailureBreaks is the rig's command that fails while breaks.txt is
// in the checkout.
const storyFailureBreaks = `, "test ! -f breaks.txt"`

// storyFailureBugAt files a bug below story, as the App files one for its
// branch's failing tip tip, with its first move to ready, then moves it
// to stage, as the App would, by moves, each a transition marker, and
// returns its number.
func storyFailureBugAt(f *githubtest.Fake, story int, tip, stage string, moves ...string) int {
	n := f.AddIssue(app, "Fix story #"+strconv.Itoa(story)+"'s branch, which fails its checks at "+tip[:7],
		"Rule 1 failed.\n\n<!-- issuedog:origin story=#"+strconv.Itoa(story)+" tip="+tip+" -->", "type:bug", "priority:p1")
	f.SetParent(n, story)
	f.AddLabel(n, app, "stage:"+stage)
	f.AddComment(n, app, "Filed.\n\n<!-- issuedog:transition from=intake to=ready event=t"+tip+" -->")
	for _, m := range moves {
		f.AddComment(n, app, "Moved.\n\n"+m)
	}
	return n
}

// storyFailureNewest is the number of the newest issue on f, as a failing
// tip's filing leaves it, failing the test unless it's a bug below story
// whose origin names tip.
func storyFailureNewest(t *testing.T, f *githubtest.Fake, story int, tip string) int {
	t.Helper()
	n := len(f.State().Issues)
	is := f.State().Issues[n-1]
	origin := "<!-- issuedog:origin story=#" + strconv.Itoa(story) + " tip=" + tip + " -->"
	if !slices.Contains(is.Labels, "type:bug") || is.Parent != story || !strings.HasSuffix(is.Body, origin) || is.Closed {
		t.Fatalf("#%d = %+v; want the open bug filed below #%d for %s", n, is, story, tip)
	}
	return n
}

// storyFailureHead commits files on base in v's origin's work tree, each
// path to its content, or deleted for "", goes back to main, makes the
// commit issue n's head in v's daemon clone, as an earlier run's handoff
// left it (SPEC §3.2), and returns it. The origin never gets it.
func (v *verdictRig) storyFailureHead(n int, base string, files map[string]string) string {
	v.t.Helper()
	w := v.o.work
	gitIn(v.t, w, "checkout", "--quiet", "-B", "bugfix", base)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if files[path] == "" {
			gitIn(v.t, w, "rm", "--quiet", path)
			continue
		}
		full := filepath.Join(w, path)
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			v.t.Fatal(err)
		}
		gitIn(v.t, w, "add", path)
	}
	gitIn(v.t, w, "commit", "--quiet", "-m", "the bug's earlier run")
	head := gitIn(v.t, w, "rev-parse", "HEAD")
	gitIn(v.t, w, "checkout", "--quiet", "main")
	gitIn(v.t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", w, "+refs/heads/bugfix:"+repo.TasksRef+strconv.Itoa(n))
	return head
}

// A failing tip, here main's commit the pick cut the branch from, which
// fails the commands as main holds breaks.txt, files a bug at ready
// (SPEC §7.5). The next pass doesn't pick the story's other task, #2,
// which waits, uncounted, with no blocked_by read, and that's logged once,
// but picks the bug, from that failing tip, which its lease names as its
// start and story tip, with no target. After a restart, that pass first
// reads the tip's check, one request, and runs no command. The bug's
// checks at verifying, the pass after, run against the failing tip, which
// its findings name, and send it back to in-progress. There its retry
// waits while a later tip fails rule 2, uncounted, logged once, and once
// a tip after that passes rule 2, though it fails the commands still, the
// retry starts from the bug's head with that tip named as its target, to
// merge first (SPEC §7.5, When the branch moves under a failure's bug).
func TestStoryFailureBugFirst(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	for _, restart := range []bool{false, true} {
		t.Run("restart "+strconv.FormatBool(restart), func(t *testing.T) { storyFailureBugFirst(t, restart) })
	}
}

// storyFailureBugFirst is TestStoryFailureBugFirst, with a restart before
// the pick's pass, with restart.
func storyFailureBugFirst(t *testing.T, restart bool) {
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", storyFailureBreaks)
	v.o.commit("breaks.txt", "breaks the commands\n")
	tipStory(t, f)
	pickStoryTask(f, "Refuse an expired quote")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	cut := v.pickStoryCut(tipBranch, 2)
	v.pickStoryChecked(tipBranch, cut, "failure", 2)
	bug := storyFailureNewest(t, f, 1, cut)
	if restart {
		v = v.restart()
	}

	stop := logtest.Capture(t, logr.I)
	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	logs := stop()
	if got := tipReads(f, from, cut); v.checksRan() != ran || restart != (got == 1) || got > 1 {
		t.Errorf("the pick's pass read the tip's checks %d times, and ran the commands %d times; want once after a restart, else none, and no command",
			got, v.checksRan()-ran)
	}
	wait := "o/r#1: no task in it is picked, as its branch " + tipBranch + "'s tip " + cut + " has " + github.TipCheck + " as failure"
	if got := strings.Count(logs, wait); got != 1 {
		t.Errorf("the sibling's wait was logged %d times; want once, in\n%s", got, logs)
	}
	if got := pickStoryReads(f, from); got[2] != 0 || got[bug] != 1 {
		t.Errorf("the pick's pass read blocked_by %v; want the bug's once, and none of #2's", got)
	}
	if v.runs() != 1 || pickStage(f, 2) != "stage:ready" || pickStage(f, bug) != "stage:verifying" {
		t.Fatalf("the pick's pass ran claude %d times, and left #2 at %s, the bug at %s; want once, ready, and verifying",
			v.runs(), pickStage(f, 2), pickStage(f, bug))
	}
	if l := v.leaseOf("1"); l.Issue != bug || l.Start != cut || l.StoryTip != cut || l.Target != "" {
		t.Errorf("the bug's run's lease = %+v; want it on #%d, from the failing tip %s, which it names, with no target", l, bug, cut)
	}

	v.pass()
	moves := storyFailureComments(f, bug, "<!-- issuedog:transition from=verifying to=in-progress ")
	if len(moves) != 1 || pickStage(f, bug) != "stage:in-progress" {
		t.Fatalf("the checks' pass left the bug at %s, with the findings moves %q; want in-progress, by one", pickStage(f, bug), moves)
	}
	if against := "checked against the story branch `" + tipBranch + "` at `" + cut + "`"; !strings.Contains(moves[0], against) {
		t.Errorf("the findings =\n%s\nwant them %s", moves[0], against)
	}

	second := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	stop = logtest.Capture(t, logr.I)
	v.passes(3)
	logs = stop()
	wait = "#" + strconv.Itoa(bug) + ": no implementer run: its story branch " + tipBranch + "'s tip " + second +
		" isn't one step 4 judged that passes rule 2, which a failure's bug takes, so it isn't named as the run's base"
	if got := strings.Count(logs, wait); got != 1 || v.runs() != 1 || pickStage(f, bug) != "stage:in-progress" {
		t.Errorf("on a tip that fails rule 2, the bug's wait was logged %d times, claude ran %d times, and the bug is at %s; want once, once, and in-progress, in\n%s",
			got, v.runs(), pickStage(f, bug), logs)
	}
	third := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": ""})
	v.out(2, ivResult(t, implementerOutput(t, "commits")))
	v.passes(2)
	if v.runs() != 2 {
		t.Fatalf("claude ran %d times once a tip passes rule 2 again; want twice", v.runs())
	}
	if l, head := v.leaseOf("2"), v.trimmed("commit.1"); l.Issue != bug || l.Start != head || l.Target != third || l.StoryTip != third {
		t.Errorf("the bug's retry's lease = %+v; want it on #%d, from its head %s, with the newest tip %s as its target and story tip", l, bug, head, third)
	}
}

// storyFailureComments are the bodies of issue n's App comments on f that
// hold text, oldest first.
func storyFailureComments(f *githubtest.Fake, n int, text string) []string {
	var found []string
	for _, c := range f.State().Issues[n-1].Comments {
		if c.Author == app && strings.Contains(c.Body, text) {
			found = append(found, c.Body)
		}
	}
	return found
}

// While a failure's bug of the story is open, here handed off at
// needs-human, its story's other task, #2, waits, even on a tip whose
// check is success (SPEC §3.4's A story's waits, §8.1): at ready it isn't
// picked, and at in-progress, as on a retry after findings, its run
// doesn't start, each uncounted, logged once, with no request but 304s on
// the passes that wait, no blocked_by read and no run. A fresh daemon
// reads the tip's check once, and #2 still waits. Once the owner closes
// the bug, #2 is picked, or its run starts, from the tip.
func TestStoryFailureSiblingWaits(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	for _, tt := range []struct {
		stage string
		wait  string // the log's line on #2's wait, with the tip for %s
	}{
		{"ready", "o/r#1: no task in it but a failure's bug is picked, as a failure on its story branch " + tipBranch + " is open, as its bug #3 is, which goes first"},
		{"in-progress", "#2: no implementer run: its story branch " + tipBranch + "'s tip %s is held, as a failure on its story branch " + tipBranch +
			" is open, as its bug #3 is, which goes first"},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			tipStory(t, f)
			if tt.stage == "ready" {
				pickStoryTask(f, "Refuse an expired quote")
			} else {
				storyRunTask(t, f)
			}
			cut := v.tipCut(tipBranch)
			if bug := storyFailureBugAt(f, 1, strings.Repeat("e", 40), "needs-human",
				"<!-- issuedog:transition from=ready to=in-progress event=s1 -->",
				"<!-- issuedog:transition from=in-progress to=needs-human run=run1 -->"); bug != 3 {
				t.Fatalf("the bug is #%d, want #3", bug)
			}
			v.out(1, ivResult(t, implementerOutput(t, "commits")))
			wait := strings.Replace(tt.wait, "%s", cut, 1)

			stop := logtest.Capture(t, logr.I)
			v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
			from := len(f.Requests())
			v.passes(3)
			v.quietPass("the bug open")
			v.quietPass("again")
			logs := stop()
			if got := strings.Count(logs, wait); got != 1 {
				t.Errorf("#2's wait was logged %d times; want once, in\n%s", got, logs)
			}
			if got := pickStoryReads(f, from); len(got) != 0 || v.runs() != 0 || pickStage(f, 2) != "stage:"+tt.stage {
				t.Errorf("the passes read blocked_by %v, ran claude %d times, and left #2 at %s; want no read, no run, and %s",
					got, v.runs(), pickStage(f, 2), tt.stage)
			}
			if l := v.lease(); l.RunID != "" || l.Attempt != 0 {
				t.Errorf("the lease is %+v; want none, and no attempt counted", l)
			}

			v = v.restart()
			from = len(f.Requests())
			v.pass()
			if got := tipReads(f, from, cut); got != 1 || v.runs() != 0 {
				t.Errorf("a fresh daemon's first pass read the tip's checks %d times, and ran claude %d times; want once, and no run", got, v.runs())
			}
			v.quietPass("after the restart")

			f.CloseBy(3, "owner")
			v.pass()
			if v.runs() != 1 || pickStage(f, 2) != "stage:verifying" {
				t.Fatalf("the pass after the bug's close ran claude %d times, and left #2 at %s; want once, and verifying", v.runs(), pickStage(f, 2))
			}
			if l := v.leaseOf("1"); l.Issue != 2 || l.StoryTip != cut {
				t.Errorf("#2's run's lease = %+v; want it on #2, naming the story tip %s", l, cut)
			}
		})
	}
}

// A review's move of a task in a story at pr-open into in-progress waits
// while a failure's bug of its story is open, though its story branch's
// tip is success (SPEC §3.4's A story's waits): it's kept, with no write,
// its PR isn't merged meanwhile, and the passes that wait make no request
// but 304s. Once the owner closes the bug, the next pass makes the move.
// The rig picks nothing (pickOff), so the bug stays at ready.
func TestStoryFailureReviewWaits(t *testing.T) {
	t.Parallel()
	f, v, _, _, pr := storyMergeReady(t)
	v.pickOff()
	bug := storyFailureBugAt(f, 2, strings.Repeat("e", 40), "ready")
	f.ReviewPR(pr, wholeRunOwner, github.ReviewChangesRequested, "Rename it.", nil)
	writes := len(f.Writes())
	v.pass()
	if pickStage(f, 1) != "stage:pr-open" || len(f.Writes()) != writes {
		t.Fatalf("the review's pass left the task at %s, and wrote %+v; want pr-open, and nothing", pickStage(f, 1), f.Writes()[writes:])
	}
	v.quietPass("a review waiting on the open failure")
	v.quietPass("again")
	f.CloseBy(bug, "owner")
	v.pass()
	if moves := storyFailureComments(f, 1, "<!-- issuedog:transition from=pr-open to=in-progress "); len(moves) != 1 || pickStage(f, 1) != "stage:in-progress" {
		t.Fatalf("the pass after the bug's close left the task at %s, with its moves to in-progress %q; want in-progress, by one", pickStage(f, 1), moves)
	}
	if got := storyMerges(f, 0, pr); len(got) != 0 {
		t.Errorf("the merges = %+v; want none", got)
	}
}

// A sibling's move back on its story branch's tip's moving waits while a
// failure's bug of its story is open (SPEC §3.4's A story's waits): its
// head conflicts with task 1's squash commit, whose check is carried over
// as success, so issuedog/verify is failure on its head, naming the new
// tip, but its move pr-open → in-progress is kept, with no write, and the
// passes that wait make no request but 304s. Once the owner closes the
// bug, the next pass makes the move, keyed by the tip, with the seen= it
// was found with (prWaitingMain). The rig picks nothing (pickOff).
func TestStoryFailureMovedTipWaits(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	v.pickOff()
	head, _ := storyTipSibling(t, f, v, tip, map[string]string{"quote.txt": "stale\n"})
	v.pass()
	squash := v.storyMerged(pr, tip)
	bug := storyFailureBugAt(f, 2, strings.Repeat("e", 40), "ready")
	seen := storyTipSeen(t, f, 5)
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) ||
		!slices.Contains(got, storyTipVerify(head, squash, "failure")) || pickStage(f, 5) != "stage:pr-open" || len(storyTipMoves(f, 5)) != 0 {
		t.Fatalf("the pass after the merge left the checks %+v, the sibling at %s, with the moves %q; want the carried success on %s, a failure on %s naming it, pr-open, and no move",
			got, pickStage(f, 5), storyTipMoves(f, 5), squash, head)
	}
	v.passes(2)
	v.quietPass("a move back waiting on the open failure")
	if moves := storyTipMoves(f, 5); len(moves) != 0 || pickStage(f, 5) != "stage:pr-open" {
		t.Fatalf("while the bug is open, the sibling is at %s, with the moves %q; want pr-open, and none", pickStage(f, 5), moves)
	}
	f.CloseBy(bug, "owner")
	v.pass()
	storyTipMoved(t, f, 5, "stage:in-progress", storyTipMovedText(head, squash, false), squash, seen)
}

// storyFailureMergeReady files bug #1, the App's for a failing tip of
// story #2's branch, at verifying, with a clean pass of the verifier's on
// its head, and task #3, at ready, in the same story, on a
// newStoryVerifyRig: the branch is cut, and a commit of the story's
// pushed. With failing, a commit that breaks personBrokenCommand is pushed
// too, and the bug is filed for it, its head fixing it; else the bug was
// filed for an earlier tip, and its head is on the branch's tip, which
// passes. #3's first run commits. It returns the fake, the rig, the
// branch's tip and the bug's head.
func storyFailureMergeReady(t *testing.T, failing bool) (*githubtest.Fake, *verdictRig, string, string) {
	t.Helper()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	filed, fix := strings.Repeat("e", 40), map[string]string{"fix.txt": "the bug's fix\n"}
	if failing {
		tip = v.tipPush(storyVerifyBranch, map[string]string{"broken.txt": "the story broke it\n"})
		filed, fix = tip, map[string]string{"broken.txt": ""}
	}
	if n := f.AddIssue(app, "Fix story #2's branch, which fails its checks at "+filed[:7],
		"Rule 1 failed.\n\n<!-- issuedog:origin story=#2 tip="+filed+" -->", "type:bug", "priority:p1"); n != 1 {
		t.Fatalf("the bug is #%d, want #1", n)
	}
	if n := tipStoryAt(f, tipStoryTitle, "planned", "type:story"); n != 2 {
		t.Fatalf("the story is #%d, want #2", n)
	}
	f.SetParent(1, 2)
	f.AddLabel(1, app, "stage:verifying")
	f.AddComment(1, app, "Filed.\n\n<!-- issuedog:transition from=intake to=ready event=t"+filed+" -->")
	head := v.storyFailureHead(1, tip, fix)
	f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->")
	f.AddComment(1, app, prPassComment(t, "rv", head, "Fine."))
	sibling := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	if sibling != 3 {
		t.Fatalf("the sibling is #%d, want #3", sibling)
	}
	f.SetParent(3, 2)
	f.AddLabel(3, app, "stage:ready")
	f.AddComment(3, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	return f, v, tip, head
}

// A failure's bug is verified and merged onto its story branch's tip,
// here a failing one that passes rule 2, or one that passes (SPEC §7.5's A
// checked base): step 4 judges the tip, and for a failing one, finds the
// bug filed for it, so it files nothing; the bug's checks at verifying run
// against that tip, and pass, as its head fixes it; the PR step opens its
// PR into the story branch, verified against that tip; and the pass after
// merges it there, while the story's other task, #3, isn't picked. The
// pass after that carries no check over to the squash commit, even from a
// passing tip, closes the bug, and checks the squash commit afresh,
// running the commands, as success. Then #3 is picked from it.
func TestStoryFailureBugMerges(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		failing    bool
		conclusion string
	}{
		{"onto a failing tip", true, "failure"},
		{"onto a passing tip", false, "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storyFailureBugMerges(t, tt.failing, tt.conclusion)
		})
	}
}

// storyFailureBugMerges is TestStoryFailureBugMerges on a bug merged onto
// a tip that fails, with failing, whose check is conclusion.
func storyFailureBugMerges(t *testing.T, failing bool, conclusion string) {
	f, v, tip, head := storyFailureMergeReady(t, failing)
	ran := v.checksRan()
	v.pass()
	bugs := slices.DeleteFunc(slices.Clone(f.State().Issues), func(is githubtest.IssueState) bool { return !slices.Contains(is.Labels, "type:bug") })
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(tip, storyVerifyBranch, conclusion)) || v.checksRan() != ran+1 || len(bugs) != 1 {
		t.Fatalf("the tip's pass left the checks %+v, ran the commands %d times, and left the bugs %+v; want its %s, once, and #1 alone",
			got, v.checksRan()-ran, bugs, conclusion)
	}
	v.pass()
	if pickStage(f, 1) != "stage:verifying" || v.checksRan() != ran+2 {
		t.Fatalf("the bug's checks' pass left it at %s, and ran the commands %d times; want verifying, and once more", pickStage(f, 1), v.checksRan()-ran)
	}
	v.pass()
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].Base != storyVerifyBranch || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the PR step's pass left the PRs %+v, the bug at %s; want one into %s at %s, and pr-open", prs, pickStage(f, 1), storyVerifyBranch, head)
	}
	pr := prs[0].Number
	moves := storyFailureComments(f, 1, "<!-- issuedog:transition from=verifying to=pr-open ")
	if len(moves) != 1 || prMoveFields(t, moves[0])["tip"] != tip {
		t.Fatalf("the bug's moves to pr-open = %q; want one, with tip=%s", moves, tip)
	}

	v.pass()
	if got := storyMerges(f, 0, pr); len(got) != 1 || pickStage(f, 3) != "stage:ready" || v.runs() != 0 {
		t.Fatalf("the merge's pass made the merges %+v, left #3 at %s, and ran claude %d times; want one, ready, and no run", got, pickStage(f, 3), v.runs())
	}
	squash := v.originTip(storyVerifyBranch)
	if got := gitIn(t, v.o.url, "rev-list", "--parents", "-n", "1", squash); got != squash+" "+tip {
		t.Errorf("the story branch's tip and its parents are %q; want %s with the one parent %s", got, squash, tip)
	}

	ran = v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran+1 {
		t.Fatalf("the pass after the merge left the checks %+v, and ran the commands %d times; want success on %s, checked afresh, once", got, v.checksRan()-ran, squash)
	}
	for _, c := range prMoveChecks(t, f) {
		if c.HeadSHA == squash && strings.Contains(c.Output.Summary, "carried this check over") {
			t.Errorf("the squash commit's check = %+v; want it checked afresh, not carried over", c)
		}
	}
	if !f.State().Issues[0].Closed || pickStage(f, 3) != "stage:ready" || v.runs() != 0 {
		t.Fatalf("the pass after the merge left the bug closed %t, #3 at %s, and claude run %d times; want closed, ready, and no run",
			f.State().Issues[0].Closed, pickStage(f, 3), v.runs())
	}

	v.pass()
	if v.runs() != 1 || pickStage(f, 3) != "stage:verifying" {
		t.Fatalf("the pass after the fresh check ran claude %d times, and left #3 at %s; want once, and verifying", v.runs(), pickStage(f, 3))
	}
	if l := v.leaseOf("1"); l.Issue != 3 || l.Start != squash || l.StoryTip != squash {
		t.Errorf("#3's run's lease = %+v; want it on #3, from the squash commit %s, which it names", l, squash)
	}
}

// Once the story branch moves past the tip a failure's bug was filed for,
// the bug waits for a tip that passes rule 2 (SPEC §7.5, When the branch
// moves under a failure's bug): a later tip that changes a protected path
// files its human task, and the bug, at ready, with a head of an earlier
// run's on the tip it was filed for, isn't picked, uncounted, logged once,
// and the passes that wait make no request but 304s. A tip after that
// reverts the protected path but still fails the commands comments on the
// bug, and the next pass picks it, its run starting from its head with
// that newest tip named as its target, to merge first, as its lease says.
func TestStoryFailureMovedPast(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", storyFailureBreaks)
	tipStory(t, f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	first := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	v.tipJudged(first, 1, tipFailureChecks(cut, first))
	bug := storyFailureNewest(t, f, 1, first)
	head := v.storyFailureHead(bug, first, map[string]string{"fix.txt": "the bug's earlier work\n"})

	second := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	stop := logtest.Capture(t, logr.I)
	v.tipJudged(second, 0, tipFailureChecks(cut, first, second))
	if is := f.State().Issues[bug]; !strings.HasSuffix(is.Body, tipFailureOrigin(second, true)) {
		t.Fatalf("#%d's body =\n%s\nwant the human task for %s", bug+1, is.Body, second)
	}
	v.passes(3)
	v.quietPass("the bug waiting for a tip that passes rule 2")
	logs := stop()
	wait := "o/r#" + strconv.Itoa(bug) + " isn't picked, as its story #1's branch " + tipBranch + "'s tip " + second +
		" isn't one step 4 judged that passes rule 2, which a failure's bug takes"
	if got := strings.Count(logs, wait); got != 1 || v.runs() != 0 || pickStage(f, bug) != "stage:ready" {
		t.Errorf("the bug's wait was logged %d times, claude ran %d times, and the bug is at %s; want once, no run, and ready, in\n%s",
			got, v.runs(), pickStage(f, bug), logs)
	}

	third := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": ""})
	v.tipJudged(third, 1, tipFailureChecks(cut, first, second, third))
	if got := storyFailureComments(f, bug, "<!-- issuedog:failure tip="+third+" -->"); len(got) != 1 {
		t.Fatalf("the bug's comments on the newest failing tip = %q; want one", got)
	}
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	if v.runs() != 1 || pickStage(f, bug) != "stage:verifying" {
		t.Fatalf("the pass after the newest tip's check ran claude %d times, and left the bug at %s; want once, and verifying", v.runs(), pickStage(f, bug))
	}
	if l := v.leaseOf("1"); l.Issue != bug || l.Start != head || l.Target != third || l.StoryTip != third {
		t.Errorf("the bug's run's lease = %+v; want it on #%d, from its head %s, with the newest tip %s as its target and story tip", l, bug, head, third)
	}
}

// The App's merge of a failure's bug's PR onto its story branch's failing
// tip, a crash at each of its writes, then a restart: the replay ends with
// the PR merged once, the bug closed, and the squash commit checked afresh
// as success, never carried over (SPEC §5.2, §7.5's A checked base). Each
// pass is a fresh daemon on the fake's own bare clone, so its first
// iteration reads the failing tip's check, its second merges, and its
// third checks the new tip as any push and closes the bug, or after a
// crash, does what's left. The origin is shared, so each setup puts the
// bug's branch and the story branch back.
func TestStoryFailureMergeReplay(t *testing.T) {
	t.Parallel()
	_, base := newStoryVerifyRig(t)
	base.storyCut()
	broken := base.tipPush(storyVerifyBranch, map[string]string{"broken.txt": "the story broke it\n"})
	head := base.storyFailureHead(1, broken, map[string]string{"broken.txt": ""})
	gitIn(t, base.o.work, "push", "--quiet", base.o.url, head+":refs/heads/"+prBranch)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		gitIn(tb, base.o.url, "update-ref", "refs/heads/"+prBranch, head)
		gitIn(tb, base.o.url, "update-ref", "refs/heads/"+storyVerifyBranch, broken)
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		bug := f.AddIssue(app, "Refuse an expired quote", "Rule 1 failed.\n\n<!-- issuedog:origin story=#2 tip="+broken+" -->", "type:bug", "priority:p1")
		if story := tipStoryAt(f, tipStoryTitle, "planned", "type:story"); bug != 1 || story != 2 {
			tb.Fatalf("the bug is #%d, the story #%d; want #1 and #2", bug, story)
		}
		f.SetParent(1, 2)
		c := f.Client(tb)
		pr, err := c.CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: "fix: Refuse an expired quote", Head: prBranch,
			Base: storyVerifyBranch, Body: "Task #1, in story #2\n\nThe fix."})
		if err != nil {
			tb.Fatal(err)
		}
		f.AddLabel(pr.Number, app, "priority:p1")
		f.AddLabel(1, app, "stage:pr-open")
		f.AddComment(1, app, "Filed.\n\n<!-- issuedog:transition from=intake to=ready event=t"+broken+" -->")
		f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->")
		f.AddComment(1, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr="+strconv.Itoa(pr.Number)+" tip="+broken+" -->")
		for _, run := range []struct{ name, sha, id, conclusion string }{
			{github.TipCheck, broken, "branch=" + storyVerifyBranch, "failure"},
			{github.VerifyCheck, head, "main=" + base.mainSHA(), "success"},
		} {
			if _, err := c.PostCheck(context.Background(), "o", "r", run.name, github.NewCheckRun{HeadSHA: run.sha, Conclusion: run.conclusion,
				Title: "t", Summary: "s", ExternalID: run.id}); err != nil {
				tb.Fatal(err)
			}
		}
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, base.o, roots[f])
		if clones[f] == nil {
			clones[f] = g.d.Clone
		}
		g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
		v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
		v.wire()
		gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", base.o.work, "+refs/heads/bugfix:"+repo.TasksRef+"1")
		for range 5 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for f := range roots {
		prs := f.State().PRs
		if len(prs) != 1 || !prs[0].Merged || prs[0].MergedBy != app || !f.State().Issues[0].Closed {
			t.Fatalf("after the replay, the PRs = %+v, the bug closed %t; want one, merged by %s, and the bug closed", prs, f.State().Issues[0].Closed, app)
		}
		if got := f.State().CheckRuns; !slices.ContainsFunc(got, func(c githubtest.CheckRunState) bool {
			return c.Name == github.TipCheck && c.Conclusion == "success" && c.SHA != broken
		}) {
			t.Errorf("after the replay, the checks = %+v; want success on the squash commit", got)
		}
	}
}

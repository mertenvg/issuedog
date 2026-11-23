package daemon_test

import (
	"context"
	"fmt"
	"maps"
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

// A task in a story whose PR merges into its story branch (SPEC §6.4's
// Merges into a story branch, §7.2, §7.5), on a newStoryVerifyRig, with
// real temporary repos: task #1 in story #2 is at pr-open, its PR into the
// story branch open at its verified head, as the PR step left it
// (storyPRReady), and the fake's MergePR squashes the PR onto the story
// branch in the origin, as the App, a trusted user or anyone else. The
// App's merge carries the tip's check over to the merge commit, then the
// task's branch is deleted, then the task closes; a trusted user's carries
// nothing, so step 4 checks the merged tip; anyone else's closes nothing,
// and hands the task to the owner.

// mergedReady is storyPRReady, then the pass that opens the PR and moves
// the task to pr-open (storyPROpened). It returns the fake, the rig, the
// story branch's tip the head was verified against, the head, the pass
// comment's ID and the PR's number.
func mergedReady(t *testing.T) (*githubtest.Fake, *verdictRig, string, string, int64, int) {
	t.Helper()
	f, v, _, tip, head, pass := storyPRReady(t)
	v.pass()
	return f, v, tip, head, pass, storyPROpened(t, f, v, head, tip, pass)
}

// mergedMerge merges PR pr as login, which squashes it onto the story
// branch in v's origin, and returns the merge commit, the branch's new tip.
func (v *verdictRig) mergedMerge(pr int, login string) string {
	v.t.Helper()
	before := v.originTip(storyVerifyBranch)
	if v.f.MergePR(pr, login) == 0 {
		v.t.Fatalf("PR #%d didn't merge", pr)
	}
	commit := v.originTip(storyVerifyBranch)
	if commit == before || gitIn(v.t, v.o.url, "rev-parse", commit+"^") != before {
		v.t.Fatalf("the merge left the story branch at %s, from %s; want a squash commit on it", commit, before)
	}
	return commit
}

// mergedMove is task 1's one move keyed event=g<pr>, its comment and its
// marker's fields, failing the test unless there's exactly one.
func mergedMove(t *testing.T, f *githubtest.Fake, pr int) (github.Comment, map[string]string) {
	t.Helper()
	moves := personComments(t, f, " event=g"+strconv.Itoa(pr)+" ")
	if len(moves) != 1 {
		t.Fatalf("task 1's moves on PR #%d's merge = %+v; want one", pr, moves)
	}
	return moves[0], prMoveFields(t, moves[0].Body)
}

// mergedClosed fails unless task 1 is closed by one move from from to
// closed, as completed, keyed event=g<pr>, its comment starting text.
func mergedClosed(t *testing.T, f *githubtest.Fake, pr int, from, text string) {
	t.Helper()
	move, fields := mergedMove(t, f, pr)
	want := map[string]string{"from": from, "to": "closed", "reason": "completed", "event": "g" + strconv.Itoa(pr)}
	if !maps.Equal(fields, want) || !strings.HasPrefix(move.Body, text) {
		t.Errorf("the merge's move =\n%s\nwith %v; want %v, starting\n%s", move.Body, fields, want, text)
	}
	if !f.State().Issues[0].Closed {
		t.Error("task 1 is open; want it closed")
	}
}

// mergedWrites are the writes since from, as writesOf gives them, to task
// 1's comments, labels and state, to a branch and to check runs, leaving
// out the digest's and assignment's, which a hand-off's item changes.
func mergedWrites(f *githubtest.Fake, from int) []string {
	var got []string
	for _, w := range writesOf(f)[from:] {
		_, path, _ := strings.Cut(w, " ")
		if path == "/issues/1" || strings.HasPrefix(path, "/issues/1/comments") || strings.HasPrefix(path, "/issues/1/labels") ||
			strings.HasPrefix(path, "/git/") || path == "/check-runs" {
			got = append(got, w)
		}
	}
	return got
}

// mergedPRReads counts the requests since from that read PR pr alone.
func mergedPRReads(f *githubtest.Fake, from, pr int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/pulls/"+strconv.Itoa(pr) {
			n++
		}
	}
	return n
}

// mergedCarried is the summary of the check carried over to commit, the
// App's squash merge of task 1's head onto tip, linking pass.
func mergedCarried(pr int, commit, tip, head string, pass int64) string {
	return "issuedog carried this check over to `" + commit + "`, the tip of `" + storyVerifyBranch + "` for story #2: it's the App's squash merge of PR #" +
		strconv.Itoa(pr) + ", task #1's head `" + head + "`, onto `" + tip + "`, the tip the head was verified against, whose check passed, " +
		"so its tree is the verified head's. The verifier passed the head in [its comment](https://github.com/o/r/issues/1#issuecomment-" +
		strconv.FormatInt(pass, 10) + ")."
}

// The App's merge at pr-open: the next pass reads the PR once, posts
// issuedog/tip as success on the merge commit, naming the branch, whose
// summary names the merge and links the verifier's pass, as its parent is
// the tip the head was verified against and that tip is success; then it
// deletes the task's branch, then closes the task as completed, keyed
// event=g<PR>, naming the merge commit. No command runs: step 4 takes the
// carried check for the new tip. The passes after make no request for the
// PR.
func TestPRMergedApp(t *testing.T) {
	t.Parallel()
	f, v, tip, head, pass, pr := mergedReady(t)
	commit := v.mergedMerge(pr, app)
	from, writes, ran := len(f.Requests()), len(f.Writes()), v.checksRan()
	v.pass()
	want := []string{"POST /check-runs", "DELETE /git/refs/heads/" + prBranch, "POST /issues/1/comments", "PATCH /issues/1"}
	if got := mergedWrites(f, writes); !slices.Equal(got, want) {
		t.Errorf("the merge's pass wrote %q; want %q", got, want)
	}
	if got := mergedPRReads(f, from, pr); got != 1 {
		t.Errorf("the merge's pass read PR #%d %d times; want once", pr, got)
	}
	check := tipLast(t, f)
	if check.HeadSHA != commit || check.Name != github.TipCheck || check.Conclusion != "success" || check.Output.Summary != mergedCarried(pr, commit, tip, head, pass) {
		t.Errorf("the carried check = %+v; want issuedog/tip success on %s, its summary\n%s", check, commit, mergedCarried(pr, commit, tip, head, pass))
	}
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(commit, storyVerifyBranch, "success")) {
		t.Errorf("the check runs = %+v; want %+v among them", got, tipCheck(commit, storyVerifyBranch, "success"))
	}
	if got := v.originTip(prBranch); got != "" {
		t.Errorf("the task's branch is at %q; want it deleted", got)
	}
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+commit+"`, so this task is complete.\n\n")
	if v.checksRan() != ran || v.runs() != 0 {
		t.Errorf("the merge's pass ran the commands %d times, claude %d; want neither", v.checksRan()-ran, v.runs())
	}
	v.passes(2)
	if got := pickStage(f, 1); got != "stage:completed" {
		t.Errorf("the task is at %s; want stage:completed", got)
	}
	from = len(f.Requests())
	v.quietPass("after the merge's close")
	if got := mergedPRReads(f, from, pr); got != 0 {
		t.Errorf("the quiet pass read PR #%d %d times; want never", pr, got)
	}
}

// A trusted user's merge carries no check over (SPEC §7.5's Someone else's
// merge): at pr-open, at needs-human on an earlier hand-off, and at
// in-progress after a review sent the task back, the next pass deletes
// the task's branch and closes the task as completed, keyed event=g<PR>,
// naming the merger, from pr-open or needs-human on merged, and from
// in-progress on closed, and step 4 checks the merged tip as any push,
// running the commands once and posting its issuedog/tip. No claude runs.
func TestPRMergedTrusted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		stage string
		to    func(f *githubtest.Fake, v *verdictRig, pr int)
	}{
		{"pr-open", func(*githubtest.Fake, *verdictRig, int) {}},
		{"needs-human", func(f *githubtest.Fake, v *verdictRig, pr int) {
			f.ClosePR(pr, wholeRunOwner)
			v.pass()
			f.ReopenPR(pr, "mallory")
		}},
		{"in-progress", func(f *githubtest.Fake, v *verdictRig, pr int) {
			f.ReviewPR(pr, wholeRunOwner, github.ReviewChangesRequested, "Rename it.", nil)
			v.pass()
		}},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()
			f, v, _, _, _, pr := mergedReady(t)
			tt.to(f, v, pr)
			if got := pickStage(f, 1); got != "stage:"+tt.stage {
				t.Fatalf("the task is at %s; want %s", got, tt.stage)
			}
			commit := v.mergedMerge(pr, wholeRunOwner)
			writes, ran := len(f.Writes()), v.checksRan()
			v.pass()
			want := []string{"DELETE /git/refs/heads/" + prBranch, "POST /issues/1/comments", "PATCH /issues/1", "POST /check-runs"}
			if got := mergedWrites(f, writes); !slices.Equal(got, want) {
				t.Errorf("the merge's pass wrote %q; want %q", got, want)
			}
			mergedClosed(t, f, pr, tt.stage, "`"+wholeRunOwner+"` merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+commit+"`, so this task is complete.\n\n")
			if got := v.originTip(prBranch); got != "" {
				t.Errorf("the task's branch is at %q; want it deleted", got)
			}
			check := tipLast(t, f)
			if check.HeadSHA != commit || check.Conclusion != "success" || strings.Contains(check.Output.Summary, "carried") || v.checksRan() != ran+1 {
				t.Errorf("the merged tip's check = %+v, after %d runs of the commands; want step 4's own success on %s, after one", check, v.checksRan()-ran, commit)
			}
			if v.runs() != 0 {
				t.Errorf("claude ran %d times; want never", v.runs())
			}
		})
	}
}

// Anyone else's merge closes nothing (SPEC §7.5's Someone else's merge):
// the next pass moves the task pr-open → needs-human, keyed event=g<PR>,
// with a hand-off naming the merger, in code, linking the PR and ending in
// the cc line, carries no check over and deletes no branch; step 4 checks
// the merged tip as any push. The digest's item says why it waits. Then a
// pass makes no request for the PR, and quiet passes make none but 304s.
func TestPRMergedUntrusted(t *testing.T) {
	t.Parallel()
	f, v, _, head, _, pr := mergedReady(t)
	commit := v.mergedMerge(pr, "mallory")
	writes := len(f.Writes())
	v.pass()
	want := []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open", "POST /check-runs"}
	if got := mergedWrites(f, writes); !slices.Equal(got, want) {
		t.Errorf("the merge's pass wrote %q; want %q", got, want)
	}
	move, fields := mergedMove(t, f, pr)
	if want := map[string]string{"from": "pr-open", "to": "needs-human", "event": "g" + strconv.Itoa(pr)}; !maps.Equal(fields, want) {
		t.Errorf("the hand-off's marker has %v; want %v", fields, want)
	}
	link := "https://github.com/o/r/pull/" + strconv.Itoa(pr)
	for _, text := range []string{
		"PR #" + strconv.Itoa(pr) + " for this task was merged into the story branch by `mallory`, who isn't trusted, and only the App's or a trusted user's merge closes the task, so it waits on you.\n\n",
		"**Goal:** Decide what becomes of this task, whose PR someone who isn't trusted merged into the story branch.\n",
		"- " + link + "\n",
		"Closing this issue accepts the merge. Replying `/done` on this issue or reacting 👍 to this comment sends it back to `ready` for a new round.\n\ncc @" + wholeRunOwner,
	} {
		if !strings.Contains(move.Body, text) {
			t.Errorf("the hand-off =\n%s\nwant it to hold\n%s", move.Body, text)
		}
	}
	if pickStage(f, 1) != "stage:needs-human" || f.State().Issues[0].Closed {
		t.Errorf("the task is at %s, closed %t; want it open at needs-human", pickStage(f, 1), f.State().Issues[0].Closed)
	}
	if got := v.originTip(prBranch); got != head {
		t.Errorf("the task's branch is at %q; want it kept at %s", got, head)
	}
	if check := tipLast(t, f); check.HeadSHA != commit || strings.Contains(check.Output.Summary, "carried") {
		t.Errorf("the merged tip's check = %+v; want step 4's own on %s", check, commit)
	}
	from := len(f.Requests())
	v.pass()
	item := "A hand-off waits for you: the task's PR was merged into its story branch by someone who isn't trusted."
	if digest := prMoveDigest(f); !strings.Contains(digest, item) || !strings.Contains(digest, "Close the issue to accept the merge") {
		t.Errorf("the digest =\n%s\nwant it to hold\n%s", digest, item)
	}
	v.quietPass("after the hand-off")
	if got := mergedPRReads(f, from, pr); got != 0 {
		t.Errorf("the passes after the hand-off read PR #%d %d times; want never", pr, got)
	}
}

// A task held by the owner at pr-open whose PR the App merges meanwhile:
// nothing closes while it's held, and step 4 checks the merged tip as any
// push, as a new tip. Its /resume brings it back to pr-open, and the same
// pass closes it as completed and deletes its branch, carrying no check
// over, as the merged tip has its own already.
func TestPRMergedHeld(t *testing.T) {
	t.Parallel()
	f, v, _, _, _, pr := mergedReady(t)
	f.AddComment(1, wholeRunOwner, "/hold")
	v.pass()
	if got := pickStage(f, 1); got != "stage:held" {
		t.Fatalf("the task is at %s; want held", got)
	}
	commit := v.mergedMerge(pr, app)
	v.pass()
	if f.State().Issues[0].Closed || pickStage(f, 1) != "stage:held" || len(personComments(t, f, " event=g")) != 0 {
		t.Fatalf("the task is closed %t, at %s; want it held, with no move on its merge", f.State().Issues[0].Closed, pickStage(f, 1))
	}
	tips := func() int {
		n := 0
		for _, c := range f.State().CheckRuns {
			if c.SHA == commit && c.Name == github.TipCheck {
				n++
			}
		}
		return n
	}
	if tips() != 1 {
		t.Fatalf("the merged tip has %d issuedog/tip checks; want step 4's one", tips())
	}
	f.AddComment(1, wholeRunOwner, "/resume")
	v.pass()
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+commit+"`, so this task is complete.\n\n")
	if got := v.originTip(prBranch); got != "" || tips() != 1 {
		t.Errorf("the task's branch is at %q, the merged tip has %d checks; want it deleted, and one check", got, tips())
	}
}

// After a restart, the daemon holds no tip's judging (Daemon.tips), so the
// App's merge carries no check over, though its parent is the verified
// tip: the task's branch is deleted and the task closed, and step 4 checks
// the merged tip afresh, running the commands once.
func TestPRMergedRestart(t *testing.T) {
	t.Parallel()
	f, v, _, _, _, pr := mergedReady(t)
	commit := v.mergedMerge(pr, app)
	v = v.restart()
	writes, ran := len(f.Writes()), v.checksRan()
	v.pass()
	want := []string{"DELETE /git/refs/heads/" + prBranch, "POST /issues/1/comments", "PATCH /issues/1", "POST /check-runs"}
	if got := mergedWrites(f, writes); !slices.Equal(got, want) {
		t.Errorf("the restart's pass wrote %q; want %q", got, want)
	}
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `")
	check := tipLast(t, f)
	if check.HeadSHA != commit || check.Conclusion != "success" || strings.Contains(check.Output.Summary, "carried") || v.checksRan() != ran+1 {
		t.Errorf("the merged tip's check = %+v, after %d runs of the commands; want step 4's own success on %s, after one", check, v.checksRan()-ran, commit)
	}
}

// The carried check is posted even when the App's newest issuedog/tip on
// the merge commit naming the branch is a failure: only one with the same
// conclusion stands for it (tipPost).
func TestPRMergedCarriedOverFailure(t *testing.T) {
	t.Parallel()
	f, v, _, _, _, pr := mergedReady(t)
	commit := v.mergedMerge(pr, app)
	if _, err := f.Client(t).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: commit, Conclusion: "failure",
		Title: "t", Summary: "s", ExternalID: "branch=" + storyVerifyBranch}); err != nil {
		t.Fatal(err)
	}
	v.pass()
	var got []githubtest.CheckRunState
	for _, c := range f.State().CheckRuns {
		if c.SHA == commit {
			got = append(got, c)
		}
	}
	want := []githubtest.CheckRunState{tipCheck(commit, storyVerifyBranch, "failure"), tipCheck(commit, storyVerifyBranch, "success")}
	if !slices.Equal(got, want) {
		t.Errorf("the merge commit's checks = %+v; want %+v", got, want)
	}
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #")
}

// The App's merge onto a tip that isn't the one the head was verified
// against, as when someone pushed to the story branch between: its merge
// commit's parent isn't the move's tip=, so no check is carried over, and
// step 4 checks the merged tip as any push. The task still closes.
func TestPRMergedOtherTip(t *testing.T) {
	t.Parallel()
	f, v, tip, _, _, pr := mergedReady(t)
	pushed := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	commit := v.mergedMerge(pr, app)
	if pushed == tip || gitIn(t, v.o.url, "rev-parse", commit+"^") != pushed {
		t.Fatalf("the merge is on %s; want it on the pushed tip %s", gitIn(t, v.o.url, "rev-parse", commit+"^"), pushed)
	}
	ran := v.checksRan()
	v.pass()
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #")
	check := tipLast(t, f)
	if check.HeadSHA != commit || strings.Contains(check.Output.Summary, "carried") || v.checksRan() != ran+1 {
		t.Errorf("the merged tip's check = %+v, after %d runs of the commands; want step 4's own on %s, after one", check, v.checksRan()-ran, commit)
	}
}

// The App's merge at a head that isn't the one the task's move verified,
// as a commit pushed to the task's branch before the merge makes it: the
// merge commit's tree isn't the verified head's, so no check is carried
// over, and step 4 checks the merged tip as any push. The branch, at the
// merged head, is deleted, and the task closes.
func TestPRMergedOtherHead(t *testing.T) {
	t.Parallel()
	f, v, _, head, _, pr := mergedReady(t)
	gitIn(t, v.o.work, "checkout", "--quiet", "-B", "later", head)
	gitIn(t, v.o.work, "commit", "--quiet", "--allow-empty", "-m", "before the merge")
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "later:refs/heads/"+prBranch)
	gitIn(t, v.o.work, "checkout", "--quiet", "main")
	commit := v.mergedMerge(pr, app)
	ran := v.checksRan()
	v.pass()
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #")
	if got := v.originTip(prBranch); got != "" {
		t.Errorf("the task's branch is at %q; want it deleted", got)
	}
	check := tipLast(t, f)
	if check.HeadSHA != commit || strings.Contains(check.Output.Summary, "carried") || v.checksRan() != ran+1 {
		t.Errorf("the merged tip's check = %+v, after %d runs of the commands; want step 4's own on %s, after one", check, v.checksRan()-ran, commit)
	}
}

// A commit pushed to the task's branch after the App's merge: the branch
// isn't the merged head, so it's kept, and that's warned of once, though
// the pass's close fails once and the next pass judges the merge again.
// The check is carried over once, and the task closes. It reads logr, so
// it isn't parallel.
func TestPRMergedBranchMoved(t *testing.T) {
	f, v, _, head, _, pr := mergedReady(t)
	commit := v.mergedMerge(pr, app)
	gitIn(t, v.o.work, "checkout", "--quiet", "-B", "later", head)
	gitIn(t, v.o.work, "commit", "--quiet", "--allow-empty", "-m", "after the merge")
	later := gitIn(t, v.o.work, "rev-parse", "HEAD")
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "later:refs/heads/"+prBranch)
	gitIn(t, v.o.work, "checkout", "--quiet", "main")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	stop := logtest.Capture(t, logr.W)
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose close GitHub refused gave no error")
	}
	v.pass()
	logs := stop()
	warning := "o/r#1: its branch " + prBranch + " is at " + later + ", not its PR's merged head " + head + ", so it's kept"
	if got := strings.Count(logs, warning); got != 1 {
		t.Errorf("the kept branch was warned of %d times; want once, in\n%s", got, logs)
	}
	if got := v.originTip(prBranch); got != later {
		t.Errorf("the task's branch is at %q; want it kept at %s", got, later)
	}
	if got := slices.Collect(func(yield func(githubtest.CheckRunState) bool) {
		for _, c := range f.State().CheckRuns {
			if c.SHA == commit && !yield(c) {
				return
			}
		}
	}); !slices.Equal(got, []githubtest.CheckRunState{tipCheck(commit, storyVerifyBranch, "success")}) {
		t.Errorf("the merge commit's checks = %+v; want the carried one alone", got)
	}
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #")
}

// mergedReplaySetup is a fake for Replay on base's origin: task #1 in story
// #2 at pr-open, its PR from prBranch into the story branch open at head,
// verified against tip, which has the App's issuedog/tip as success. The
// origin is shared, so it puts the story branch back at tip and the task's
// branch at head first.
func mergedReplaySetup(t *testing.T, base *verdictRig, tip, head string) func(testing.TB) *githubtest.Fake {
	return func(tb testing.TB) *githubtest.Fake {
		gitIn(t, base.o.url, "update-ref", "refs/heads/"+storyVerifyBranch, tip)
		gitIn(t, base.o.url, "update-ref", "refs/heads/"+prBranch, head)
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		storyVerifyTask(tb, f, head, "planned")
		f.AddComment(1, app, prPassComment(tb, "rv", head, "Fine."))
		ctx, c := context.Background(), f.Client(tb)
		if _, err := c.PostCheck(ctx, "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + storyVerifyBranch}); err != nil {
			tb.Fatal(err)
		}
		pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: storyPRTitle, Head: prBranch, Base: storyVerifyBranch, Body: "Task #1, in story #2"})
		if err != nil {
			tb.Fatal(err)
		}
		f.AddComment(1, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr="+strconv.Itoa(pr.Number)+" tip="+tip+" -->")
		f.RemoveLabel(1, app, "stage:verifying")
		f.AddLabel(1, app, "stage:pr-open")
		f.ForgetWrites()
		return f
	}
}

// The merge's writes, a crash at each, then a fresh daemon (SPEC §5.2):
// the App's merge, whose clean run carries the check over, deletes the
// branch and closes the task; a trusted merge, which deletes and closes;
// and an untrusted one, which hands off. Each run is a fresh daemon on the
// fake's own bare clone: its first pass judges the story's tip, then the
// PR merges, if it's still open, then two passes judge the merge and read
// what they wrote. A restart after a crash holds no tip's judging, so it
// carries nothing over, and step 4 checks the merged tip with the same
// conclusion instead.
func TestPRMergedReplay(t *testing.T) {
	t.Parallel()
	for _, by := range []string{app, wholeRunOwner, "mallory"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			_, base := newStoryVerifyRig(t)
			_, tip := base.storyCut()
			head := base.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
			gitIn(t, base.o.work, "push", "--quiet", base.o.url, head+":refs/heads/"+prBranch)
			roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
			setup := mergedReplaySetup(t, base, tip, head)
			var clean *githubtest.Fake
			githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
				f := setup(tb)
				roots[f] = t.TempDir()
				if clean == nil {
					clean = f
				}
				return f
			}, func(f *githubtest.Fake) error {
				g := newRig(t, f, base.o, roots[f])
				if clones[f] == nil {
					clones[f] = g.d.Clone
				}
				g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
				v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				// The digest links the hand-off by its comment's ID, which a
				// replay is free to change.
				v.d.Digest = false
				if err := v.d.Iterate(context.Background()); err != nil {
					return err
				}
				if pr := f.State().PRs[0]; pr.State == "open" {
					f.MergePR(pr.Number, by)
				}
				for range 2 {
					if err := v.d.Iterate(context.Background()); err != nil {
						return err
					}
				}
				return nil
			})
			// The clean run's merge writes, in order, before the task's
			// labels and step 4's check on the merged tip, if any.
			want := map[string][]string{
				app:           {"POST /check-runs", "DELETE /git/refs/heads/" + prBranch, "POST /issues/1/comments", "PATCH /issues/1"},
				wholeRunOwner: {"DELETE /git/refs/heads/" + prBranch, "POST /issues/1/comments", "PATCH /issues/1", "POST /check-runs"},
				"mallory":     {"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open", "POST /check-runs"},
			}[by]
			if got := mergedWrites(clean, 0); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
				t.Errorf("the clean run's writes = %q; want them to start %q", got, want)
			}
			for f := range roots {
				st, closed := f.State(), by != "mallory"
				if pr := st.PRs[0]; !pr.Merged || pr.MergedBy != by || st.Issues[0].Closed != closed {
					t.Fatalf("after the replay, the PR = %+v, the task closed %t; want it merged by %s, and the task closed %t", pr, st.Issues[0].Closed, by, closed)
				}
				kept := slices.ContainsFunc(st.Branches, func(b githubtest.BranchState) bool { return b.Name == prBranch })
				if kept == closed {
					t.Errorf("after the replay, the branches = %+v; want the task's deleted %t", st.Branches, closed)
				}
			}
		})
	}
}

// mergedDroppedBranch is story #1's branch in dropped_pr_test.go's rig,
// which T2's PR targets here, as a task in a story's does.
const mergedDroppedBranch = "issuedog/1-story"

// mergedDroppedSettled is droppedPRSettled, but T2's PR #6, from
// droppedPRBranch on o, is into story #1's branch, mergedDroppedBranch,
// naming its task with no closing keyword (SPEC §11.5): two passes hold
// T2's child for it, as a revision dropped it, with no write logged since.
func mergedDroppedSettled(t testing.TB, o *origin) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	changedStoryWith(t, f, false, func(f *githubtest.Fake, r changedIDs) {
		f.SetGitRemote(o.url)
		pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
			github.NewPullRequest{Title: droppedPRTitle, Head: droppedPRBranch, Base: mergedDroppedBranch, Body: fmt.Sprintf("Task #%d, in story #1\n\nWhat changed.", r.t2)})
		if err != nil {
			t.Fatal(err)
		}
		if pr.Number != 6 {
			t.Fatalf("T2's PR is #%d, want #6", pr.Number)
		}
		head := gitIn(t, o.url, "rev-parse", "refs/heads/"+droppedPRBranch)
		f.AddComment(r.t2, app, fmt.Sprintf("Verified.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head=%s pr=6 -->", head))
		f.RemoveLabel(r.t2, app, "stage:in-progress")
		f.AddLabel(r.t2, app, "stage:pr-open")
		f.SetBlockedBy(r.other, r.t2)
	}, changedPlanData(t, droppedRunRevision(t)))
	d := droppedPRDaemon(t, f, o, false, false)
	for range 2 {
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if pickStage(f, 3) != "stage:held" {
		t.Fatalf("T2's child is at %s; want held for its PR", pickStage(f, 3))
	}
	f.ForgetWrites()
	return f
}

// A child held for its PR, as a revision dropped it, whose PR merges into
// its story branch (SPEC §6.4): the next pass reads the PR once and closes
// it, keyed apart from its hold. For the App's merge, or the owner's, a
// trusted user's, it's as completed, naming the merge commit, its branch
// deleted first, and its link to the bug kept. For mallory's, who isn't
// trusted, it's as not planned, as its plan dropped it: its branch is
// kept, and its links removed first. Each crash, then a fresh daemon,
// writes nothing twice.
func TestPRMergedDropped(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		by, reason, text string
	}{
		{app, "completed", "The App merged PR #6 into the story branch as `%s`, so this task is complete. A plan's revision dropped it, but its PR merged first."},
		{"owner", "completed", "`owner` merged PR #6 into the story branch as `%s`, so this task is complete. A plan's revision dropped it, but its PR merged first."},
		{"mallory", "not_planned", "Closed as not planned: its PR #6 was merged into the story branch by `mallory`, who isn't trusted, and a plan's revision dropped it."},
	} {
		t.Run(tt.by, func(t *testing.T) {
			t.Parallel()
			o := droppedPROrigin(t)
			gitIn(t, o.work, "push", "--quiet", o.url, "main:refs/heads/"+mergedDroppedBranch)
			cut := gitIn(t, o.url, "rev-parse", "main")
			head := gitIn(t, o.url, "rev-parse", "refs/heads/"+droppedPRBranch)
			setup := func(tb testing.TB) *githubtest.Fake {
				gitIn(t, o.url, "update-ref", "refs/heads/"+mergedDroppedBranch, cut)
				gitIn(t, o.url, "update-ref", "refs/heads/"+droppedPRBranch, head)
				f := mergedDroppedSettled(tb, o)
				f.MergePR(6, tt.by)
				f.ForgetWrites()
				return f
			}
			f := setup(t)
			commit := gitIn(t, o.url, "rev-parse", "refs/heads/"+mergedDroppedBranch)
			p, _ := changedApproval(t, f)
			from := len(f.Requests())
			iterate(t, droppedPRDaemon(t, f, o, false, false))
			if got := mergedPRReads(f, from, 6); got != 1 {
				t.Errorf("the pass read PR #6 %d times; want once", got)
			}
			if t2 := f.State().Issues[2]; !t2.Closed || droppedReason(t, f, 3) != tt.reason {
				t.Errorf("T2's child is closed %t, as %q; want it closed as %s", t2.Closed, droppedReason(t, f, 3), tt.reason)
			}
			text := tt.text
			if tt.reason == "completed" {
				text = fmt.Sprintf(tt.text, commit)
			}
			want := fmt.Sprintf("%s\n\n<!-- issuedog:transition from=held to=closed reason=%s event=p%d-closed -->", text, tt.reason, p)
			if got := droppedPRNewest(f, 3); got != want {
				t.Errorf("T2's child's newest comment is\n%s\nwant\n%s", got, want)
			}
			kept := gitIn(t, o.url, "for-each-ref", "--format=%(objectname)", "refs/heads/"+droppedPRBranch)
			links := f.State().Issues[4].BlockedBy
			if completed := tt.reason == "completed"; (kept == "") != completed || (len(links) == 1) != completed {
				t.Errorf("T2's branch is at %q, the bug blocked by %v; want the branch deleted and the link kept %t", kept, links, completed)
			}
			githubtest.Replay(t, setup, droppedPRPass(t, o, false))
		})
	}
}

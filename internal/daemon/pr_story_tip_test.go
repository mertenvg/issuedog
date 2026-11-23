package daemon_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A task in a story at pr-open whose story branch's tip moved since its
// verify, as by a sibling's merge or a push, is checked on its merge with
// the new tip, once that tip's issuedog/tip is success, and its
// issuedog/verify check says the result, naming the tip (SPEC §7.2, §7.5's
// The merge's guards, §8.1 step 4, §10.4), on a newStoryVerifyRig, with
// real temporary repos: task #1 is in story #2, and storyMergeReady leaves
// it at pr-open with its PR into the story branch open at its verified
// head, verified against the branch's tip. A pass merges the PR onto the
// new tip; a failure sends the task back to in-progress, keyed by that
// tip, and its next run names that tip as its target, to merge first.

// storyTipVerify is the App's issuedog/verify on head, with conclusion,
// naming tip, the story branch's tip its merge was checked with, in its
// external_id (SPEC §5, §10.4), as State has it.
func storyTipVerify(head, tip, conclusion string) githubtest.CheckRunState {
	return githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: conclusion, AppID: githubtest.AppID, ExternalID: "tip=" + tip}
}

// storyTipPassed runs the pass that checks head, task n's PR's verified
// head, on its merge with tip, its story branch's tip that moved since the
// verify, and fails unless it ran the commands once, built the merge under
// refs/issuedog/merges/<n>, with tip and the head as its parents, and
// posted issuedog/verify as success on the head, naming tip, titled for
// the merge, its summary naming the head, the task, the branch and the
// tip, and linking the task, and left the task at pr-open with its PR, pr,
// unmerged.
func (v *verdictRig) storyTipPassed(n, pr int, head, tip string) {
	v.t.Helper()
	ran, writes := v.checksRan(), len(v.f.Writes())
	v.pass()
	if got := v.f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, tip, "success")) || v.checksRan() != ran+1 {
		v.t.Fatalf("the pass on the merge with the moved tip left the checks %+v, and ran the commands %d times; want a success on %s naming %s, and once",
			got, v.checksRan()-ran, head, tip)
	}
	if got := storyMerges(v.f, writes, pr); len(got) != 0 || pickStage(v.f, n) != "stage:pr-open" {
		v.t.Errorf("the pass on the merge with the moved tip merged %+v, leaving #%d at %s; want none, and pr-open", got, n, pickStage(v.f, n))
	}
	var last prMoveCheck
	for _, c := range prMoveChecks(v.t, v.f) {
		if c.HeadSHA == head {
			last = c
		}
	}
	summary := "issuedog checked `" + head + "` for #" + strconv.Itoa(n) + " on its merge with the story branch `" + storyVerifyBranch + "` at `" + tip +
		"`: the checks passed.\n\n| Check | Result |\n"
	if last.Output.Title != "issuedog verified this head's merge with its story branch's tip" || !strings.HasPrefix(last.Output.Summary, summary) ||
		last.DetailsURL != "https://github.com/o/r/issues/"+strconv.Itoa(n) {
		v.t.Errorf("the check = %+v; want it titled for the merge with the tip, with a summary starting\n%s\nand a link to the task", last, summary)
	}
	if got, want := gitIn(v.t, v.d.Clone.Dir(), "rev-list", "--parents", "-n", "1", repo.MergesRef+strconv.Itoa(n)), " "+tip+" "+head; !strings.HasSuffix(got, want) {
		v.t.Errorf("the merge result is %q; want a commit whose parents are%s", got, want)
	}
}

// storyTipMergedOnto runs the pass that merges PR pr, at head, onto tip,
// its story branch's tip that moved since the verify, and fails unless it
// merged it once, at head, and the story branch is at a squash commit
// whose only parent is tip. It returns the squash commit.
func (v *verdictRig) storyTipMergedOnto(pr int, head, tip string) string {
	v.t.Helper()
	writes := len(v.f.Writes())
	v.pass()
	merges := storyMerges(v.f, writes, pr)
	if len(merges) != 1 {
		v.t.Fatalf("the merge's pass merged %+v; want PR #%d once", merges, pr)
	}
	var sent storyMergeBody
	if err := json.Unmarshal([]byte(merges[0].Body), &sent); err != nil {
		v.t.Fatal(err)
	}
	if sent.SHA != head || sent.MergeMethod != "squash" {
		v.t.Errorf("the merge sent %+v; want a squash at %s", sent, head)
	}
	squash := v.originTip(storyVerifyBranch)
	if got := gitIn(v.t, v.o.url, "rev-list", "--parents", "-n", "1", squash); got != squash+" "+tip {
		v.t.Errorf("the story branch's tip and its parents are %q; want %s with the one parent %s", got, squash, tip)
	}
	return squash
}

// storyTipCarried runs the pass after the App's merge of task n's PR, pr,
// at head, onto onto, a tip that moved since its verify, as squash, and
// fails unless it carried the tip's check over to squash as success, its
// summary naming the merge onto the tip the head's merge passed on, with
// the commands run no more, and closed the task, keyed event=g<pr> (SPEC
// §7.5's Carrying the check over).
func (v *verdictRig) storyTipCarried(n, pr int, squash, onto, head string) {
	v.t.Helper()
	ran := v.checksRan()
	v.pass()
	if got := v.f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran {
		v.t.Errorf("the pass after the merge left the checks %+v, and ran the commands %d times; want the carried success on %s, and never",
			got, v.checksRan()-ran, squash)
	}
	summary := "issuedog carried this check over to `" + squash + "`, the tip of `" + storyVerifyBranch + "` for story #2: it's the App's squash merge of PR #" +
		strconv.Itoa(pr) + ", task #" + strconv.Itoa(n) + "'s head `" + head + "`, onto `" + onto + "`, a tip the head's merge with passed the checks on, " +
		"whose check passed, so its tree is that checked merge's."
	if last := tipLast(v.t, v.f); last.HeadSHA != squash || !strings.HasPrefix(last.Output.Summary, summary) {
		v.t.Errorf("the carried check = %+v; want it on %s, with a summary starting\n%s", last, squash, summary)
	}
	is := v.f.State().Issues[n-1]
	closed := slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool {
		return c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition from=pr-open to=closed reason=completed event=g"+strconv.Itoa(pr)+" ")
	})
	if !is.Closed || !closed {
		v.t.Errorf("#%d is closed %t, with its close's move %t; want both", n, is.Closed, closed)
	}
}

// storyTipMoves are the App's comments on task n that hold a move pr-open
// → in-progress.
func storyTipMoves(f *githubtest.Fake, n int) []string {
	var bodies []string
	for _, c := range f.State().Issues[n-1].Comments {
		if c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition from=pr-open to=in-progress ") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// storyTipSeen is the newest comment on task n, as step 1 reads it, which
// the move back on its story branch's tip carries as seen= (SPEC §5).
func storyTipSeen(t *testing.T, f *githubtest.Fake, n int) string {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	var seen int64
	for _, c := range s.Comments {
		seen = max(seen, c.ID)
	}
	return strconv.FormatInt(seen, 10)
}

// storyTipMovedText is the start of the comment of a move back when head's
// merge with the story branch's tip tip doesn't merge cleanly, with
// quote.txt conflicting, or with findings, fails the checks.
func storyTipMovedText(head, tip string, findings bool) string {
	if findings {
		return "The story branch `" + storyVerifyBranch + "` moved to `" + tip + "`, and this task's PR, verified at `" + head + "`, fails verify once merged with it. " +
			"The findings below are the merge's. The next run merges the story branch `" + storyVerifyBranch + "` first.\n\n" +
			"Verify failed at `" + head + "`, checked against the story branch `" + storyVerifyBranch + "` at `" + tip + "`."
	}
	return "The story branch `" + storyVerifyBranch + "` moved to `" + tip + "`, and this task's PR, verified at `" + head + "`, doesn't merge cleanly with it. " +
		"These files conflict:\n\n- `quote.txt`\n\nSo the task is back at `in-progress`, for the implementer to merge the story branch `" + storyVerifyBranch +
		"` first and resolve the conflicts.\n\n"
}

// storyTipMoved fails unless task n is at stage, after one move pr-open →
// in-progress on its story branch's tip's moving: its comment starts with
// text, and its marker is keyed event=b<tip>, with seen= seen.
func storyTipMoved(t *testing.T, f *githubtest.Fake, n int, stage, text, tip, seen string) {
	t.Helper()
	moves := storyTipMoves(f, n)
	if pickStage(f, n) != stage || len(moves) != 1 || !strings.HasPrefix(moves[0], text) {
		t.Fatalf("#%d at %s, its moves to in-progress %q; want %s, after one move, starting\n%s", n, pickStage(f, n), moves, stage, text)
	}
	want := map[string]string{"from": "pr-open", "to": "in-progress", "event": "b" + tip, "seen": seen}
	if got := prMoveFields(t, moves[0]); !maps.Equal(got, want) {
		t.Errorf("the move's marker has %v; want %v", got, want)
	}
}

// storyTipTarget fails unless claude's run k, the implementer's on task n,
// started at head with tip, its story branch's tip, named as its target,
// which its clone holds as refs/heads/target, and its input.md gives the
// move back's comment, body, under a heading naming the merge with the
// story branch's tip (SPEC §3.4, §12).
func (v *verdictRig) storyTipTarget(k, n int, head, tip, body string) {
	v.t.Helper()
	l := v.leaseOf(strconv.Itoa(k))
	if l.Issue != n || l.Start != head || l.Target != tip || l.StoryTip != tip {
		v.t.Errorf("run %d on #%d started at %s with the target %q and story_tip %q; want #%d, at %s, with the tip %s as both", k, l.Issue, l.Start, l.Target, l.StoryTip, n, head, tip)
	}
	if refs := v.file("refs." + strconv.Itoa(k)); !strings.Contains(refs, tip+" refs/heads/target") {
		v.t.Errorf("run %d's clone has the refs\n%s\nwant refs/heads/target at %s", k, refs, tip)
	}
	text := body[:strings.Index(body, "\n\n<!-- issuedog:transition")]
	got := v.roundInput(k)
	if !strings.Contains(got, "\n## The merge with the story branch's tip\n\n```") || !strings.Contains(got, text) {
		v.t.Errorf("run %d's input.md =\n%s\nwant the move's comment under \"The merge with the story branch's tip\":\n%s", k, got, text)
	}
}

// storyTipReady is storyMergeReady on a rig whose verify.commands are
// newStoryVerifyRig's, then commands, each with a comma before it: it cuts
// story #2's branch, makes task #1's head on its tip, quote.txt, files the
// task at verifying with a clean pass on that head, and runs the passes
// that check the tip, check the head against it and open the PR, so the
// task is at pr-open. It returns the fake, the rig, the tip, the head and
// the PR's number.
func storyTipReady(t *testing.T, commands string) (*githubtest.Fake, *verdictRig, string, string, int) {
	t.Helper()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "`+personBrokenCommand+`"`+commands)
	_, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	f.AddComment(1, app, prPassComment(t, "rv", head, "Fine."))
	v.storyTipJudged(tip)
	v.passes(2) // the head's checks against the tip, then the PR step
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the PRs = %+v, the task at %s; want one, open at %s, and pr-open", prs, pickStage(f, 1), head)
	}
	return f, v, tip, head, prs[0].Number
}

// A sibling's head that doesn't merge cleanly with the story branch's new
// tip, task 1's squash commit, as both write quote.txt: the pass after the
// merge carries the tip's check over and closes task 1, then posts
// issuedog/verify as failure on the sibling's head, naming the new tip and
// the conflicting file, and runs no command, then moves the sibling pr-open
// → in-progress, keyed event=b<tip>, naming the tip, the head and the file.
// Its PR isn't merged. The implementer's next run starts from its head
// with the tip named as its target, its input.md giving the move's comment
// under a heading naming the merge with the story branch's tip.
func TestPRStoryTipConflict(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	head, sibling := storyTipSibling(t, f, v, tip, map[string]string{"quote.txt": "stale\n"})
	v.pass()
	squash := v.storyMerged(pr, tip)
	seen := storyTipSeen(t, f, 5)
	ran := v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) ||
		!slices.Contains(got, storyTipVerify(head, squash, "failure")) || v.checksRan() != ran {
		t.Fatalf("the pass after the merge left the checks %+v, and ran the commands %d times; want the carried success on %s, a failure on %s naming it, and none",
			got, v.checksRan()-ran, squash, head)
	}
	checks := prMoveChecks(t, f)
	last := checks[len(checks)-1]
	summary := "issuedog couldn't merge `" + head + "` for #5 with the story branch `" + storyVerifyBranch + "` at `" + squash + "`: these files conflict.\n\n- `quote.txt`"
	if last.HeadSHA != head || last.Output.Title != "This head doesn't merge cleanly with its story branch's tip" || last.Output.Summary != summary {
		t.Errorf("the check = %+v; want it titled for the conflict, with the summary\n%s", last, summary)
	}
	storyTipMoved(t, f, 5, "stage:in-progress", storyTipMovedText(head, squash, false), squash, seen)
	if got := storyMerges(f, 0, sibling); len(got) != 0 {
		t.Errorf("the sibling's merges = %+v; want none", got)
	}

	v.out(1, ivResult(t, implementerOutput(t, "question")))
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("claude ran %d times; want the implementer once", v.runs())
	}
	v.storyTipTarget(1, 5, head, squash, storyTipMoves(f, 5)[0])
}

// A sibling's head that breaks a command only on its merge with the new
// tip: each head alone passes it, and so does the tip, but once task 1's
// PR merges, the commands run on the sibling's merge with the new tip, one
// fails, and issuedog/verify is failure on its head, naming the tip, with
// the findings. Then the sibling moves back as a conflict does, its
// comment giving the merge's findings, and its next run names the tip as
// its target, though its head merges cleanly with it.
func TestPRStoryTipFailingCommand(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyTipReady(t, `, "test ! -f stale.txt -o ! -f quote.txt"`)
	head, _ := storyMergeSibling(t, f, v, tip)
	v.pass()
	squash := v.storyMerged(pr, tip)
	seen := storyTipSeen(t, f, 5)
	ran := v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, squash, "failure")) || v.checksRan() != ran+1 {
		t.Fatalf("the pass after the merge left the checks %+v, and ran the commands %d times; want a failure on %s naming %s, and once",
			got, v.checksRan()-ran, head, squash)
	}
	checks := prMoveChecks(t, f)
	last := checks[len(checks)-1]
	if last.Output.Title != "This head fails verify on its merge with its story branch's tip" ||
		!strings.HasPrefix(last.Output.Summary, storyTipMovedText(head, squash, true)) ||
		!strings.Contains(last.Output.Summary, "test ! -f stale.txt -o ! -f quote.txt") {
		t.Errorf("the check = %+v; want it titled for the failure, with the merge's findings", last)
	}
	storyTipMoved(t, f, 5, "stage:in-progress", storyTipMovedText(head, squash, true), squash, seen)

	v.out(1, ivResult(t, implementerOutput(t, "question")))
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("claude ran %d times; want the implementer once", v.runs())
	}
	v.storyTipTarget(1, 5, head, squash, storyTipMoves(f, 5)[0])
}

// A new tip whose check is failure isn't the story's tip to check a merge
// with: the task waits at pr-open, uncounted, and the passes that wait
// make no request for the task, its PR or its head, run no command and
// merge nothing (SPEC §7.5).
func TestPRStoryTipWaitsForItsCheck(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	broken := v.tipPush(storyVerifyBranch, map[string]string{"broken.txt": "the story broke it\n"})
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(broken, storyVerifyBranch, "failure")) {
		t.Fatalf("the pass on the broken tip left the checks %+v; want its failure", got)
	}
	v.prMoveQuiet("waiting on the tip's check", true)
	v.prMoveQuiet("again", true)
	for _, c := range f.State().CheckRuns {
		if c.SHA == head && c.ExternalID != "main="+v.mainSHA() {
			t.Errorf("the head has the check %+v; want the PR step's alone", c)
		}
	}
	if got := storyMerges(f, 0, pr); len(got) != 0 || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the task at %s; want none, and pr-open", got, pickStage(f, 1))
	}
}

// After a restart, a task whose story branch's tip is the one its head was
// verified against makes no request for its head's checks and runs no
// command: that tip is its move's tip=, so its merge isn't judged (SPEC
// §8.1's Polling cost). A second branch with the task's prefix keeps it
// from merging, so it stays at pr-open.
func TestPRStoryTipUnchanged(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	gitIn(t, v.o.url, "update-ref", "refs/heads/issuedog/1-a-copy", head)
	v.pass()
	v = v.restart()
	from, ran := len(f.Requests()), v.checksRan()
	v.passes(2)
	if got := prMainReads(f, from, head); got != 0 || v.checksRan() != ran {
		t.Errorf("after the restart, the head's checks were read %d times, and the commands ran %d times; want neither", got, v.checksRan()-ran)
	}
	v.prMoveQuiet("the tip unchanged", true)
	if got := storyMerges(f, 0, pr); len(got) != 0 || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the task at %s; want none, and pr-open", got, pickStage(f, 1))
	}
}

// The story branch's tip moves again during the checks on the head's merge
// with its new tip, here just before the check is posted: the check names
// the tip the merge was built on, and nothing merges onto it, as it's the
// story's tip no more. The next pass checks the newer tip, and the one
// after the head's merge with it, posting a check naming it. Then the PR
// merges onto the newer tip, and its check is carried over.
func TestPRStoryTipMovesDuringTheChecks(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.pass()
	var newer string
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" },
		func() { newer = v.tipPush(storyVerifyBranch, map[string]string{"later.txt": "later in the story\n"}) })
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, moved, "success")) || newer == "" || newer == moved {
		t.Fatalf("the check runs = %+v, the tip moved to %q meanwhile; want a success naming %s, and a newer tip", got, newer, moved)
	}
	ran, writes := v.checksRan(), len(f.Writes())
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(newer, storyVerifyBranch, "success")) || v.checksRan() != ran+1 ||
		len(storyMerges(f, writes, pr)) != 0 {
		t.Fatalf("the pass on the newer tip left the checks %+v, ran the commands %d times, and merged %+v; want its success, once, and nothing",
			got, v.checksRan()-ran, storyMerges(f, writes, pr))
	}
	v.storyTipPassed(1, pr, head, newer)
	squash := v.storyTipMergedOnto(pr, head, newer)
	v.storyTipCarried(1, pr, squash, newer, head)
}

// A person's head in a story that failed verify is back at pr-open with
// no tip=, and its App issuedog/verify is failure, naming the story's
// baseline. Once the story branch's tip moves, the head's checks are read
// once, and it isn't checked on its merge with the new tip: no command
// runs, no check is posted, and it never goes to in-progress. The passes
// after make no request, and neither do a fresh daemon's, once it has read
// the head's checks once.
func TestPRStoryTipPersonFailed(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	person, _ := v.personPush(head, "broken.txt")
	v.pass()
	if _, fields := personBack(t, f, person); fields["tip"] != "" {
		t.Fatalf("the move back has %v; want no tip", fields)
	}
	v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.pass() // the moved tip's check
	checks, ran := f.State().CheckRuns, v.checksRan()
	from := len(f.Requests())
	v.pass()
	if got := prMainReads(f, from, person); got != 1 || v.checksRan() != ran || !slices.Equal(f.State().CheckRuns, checks) {
		t.Errorf("the pass after the tip moved read the head's checks %d times, ran the commands %d times, and left the checks %+v; want once, none, and %+v",
			got, v.checksRan()-ran, f.State().CheckRuns, checks)
	}
	v.prMoveQuiet("a failed person's head on a moved tip", true)
	v = v.restart()
	from = len(f.Requests())
	v.pass()
	if got := prMainReads(f, from, person); got != 1 || v.checksRan() != ran || !slices.Equal(f.State().CheckRuns, checks) {
		t.Errorf("the restart's first pass read the head's checks %d times, ran the commands %d times, and left the checks %+v; want once, none, and %+v",
			got, v.checksRan()-ran, f.State().CheckRuns, checks)
	}
	v.prMoveQuiet("after the restart", true)
	personNever(t, f)
	if got := storyMerges(f, 0, pr); len(got) != 0 || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the task at %s; want none, and pr-open", got, pickStage(f, 1))
	}
}

// A fresh daemon after the pass that checked the head's merge with the
// moved tip reads the head's checks once, finds the success naming the
// tip, and runs nothing; its step 2 holds no judging of the tip on that
// first pass, so the next pass merges the PR onto the tip, and the one
// after carries the tip's check over, from what the restart read.
func TestPRStoryTipRestart(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.pass()
	v.storyTipPassed(1, pr, head, moved)
	checks := f.State().CheckRuns
	v = v.restart()
	from, ran, writes := len(f.Requests()), v.checksRan(), len(f.Writes())
	v.pass()
	if got := prMainReads(f, from, head); got != 1 || v.checksRan() != ran || !slices.Equal(f.State().CheckRuns, checks) || len(storyMerges(f, writes, pr)) != 0 {
		t.Fatalf("the restart's first pass read the head's checks %d times, ran the commands %d times, left the checks %+v, and merged %+v; want once, none, %+v, and nothing",
			got, v.checksRan()-ran, f.State().CheckRuns, storyMerges(f, writes, pr), checks)
	}
	squash := v.storyTipMergedOnto(pr, head, moved)
	v.storyTipCarried(1, pr, squash, moved, head)
}

// A crash just after the conflict's check, and just after the move's
// comment, each a write GitHub made whose answer was lost, then a fresh
// daemon: the replay posts no second check and no second move. After the
// check, the restart reads the moved tip's check and the head's, finds its
// failure naming the tip, and makes the move. After the comment, its step
// 1 finishes the move's labels. Either way the implementer's run then
// names the tip as its target, once its check is read.
func TestPRStoryTipCrash(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		path string // the write whose answer is lost
	}{
		{"after the check", "/check-runs"},
		{"after the move", "/issues/1/comments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, v, _, head, _ := storyMergeReady(t)
			moved := v.tipPush(storyVerifyBranch, map[string]string{"quote.txt": "the story's own quote\n"})
			v.pass() // the moved tip's check
			seen := prMainSeen(t, f)
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == tt.path },
				func() { f.FailWriteAfter(len(f.Writes()) + 1) })
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass whose answer was lost gave no error")
			}
			f.FailWriteAfter(0)
			want := storyTipVerify(head, moved, "failure")
			if got := f.State().CheckRuns; !slices.Contains(got, want) || pickStage(f, 1) != "stage:pr-open" {
				t.Fatalf("after the lost answer, the check runs = %+v, the task at %s; want %+v among them, and still at pr-open", got, pickStage(f, 1), want)
			}
			checks := f.State().CheckRuns

			v = v.restart()
			v.out(1, ivResult(t, implementerOutput(t, "question")))
			v.passes(2)
			if got := f.State().CheckRuns; !slices.Equal(got, checks) {
				t.Errorf("after the restart, the check runs = %+v; want %+v", got, checks)
			}
			storyTipMoved(t, f, 1, "stage:clarifying", storyTipMovedText(head, moved, false), moved, seen)
			if v.runs() != 1 {
				t.Fatalf("after the restart, claude ran %d times; want the implementer once", v.runs())
			}
			v.storyTipTarget(1, 1, head, moved, storyTipMoves(f, 1)[0])
		})
	}
}

// A conflict with the moved tip while another task holds the worker, at
// verifying under a closed story, so it gets no checks: the check is
// posted, but the move waits, with no write, and the pass that waits runs
// nothing and reads nothing for the task's head. The tip moving again
// drops it, and once the newer tip is checked, the head is checked on its
// merge with it, posting a check naming it, and waits again. Then the
// other task closes, freeing the worker, and the next pass makes the move
// keyed by the newer tip, once.
func TestPRStoryTipWaitsForTheWorker(t *testing.T) {
	t.Parallel()
	f, v, _, head, _ := storyMergeReady(t)
	seen := prMainSeen(t, f)
	story := f.AddIssue(wholeRunOwner, "A closed story", "Its plan.", "type:story")
	f.Close(story)
	other := f.AddIssue(wholeRunOwner, "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.SetParent(other, story)
	f.AddLabel(other, wholeRunOwner, "stage:verifying")
	moved := v.tipPush(storyVerifyBranch, map[string]string{"quote.txt": "the story's own quote\n"})
	v.passes(2) // the moved tip's check, then the head's merge with it
	if got := f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, moved, "failure")) || pickStage(f, 1) != "stage:pr-open" || len(storyTipMoves(f, 1)) != 0 {
		t.Fatalf("the check runs = %+v, the task at %s, its moves %q; want a failure naming %s, still at pr-open, and none",
			got, pickStage(f, 1), storyTipMoves(f, 1), moved)
	}
	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	if got := prMainReads(f, from, head); got != 0 || v.checksRan() != ran || len(storyTipMoves(f, 1)) != 0 {
		t.Errorf("the pass that waits read the head's checks %d times, ran the commands %d times, with the moves %q; want none of each",
			got, v.checksRan()-ran, storyTipMoves(f, 1))
	}

	newer := v.tipPush(storyVerifyBranch, map[string]string{"quote.txt": "the story's later quote\n"})
	v.passes(2) // the newer tip's check, then the head's merge with it
	if got := f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, newer, "failure")) || len(storyTipMoves(f, 1)) != 0 {
		t.Fatalf("after the tip moved again, the check runs = %+v, the moves %q; want a failure naming %s, and none", got, storyTipMoves(f, 1), newer)
	}

	f.CloseBy(other, wholeRunOwner)
	v.pass()
	storyTipMoved(t, f, 1, "stage:in-progress", storyTipMovedText(head, newer, false), newer, seen)
}

// The App's merge onto the moved tip whose squash commit hasn't the tree
// of the head's merge with that tip that the checks passed on, as when
// GitHub's merge differs from git's, here shown by that merge result
// replaced in the bare repo: no check is carried over, and step 4 checks
// the squash commit as any push. The task still closes.
func TestPRStoryTipCarryOtherTree(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.pass()
	v.storyTipPassed(1, pr, head, moved)
	squash := v.storyTipMergedOnto(pr, head, moved)
	gitIn(t, v.d.Clone.Dir(), "update-ref", repo.MergesRef+"1", head)
	ran := v.checksRan()
	v.pass()
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+squash+"`, so this task is complete.\n\n")
	check := tipLast(t, f)
	if check.HeadSHA != squash || strings.Contains(check.Output.Summary, "carried") || v.checksRan() != ran+1 {
		t.Errorf("the merged tip's check = %+v, after %d runs of the commands; want step 4's own on %s, after one", check, v.checksRan()-ran, squash)
	}
}

// A merge onto the moved tip whose checks failed, as the App's made by
// hand after the task went back, carries no check over, though the tip is
// success and the merge result in the bare repo has the squash commit's
// tree: only a merge whose checks passed does (prMergeOnto). The task
// closes, and step 4 checks the squash commit as any push, which fails it.
func TestPRStoryTipCarryFailedMerge(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyTipReady(t, `, "test ! -f stale.txt -o ! -f quote.txt"`)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"stale.txt": "stale\n"})
	v.passes(2) // the moved tip's check, then the head's merge with it
	if got := f.State().CheckRuns; !slices.Contains(got, storyTipVerify(head, moved, "failure")) || pickStage(f, 1) != "stage:in-progress" {
		t.Fatalf("the check runs = %+v, the task at %s; want a failure naming %s, and in-progress", got, pickStage(f, 1), moved)
	}
	squash := v.mergedMerge(pr, app)
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.MergesRef+"1^{tree}"); got != gitIn(t, v.o.url, "rev-parse", squash+"^{tree}") {
		t.Fatalf("the merge result's tree is %s; want the squash commit's", got)
	}
	ran := v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "failure")) || slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) ||
		v.checksRan() != ran+1 || !f.State().Issues[0].Closed {
		t.Errorf("the pass after the merge left the checks %+v, ran the commands %d times, with the task closed %t; want step 4's failure on %s alone, once, and closed",
			got, v.checksRan()-ran, f.State().Issues[0].Closed, squash)
	}
}

// main moving after the story's cut, with a protected path, doesn't move a
// task in a story's target: its head isn't checked on main's moving. Once
// the story branch's tip moves, the head's merge with it is checked with
// the story's baseline, the merge base of that tip and main, as rule 2's,
// not main as it is now, so it passes, as main's protected path is no
// change of the task's or the story's.
func TestPRStoryTipBaseline(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	gitIn(t, v.o.url, "update-ref", "refs/heads/issuedog/1-a-copy", head) // so the PR waits, unmerged, on the old tip
	v.pass()
	main := v.prMainCommit("secrets/key.txt", "a key\n")
	from, ran := len(f.Requests()), v.checksRan()
	v.passes(2)
	if got := prMainReads(f, from, head); got != 0 || v.checksRan() != ran {
		t.Errorf("after main moved to %s, the head's checks were read %d times, and the commands ran %d times; want neither", main, got, v.checksRan()-ran)
	}
	gitIn(t, v.o.url, "update-ref", "-d", "refs/heads/issuedog/1-a-copy")
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.pass()
	v.storyTipPassed(1, pr, head, moved)
}

// Replay passes for the check and the move, whatever write a crash comes
// at (SPEC §5.2), on a story branch's tip that moved since the verify: on a
// conflict, whose writes are the check and the move, and on a merge that
// passes, whose write is the check. Each pass is a fresh daemon's, with its
// own bare clone, and the digest and assignment off, so its writes are the
// merge's alone: its first iteration reads the moved tip's check, then
// judges the head's merge with it. The origin is shared, so each setup
// puts the task's branch and the story branch back.
func TestPRStoryTipReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		path   string // the file the moved tip writes
		writes []string
	}{
		{"conflict", "quote.txt", []string{"POST /check-runs", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open"}},
		{"pass", "more.txt", []string{"POST /check-runs"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, base := newStoryVerifyRig(t)
			_, tip := base.storyCut()
			head := base.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
			gitIn(t, base.o.work, "push", "--quiet", base.o.url, head+":refs/heads/"+prBranch)
			moved := base.tipPush(storyVerifyBranch, map[string]string{tt.path: "the story's own change\n"})
			roots := map[*githubtest.Fake]string{}
			setup := func(tb testing.TB) *githubtest.Fake {
				gitIn(tb, base.o.url, "update-ref", "refs/heads/"+prBranch, head)
				gitIn(tb, base.o.url, "update-ref", "refs/heads/"+storyVerifyBranch, moved)
				f := newFake(tb)
				f.SetPermission(wholeRunOwner, "admin")
				f.SetGitRemote(base.o.url)
				task := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
				if story := tipStoryAt(f, tipStoryTitle, "planned", "type:story"); task != 1 || story != 2 {
					tb.Fatalf("the task is #%d, the story #%d; want #1 and #2", task, story)
				}
				f.SetParent(1, 2)
				c := f.Client(tb)
				pr, err := c.CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: storyPRTitle, Head: prBranch, Base: storyVerifyBranch,
					Body: "Task #1, in story #2\n\nThe change."})
				if err != nil {
					tb.Fatal(err)
				}
				f.AddLabel(pr.Number, app, "priority:p1")
				f.AddLabel(1, app, "stage:pr-open")
				f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->")
				f.AddComment(1, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr="+strconv.Itoa(pr.Number)+" tip="+tip+" -->")
				if _, err := c.PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: moved, Conclusion: "success",
					Title: "t", Summary: "s", ExternalID: "branch=" + storyVerifyBranch}); err != nil {
					tb.Fatal(err)
				}
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
			if got := writesOf(clean); !slices.Equal(got, tt.writes) {
				t.Errorf("the clean pass wrote %q; want %q", got, tt.writes)
			}
			conclusion := "success"
			if len(tt.writes) > 1 {
				conclusion = "failure"
			}
			if got, want := clean.State().CheckRuns, storyTipVerify(head, moved, conclusion); !slices.Contains(got, want) {
				t.Errorf("the clean pass left the check runs %+v; want %+v among them", got, want)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}

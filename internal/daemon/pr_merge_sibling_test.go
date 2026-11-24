package daemon_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The App's merge and what else names its story branch's tip in the same
// pass (SPEC §7.5's The merge's guards): once the App merges task 1's PR
// onto the tip, the branch is past it, though the bare clone names it till
// the next pass's sync. So nothing names that tip as the story's tip for
// the rest of the pass: a sibling at pr-open verified against it isn't
// merged onto the merge's squash commit, and a run in the story doesn't
// start from it.

// storySiblingBranch is task 5's branch, from its title.
const storySiblingBranch = "issuedog/5-refuse-a-stale-quote"

// storyMergeSiblingTask files task 5 in story #2, after storyMergeReady's
// PR step, with priority:p1, scope:booking and stage, as the App moved it
// there, and returns its number.
func storyMergeSiblingTask(t *testing.T, f *githubtest.Fake, stage string) int {
	t.Helper()
	n := f.AddIssue("owner", "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	if n != 5 {
		t.Fatalf("the sibling is #%d, want #5", n)
	}
	f.SetParent(n, 2)
	f.AddLabel(n, app, "stage:"+stage)
	return n
}

// storyMergeSibling files task 5 in story #2 at pr-open, as its PR step
// would leave it: its head, a commit of its own on tip, stale.txt, on its
// branch in v's origin, its PR from that branch into the story branch
// open, the App's, and its move to pr-open naming the head, the PR and
// tip. It returns the head and the PR's number.
func storyMergeSibling(t *testing.T, f *githubtest.Fake, v *verdictRig, tip string) (string, int) {
	t.Helper()
	return storyTipSibling(t, f, v, tip, map[string]string{"stale.txt": "stale\n"})
}

// storyTipSibling is storyMergeSibling with the sibling's head committing
// files on tip, each path to its content, and that head made task 5's in
// v's clone too, as its run's handoff left it (SPEC §3.2), so a run of its
// starts from it.
func storyTipSibling(t *testing.T, f *githubtest.Fake, v *verdictRig, tip string, files map[string]string) (string, int) {
	t.Helper()
	n := storyMergeSiblingTask(t, f, "pr-open")
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "sibling", tip)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := os.WriteFile(filepath.Join(w, path), []byte(files[path]), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, w, "add", path)
	}
	gitIn(t, w, "commit", "--quiet", "-m", "the sibling's run")
	head := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", v.o.url, "sibling:refs/heads/"+storySiblingBranch)
	gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", w, "+refs/heads/sibling:"+repo.TasksRef+strconv.Itoa(n))
	gitIn(t, w, "checkout", "--quiet", "main")
	pr := f.OpenPRAs(app, storySiblingBranch, storyVerifyBranch, "feat: Refuse a stale quote", "Task #5, in story #2\n\nThe change.")
	f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r5 head="+head+" -->")
	f.AddComment(n, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=r5v head="+head+" pr="+strconv.Itoa(pr)+" tip="+tip+" -->")
	return head, pr
}

// Two tasks of one story at pr-open, both verified against the same tip:
// the pass that merges the first, the lower-numbered, merges only it, as
// the second would go onto the first's squash commit, a tip it wasn't
// verified against, and runs no command on the second's merge with it,
// as that tip isn't the story's tip till the next sync. The pass after
// carries the tip's check over to the squash commit and closes the first
// task, as its merge is onto the tip it was verified against (SPEC §7.5's
// Carrying the check over), and then, in step 4, checks the second's head
// on its merge with that new tip, once, posting issuedog/verify as success
// on the head, naming the tip (SPEC §10.4). The pass after merges the
// second's PR onto it, and the one after carries no check over to that
// merge's squash commit: it closes the second task, and step 4 checks the
// squash commit afresh, running the commands once.
// Once the closes are read, the passes are quiet, and the sibling's PR is
// read no more.
func TestPRMergeSiblings(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	head, sibling := storyMergeSibling(t, f, v, tip)
	writes, ran := len(f.Writes()), v.checksRan()
	v.pass()
	if first, second := storyMerges(f, writes, pr), storyMerges(f, writes, sibling); len(first) != 1 || len(second) != 0 || v.checksRan() != ran {
		t.Fatalf("the pass merged %+v and %+v, and ran the commands %d times; want the first PR, #%d, alone, and never", first, second, v.checksRan()-ran, pr)
	}
	squash := v.storyMerged(pr, tip)
	v.storyTipPassed(5, sibling, head, squash)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) {
		t.Errorf("the pass after the merge left the checks %+v; want the carried success on %s", got, squash)
	}
	mergedClosed(t, f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+squash+"`, so this task is complete.\n\n")
	next := v.storyTipMergedOnto(sibling, head, squash)
	v.storyTipMergedChecked(5, sibling, next, "success")
	v.passes(2)
	from := len(f.Requests())
	v.quietPass("after both merges")
	v.quietPass("again")
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/pulls/"+strconv.Itoa(sibling)) {
			t.Errorf("a pass after the merges sent %+v; want nothing for the sibling's PR", r)
		}
	}
	if got := storyMerges(f, 0, sibling); len(got) != 1 || pickStage(f, 5) != "stage:completed" || v.originTip(storyVerifyBranch) != next {
		t.Errorf("the sibling's merges = %+v, the sibling at %s, the story branch at %s; want one, completed, and %s",
			got, pickStage(f, 5), v.originTip(storyVerifyBranch), next)
	}
}

// A task in the story at in-progress, whose run would start in step 4 of
// the pass that merges task 1's PR, doesn't start from the tip the merge
// went onto. The pass after carries the tip's check over to the merge's
// squash commit, so its step 4 starts the run from that new tip, which
// the lease records as story_tip (SPEC §3.4), with no command run on it.
func TestPRMergeRunStartAfter(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	n := storyMergeSiblingTask(t, f, "in-progress")
	f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress run=rpick -->")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	squash := v.storyMerged(pr, tip)
	if v.runs() != 0 {
		t.Fatalf("the merge's pass ran claude %d times; want no run from the old tip %s", v.runs(), tip)
	}
	ran := v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran {
		t.Fatalf("the pass after the merge left the checks %+v, and ran the commands %d times; want the carried success on %s, and never",
			got, v.checksRan()-ran, squash)
	}
	if v.runs() != 1 {
		t.Fatalf("the task at %s, after %d runs; want one", pickStage(f, n), v.runs())
	}
	if l := v.leaseOf("1"); l.Issue != n || l.StoryTip != squash || l.Start != squash {
		t.Errorf("the run leased #%d, with story_tip %s and start %s; want #%d, from the new tip %s", l.Issue, l.StoryTip, l.Start, n, squash)
	}
}

// A ready task in the story, which the pick would take in step 4 of the
// pass that merges task 1's PR, isn't picked onto the tip the merge went
// onto: it stays at ready that pass. The pass after carries the tip's
// check over to the merge's squash commit, and picks it, its run starting
// from that new tip (SPEC §8.1).
func TestPRMergePickAfter(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	n := storyMergeSiblingTask(t, f, "ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.pass()
	squash := v.storyMerged(pr, tip)
	if pickStage(f, n) != "stage:ready" || v.runs() != 0 {
		t.Fatalf("the merge's pass left the task at %s, with %d runs; want ready, and none from the old tip %s", pickStage(f, n), v.runs(), tip)
	}
	v.pass()
	if v.runs() != 1 || pickStage(f, n) == "stage:ready" {
		t.Fatalf("the pass after the merge left the task at %s, after %d runs; want it picked, and one run", pickStage(f, n), v.runs())
	}
	if l := v.leaseOf("1"); l.Issue != n || l.StoryTip != squash || l.Start != squash {
		t.Errorf("the run leased #%d, with story_tip %s and start %s; want #%d, from the new tip %s", l.Issue, l.StoryTip, l.Start, n, squash)
	}
}

// A task in the story at verifying, whose checks would run in step 4 of
// the pass that merges task 1's PR, isn't checked against the tip the
// merge went onto: no command runs that pass. The pass after carries the
// tip's check over to the merge's squash commit, and checks the task's
// head against that new tip (SPEC §10.1).
func TestPRMergeVerifyAfter(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	n := storyMergeSiblingTask(t, f, "verifying")
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "sibling", tip)
	if err := os.WriteFile(filepath.Join(w, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", "stale.txt")
	gitIn(t, w, "commit", "--quiet", "-m", "the sibling's run")
	head := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "checkout", "--quiet", "main")
	gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", w, "+refs/heads/sibling:"+repo.TasksRef+strconv.Itoa(n))
	f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r5 head="+head+" -->")
	ran := v.checksRan()
	v.pass()
	squash := v.storyMerged(pr, tip)
	if v.checksRan() != ran || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("the merge's pass ran the commands %d times, with the task at %s; want never against the old tip %s, and verifying",
			v.checksRan()-ran, pickStage(f, n), tip)
	}
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran+1 {
		t.Errorf("the pass after the merge left the checks %+v, and ran the commands %d times; want the carried success on %s, and the head's checks once",
			got, v.checksRan()-ran, squash)
	}
}

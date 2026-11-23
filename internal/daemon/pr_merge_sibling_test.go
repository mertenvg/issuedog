package daemon_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
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
// would leave it: its head, a commit of its own on tip, on its branch in
// v's origin, its PR from that branch into the story branch open, the
// App's, and its move to pr-open naming the head, the PR and tip. It
// returns the head and the PR's number.
func storyMergeSibling(t *testing.T, f *githubtest.Fake, v *verdictRig, tip string) (string, int) {
	t.Helper()
	n := storyMergeSiblingTask(t, f, "pr-open")
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "sibling", tip)
	if err := os.WriteFile(filepath.Join(w, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", "stale.txt")
	gitIn(t, w, "commit", "--quiet", "-m", "the sibling's run")
	head := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", v.o.url, "sibling:refs/heads/"+storySiblingBranch)
	gitIn(t, w, "checkout", "--quiet", "main")
	pr := f.OpenPRAs(app, storySiblingBranch, storyVerifyBranch, "feat: Refuse a stale quote", "Task #5, in story #2\n\nThe change.")
	f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r5 head="+head+" -->")
	f.AddComment(n, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=r5v head="+head+" pr="+strconv.Itoa(pr)+" tip="+tip+" -->")
	return head, pr
}

// Two tasks of one story at pr-open, both verified against the same tip:
// the pass that merges the first, the lower-numbered, merges only it, as
// the second would go onto the first's squash commit, a tip it wasn't
// verified against. The pass after checks the new tip, and the second then
// waits at pr-open, with no request for its PR, as no check on its merge
// with the new tip is built.
func TestPRMergeSiblings(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	_, sibling := storyMergeSibling(t, f, v, tip)
	writes := len(f.Writes())
	v.pass()
	if first, second := storyMerges(f, writes, pr), storyMerges(f, writes, sibling); len(first) != 1 || len(second) != 0 {
		t.Fatalf("the pass merged %+v and %+v; want the first PR, #%d, alone", first, second, pr)
	}
	squash := v.storyMerged(pr, tip)
	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran+1 {
		t.Fatalf("the pass after the merge left the checks %+v, and ran the commands %d times; want the new tip's success, once", got, v.checksRan()-ran)
	}
	v.quietPass("the sibling waiting on the new tip")
	v.quietPass("again")
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/pulls/"+strconv.Itoa(sibling)) {
			t.Errorf("a pass after the merge sent %+v; want nothing for the sibling's PR", r)
		}
	}
	i := slices.IndexFunc(f.State().PRs, func(p githubtest.PRState) bool { return p.Number == sibling })
	if got := storyMerges(f, 0, sibling); len(got) != 0 || i < 0 || f.State().PRs[i].State != "open" || pickStage(f, 5) != "stage:pr-open" ||
		v.originTip(storyVerifyBranch) != squash {
		t.Errorf("the sibling's merges = %+v, its PRs %+v, the sibling at %s, the story branch at %s; want none, #%d open, pr-open, and %s",
			got, f.State().PRs, pickStage(f, 5), v.originTip(storyVerifyBranch), sibling, squash)
	}
}

// A task in the story at in-progress, whose run would start in step 4 of
// the pass that merges task 1's PR, doesn't start from the tip the merge
// went onto: it waits for the new tip, which the pass after checks, and
// the pass after that starts its run from the new tip, the merge's squash
// commit, which its lease records as story_tip (SPEC §3.4).
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
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.runs() != 0 {
		t.Fatalf("the pass after the merge left the checks %+v, and ran claude %d times; want the new tip's success, and no run", got, v.runs())
	}
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("the task at %s, after %d runs; want one", pickStage(f, n), v.runs())
	}
	if l := v.leaseOf("1"); l.Issue != n || l.StoryTip != squash || l.Start != squash {
		t.Errorf("the run leased #%d, with story_tip %s and start %s; want #%d, from the new tip %s", l.Issue, l.StoryTip, l.Start, n, squash)
	}
}

package daemon_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// The branches state error (SPEC §5.3, §7.5), on a verdictRig wired as
// main's, with real temporary repos: two or more branches with one issue's
// prefix, or none for a story whose branch was deleted once a task or bug
// in it had been in-progress, give the issue one kind=branches error, in
// step 2, keyed by the names, or none. While it stands, nothing runs for
// the issue, and no task in its story is picked or runs.

// branchesErrs are the keys of the branches errors posted on issue n, in
// order, ack replies left out.
func branchesErrs(f *githubtest.Fake, n int) []string {
	var keys []string
	for _, c := range f.State().Issues[n-1].Comments {
		for _, m := range github.ParseMarkers(c.Body) {
			if m.Kind == "error" && m.Fields["kind"] == "branches" && m.Fields["ack"] == "" {
				keys = append(keys, m.Fields["key"])
			}
		}
	}
	return keys
}

// branchesErrIDs are the IDs of the comments on issue n, as v's client reads
// them, that carry a branches error, in order, ack replies left out.
func (v *verdictRig) branchesErrIDs(n int) []int64 {
	v.t.Helper()
	var ids []int64
	for _, c := range v.snapshot(n).Comments {
		if strings.Contains(c.Body, "kind=branches") && !strings.Contains(c.Body, " ack=") {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// branchesAt makes branch, at sha, in v's origin, as an admin may.
func (v *verdictRig) branchesAt(branch, sha string) {
	v.t.Helper()
	gitIn(v.t, v.o.url, "update-ref", "refs/heads/"+branch, sha)
}

// branchesDrop deletes branch in v's origin, as an admin may.
func (v *verdictRig) branchesDrop(branch string) {
	v.t.Helper()
	gitIn(v.t, v.o.url, "update-ref", "-d", "refs/heads/"+branch)
}

// branchesSettle runs the two passes after a post of a state error: the
// first reads the issue again, so step 1 finds the error open, and lists
// and assigns it, and the second reads those writes back, so the next
// unchanged pass is quiet.
func (v *verdictRig) branchesSettle() {
	v.t.Helper()
	v.passes(2)
}

// branchesSubReads counts the reads of issue n's sub-issues among f's
// requests from from on.
func branchesSubReads(f *githubtest.Fake, from, n int) int {
	reads := 0
	for _, r := range f.Requests()[from:] {
		if r.Path == "/issues/"+strconv.Itoa(n)+"/sub_issues" {
			reads++
		}
	}
	return reads
}

// branchesTask files task 1, with no parent, at in-progress, as its pick
// moved it there, whose run commits, on a verdictRig.
func branchesTask(t *testing.T) (*githubtest.Fake, *verdictRig) {
	t.Helper()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	if n != 1 {
		t.Fatalf("the task is #%d, want #1", n)
	}
	f.AddLabel(n, app, "stage:in-progress")
	f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress run=rpick -->")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	return f, v
}

// Two branches with a story's prefix give the story one branches error,
// keyed by their names, sorted, in step 2 (SPEC §5.3, §7.5). Its comment
// names each branch as a code span, and says no task in the story is
// picked or runs. The passes after post none again, pick nothing and run
// nothing, and once its writes are read back, an unchanged pass makes no
// request but 304s.
func TestBranchesTwoOnStory(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	const other = "issuedog/1-another-slug"
	v.tipCut(tipBranch)
	v.tipCut(other)
	v.pass()
	key := other + "," + tipBranch
	if got := branchesErrs(f, 1); !slices.Equal(got, []string{key}) {
		t.Fatalf("the story's branches errors are %q; want %q alone", got, key)
	}
	body := f.State().Issues[0].Comments[len(f.State().Issues[0].Comments)-1].Body
	for _, want := range []string{"2 branches with its prefix `issuedog/1-`", "`" + other + "`, `" + tipBranch + "`",
		"no task or bug in the story is picked or runs", "A 👍 on this comment acknowledges it"} {
		if !strings.Contains(body, want) {
			t.Errorf("the error's comment is\n%s\nwant it to say %q", body, want)
		}
	}
	v.branchesSettle()
	v.quietPass("two branches")
	if got := branchesErrs(f, 1); len(got) != 1 || pickStage(f, 2) != "stage:ready" || v.runs() != 0 {
		t.Errorf("after the passes, the errors are %q, the task at %s, with %d runs; want one, ready, and none", got, pickStage(f, 2), v.runs())
	}
	if got := f.State().Issues[0].Assignees; !slices.Equal(got, []string{wholeRunOwner}) {
		t.Errorf("the story's assignees are %q; want %s, as the error is open", got, wholeRunOwner)
	}
}

// A task with two branches of its own gets one branches error, and starts
// none of its runs, uncounted: no lease, no attempt, and the wait is logged
// once, not every pass (SPEC §7.5). Once one branch is deleted, its run
// starts on the next pass, and the error, its key no longer the set's,
// ends: the task is unassigned. It reads logr, so it isn't parallel.
func TestBranchesTwoOnTask(t *testing.T) {
	f, v := branchesTask(t)
	main := v.mainSHA()
	const mine, copied = "issuedog/1-refuse-an-expired-quote", "issuedog/1-a-copy"
	v.branchesAt(mine, main)
	v.branchesAt(copied, main)
	stop := logtest.Capture(t, logr.I)
	v.pass()
	v.branchesSettle()
	logs := stop()
	if got, want := branchesErrs(f, 1), []string{copied + "," + mine}; !slices.Equal(got, want) {
		t.Fatalf("the task's branches errors are %q; want %q", got, want)
	}
	wait := "#1: no implementer run: it can't tell which is its branch, so its branches state error stands"
	if got := strings.Count(logs, wait); got != 1 {
		t.Errorf("the wait was logged %d times; want once, in\n%s", got, logs)
	}
	if l := v.lease(); v.runs() != 0 || l.RunID != "" || l.Attempt != 0 || pickStage(f, 1) != "stage:in-progress" {
		t.Errorf("claude ran %d times, the lease is %+v, and the task is at %s; want no run, no lease, and in-progress", v.runs(), l, pickStage(f, 1))
	}
	if got := f.State().Issues[0].Assignees; !slices.Equal(got, []string{wholeRunOwner}) {
		t.Errorf("the task's assignees are %q; want %s, as the error is open", got, wholeRunOwner)
	}
	v.quietPass("two branches of its own")

	v.branchesDrop(copied)
	v.pass()
	if v.runs() != 1 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("once one branch is left, claude ran %d times, and the task is at %s; want once, and verifying", v.runs(), pickStage(f, 1))
	}
	v.passes(2)
	if got := f.State().Issues[0].Assignees; len(got) != 0 {
		t.Errorf("once the error ended, the task's assignees are %q; want none", got)
	}
	if got := branchesErrs(f, 1); len(got) != 1 {
		t.Errorf("the task's branches errors are %q; want the first alone", got)
	}
}

// A ready task with two branches of its own isn't picked, as its runs
// wait while its branches error stands (SPEC §7.5): it stays at ready,
// and the worker stays free.
func TestBranchesTwoOnReadyTask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	n := pickReady(f, "p1")
	f.AddLabel(n, "owner", "scope:booking")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	main := v.mainSHA()
	v.branchesAt("issuedog/1-issue", main)
	v.branchesAt("issuedog/1-a-copy", main)
	v.passes(3)
	if got := branchesErrs(f, n); len(got) != 1 || pickStage(f, n) != "stage:ready" || v.runs() != 0 {
		t.Errorf("the errors are %q, the task at %s, with %d runs; want one, ready, and none", got, pickStage(f, n), v.runs())
	}
}

// A set that changes is a new occurrence (SPEC §5.3): three branches, then
// two, post two errors. Back at one branch, no error stands. The same two
// again, after the daemon saw the set change, are a new occurrence, keyed
// with ":" and the ID of the two's newest comment. After a restart, the
// same three again, then the same two again, are new occurrences too, as a
// newer comment for another set shows each set changed. A change the
// daemon doesn't see, between two passes, reuses the error that stands.
func TestBranchesSetChanges(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	tipStory(t, f)
	main := v.mainSHA()
	const a, b, c = "issuedog/1-a", "issuedog/1-b", "issuedog/1-c"
	for _, br := range []string{a, b, c} {
		v.branchesAt(br, main)
	}
	v.pass()
	v.branchesDrop(c)
	v.pass()
	three, two := a+","+b+","+c, a+","+b
	if got, want := branchesErrs(f, 1), []string{three, two}; !slices.Equal(got, want) {
		t.Fatalf("after three branches, then two, the errors are %q; want %q", got, want)
	}
	v.branchesDrop(b)
	v.passes(2)
	if got := branchesErrs(f, 1); len(got) != 2 {
		t.Fatalf("with one branch, the errors are %q; want the two before alone", got)
	}
	v.branchesAt(b, main)
	v.pass()
	ids := v.branchesErrIDs(1)
	twoAgain := two + ":" + strconv.FormatInt(ids[1], 10)
	if got, want := branchesErrs(f, 1), []string{three, two, twoAgain}; !slices.Equal(got, want) {
		t.Fatalf("with the same two again, the errors are %q; want %q", got, want)
	}

	v.branchesSettle()
	v = v.restart()
	v.branchesAt(c, main)
	v.pass()
	v.branchesDrop(c)
	v.pass()
	ids = v.branchesErrIDs(1)
	threeAgain := three + ":" + strconv.FormatInt(ids[0], 10)
	twoThird := two + ":" + strconv.FormatInt(ids[2], 10)
	if got, want := branchesErrs(f, 1), []string{three, two, twoAgain, threeAgain, twoThird}; !slices.Equal(got, want) {
		t.Fatalf("after a restart, three then two again, the errors are %q; want %q", got, want)
	}

	v.branchesDrop(b)
	v.branchesAt(b, main)
	v.branchesSettle()
	if got := branchesErrs(f, 1); len(got) != 5 {
		t.Errorf("after a change between two passes, the errors are %q; want the five before alone", got)
	}
}

// A story whose branch an admin deleted once its task was in-progress gets
// one branches error, keyed none (SPEC §7.5): its branch isn't cut again,
// and its task's run doesn't start, uncounted, while the error stands, and
// once its writes are read back, an unchanged pass makes no request but
// 304s. Once the branch is restored, the task's run starts from its tip,
// and the error ends: the story is unassigned.
func TestBranchesDeletedStoryInProgress(t *testing.T) {
	t.Parallel()
	f, v := newStoryRunRig(t)
	tip := v.tipCut(tipBranch)
	v.storyRunChecked(tip, "success")
	if err := f.DeleteBranchAs(wholeRunOwner, tipBranch); err != nil {
		t.Fatal(err)
	}
	v.pass()
	if got := branchesErrs(f, 1); !slices.Equal(got, []string{"none"}) {
		t.Fatalf("the story's branches errors are %q; want none's alone", got)
	}
	body := f.State().Issues[0].Comments[len(f.State().Issues[0].Comments)-1].Body
	if want := "This story's branch, with the prefix `issuedog/1-`, is gone"; !strings.HasPrefix(body, want) {
		t.Errorf("the error's comment is\n%s\nwant it to start %q", body, want)
	}
	v.branchesSettle()
	v.quietPass("the story's branch deleted")
	if l := v.lease(); v.runs() != 0 || l.RunID != "" || len(pickStoryBranches(f)) != 0 || pickStage(f, 2) != "stage:in-progress" {
		t.Fatalf("claude ran %d times, the lease is %+v, the branches are %+v, and the task is at %s; want no run, no lease, no branch cut, and in-progress",
			v.runs(), l, pickStoryBranches(f), pickStage(f, 2))
	}
	if got := f.State().Issues[0].Assignees; !slices.Equal(got, []string{wholeRunOwner}) {
		t.Errorf("the story's assignees are %q; want %s, as the error is open", got, wholeRunOwner)
	}

	if err := f.RestoreBranch(tipBranch, tip); err != nil {
		t.Fatal(err)
	}
	v.pass()
	if l := v.leaseOf("1"); v.runs() != 1 || l.StoryTip != tip || l.Attempt != 1 {
		t.Fatalf("once the branch is back, claude ran %d times, with the lease %+v; want once, from the story tip %s, on attempt 1", v.runs(), l, tip)
	}
	v.passes(2)
	if got := f.State().Issues[0].Assignees; len(got) != 0 {
		t.Errorf("once the branch is back, the story's assignees are %q; want none", got)
	}
	if got := branchesErrs(f, 1); len(got) != 1 {
		t.Errorf("the story's branches errors are %q; want none's alone", got)
	}
}

// A story whose branch an admin deleted while its task was at pr-open: the
// delete closed the task's PR into it, so the task goes to needs-human, as
// for any PR closed without merging, and the story gets its branches
// error, keyed none, as the task has been in-progress (SPEC §7.5). Nothing
// is merged. Restoring the branch ends the error, and reopens no PR.
func TestBranchesDeletedStoryPROpen(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	from := len(f.Writes())
	if err := f.DeleteBranchAs(wholeRunOwner, storyVerifyBranch); err != nil {
		t.Fatal(err)
	}
	v.passes(2)
	if got := branchesErrs(f, 2); !slices.Equal(got, []string{"none"}) {
		t.Fatalf("the story's branches errors are %q; want none's alone", got)
	}
	if got := storyMerges(f, from, pr); len(got) != 0 || pickStage(f, 1) != "stage:needs-human" || f.State().PRs[0].State != "closed" {
		t.Errorf("the merges are %+v, the task at %s, its PR %s; want none, needs-human, and closed", got, pickStage(f, 1), f.State().PRs[0].State)
	}
	if got := pickStoryBranches(f); slices.ContainsFunc(got, func(b githubtest.BranchState) bool { return strings.HasPrefix(b.Name, "issuedog/2-") }) {
		t.Errorf("the branches are %+v; want none of the story's cut", got)
	}

	if err := f.RestoreBranch(storyVerifyBranch, tip); err != nil {
		t.Fatal(err)
	}
	v.passes(3)
	if got := branchesErrs(f, 2); len(got) != 1 || f.State().PRs[0].State != "closed" {
		t.Errorf("once the branch is back, the errors are %q, the PR %s; want none's alone, and closed", got, f.State().PRs[0].State)
	}
	if got := f.State().Issues[1].Assignees; len(got) != 0 {
		t.Errorf("once the branch is back, the story's assignees are %q; want none", got)
	}
}

// A story's own runs wait while its branches error stands too (SPEC
// §5.3): a story at planning, whose branch is gone once its task was
// in-progress, gets the error, keyed none, and no planner run, uncounted.
func TestBranchesStoryOwnRunWaits(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.o.commit(agent.PromptDir+"/planner.md", "Plan the story.\n")
	if n := tipStoryAt(f, tipStoryTitle, "planning", "type:story"); n != 1 {
		t.Fatalf("the story is #%d, want #1", n)
	}
	task := pickStoryTask(f, "Refuse an expired quote")
	f.AddComment(task, app, "<!-- issuedog:transition from=ready to=in-progress run=rpick -->")
	f.AddComment(task, app, "<!-- issuedog:transition from=in-progress to=ready run=ranswer -->")
	v.passes(3)
	if got := branchesErrs(f, 1); !slices.Equal(got, []string{"none"}) {
		t.Fatalf("the story's branches errors are %q; want none's alone", got)
	}
	if l := v.lease(); v.runs() != 0 || l.RunID != "" || pickStage(f, 1) != "stage:planning" {
		t.Errorf("claude ran %d times, the lease is %+v, the story is at %s; want no run, no lease, and planning", v.runs(), l, pickStage(f, 1))
	}
}

// A story with no branch whose only worked task is closed as completed:
// the daemon holds no closed issue, so before a cut for its ready task the
// pick reads the story's sub-issues, once, finds the closed task, and
// posts the story's branches error, keyed none, instead of the cut (SPEC
// §7.5). The passes after read them no more, cut nothing, pick nothing,
// and are quiet once the writes are read back. A closed human task, which
// is never in-progress, doesn't count, nor does a task closed as not
// planned: the pick cuts the branch.
func TestBranchesDeletedStoryClosedTasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, marker, stage string
		worked              bool
	}{
		{"a task", "<!-- issuedog:origin plan=#1 rev=1 item=T1 -->", "completed", true},
		{"a human task", "<!-- issuedog:origin plan=#1 rev=1 item=T1 human=1 -->", "completed", false},
		{"a task not planned", "<!-- issuedog:origin plan=#1 rev=1 item=T1 -->", "discarded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			tipStory(t, f)
			done := f.AddIssue(app, "Store the quote's expiry", "The expiry.\n\n"+tc.marker, "type:task", "priority:p1", "scope:booking")
			f.SetParent(done, 1)
			f.AddLabel(done, app, "stage:"+tc.stage)
			f.Close(done)
			ready := pickStoryTask(f, "Refuse an expired quote")
			v.out(1, ivResult(t, implementerOutput(t, "commits")))
			from := len(f.Requests())
			v.pass()
			if !tc.worked {
				if got := pickStoryBranches(f); len(got) != 1 || len(branchesErrs(f, 1)) != 0 {
					t.Fatalf("the branches are %+v, the story's errors %q; want the cut, and none", got, branchesErrs(f, 1))
				}
				return
			}
			if got := branchesErrs(f, 1); !slices.Equal(got, []string{"none"}) || len(pickStoryBranches(f)) != 0 || pickStage(f, ready) != "stage:ready" {
				t.Fatalf("the story's errors are %q, the branches %+v, the task at %s; want none's alone, no cut, and ready",
					got, pickStoryBranches(f), pickStage(f, ready))
			}
			waits := len(f.Requests())
			v.branchesSettle()
			v.quietPass("a closed task worked in it")
			if got := branchesSubReads(f, from, 1); got != 1 || len(branchesErrs(f, 1)) != 1 || len(pickStoryBranches(f)) != 0 || v.runs() != 0 {
				t.Errorf("the passes read the story's sub-issues %d times, posted the errors %q, left the branches %+v, and ran claude %d times; want once, one, none, and no run",
					got, branchesErrs(f, 1), pickStoryBranches(f), v.runs())
			}
			if got := pickStoryReads(f, waits); len(got) != 0 {
				t.Errorf("the passes after the error read blocked_by %v; want none, as the task waits on the error", got)
			}
		})
	}
}

// A story whose branch is deleted before any task in it has been
// in-progress holds no task's work, so the next pick cuts it again from
// main, with no state error (SPEC §7.5).
func TestBranchesStoryNoWorkCutAgain(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	v.pickStoryCut(tipBranch, 2)
	if err := f.DeleteBranchAs(wholeRunOwner, tipBranch); err != nil {
		t.Fatal(err)
	}
	v.pickStoryCut(tipBranch, 2)
	if got := branchesErrs(f, 1); len(got) != 0 {
		t.Errorf("the story's branches errors are %q; want none", got)
	}
}

// A trusted 👍 on a branches error acknowledges it as any other (SPEC
// §5.3): one reply records it, the error isn't posted again, and the story
// is unassigned. Its tasks still wait: none is picked, and no branch is
// cut.
func TestBranchesAck(t *testing.T) {
	t.Parallel()
	f, v := newPickStoryRig(t)
	const other = "issuedog/1-another-slug"
	v.tipCut(tipBranch)
	v.tipCut(other)
	v.pass()
	v.branchesSettle()
	key := other + "," + tipBranch
	ids := v.branchesErrIDs(1)
	if len(ids) != 1 {
		t.Fatalf("the story's branches errors are %q; want one", branchesErrs(f, 1))
	}
	r := f.AddReaction(ids[0], wholeRunOwner, "+1")
	v.pass()
	v.branchesSettle()
	v.quietPass("acknowledged")
	comments := f.State().Issues[0].Comments
	if got, want := comments[len(comments)-1].Body, errAckReply(wholeRunOwner, "branches", key, r); got != want {
		t.Errorf("the last comment is\n%s\nwant the ack\n%s", got, want)
	}
	if got := branchesErrs(f, 1); len(got) != 1 || pickStage(f, 2) != "stage:ready" || v.runs() != 0 || len(pickStoryBranches(f)) != 2 {
		t.Errorf("the errors are %q, the task at %s, claude ran %d times, the branches %+v; want one, ready, no run, and the two", got, pickStage(f, 2), v.runs(), pickStoryBranches(f))
	}
	if got := f.State().Issues[0].Assignees; len(got) != 0 {
		t.Errorf("once acknowledged, the story's assignees are %q; want none", got)
	}
}

// The branches error's post, a failed write at it, then the next pass: the
// replay ends with the same writes, one error, as the pass after looks for
// it first (SPEC §5.2, §5.3). Each fake keeps its daemon, as the loop does
// after a pass's error.
func TestBranchesReplay(t *testing.T) {
	t.Parallel()
	_, base := branchesTask(t)
	main := base.mainSHA()
	base.branchesAt("issuedog/1-refuse-an-expired-quote", main)
	base.branchesAt("issuedog/1-a-copy", main)
	roots, rigs := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*verdictRig{}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
		f.AddLabel(n, app, "stage:in-progress")
		f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress run=rpick -->")
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
			rigs[f] = v
		}
		return v.d.Iterate(context.Background())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if got, want := branchesErrs(clean, 1), []string{"issuedog/1-a-copy,issuedog/1-refuse-an-expired-quote"}; !slices.Equal(got, want) {
		t.Fatalf("the clean pass posted the errors %q; want %q", got, want)
	}
	githubtest.Replay(t, setup, pass)
}

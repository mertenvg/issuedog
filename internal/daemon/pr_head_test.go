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
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// A head the daemon didn't push, at pr-open, takes the task back to
// verifying on that head, and the task's head follows it (SPEC §3.2, §7.2,
// §8.1 step 2), against the fakes and a real origin: on a verdictRig's
// task #1, whose PR the PR step opened at its verified head, or on
// prItemFake's task #1 at pr-open with its PR #2.

// prHeadPush commits file on top of base in o's work tree, as someone
// pushing to the task's branch would, and pushes it to prBranch on o. It
// returns the new tip.
func prHeadPush(t *testing.T, o *origin, base, file string) string {
	t.Helper()
	gitIn(t, o.work, "checkout", "--quiet", "-B", "person", base)
	if err := os.WriteFile(filepath.Join(o.work, file), []byte("a person's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, o.work, "add", file)
	gitIn(t, o.work, "commit", "--quiet", "-m", "a person's change to "+file)
	tip := gitIn(t, o.work, "rev-parse", "HEAD")
	gitIn(t, o.work, "push", "--quiet", o.url, "person:refs/heads/"+prBranch)
	gitIn(t, o.work, "checkout", "--quiet", "main")
	return tip
}

// prHeadMoves are the App's comments on task 1 that hold a move pr-open →
// verifying.
func prHeadMoves(f *githubtest.Fake) []string {
	var bodies []string
	for _, c := range f.State().Issues[0].Comments {
		if c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition from=pr-open to=verifying ") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// prHeadText is the text of the move to verifying on tip over old.
func prHeadText(old, tip string) string {
	return "This task's branch is at `" + tip + "`, not at `" + old + "`, the head its PR was verified at, and the daemon didn't push it. " +
		"So the task is back at `verifying` to check the new head, and its PR waits until that head is verified.\n\n"
}

// prHeadOpen files task 1 at verifying with a clean pass on its head
// (prTask), runs the pass that checks it, then the PR step's passes
// (prOpens), so task 1 is at pr-open, and the next unchanged pass is
// quiet. It returns the head.
func (v *verdictRig) prHeadOpen() string {
	v.t.Helper()
	head, _ := v.prTask()
	v.pass()
	v.prOpens(head)
	return head
}

// prHeadRound is task 1's round bound as it stands (state.Issue.Round),
// which its move on a new head carries as seen= (SPEC §5).
func prHeadRound(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(state.Derive(s, slug, func(int64) bool { return false }).Round, 10)
}

// prHeadMoved fails unless task 1 is at verifying by one move from pr-open
// on tip, over old: its comment names both heads, and its marker is keyed
// event=n<tip>, with head= tip and seen= seen, the round bound the task
// had at pr-open (prHeadRound). And the task's head in clone, the daemon's
// bare repo, is tip (SPEC §3.2).
func prHeadMoved(t *testing.T, f *githubtest.Fake, clone *repo.Repo, old, tip, seen string) {
	t.Helper()
	moves := prHeadMoves(f)
	if pickStage(f, 1) != "stage:verifying" || len(moves) != 1 || !strings.HasPrefix(moves[0], prHeadText(old, tip)) {
		t.Fatalf("the task at %s, its moves to verifying %q; want verifying, by one move, starting\n%s", pickStage(f, 1), moves, prHeadText(old, tip))
	}
	want := map[string]string{"from": "pr-open", "to": "verifying", "event": "n" + tip, "head": tip, "seen": seen}
	if got := prMoveFields(t, moves[0]); !maps.Equal(got, want) {
		t.Errorf("the move's marker has %v; want %v", got, want)
	}
	if got := gitIn(t, clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != tip {
		t.Errorf("the task's head is %s; want the new head %s", got, tip)
	}
}

// prHeadNone fails unless task 1 is at stage, with no move to verifying,
// and its head in clone is still head.
func prHeadNone(t *testing.T, f *githubtest.Fake, clone *repo.Repo, step, stage, head string) {
	t.Helper()
	if pickStage(f, 1) != stage || len(prHeadMoves(f)) != 0 {
		t.Errorf("%s: the task at %s, its moves to verifying %q; want %s, and none", step, pickStage(f, 1), prHeadMoves(f), stage)
	}
	if got := gitIn(t, clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != head {
		t.Errorf("%s: the task's head is %s; want it still at %s", step, got, head)
	}
}

// A push to the branch of a task at pr-open: a pass with the branch
// unchanged before it makes no request but 304s, and the pass after the
// push moves the task pr-open → verifying on the new head, keyed by it,
// naming the old head and the new one, and moves the task's head there.
// Its PR's item ends, so notify is unassigned from the PR on that pass.
// The next pass runs the checks on the new head, and the one after runs
// the verifier there, so later runs build on it.
func TestPRHeadPushAtPROpen(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	v.prMoveQuiet("with the branch unchanged", true)
	pr := f.State().PRs[0].Number
	tip := prHeadPush(t, v.o, head, "person.txt")

	seen := prHeadRound(t, f)
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
	if got := assignees(f, pr); len(got) != 0 {
		t.Errorf("the PR's assignees = %q; want nobody, as the task left pr-open", got)
	}
	if v.checksRan() != 1 || v.runs() != 0 {
		t.Errorf("the move's pass ran the commands %d times and claude %d; want once, before, and never: the move was step 2's", v.checksRan(), v.runs())
	}

	v.pass()
	if v.checksRan() != 2 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the pass after the move ran the commands %d times, the task at %s; want twice in all, and still at verifying", v.checksRan(), pickStage(f, 1))
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	if v.runs() != 1 || v.leaseOf("1").Start != tip {
		t.Errorf("after %d runs, the verifier started at %s; want one, at the new head %s", v.runs(), v.leaseOf("1").Start, tip)
	}
	if len(prHeadMoves(f)) != 1 {
		t.Errorf("the moves to verifying = %q; want the one", prHeadMoves(f))
	}
}

// A task held at pr-open isn't judged: a push while it's held moves
// nothing, and the task's head stays. /resume takes it back to pr-open in
// step 1, and step 2 of that pass judges the head that moved meanwhile,
// moving it to verifying on that head, once, with the round bound from
// before the hold.
func TestPRHeadHeldThenResume(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	seen := prHeadRound(t, f) // the hold keeps it (SPEC §6.4)
	f.AddComment(1, wholeRunOwner, "/hold")
	v.pass()
	if got := pickStage(f, 1); got != "stage:held" {
		t.Fatalf("after /hold, the task is at %s; want held", got)
	}
	tip := prHeadPush(t, v.o, head, "person.txt")
	v.passes(2)
	prHeadNone(t, f, v.d.Clone, "held", "stage:held", head)

	f.AddComment(1, wholeRunOwner, "/resume")
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
}

// A push while the task's PR is closed, and its task handed off, moves
// nothing. The owner's reopen sends the task back to pr-open, and the
// head that moved meanwhile is judged then, moving it to verifying on
// that head, once.
func TestPRHeadClosedThenTrustedReopen(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	pr := f.State().PRs[0].Number
	f.ClosePR(pr, wholeRunOwner)
	v.pass()
	if got := pickStage(f, 1); got != "stage:needs-human" {
		t.Fatalf("after the close, the task is at %s; want needs-human", got)
	}
	tip := prHeadPush(t, v.o, head, "person.txt")
	v.passes(2)
	prHeadNone(t, f, v.d.Clone, "closed", "stage:needs-human", head)

	f.ReopenPR(pr, wholeRunOwner)
	v.pass()
	if got := pickStage(f, 1); got != "stage:pr-open" || len(prHeadMoves(f)) != 0 {
		t.Fatalf("after the reopen, the task is at %s, its moves to verifying %q; want pr-open, by the reopen, and none yet", got, prHeadMoves(f))
	}
	seen := prHeadRound(t, f)
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
}

// prHeadAbout reports whether r is a request about task 1, its PR pr, or a
// commit's check runs.
func prHeadAbout(r githubtest.Request, pr int) bool {
	p := "/issues/" + strconv.Itoa(pr)
	return r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/") || r.Path == p || strings.HasPrefix(r.Path, p+"/") ||
		strings.HasPrefix(r.Path, "/pulls") || strings.HasPrefix(r.Path, "/commits/")
}

// A push while another task holds the worker at in-progress: the new head
// is found, but its move waits, with no write, while the other task's
// implementer commits, its checks run and its verifier asks a question,
// and the passes that wait make no request about the task or its PR.
// Once the other task is at clarifying, the worker is free, and the next
// pass makes the move.
func TestPRHeadWaitsForTheWorker(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	pr := f.State().PRs[0].Number
	other := f.AddIssue(wholeRunOwner, "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(other, wholeRunOwner, "stage:in-progress")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.out(2, ivResult(t, verifierOutput(t, "question")))
	tip := prHeadPush(t, v.o, head, "person.txt")
	seen := prHeadRound(t, f)

	v.pass()
	prHeadNone(t, f, v.d.Clone, "found", "stage:pr-open", head)
	if v.runs() != 1 || pickStage(f, other) != "stage:verifying" {
		t.Fatalf("after %d runs, the other task is at %s; want its implementer's run, and verifying", v.runs(), pickStage(f, other))
	}
	for _, step := range []string{"the other task's checks", "the other task's verifier"} {
		from, writes := len(f.Requests()), len(f.Writes())
		v.pass()
		for _, r := range f.Requests()[from:] {
			if prHeadAbout(r, pr) {
				t.Errorf("%s: the pass sent %+v; want nothing about the waiting task or its PR", step, r)
			}
		}
		for _, w := range f.Writes()[writes:] {
			if prHeadAbout(githubtest.Request{Method: w.Method, Path: w.Path}, pr) {
				t.Errorf("%s: the pass wrote %s %s; want nothing about the waiting task or its PR", step, w.Method, w.Path)
			}
		}
		prHeadNone(t, f, v.d.Clone, step, "stage:pr-open", head)
	}
	if v.runs() != 2 || pickStage(f, other) != "stage:clarifying" {
		t.Fatalf("after %d runs, the other task is at %s; want its verifier's run, and its question at clarifying", v.runs(), pickStage(f, other))
	}

	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
}

// A crash just after the move's comment, whose answer is lost, leaves the
// task's head behind. A restart's pass finishes the move's labels, and at
// verifying, whose latest move is the new head's, moves the task's head
// there, before the checks run on it, with no second move.
func TestPRHeadCrashBeforeTheRef(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	tip := prHeadPush(t, v.o, head, "person.txt")
	seen := prHeadRound(t, f)
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" },
		func() { f.FailWriteAfter(len(f.Writes()) + 1) })
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose answer was lost gave no error")
	}
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); len(prHeadMoves(f)) != 1 || got != head {
		t.Fatalf("after the lost answer, the moves %q, the task's head %s; want the move written, and the head still %s", prHeadMoves(f), got, head)
	}

	v = v.restart()
	ran := v.checksRan()
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
	if v.checksRan() != ran+1 {
		t.Errorf("the restart's pass ran the commands %d times; want once, on the new head", v.checksRan()-ran)
	}
}

// A fresh daemon on a task at pr-open whose branch moved while none ran
// finds the new head on its first pass, from its read of the PR and the
// bare clone, and moves the task.
func TestPRHeadRestart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	tip := prHeadPush(t, v.o, head, "person.txt")
	seen := prHeadRound(t, f)
	v = v.restart()
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
}

// A push between the App's push and its PR, here just before the PR is
// opened: the PR step's move names the head it verified and pushed, so the
// next pass finds the branch at the person's commit, a new head, and moves
// the task to verifying on it.
func TestPRHeadPushBeforeThePR(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	v.pass() // the checks
	var tip string
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/pulls" },
		func() { tip = prHeadPush(t, v.o, head, "person.txt") })
	v.pass()
	moves := prMoveComments(f, "pr-open")
	if pickStage(f, 1) != "stage:pr-open" || len(moves) != 1 || prMoveFields(t, moves[0])["head"] != head || tip == "" {
		t.Fatalf("after the PR step, the task at %s, its moves %q, the person's tip %q; want pr-open, by one move naming %s", pickStage(f, 1), moves, tip, head)
	}
	seen := prHeadRound(t, f)
	v.pass()
	prHeadMoved(t, f, v.d.Clone, head, tip, seen)
}

// prHeadFake is prItemFake, but with the move to pr-open naming old as
// its verified head, whatever prBranch's tip on o is.
func prHeadFake(t testing.TB, o *origin, old string) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	f.SetGitRemote(o.url)
	n := f.AddIssue(digestRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1")
	f.AddLabel(n, app, "stage:pr-open")
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: prItemTitle, Head: prBranch, Base: "main", Body: "Closes #1\n\nWhat changed."})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 2 {
		t.Fatalf("the PR is #%d, want #2", pr.Number)
	}
	f.AddLabel(pr.Number, app, "priority:p1")
	f.AddComment(n, app, prMoveText(pr.Number, old)+"<!-- issuedog:transition from=verifying to=pr-open run=rv head="+old+" pr=2 -->")
	f.ForgetWrites()
	return f
}

// Replay passes for the move to verifying on a new head, whatever write a
// crash comes at (SPEC §5.2): each pass is a fresh daemon's, with its own
// bare clone, and the digest and assignment off, so its writes are the
// move's alone.
func TestPRHeadReplay(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	old := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	prHeadPush(t, o, old, "person.txt")
	setup := func(tb testing.TB) *githubtest.Fake { return prHeadFake(tb, o, old) }
	pass := func(f *githubtest.Fake) error {
		d := prItemDaemon(t, f, o)
		d.Digest, d.Assign = false, false
		return d.Iterate(t.Context())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if got, want := writesOf(clean), []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open"}; !slices.Equal(got, want) {
		t.Errorf("the clean pass wrote %q; want %q", got, want)
	}
	githubtest.Replay(t, setup, pass)
}

// Two branches with the task's prefix: the daemon can't tell which is the
// task's, so its tip isn't judged, nothing moves, and that's warned of
// once, not on every pass, while it lasts.
func TestPRHeadTwoBranches(t *testing.T) {
	// Not parallel: it reads logr's output.
	o := prItemOrigin(t)
	gitIn(t, o.work, "push", "--quiet", o.url, "main:refs/heads/issuedog/1-another-branch")
	f := prItemFake(t, o)
	d := prItemDaemon(t, f, o)
	stop := logtest.Capture(t, logr.W)
	iterate(t, d)
	iterate(t, d)
	iterate(t, d)
	logs := stop()
	if got := strings.Count(logs, "more than one branch has the task's prefix"); got != 1 {
		t.Errorf("the warnings =\n%s\nwant one about the two branches", logs)
	}
	if pickStage(f, 1) != "stage:pr-open" || len(prHeadMoves(f)) != 0 {
		t.Errorf("the task at %s, its moves to verifying %q; want still at pr-open, and none", pickStage(f, 1), prHeadMoves(f))
	}
}

// A push with a close and an untrusted reopen whose event is late: while
// the PR's newest close or reopen is a close, on a PR listed open, the
// PR isn't judged, so its branch isn't either, and nothing moves. Once
// the reopened event is there, the untrusted reopen hands the task off,
// and the new head moves nothing (SPEC §6.4).
func TestPRHeadWaitsForALateReopen(t *testing.T) {
	t.Parallel()
	o, f, d := prReconcileRig(t)
	old := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	prHeadPush(t, o, old, "person.txt")
	f.LagEvents(30 * time.Second)
	f.ClosePR(2, digestRunOwner)
	f.Advance(time.Minute) // the closed event is written
	id := f.ReopenPR(2, "mallory")
	f.LagEvents(0) // the App's writes' events aren't late
	for _, step := range []string{"the pass that lists the reopen", "the pass after"} {
		iterate(t, d)
		if pickStage(f, 1) != "stage:pr-open" || len(prHeadMoves(f)) != 0 || len(prReconcileMoves(f)) != 0 {
			t.Errorf("%s: the task at %s, its moves to verifying %q, its hand-offs %q; want pr-open, unmoved", step, pickStage(f, 1), prHeadMoves(f), prReconcileMoves(f))
		}
	}
	f.Advance(time.Minute) // the reopened event is written
	iterate(t, d)
	prReconcileHandedOff(t, f, strconv.FormatInt(id, 10), prReconcileReopenedIntro("mallory"), prReconcileReopenedWhy)
	if moves := prHeadMoves(f); len(moves) != 0 {
		t.Errorf("the moves to verifying = %q; want none", moves)
	}
}

// A task set back to pr-open by hand after its PR's close was handed off
// isn't judged for a new head: its PR is closed, so the close is the
// PR's, and a push to its branch moves nothing (SPEC §6.4).
func TestPRHeadClosedPRSetBack(t *testing.T) {
	t.Parallel()
	o, f, d := prReconcileRig(t)
	old := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	f.ClosePR(2, digestRunOwner)
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:needs-human" {
		t.Fatalf("after the close, the task is at %s; want needs-human", got)
	}
	f.RemoveLabel(1, digestRunOwner, "stage:needs-human")
	f.AddLabel(1, digestRunOwner, "stage:pr-open")
	prHeadPush(t, o, old, "person.txt")
	iterate(t, d)
	iterate(t, d)
	if pickStage(f, 1) != "stage:pr-open" || len(prHeadMoves(f)) != 0 {
		t.Errorf("the task at %s, its moves to verifying %q; want still at pr-open, and none", pickStage(f, 1), prHeadMoves(f))
	}
}

// A task the owner sets back to pr-open by hand after a new head moved it
// to verifying, with its branch still at that head, isn't moved again by
// the same head: that key moves it once (SPEC §5.2), so once the PR is
// judged again, a pass makes no write, and no request about the task or
// its PR.
func TestPRHeadSetBackByHand(t *testing.T) {
	t.Parallel()
	o, f, d := prReconcileRig(t)
	old := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	prHeadPush(t, o, old, "person.txt")
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:verifying" || len(prHeadMoves(f)) != 1 {
		t.Fatalf("after the push, the task is at %s, its moves to verifying %q; want verifying, by one", got, prHeadMoves(f))
	}
	f.RemoveLabel(1, digestRunOwner, "stage:verifying")
	f.AddLabel(1, digestRunOwner, "stage:pr-open")
	iterate(t, d)
	iterate(t, d)
	iterate(t, d)
	from := len(f.Requests())
	prItemQuiet(t, f, d, "set back by hand")
	for _, r := range f.Requests()[from:] {
		if prHeadAbout(r, 2) {
			t.Errorf("set back by hand: the pass sent %+v; want nothing about the task or its PR", r)
		}
	}
	if pickStage(f, 1) != "stage:pr-open" || len(prHeadMoves(f)) != 1 {
		t.Errorf("the task at %s, its moves to verifying %q; want pr-open, by hand, and the one move", pickStage(f, 1), prHeadMoves(f))
	}
}

// A new head's move naming a commit the bare repo lacks moves no task
// head: the pass at verifying gives no error, and leaves the checks to
// find the head missing (SPEC §10.1).
func TestPRHeadMissingCommit(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	missing := strings.Repeat("ab", 20)
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, app, "stage:verifying")
	f.AddComment(n, app, "A new head.\n\n<!-- issuedog:transition from=pr-open to=verifying event=n"+missing+" head="+missing+" -->")
	v.pass()
	if got := gitIn(t, v.d.Clone.Dir(), "for-each-ref", repo.TasksRef+"1"); got != "" {
		t.Errorf("the task's head is %q; want none, as the bare repo lacks %s", got, missing)
	}
}

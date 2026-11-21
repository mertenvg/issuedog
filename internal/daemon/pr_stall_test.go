package daemon_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The PR step's quiet waits as the digest's stalled task (SPEC §9.4,
// §9.6), against the fakes and a real origin: on a verdictRig's task #1 at
// verifying, held up by someone else's open PR from its branch or by
// someone else's tip on it, or on prReconcileRig's task #1, set back to
// pr-open by hand with its PR #2 closed and that close handed off.

// prStallItem is the digest's stalled task item on task #1 at rank 1, with
// lines, linking link.
func prStallItem(link string, lines ...string) string {
	return "### 1. #1: Refuse an expired quote\n\n**Stalled task**, priority p1, unblocks 1 open issue (itself included)\n\n- " +
		strings.Join(lines, "\n- ") + "\n- Answer here: " + link
}

// prStallPRLines are the lines of task #1's item while someone else's
// open PR, theirs, stands from its branch.
func prStallPRLines(theirs int) []string {
	pr := strconv.Itoa(theirs)
	return []string{
		"This task's head passed, but someone else's open PR #" + pr + " from its branch to `main` stands, so GitHub won't open the task's, and it waits at `verifying`. " +
			"It holds the worker, so no other task is picked.",
		"Close PR #" + pr + ", and the task's opens once a poll lists it closed; or set `stage:ready` for a new round.",
	}
}

// prStallTipLines are the lines of task #1's item while its branch is at
// tip, someone else's commits, over its head.
func prStallTipLines(head, tip string) []string {
	return []string{
		"This task's head `" + head + "` passed, but its branch is at `" + tip + "`, which isn't in the head's history and no App marker names, so it's someone else's commits: " +
			"nothing is pushed over them, and it waits at `verifying`. It holds the worker, so no other task is picked.",
		"Reset the branch to a commit in `" + head + "`'s history, and the head is pushed and its PR opened; or set `stage:ready` for a new round.",
	}
}

// prStallClosedLines are the lines of task #1's item at pr-open with its
// PR #2 closed, and that close handed off already.
var prStallClosedLines = []string{
	"This task is at `pr-open`, but its PR #2 is closed, and its close was handed off already, so the close moves it no more.",
	"Reopen PR #2 to wait on its review, or set `stage:ready` for a new round, or close this issue.",
}

// prStallListed fails unless the digest on f lists item, and notify,
// wholeRunOwner, is assigned to task #1.
func prStallListed(t *testing.T, f *githubtest.Fake, step, item string) {
	t.Helper()
	if digest := prMoveDigest(f); !strings.Contains(digest, item) {
		t.Errorf("%s: the digest =\n%s\nwant it to list\n%s", step, digest, item)
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{wholeRunOwner}) {
		t.Errorf("%s: the task's assignees = %q; want %s", step, got, wholeRunOwner)
	}
}

// prStallEnded fails unless the digest on f lists no stalled task, and
// task #1 is at pr-open, with nobody assigned to it, and its PR, the
// App's, listed with notify assigned to it.
func prStallEnded(t *testing.T, f *githubtest.Fake, step string) {
	t.Helper()
	digest := prMoveDigest(f)
	if strings.Contains(digest, "**Stalled task**") || !strings.Contains(digest, "**Pull request**") || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("%s: the task at %s, the digest =\n%s\nwant pr-open, its pull request, and no stalled task", step, pickStage(f, 1), digest)
	}
	if got := assignees(f, 1); len(got) != 0 {
		t.Errorf("%s: the task's assignees = %q; want nobody", step, got)
	}
}

// prStallTheirs files task #1 at verifying with a clean pass on its head
// (prTask), with mallory's open PR from its branch, at main, to main, and
// runs the pass that checks the head, the PR step's pass, whose open
// GitHub refuses, and the pass after, whose poll lists mallory's PR as
// the App's push moved its head, so the step tries once more. It returns
// mallory's PR's number.
func (v *verdictRig) prStallTheirs() int {
	v.t.Helper()
	v.prTask()
	gitIn(v.t, v.o.work, "push", "--quiet", v.o.url, "main:refs/heads/"+prBranch)
	theirs := v.f.OpenPRAs("mallory", prBranch, "main", "mallory's", "Mine.")
	v.passes(3)
	if pickStage(v.f, 1) != "stage:verifying" || len(v.f.State().PRs) != 1 {
		v.t.Fatalf("the task at %s, PRs %+v; want verifying, and mallory's PR alone", pickStage(v.f, 1), v.f.State().PRs)
	}
	return theirs
}

// Someone else's open PR from the task's branch makes the PR step wait at
// verifying, holding the worker: the digest lists the task as stalled on
// it, naming and linking her PR, saying to close it or set stage:ready,
// and notify is assigned to the task. It's found from the PR step's own
// listing and its refused open, so a pass after makes no request but
// 304s. Once mallory closes her PR, the next pass opens the App's and
// moves the task to pr-open: the stalled item ends, notify is unassigned
// from the task, and the PR's item is listed instead.
func TestPRStallSomeoneElsesPR(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	theirs := v.prStallTheirs()
	item := prStallItem("https://github.com/o/r/pull/"+strconv.Itoa(theirs), prStallPRLines(theirs)...)
	prStallListed(t, f, "waiting on mallory's PR", item)
	v.pass()
	v.quietPass("waiting on mallory's PR")
	prStallListed(t, f, "the quiet pass", item)

	// The App's PR opens once mallory's closes, but adding its label fails,
	// so the task stays at verifying: the wait has ended all the same, and
	// the next pass lists it no more.
	f.ClosePR(theirs, "mallory")
	f.AnswerOnce(func(r githubtest.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/labels") && r.Path != "/issues/1/labels"
	}, http.StatusInternalServerError)
	if err := v.d.Iterate(context.Background()); err == nil || pickStage(f, 1) != "stage:verifying" || len(f.State().PRs) != 2 {
		t.Fatalf("the pass whose label failed gave %v, the task at %s, PRs %+v; want its error, verifying, and the App's PR open", err, pickStage(f, 1), f.State().PRs)
	}
	v.pass()
	if digest := prMoveDigest(f); strings.Contains(digest, "**Stalled task**") {
		t.Errorf("once the App's PR opened, the digest =\n%s\nwant no stalled task", digest)
	}
	v.pass()
	prStallEnded(t, f, "once mallory's PR closed")
}

// Someone else's tip on the task's branch makes the PR step wait at
// verifying, pushing nothing: the digest lists the task as stalled on it,
// naming the head and the tip, saying to reset the branch or set
// stage:ready, linking the issue, and notify is assigned to the task. It's
// found from the bare repo, so a pass after makes no request but 304s. A
// tip that moves on to other commits of theirs changes the item in place,
// keyed by the pass's run, so the digest names the new tip. Once the
// branch is reset to main, in the head's history, the next pass pushes the
// head, opens the PR and moves the task to pr-open: the item ends, and
// notify is unassigned from the task.
func TestPRStallSomeoneElsesTip(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	tip := v.otherHead("theirs", map[string]string{"theirs.txt": "Theirs.\n"})
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "theirs:refs/heads/"+prBranch)
	v.passes(3)
	prStallListed(t, f, "waiting on their tip", prStallItem("https://github.com/o/r/issues/1", prStallTipLines(head, tip)...))
	v.pass()
	v.quietPass("waiting on their tip")

	next := v.otherHead("more", map[string]string{"more.txt": "More of theirs.\n"})
	gitIn(t, v.o.work, "push", "--quiet", "--force", v.o.url, "more:refs/heads/"+prBranch)
	v.passes(2)
	digest := prMoveDigest(f)
	prStallListed(t, f, "on their next tip", prStallItem("https://github.com/o/r/issues/1", prStallTipLines(head, next)...))
	if strings.Count(digest, "**Stalled task**") != 1 || len(f.State().PRs) != 0 {
		t.Errorf("on their next tip, the digest =\n%s\nPRs %+v; want one stalled task, and no PR", digest, f.State().PRs)
	}

	// Once the branch is reset, the head is pushed, but GitHub fails the
	// open, so the task stays at verifying: the wait has ended all the
	// same, and the next pass lists it no more.
	gitIn(t, v.o.work, "push", "--quiet", "--force", v.o.url, "main:refs/heads/"+prBranch)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/pulls" }, http.StatusInternalServerError)
	if err := v.d.Iterate(context.Background()); err == nil || pickStage(f, 1) != "stage:verifying" || len(f.State().PRs) != 0 {
		t.Fatalf("the pass whose open failed gave %v, the task at %s, PRs %+v; want its error, verifying, and no PR", err, pickStage(f, 1), f.State().PRs)
	}
	v.pass()
	if digest := prMoveDigest(f); strings.Contains(digest, "**Stalled task**") {
		t.Errorf("once the branch was reset, the digest =\n%s\nwant no stalled task", digest)
	}
	v.pass()
	if got := v.originTip(prBranch); got != head {
		t.Errorf("once the branch was reset, it's at %s; want the head %s pushed", got, head)
	}
	prStallEnded(t, f, "once the branch was reset")
}

// A task set back to pr-open by hand after its PR's close was handed off,
// with the PR still closed, is stalled at pr-open: the close moves it no
// more, so the digest lists it, saying to reopen the PR or set
// stage:ready, linking the PR, and notify is assigned to the task, not the
// PR. It's found from the poll's listing of the PR, so a pass after writes
// nothing and sends no request about the PR. The owner's reopen moves
// nothing, and ends the item on that pass: the pull request item is
// listed again, with notify assigned to the PR and unassigned from the
// task. Closing the task ends it as well.
func TestPRStallClosed(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*testing.T, *githubtest.Fake){
		"the PR reopened": func(t *testing.T, f *githubtest.Fake) {
			f.ReopenPR(2, digestRunOwner)
		},
		"the task closed": func(t *testing.T, f *githubtest.Fake) {
			f.CloseBy(1, digestRunOwner)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			prReconcileHandOff(t, f, d)
			f.RemoveLabel(1, digestRunOwner, "stage:needs-human")
			f.AddLabel(1, digestRunOwner, "stage:pr-open")
			iterate(t, d)
			item := prStallItem("https://github.com/o/r/pull/2", prStallClosedLines...)
			if digest := f.State().Issues[2].Body; !strings.Contains(digest, item) || strings.Contains(digest, "**Pull request**") {
				t.Errorf("set back to pr-open by hand, the digest =\n%s\nwant it to list\n%s\nand no pull request", digest, item)
			}
			if pr, task := assignees(f, 2), assignees(f, 1); len(pr) != 0 || !slices.Equal(task, []string{digestRunOwner}) || pickStage(f, 1) != "stage:pr-open" {
				t.Errorf("at %s, the PR's assignees = %q, the task's %q; want pr-open, nobody on the PR, and %s on the task", pickStage(f, 1), pr, task, digestRunOwner)
			}
			iterate(t, d)
			prItemQuiet(t, f, d, "stalled on its closed PR")

			end(t, f)
			iterate(t, d)
			digest := f.State().Issues[2].Body
			if strings.Contains(digest, "**Stalled task**") || len(prReconcileMoves(f)) != 1 {
				t.Errorf("once it ended, the digest =\n%s\nthe moves %q; want no stalled task, and the one hand-off", digest, prReconcileMoves(f))
			}
			if name != "the PR reopened" {
				return
			}
			if pr, task := assignees(f, 2), assignees(f, 1); !strings.Contains(digest, prItemBody) || !slices.Equal(pr, []string{digestRunOwner}) || len(task) != 0 {
				t.Errorf("once the PR reopened, the digest =\n%s\nthe PR's assignees %q, the task's %q; want its pull request item, %s on the PR, and nobody on the task",
					digest, pr, task, digestRunOwner)
			}
		})
	}
}

// A restart forgets the PR step's waits, which it finds again only once
// the checks run again and the PR step judges the task. Meanwhile the
// stalled task's item read back from the digest's body stands, so the
// digest's body isn't edited, nor the task's assignees, on any pass, and
// the item isn't announced again: the restart's first pass runs the
// checks, the next the PR step, whose refused open finds the wait again.
// Once mallory closes her PR, the item ends.
func TestPRStallRestart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	theirs := v.prStallTheirs()
	item := prStallItem("https://github.com/o/r/pull/"+strconv.Itoa(theirs), prStallPRLines(theirs)...)
	v.pass()
	prStallListed(t, f, "before the restart", item)
	digest, writes, ran := prMoveDigest(f), len(f.Writes()), v.checksRan()
	announced := prStallAnnounced(f)

	v = v.restart()
	v.passes(3)
	if v.checksRan() != ran+1 {
		t.Errorf("after the restart, the commands ran %d more times; want once", v.checksRan()-ran)
	}
	opens := 0
	for _, w := range f.Writes()[writes:] {
		if w.Method != http.MethodPost || w.Path != "/pulls" {
			t.Errorf("after the restart, the passes wrote %s %s; want only the PR step's open, and no write to the digest or anyone's assignees", w.Method, w.Path)
			continue
		}
		opens++
	}
	if opens != 1 {
		t.Errorf("after the restart, the PR step tried to open the PR %d times; want once, which finds the wait again", opens)
	}
	if got := prMoveDigest(f); got != digest || prStallAnnounced(f) != announced {
		t.Errorf("after the restart, the digest =\n%s\nannounced %d times; want it as it was\n%s\nannounced %d times", got, prStallAnnounced(f), digest, announced)
	}
	prStallListed(t, f, "after the restart", item)

	f.ClosePR(theirs, "mallory")
	v.passes(2)
	prStallEnded(t, f, "once mallory's PR closed")
}

// prStallAnnounced is how many comments on the digest on f announce new
// items.
func prStallAnnounced(f *githubtest.Fake) int {
	n := 0
	for _, is := range f.State().Issues {
		if is.Author != app || !strings.HasSuffix(is.Body, "<!-- issuedog:digest -->") {
			continue
		}
		for _, c := range is.Comments {
			if strings.Contains(c.Body, "<!-- issuedog:digest-new ") {
				n++
			}
		}
	}
	return n
}

// Someone else's tip on the task's branch, with a crash at each write, then
// a restart: the replay ends with the digest listing the stalled task and
// notify assigned to it, as a clean run does (SPEC §5.2, §9.6). Each pass
// is a fresh daemon on the fake's own bare clone, so it runs the checks
// first, then the PR step, which finds the wait, and its third iteration
// lists it, by then in every run. The origin is shared, so every run's
// commits, and so its SHAs, are the clean run's.
func TestPRStallReplay(t *testing.T) {
	t.Parallel()
	base := newVerdictRig(t, newFake(t), 3)
	head := base.head()
	tip := base.otherHead("theirs", map[string]string{"theirs.txt": "Theirs.\n"})
	gitIn(t, base.o.work, "push", "--quiet", base.o.url, "theirs:refs/heads/"+prBranch)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		n, _ := verifierTask(f, head)
		f.AddComment(n, app, prPassComment(t, "rv", head, "Fine."))
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, base.o, roots[f])
		if clones[f] == nil {
			clones[f] = g.d.Clone
			fetchStartHead(t, g, base.o, head)
		}
		g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
		v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
		v.wire()
		for range 3 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for f := range roots {
		prStallListed(t, f, "after the replay", prStallItem("https://github.com/o/r/issues/1", prStallTipLines(head, tip)...))
		if got := gitIn(t, base.o.url, "rev-parse", "refs/heads/"+prBranch); got != tip || len(f.State().PRs) != 0 {
			t.Errorf("after the replay, the branch is at %s, PRs %+v; want their tip %s, and no PR", got, f.State().PRs, tip)
		}
	}
}

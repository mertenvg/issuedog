package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The human task filed for a story branch that fails rule 2 closes on a
// /done or a 👍 only once the branch's tip isn't the failing tip its origin
// names (SPEC §9.3, A story branch's failure).

// humanTipBranch is story 9's branch, as the App cut it.
const humanTipBranch = "issuedog/9-the-story"

// humanTipTask is newFake with issue 1 the human task the App filed for
// story 9's branch failing rule 2 at tip, its origin marker carrying
// human=1, story and tip, at needs-human by its first move, comment 1001,
// keyed by the tip. Then it runs more.
func humanTipTask(tip string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		humanTipFile(t, f, tip, 1001)
		more(t, f)
		return f
	}
}

// humanTipFile files the human task for story 9's branch failing rule 2 at
// tip on f, at needs-human by its first move, failing t unless that move's
// comment ID is move, and returns its number.
func humanTipFile(t testing.TB, f *githubtest.Fake, tip string, move int64) int {
	t.Helper()
	n := f.AddIssue(app, "Revert the protected change on story #9's branch",
		"Revert it.\n\n<!-- issuedog:origin human=1 story=#9 tip="+tip+" -->")
	f.SetType(n, "task")
	f.AddLabel(n, app, "stage:needs-human")
	if id := f.AddComment(n, app, "<!-- issuedog:transition from=intake to=needs-human event=t"+tip+" -->"); id != move {
		t.Fatalf("first move's comment ID = %d, want %d", id, move)
	}
	return n
}

// humanTipDaemon is doneDaemon reading the story branches from its clone,
// as daemon.New wires it, with the 👍 trigger as its only reaction trigger,
// trusting thumbTrust.
func humanTipDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := doneDaemon(t, f, o)
	d.Step.StoryBranches = d.Clone.Branches
	d.Step.Reactions = daemon.DoneByReaction(thumbTrust)
	return d
}

// humanTipPass is a fresh humanTipDaemon's two passes over f, as after a
// restart: the second judges a close the first made (closingJudge).
func humanTipPass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := humanTipDaemon(t, f, o)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		return d.Iterate(context.Background())
	}
}

// humanTipPush commits on o's work tree and pushes it to each of branches,
// as a person's push does, and returns the new tip.
func humanTipPush(t testing.TB, o *origin, branches ...string) string {
	t.Helper()
	gitIn(t, o.work, "commit", "--quiet", "--allow-empty", "-m", "a person's push")
	tip := gitIn(t, o.work, "rev-parse", "HEAD")
	for _, b := range branches {
		gitIn(t, o.work, "push", "--quiet", "--force", o.url, "HEAD:refs/heads/"+b)
	}
	return tip
}

// humanTipStill is what the tip check finds while the story branch is at
// the failing tip tip.
func humanTipStill(tip string) string {
	return "The story branch `" + humanTipBranch + "` is still at the failing tip `" + tip + "`."
}

// humanTipMoved is what the tip check finds once the story branch has moved
// from the failing tip tip to now.
func humanTipMoved(tip, now string) string {
	return "The story branch `" + humanTipBranch + "` is at `" + now + "`, no longer at the failing tip `" + tip + "`."
}

// humanTipRejected is the App's reply to the /done in comment id, whose tip
// check found found.
func humanTipRejected(found string, id int64) string {
	return rejected("/done", `its "Done when" check didn't pass. `+strings.TrimSuffix(found, "."), fmt.Sprint(id))
}

// A /done while the story branch is still at the failing tip gets the App's
// 👎 and one reply naming the branch and the tip, and the issue stays open
// at needs-human. A restarted daemon writes nothing for it, nor for the
// branch moving on, which no /done asks about yet. A /done after the new tip
// closes the issue as completed, saying what the check found, and the next
// pass sets stage:completed. Replay passes for the rejection and the close.
func TestHumanTipDone(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	failing := humanTipPush(t, o, humanTipBranch, "issuedog/90-another-story")
	setup := humanTipTask(failing, doneBy("owner", 1002))
	githubtest.Replay(t, setup, humanTipPass(t, o))

	f := setup(t)
	iterate(t, humanTipDaemon(t, f, o))
	reply := humanTipRejected(humanTipStill(failing), 1002)
	doneState(t, f, []string{"stage:needs-human"}, false, []string{"/done", reply})
	if r := f.State().Issues[0].Comments[1].Reactions; !slices.Equal(r, []string{app + " -1"}) {
		t.Errorf("/done's reactions = %q, want the App's 👎", r)
	}

	d := humanTipDaemon(t, f, o) // a restart
	writes := len(f.Writes())
	iterate(t, d)
	now := humanTipPush(t, o, humanTipBranch)
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("a restarted daemon wrote %+v with no new /done, before and after the new tip; want nothing", got)
	}

	doneBy("owner", 1005)(t, f) // after the reply, 1003, and the 👎, 1004
	iterate(t, d)
	closed := doneClosed(humanTipMoved(failing, now), 1005)
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", reply, "/done", closed})
	if r := stateReason(t, f, 1); r != "completed" {
		t.Errorf("state_reason = %q, want completed", r)
	}
	iterate(t, d)
	doneState(t, f, []string{"stage:completed"}, true, []string{"/done", reply, "/done", closed})

	// The branch is at the new tip now, so a /done on a fresh task for the
	// failing tip closes it.
	githubtest.Replay(t, setup, humanTipPass(t, o))
	f = run(t, setup, humanTipPass(t, o))
	doneState(t, f, []string{"stage:completed"}, true, []string{"/done", doneClosed(humanTipMoved(failing, now), 1002)})
}

// A trusted 👍 on the issue while the story branch is still at the failing
// tip gets one reply naming the branch and the tip, and the issue stays
// open; the 👍 is checked once. Another trusted 👍 after the new tip closes
// it as completed. Replay passes for the rejection and the close.
func TestHumanTipThumb(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	failing := humanTipPush(t, o, humanTipBranch)
	var first int64
	setup := humanTipTask(failing, func(t testing.TB, f *githubtest.Fake) { first = f.AddIssueReaction(1, "owner", "+1") })
	githubtest.Replay(t, setup, humanTipPass(t, o))

	f := setup(t)
	d := humanTipDaemon(t, f, o)
	iterate(t, d)
	reply := thumbRejected("owner", humanTipStill(failing), first)
	doneState(t, f, []string{"stage:needs-human"}, false, []string{reply})
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the next pass wrote %+v, want nothing", got)
	}

	now := humanTipPush(t, o, humanTipBranch)
	second := f.AddIssueReaction(1, "owner2", "+1")
	iterate(t, d)
	doneState(t, f, []string{"stage:needs-human"}, true, []string{reply, thumbClosed("owner2", humanTipMoved(failing, now), second)})

	githubtest.Replay(t, setup, humanTipPass(t, o))
}

// A revert of the protected change whose own tip fails rule 2 again moves
// the branch on, so the human task filed for the first failing tip closes
// on /done. The one filed for the revert's tip doesn't: the branch is
// still at its failing tip.
func TestHumanTipRevertFailsAgain(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	failing := humanTipPush(t, o, humanTipBranch)
	revert := humanTipPush(t, o, humanTipBranch)
	f := newFake(t)
	humanTipFile(t, f, failing, 1001)
	humanTipFile(t, f, revert, 1002)
	doneBy("owner", 1003)(t, f)
	if id := f.AddComment(2, "owner", "/done"); id != 1004 {
		t.Fatalf("#2's /done's comment ID = %d, want 1004", id)
	}
	iterate(t, humanTipDaemon(t, f, o))
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed(humanTipMoved(failing, revert), 1003)})
	is := f.State().Issues[1]
	if want := []string{"/done", humanTipRejected(humanTipStill(revert), 1004)}; is.Closed || !slices.Equal(closingComments(f, 2, 1), want) {
		t.Errorf("#2 is closed %t, with comments\n%q\nwant it open, with\n%q", is.Closed, closingComments(f, 2, 1), want)
	}
}

// With no branch of the story's prefix, here only another story's whose
// number starts the same, or a deleted one, or with two of it, the check
// fails, and the reply to /done says why, naming the prefix, the branches
// and the failing tip. The issue stays open. Replay passes for each.
func TestHumanTipBranches(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		push   []string // the branches the failing tip is pushed to
		delete bool     // the story's branch is deleted after
		found  string   // what the check finds, with %[1]s the failing tip
	}{
		{"no branch", []string{"issuedog/90-another-story"}, false,
			"Story #9 has no branch with the prefix `issuedog/9-`, so it has no tip to check against the failing tip `%[1]s`."},
		{"a deleted branch", []string{humanTipBranch}, true,
			"Story #9 has no branch with the prefix `issuedog/9-`, so it has no tip to check against the failing tip `%[1]s`."},
		{"two branches", []string{humanTipBranch, "issuedog/9-a-second", "issuedog/90-another-story"}, false,
			"Story #9 has more than one branch with the prefix `issuedog/9-`, `issuedog/9-a-second` and `" + humanTipBranch + "`, " +
				"so I can't tell which one's tip to check against the failing tip `%[1]s`."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := newOrigin(t)
			failing := humanTipPush(t, o, tt.push...)
			if tt.delete {
				gitIn(t, o.work, "push", "--quiet", o.url, "--delete", "refs/heads/"+humanTipBranch)
			}
			setup := humanTipTask(failing, doneBy("owner", 1002))
			githubtest.Replay(t, setup, humanTipPass(t, o))

			f := run(t, setup, humanTipPass(t, o))
			doneState(t, f, []string{"stage:needs-human"}, false, []string{"/done", humanTipRejected(fmt.Sprintf(tt.found, failing), 1002)})
		})
	}
}

// A read of the story's branches that fails is no failed check: the pass
// fails with no reply and nothing written, and the next pass, whose read
// works, checks the /done.
func TestHumanTipReadFails(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	failing := humanTipPush(t, o, humanTipBranch)
	f := humanTipTask(failing, doneBy("owner", 1002))(t)
	d := humanTipDaemon(t, f, o)
	gitErr := errors.New("git for-each-ref: exit status 128")
	d.Step.StoryBranches = func(context.Context, string) (map[string]string, error) { return nil, gitErr }
	if err := d.Iterate(context.Background()); !errors.Is(err, gitErr) || !strings.Contains(err.Error(), "run the tip check") {
		t.Errorf("Iterate = %v; want the tip check's read error, wrapped", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("the failed pass wrote %+v, want nothing", w)
	}
	d.Step.StoryBranches = d.Clone.Branches
	iterate(t, d)
	doneState(t, f, []string{"stage:needs-human"}, false, []string{"/done", humanTipRejected(humanTipStill(failing), 1002)})
}

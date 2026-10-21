package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #205: a /resume on an issue J, then a /hold on its
// ancestor A, both before the next pass. Commands run in issue-number
// order, so when A has the lower number, the /hold is handled first: it
// writes no marker on J and the issues below it, held already (Q2b), and
// the /resume handled next mustn't release them from a /hold posted after
// it (SPEC §6.4's one rule).

// raceOrders are the numbers of A, J under A, and K under J, in every
// order: each pass runs over #1, #2 and #3 in turn.
var raceOrders = [][3]int{{1, 2, 3}, {1, 3, 2}, {2, 1, 3}, {2, 3, 1}, {3, 1, 2}, {3, 2, 1}}

// raceFamily is #1, #2 and #3, all at ready, A, J and K numbered as order
// says, J under A and K under J. With own, the owner's /hold on J holds J
// and K first, settled, and its ID is returned.
func raceFamily(t testing.TB, order [3]int, own bool) (f *githubtest.Fake, a, j, k int, hold int64) {
	t.Helper()
	f = discFake(t)   // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	a, j, k = order[0], order[1], order[2]
	f.SetParent(j, a)
	f.SetParent(k, j)
	if own {
		hold = f.AddComment(j, "owner", "/hold")
		resolveSettle(t, f, 1, 2, 3)
		raceStages(t, f, "after J's /hold", map[int]string{a: "ready", j: "held", k: "held"})
	}
	return f, a, j, k, hold
}

// raceStages checks each issue's stage, want[n] for #n.
func raceStages(t testing.TB, f *githubtest.Fake, when string, want map[int]string) {
	t.Helper()
	for n, stage := range want {
		if l := f.State().Issues[n-1].Labels; len(l) != 1 || l[0] != "stage:"+stage {
			t.Errorf("%s: #%d: labels %q, newest comment %q; want %s", when, n, l, newestComment(f, n), stage)
		}
	}
}

// The race: J's /resume, then A's /hold, in one poll, J and K held by J's
// own /hold. In every order of their numbers, J and K stay held by A's
// /hold, as A does, and A's later /resume releases all three: with A's
// lower than J's, J's /resume, handled after A's /hold, ends J's own
// /hold on J and K, which it records, so A's /resume, which never ends a
// /hold posted below it, needn't; with J's lower, J's /resume takes J and
// K out of held first, and A's /hold holds them with its markers.
func TestHoldResumeRaceAncestorHoldAfter(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, own := raceFamily(t, order, true)
			resume := f.AddComment(j, "owner", "/resume")
			hold := f.AddComment(a, "owner", "/hold")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after the race", map[int]string{a: "held", j: "held", k: "held"})
			for _, n := range []int{j, k} {
				ended := anyComment(f, n, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", own, resume))
				if a < j && !ended {
					t.Errorf("#%d: newest comment %q; want the record that J's /resume ended J's /hold", n, newestComment(f, n))
				}
				if !anyComment(f, n, fmt.Sprintf("reason=owner cmd=%d ", hold)) && !anyComment(f, n, fmt.Sprintf("held=%d heldon=%d ", hold, a)) {
					t.Errorf("#%d: newest comment %q; want a marker or a record of A's /hold", n, newestComment(f, n))
				}
			}
			if body := newestComment(f, j); strings.Contains(body, "rejected") {
				t.Errorf("#%d: newest comment %q; want J's /resume applied", j, body)
			}
			f.AddComment(a, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
		})
	}
}

// The race with A held already, by hand, so A's /hold writes no marker
// anywhere: J and K still stay held by it, until A's /resume, which ends
// the hold by hand on A too.
func TestHoldResumeRaceAncestorHeldAlready(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, true)
			f.RemoveLabel(a, "owner", "stage:ready")
			f.AddLabel(a, "owner", "stage:held")
			resolveSettle(t, f, 1, 2, 3)
			f.AddComment(j, "owner", "/resume")
			f.AddComment(a, "owner", "/hold")
			resolveSettle(t, f, 1, 2, 3)
			// With J's number lower, J's /resume comes first: J and K leave
			// held, and A's /hold, as A is held already, writes markers on
			// them alone.
			raceStages(t, f, "after the race", map[int]string{a: "held", j: "held", k: "held"})
			f.AddComment(a, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
		})
	}
}

// The reverse order, A's /hold, then J's /resume. A's /hold is handled
// first, and J's /resume, posted after it, ends it on J and K, so both
// leave held, and A stays held by its own /hold. With J's number lower,
// J's /resume waits for A's /hold, posted before it (#243), so the race
// the other way, where A's /hold, handled after it, held J and K again
// with its markers, is gone too, and neither gets one.
func TestHoldResumeRaceAncestorHoldBefore(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, true)
			hold := f.AddComment(a, "owner", "/hold")
			f.AddComment(j, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after both", map[int]string{a: "held", j: "ready", k: "ready"})
			for _, n := range []int{j, k} {
				if anyComment(f, n, fmt.Sprintf("reason=owner cmd=%d ", hold)) {
					t.Errorf("#%d: newest comment %q; want no marker of A's /hold", n, newestComment(f, n))
				}
			}
			if anyComment(f, j, "rejected") {
				t.Errorf("#%d: a rejection; want J's /resume applied", j)
			}
		})
	}
}

// The race with J and K not held first: A's /hold, handled first, holds
// all three with its markers, and J's /resume, posted before it, ends
// nothing, so it's rejected, naming A, where the holds were made. With
// J's lower, J's /resume, handled first, finds nothing held, and A's /hold
// then holds all three, as before #205.
func TestHoldResumeRaceNothingHeld(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, false)
			resume := f.AddComment(j, "owner", "/resume")
			f.AddComment(a, "owner", "/hold")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after the race", map[int]string{a: "held", j: "held", k: "held"})
			reason := "nothing here or in its open sub-issues is on an owner hold"
			if a < j {
				reason = fmt.Sprintf("the holds below were made on #%d, by `/hold` or by hand, so `/resume` there ends them", a)
			}
			if !anyComment(f, j, "I didn't apply this command: "+reason+".") || !anyComment(f, j, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
				t.Errorf("#%d: newest comment %q; want J's /resume rejected: %s", j, newestComment(f, j), reason)
			}
			f.AddComment(a, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
		})
	}
}

// raceCounting is discTrust, counting who DiscAccess is asked about.
type raceCounting struct {
	discTrust
	mu    sync.Mutex
	asked map[string]int
}

func (c *raceCounting) DiscAccess(ctx context.Context, login string) (bool, bool, error) {
	c.mu.Lock()
	c.asked[login]++
	c.mu.Unlock()
	return c.discTrust.DiscAccess(ctx, login)
}

// racePass is resolvePass with trust tr: step 1 on issue n with the hold
// commands, from a fresh client.
func racePass(t testing.TB, f *githubtest.Fake, n int, tr github.Trust) {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}}, Trust: tr,
		Commands: daemon.Commands(tr), Reactions: daemon.ResolveByReaction(tr)}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// The race with J untyped, so it has no row back out of held, and held by
// hand: J's /resume, handled after A's /hold, isn't rejected for that row,
// as J stays held by A's /hold anyway; it applies, and records the end of
// the hold by hand.
func TestHoldResumeRaceNoRowBack(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1, A
	file(f, 1, ready) // #2, J
	f.SetType(2, "")
	f.RemoveLabel(2, "owner", "stage:ready")
	f.AddLabel(2, "owner", "stage:held")
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "J held by hand", map[int]string{1: "ready", 2: "held"})
	resume := f.AddComment(2, "owner", "/resume")
	f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "after the race", map[int]string{1: "held", 2: "held"})
	if anyComment(f, 2, "rejected") {
		t.Errorf("#2: newest comment %q; want J's /resume applied", newestComment(f, 2))
	}
	if !anyComment(f, 2, fmt.Sprintf(" cmd=%d -->", resume)) || !anyComment(f, 2, "but it stays on hold by a `/hold` posted after it") {
		t.Errorf("#2: newest comment %q; want the record that J's /resume ended the hold by hand", newestComment(f, 2))
	}
}

// J's /resume reads, on the issues above J, only the /holds posted after
// it: those before it, and the comments deleted there that may have been
// /holds, it ends on J, so it doesn't read them. A, #1, is held by hand,
// so neither J's /hold, nor A's /hold after it, writes a marker on A, nor
// the latter on J, held by its own already; then mallory deletes the
// owner's note on A. J's pass on its /resume takes J back to ready,
// asking about mallory never, and for the reactions on A's /hold only
// once: to see that the App handled it, as J's /resume, posted after it,
// would otherwise wait for it (#243), not for whether J stays held.
func TestHoldResumeRaceOwnReadsOnlyLater(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1, A
	file(f, 1, ready) // #2, J
	f.RemoveLabel(1, "owner", "stage:ready")
	f.AddLabel(1, "owner", "stage:held")
	resolveSettle(t, f, 1, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 1, 2)
	early := f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1, 2)
	note := f.AddComment(1, "owner", "A note.")
	f.DeleteCommentBy("mallory", note)
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "before J's /resume", map[int]string{1: "held", 2: "held"})
	f.AddComment(2, "owner", "/resume")
	from := len(f.Requests())
	tr := &raceCounting{asked: map[string]int{}}
	racePass(t, f, 2, tr)
	raceStages(t, f, "after J's /resume", map[int]string{1: "held", 2: "ready"})
	var reads []githubtest.Request
	for _, r := range f.Requests()[from:] {
		if r.Path == fmt.Sprintf("/issues/comments/%d/reactions", early) {
			reads = append(reads, r)
		}
	}
	if len(reads) != 1 {
		t.Errorf("J's pass sent %+v; want one read of the /hold before J's /resume, whether the App handled it, but none of whether it holds J, as J's /resume ends it on J anyway", reads)
	}
	if n := tr.asked["mallory"]; n != 0 {
		t.Errorf("J's pass asked about mallory %d times; want none, as J's /resume ends her deletion's hold on J anyway", n)
	}
}

// The race with mallory's comment on J posted after J's /resume, which
// holds J for discussion too: J's /resume, handled after A's /hold, ends
// J's own /hold, and records that, with the end of the discussion before
// it, but J stays held by A's /hold and for mallory's comment, which its
// record says. A crash at any write leaves the same result.
func TestHoldResumeRaceDiscussedReplay(t *testing.T) {
	t.Parallel()
	var own, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, _, _, _, own = raceFamily(t, raceOrders[0], true) // A #1, J #2, K #3
		resume = f.AddComment(2, "owner", "/resume")
		f.AddComment(2, "mallory", "Wait.")
		f.AddComment(1, "owner", "/hold")
		resolveSettle(t, f, 1)
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	raceStages(t, f, "after J's pass", map[int]string{1: "held", 2: "held", 3: "held"})
	want := "This `/resume` ended the holds on this issue from before it, but it stays on hold by a `/hold` posted after it, which it doesn't end. " +
		"`/resume` where that `/hold` was posted, or here, ends that. It's on hold for discussion too: a write collaborator acted after the `/resume` was posted."
	if !anyComment(f, 2, want) || !anyComment(f, 2, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", own, resume)) {
		t.Errorf("#2: comments %q; want the record of J's /resume, saying what keeps it held", f.State().Issues[1].Comments)
	}
}

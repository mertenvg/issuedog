package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An owner's /hold on an issue held for discussion counters nothing and
// writes no marker (SPEC §6.4), but its comment is an owner hold of its own: the
// issue stays held once the discussion is resolved, with the record, and
// its /resume sends it to clarifying.
func TestResolveHoldOnDiscussionHold(t *testing.T) {
	t.Parallel()
	f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {})(t)
	hold := f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State().Issues[0]
	if !slices.Equal(st.Labels, []string{"stage:held"}) || st.Comments[len(st.Comments)-1].Body != "/hold" {
		t.Fatalf("labels %q, newest comment %q; want the issue held, and nothing written but the /hold's 👍", st.Labels, newestComment(f, 1))
	}
	f.AddReaction(1002, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "issuedog:ended reason=discussion") {
		t.Fatalf("labels %q, newest comment %q; want the issue held by the /hold in %d, with the record", l, body, hold)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume to clarifying", l, body)
	}
}

// A /hold posted while the issue was held, which a /resume since ended,
// holds nothing in a later hold: a new discussion, once resolved, takes the
// issue to clarifying.
func TestResolveEndedHoldCountsNoMore(t *testing.T) {
	t.Parallel()
	f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {})(t)
	f.AddComment(1, "owner", "/hold")
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Fatalf("labels %q, newest comment %q; want the /resume back to ready", l, newestComment(f, 1))
	}
	c := f.AddComment(1, "mallory", "Wait again.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want clarifying, the old /hold ended", l, newestComment(f, 1))
	}
}

// A child's own /hold that wrote no marker counts, with a discussion hold
// on the child or not, and ends only by /resume on the child; and a /hold
// counts by the App's record that it applied it, whatever a write
// collaborator made of its comment since (SPEC §6.4).

// resolveOwnHoldUnder is #1 at ready with #2 under it, both held by the
// owner's /hold on #1, then mallory's comment on #2, which holds nothing
// more but is a trigger (SPEC §6.4), and the owner's /hold on #2, which writes
// no marker, the first hold winning, by passes. It returns the fake and
// mallory's comment's ID.
func resolveOwnHoldUnder(t testing.TB) (*githubtest.Fake, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 1, ready)
	f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	w := f.AddComment(2, "mallory", "Wait.")
	resolveSteps(t, f, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2)
	for n := 1; n <= 2; n++ {
		if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Fatalf("#%d: labels %q; want it held", n, l)
		}
	}
	return f, w
}

// The parent's /resume ends the hold its /hold made on the child, but not
// the child's own /hold, which wrote no marker (SPEC §6.4) and was posted below
// it (#188's decision, round 3), so the child stays held by both, and the
// parent's move names it for each. The child records that the parent's
// /hold ended, so it stays ended (SPEC §6.4). Once the owner's 👍
// resolves mallory's comment, the child stays held by its own /hold, and
// the record says so; its own /resume then sends it to clarifying.
func TestResolveParentResumeLeavesOwnHoldUnderDiscussion(t *testing.T) {
	t.Parallel()
	var w int64
	setup := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, w = resolveOwnHoldUnder(t)
		f.AddComment(1, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, "Still on hold: #2, by a hold this `/resume` doesn't end") || !strings.Contains(body, "Still on hold for discussion: #2.") {
		t.Errorf("#1: labels %q, newest comment %q; want it back at ready, naming #2 as still on hold, by an owner hold and for discussion", l, body)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) ||
		strings.Count(body, "<!-- issuedog:ended reason=owner key=c") != 1 || !strings.Contains(body, "It's on hold for discussion too.") {
		t.Fatalf("#2: labels %q, newest comment %q; want it held, with the record that the parent's /hold ended, and only that", l, body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 2)
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Fatalf("#2: labels %q, newest comment %q; want it held by its own /hold once mallory's comment is resolved, with the record that says so", l, body)
	}
	f.AddComment(2, "owner", "/resume")
	resolveSteps(t, f, 2)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want its own /resume to send it to clarifying", l, newestComment(f, 2))
	}
}

// Without the parent's /resume, the child's own /hold keeps it held once
// mallory's comment is resolved, and the record says an owner hold stays.
func TestResolveOwnHoldUnderDiscussionStays(t *testing.T) {
	t.Parallel()
	f, w := resolveOwnHoldUnder(t)
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 2)
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("#2: labels %q, newest comment %q; want it held, with the record that an owner hold stays", l, body)
	}
}

// The issue's own /hold posted between the transition before its hold and
// the hold's marker counts: posted before mallory's comment was handled,
// it's applied in the same pass, after the discussion hold, with no
// marker, so once her comment is resolved the issue stays held for it.
func TestResolveOwnHoldBeforeHoldStart(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		w := f.AddComment(1, "mallory", "Wait.")
		f.AddComment(1, "owner", "/hold")
		resolveSteps(t, f, 1)
		if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || strings.Contains(body, "reason=owner") {
			t.Fatalf("labels %q, newest comment %q; want it held for discussion, the /hold with no marker", l, body)
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("labels %q, newest comment %q; want it held by its own /hold", l, body)
	}
}

// resolveOwnOnDiscussion is #2 at ready and #1 under it, held for
// mallory's comment w, then the owner's /hold on #1, which writes no
// marker (SPEC §6.4), by passes. It returns the fake, w and the /hold.
func resolveOwnOnDiscussion(t testing.TB) (*githubtest.Fake, int64, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, ready) // #2
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	if body := newestComment(f, 1); strings.Contains(body, fmt.Sprintf("cmd=%d", hold)) {
		t.Fatalf("#1: newest comment %q; want no marker of the /hold", body)
	}
	return f, w, hold
}

// #1's own /hold sits on its discussion hold alone, so #2's /resume, on
// an issue that isn't held, ends nothing and is rejected, naming #1. Once
// the owner's 👍 resolves mallory's comment, #1 stays held for its /hold,
// until its own /resume, which sends it to clarifying.
func TestResolveOwnHoldOnDiscussionStays(t *testing.T) {
	t.Parallel()
	var w, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, w, _ = resolveOwnOnDiscussion(t)
		resume = f.AddComment(2, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	if !resolveRejectedWith(f, 2, resume, madeOn("#1")) {
		t.Errorf("#2: newest comment %q; want the /resume rejected, naming #1", newestComment(f, 2))
	}
	if body := newestComment(f, 1); strings.Contains(body, "issuedog:ended") {
		t.Errorf("#1: newest comment %q; want no record that a hold ended", body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("#1: newest comment %q; want the record that its owner hold stays", body)
	}
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after #1's /resume", "clarifying", "ready")
}

// Three levels: #2's own /hold sits on its discussion hold alone, and
// writes a marker on #3, which wasn't held. #1's /resume ends it on
// neither: it's rejected, naming #2, and once mallory's comment is
// resolved, #2 and #3 both stay held, as the /hold on #2 holds them.
func TestResolveOwnHoldOnDiscussionThreeLevels(t *testing.T) {
	t.Parallel()
	var w, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)  // #1, at the top
		file(f, 1, ready) // #2, under #1
		file(f, 2, ready) // #3, under #2
		w = f.AddComment(2, "mallory", "Wait.")
		resolveSteps(t, f, 2)
		f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2)
		resume = f.AddComment(1, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if !resolveRejectedWith(f, 1, resume, madeOn("#2")) {
		t.Errorf("#1: newest comment %q; want the /resume rejected, naming #2", newestComment(f, 1))
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 1, 2, 3)
	resolveLabels(t, f, "after the 👍", "ready", "held", "held")
}

// #1, held for mallory's comment, then by the owner's /hold on #2, which
// writes a marker on #2 but none on #1, then by its own /hold, which
// writes none either. #2's /resume ends #2's /hold on #1 (#2's is its own
// to end, so #1 never counts it), but not #1's own /hold, posted below it
// (#188's decision, round 3), so #1 stays held, with the record that
// #2's /hold ended there, and only that, so it stays ended (SPEC §6.4);
// once the owner's 👍 resolves mallory's comment, #1 stays held by its own
// /hold, until its own /resume sends it to clarifying.
func TestResolveOwnHoldUnderParentHoldWithoutMarker(t *testing.T) {
	t.Parallel()
	var w, parent int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		f.SetParent(1, 2)
		w = f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		parent = f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		f.AddComment(1, "owner", "/hold")
		resolveSteps(t, f, 1)
		f.AddComment(2, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	resolveLabels(t, f, "after #2's /resume", "held", "ready")
	if body := newestComment(f, 1); strings.Count(body, "issuedog:ended") != 1 || !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=", parent)) {
		t.Errorf("#1: newest comment %q; want the record that #2's /hold ended, and only that", body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("#1: newest comment %q; want the record that its own /hold stays", body)
	}
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after #1's /resume", "clarifying", "ready")
}

// #1's own /hold, posted while it was held for mallory's comment, comes
// before the owner's /hold on #2, which writes no marker on #1: #2's
// /resume ends #2's hold, and #1 stays held by its own /hold once
// mallory's comment is resolved, as it was posted below #2.
func TestResolveOwnHoldBeforeParentHold(t *testing.T) {
	t.Parallel()
	f, w, _ := resolveOwnOnDiscussion(t)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after #2's /hold", "held", "held")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "ready")
}

// #3 > #2 > #1. The owner's /hold on #3 is posted before the one on #2,
// but #2's pass comes first, so #2's /hold moves #2 and #1 into held, its
// marker on both, and #3's then writes none on them. #2's /hold was posted
// below #3, so #3's /resume ends #3's hold alone, and #2 and #1 stay held
// by #2's.
func TestResolveMarkedHoldNotStacked(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(3, "owner", "/hold")
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveSettle(t, f, 3)
	resolveLabels(t, f, "after the /holds", "held", "held", "held")
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
}

// #3 > #2 > #1. An earlier hold on #2, by #3's /hold, ended with #3's
// /resume; then mallory's comment held #2, and #2's own /hold, which
// writes no marker on #2, held for discussion, and writes a marker on #1.
// A later /hold and /resume on #3 end neither, on #2 or on #1, as #2's
// /hold was posted below #3.
func TestResolveOwnHoldNotStackedOnEarlierHold(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3)
	resolveLabels(t, f, "after #3's first /resume", "ready", "ready", "ready")
	w := f.AddComment(2, "mallory", "Wait.")
	resolveSettle(t, f, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "at the end", "held", "held", "ready")
}

// #3 > #2 > #1, #1 held for mallory's comment throughout. #3's /hold,
// which writes no marker on #2, held for discussion then, nor on #1, held
// #2 in a hold that #2's /resume ended. Then mallory's comment held #2
// again, and #2's own /hold, which writes no marker, was posted below #3,
// so #3's /resume doesn't end it, on #2 or on #1, and both stay held by it
// once the discussions are resolved.
func TestResolveOwnHoldNotStackedOnEndedHoldAbove(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w1 := f.AddComment(1, "mallory", "Wait 1.")
	f.AddComment(2, "mallory", "Wait 2.")
	resolveSettle(t, f, 1, 2)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "held", "ready", "held")
	w3 := f.AddComment(2, "mallory", "Wait 3.")
	resolveSettle(t, f, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	f.AddReaction(w1, "owner", "+1")
	f.AddReaction(w3, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "at the end", "held", "held", "ready")
}

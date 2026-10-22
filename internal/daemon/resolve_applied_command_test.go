package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #188's verify, attempt 5, and the class it belongs to: a command on the
// issue or an ancestor counts toward a hold only once the App applied it.
// A rejected /resume ends nothing (SPEC §9.2), nor does a rejected /hold
// hold anything, and an owner hold ends only by a /resume that applies
// (SPEC §6.4).

// A /resume counters no comment, even one rejected, as for a bad
// argument: mallory's comment stays open, and the issue held.
func TestResolveRejectedResumeCountersNothing(t *testing.T) {
	t.Parallel()
	f := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/resume now") }), resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "reason=rejected") {
		t.Errorf("labels %q, newest comment %q; want the issue held, the /resume rejected", l, body)
	}
}

// A /hold rejected for a bad argument holds nothing: once the discussion
// is resolved, the issue goes to clarifying.
func TestResolveRejectedHoldHoldsNothing(t *testing.T) {
	t.Parallel()
	f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {})(t)
	f.AddComment(1, "owner", "/hold now")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(1002, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want clarifying, the rejected /hold no hold", l, newestComment(f, 1))
	}
}

// resolveRejectedResume is #1 at ready under #2 at ready, then the owner's
// /hold on #2, the owner's /resume on #1, while it isn't held, and
// mallory's comment on #1, by one pass over #1, then #2, then #1 again: #1
// is held for discussion, its /resume waits for #2's /hold, posted before
// it (#243), and #2's /hold, the first hold being #1's own, writes no
// marker on #1 (SPEC §6.4); then #1's /resume is rejected, as mallory's comment
// came after it. It returns the fake, the /hold's ID, the /resume's and
// mallory's comment's.
func resolveRejectedResume(t testing.TB) (*githubtest.Fake, int64, int64, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, ready)
	f.SetParent(1, 2)
	hold := f.AddComment(2, "owner", "/hold")
	resume := f.AddComment(1, "owner", "/resume")
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1, 2, 1)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("#2: labels %q; want it held by its /hold", l)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("#1: labels %q; want it held for discussion", l)
	}
	if !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
		return strings.Contains(c.Body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", resume))
	}) {
		t.Fatalf("#1: comments %q; want the /resume rejected", closingComments(f, 1, 0))
	}
	return f, hold, resume, w
}

// A /resume on the issue that the App rejected doesn't end a /hold on its
// parent posted before it: once the owner's 👍 resolves mallory's comment,
// #1 stays held, its record naming #2's /hold, while #2 is still held by
// it. Without the rejected /resume, it's the same.
func TestResolveRejectedResumeKeepsParentHold(t *testing.T) {
	t.Parallel()
	var hold int64
	setup := func(t testing.TB) *githubtest.Fake {
		f, h, _, w := resolveRejectedResume(t)
		hold = h
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=discussion held=%d heldon=2 resolved=", hold)) {
		t.Errorf("#1: labels %q, newest comment %q; want it held, with the record naming #2's /hold %d", l, body, hold)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("#2: labels %q; want it still held", l)
	}
}

// The verifier's probe as it ran, through Iterate, with #1 visited before
// #2: with the rejected /resume, and without it, #1 stays held once the
// owner's 👍 resolves mallory's comment.
func TestResolveRejectedResumeThroughIterate(t *testing.T) {
	t.Parallel()
	for _, withResume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", withResume), func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "ready", "do it")
			fileAt(f, "ready", "do it")
			f.SetType(1, "task")
			f.SetType(2, "task")
			f.SetParent(1, 2)
			d := resolveDaemon(t, f, newOrigin(t))
			iterate(t, d)
			hold := f.AddComment(2, "owner", "/hold")
			if withResume {
				f.AddComment(1, "owner", "/resume")
			}
			w := f.AddComment(1, "mallory", "Wait.")
			for range 2 {
				iterate(t, d)
			}
			if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
				t.Fatalf("#2: labels %q; want it held", l)
			}
			f.AddReaction(w, "owner", "+1")
			for range 2 {
				iterate(t, d)
			}
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
				!strings.Contains(body, fmt.Sprintf("held=%d ", hold)) {
				t.Errorf("#1: labels %q, newest comment %q; want it held by #2's /hold", l, body)
			}
		})
	}
}

// Once #2's /resume ends its /hold, #1, held by it after the rejected
// /resume, goes to clarifying, as its discussion was resolved during the
// hold.
func TestResolveRejectedResumeParentResumes(t *testing.T) {
	t.Parallel()
	f, _, _, w := resolveRejectedResume(t)
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 1)
	f.AddComment(2, "owner", "/resume")
	resolveSteps(t, f, 2)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying once #2's /resume ends the /hold", l, newestComment(f, 1))
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("#2: labels %q; want it back at ready", l)
	}
}

// resolveResumeBeforeStart is #1 at ready under #2 at ready, then the
// owner's /hold on #2, mallory's comment on #1, the owner's /resume on #1
// and mallory's second comment on #1, by one pass over #1, then #2, then
// #1 again: #1 is held for the first comment, in an App comment above the
// /resume, which waits for #2's /hold, posted before it (#243); #2's /hold
// writes no marker on #1; then the /resume accepts the first comment, while
// the second keeps #1 held, so the /resume applies, with its record on #1.
// The /resume is between the transition before #1's hold and the hold's
// marker. It returns the fake and the second comment's ID.
func resolveResumeBeforeStart(t testing.TB) (*githubtest.Fake, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, ready)
	f.SetParent(1, 2)
	f.AddComment(2, "owner", "/hold")
	f.AddComment(1, "mallory", "Wait.")
	resume := f.AddComment(1, "owner", "/resume")
	late := f.AddComment(1, "mallory", "One more thing.")
	resolveSteps(t, f, 1, 2, 1)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("cmd=%d", resume)) {
		t.Fatalf("#1: labels %q, newest comment %q; want it held, with its /resume's record", l, body)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("#2: labels %q; want it held by its /hold", l)
	}
	return f, late
}

// An applied /resume on the issue counts from the transition before its
// hold, not from the hold's start: one applied in the pass that began the
// hold, below the hold's marker, ends a /hold on the parent posted before
// it, so once the late comment is resolved, #1 goes to clarifying.
func TestResolveAppliedResumeBeforeHoldStart(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f, late := resolveResumeBeforeStart(t)
		f.ForgetWrites()
		f.AddReaction(late, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) || strings.Contains(body, "held=") {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying, with no record of #2's /hold", l, body)
	}
}

// A /resume on a parent that the App rejected doesn't end a /hold on the
// grandparent for #1 either. #3 is #2's parent, and #2 is #1's; the
// owner's comment on #3, the owner's /resume on #2, and mallory's comment
// on #1, by one pass over #1, #2, then #3: #1 is held for discussion, so
// #2's /resume, which ends no hold, is rejected. Then the owner edits the
// comment on #3 into a /hold, so a /hold posted before the /resume is
// handled after it, as one posted as a /hold no longer is (#243), and
// #3's pass holds #3 and #2, but writes no marker on #1. Once the owner's
// 👍 resolves mallory's comment, #1 stays held, its record naming #3's
// /hold.
func TestResolveRejectedParentResumeKeepsHold(t *testing.T) {
	t.Parallel()
	var hold int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready)
		file(f, 0, ready)
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		hold = f.AddComment(3, "owner", "A note.")
		resume := f.AddComment(2, "owner", "/resume")
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1, 2, 3)
		f.EditComment(hold, "owner", "/hold")
		resolveSteps(t, f, 3)
		for n, want := range map[int]string{1: "stage:held", 2: "stage:held", 3: "stage:held"} {
			if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{want}) {
				t.Fatalf("#%d: labels %q; want %s", n, l, want)
			}
		}
		if !slices.ContainsFunc(f.State().Issues[1].Comments, func(c githubtest.CommentState) bool {
			return strings.Contains(c.Body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", resume))
		}) {
			t.Fatalf("#2: comments %q; want its /resume rejected", closingComments(f, 2, 0))
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("held=%d ", hold)) {
		t.Errorf("#1: labels %q, newest comment %q; want it held, with the record naming #3's /hold %d", l, body, hold)
	}
}

// A /hold on the parent that the App rejected holds nothing: #2's type is
// one the pipeline doesn't know, so it can't go on hold, and once the
// owner's 👍 resolves mallory's comment, #1 goes to clarifying.
func TestResolveRejectedParentHoldHoldsNothing(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready)
		f.SetType(2, "Weird")
		f.SetParent(1, 2)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		hold := f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2)
		if !slices.ContainsFunc(f.State().Issues[1].Comments, func(c githubtest.CommentState) bool {
			return strings.Contains(c.Body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", hold))
		}) {
			t.Fatalf("#2: comments %q; want its /hold rejected", closingComments(f, 2, 0))
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) || strings.Contains(body, "held=") {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying, as #2's /hold was rejected", l, body)
	}
}

// A /hold on the parent that the App hasn't handled yet, as on a parent
// visited after the issue in the pass, holds nothing yet: once the owner's
// 👍 resolves mallory's comment, #1 goes to clarifying, and the /hold,
// handled then, holds it again by a marker of its own.
func TestResolvePendingParentHold(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, ready)
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 1)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) || strings.Contains(body, "held=") {
		t.Fatalf("#1: labels %q, newest comment %q; want it at clarifying, as #2's /hold isn't handled yet", l, body)
	}
	resolveSteps(t, f, 2)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("from=clarifying to=held at=clarifying origin=ready reason=owner cmd=%d ", hold)) {
		t.Errorf("#1: labels %q, newest comment %q; want it held by #2's /hold, with its marker", l, body)
	}
}

// A /hold on the issue itself that the App rejected, while its type was
// one the pipeline doesn't know, holds nothing once a discussion holds
// it: once the owner's 👍 resolves mallory's comment, it goes to
// clarifying.
func TestResolveRejectedOwnHoldHoldsNothing(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		f.SetType(1, "Weird")
		hold := f.AddComment(1, "owner", "/hold")
		resolveSteps(t, f, 1)
		if !strings.Contains(newestComment(f, 1), fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", hold)) {
			t.Fatalf("newest comment %q; want the /hold rejected", newestComment(f, 1))
		}
		f.SetType(1, "task")
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Fatalf("labels %q; want it held for discussion", l)
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want it at clarifying, as its /hold was rejected", l, body)
	}
}

// An applied /resume on an issue between the issue and the one whose
// /hold holds it counts from the transition before the issue's hold too,
// not from the hold's start. #3 is #2's parent, and #2 is #1's; #2 is on a
// hold set by hand. The owner's /hold on #3, the owner's /resume on #2,
// and mallory's comment on #1, by one pass over #1, #2, #3, then #2 again:
// #1 is held for discussion, in an App comment above the /resume; #2's
// /resume waits for #3's /hold, posted before it (#243); #3's /hold writes
// no marker on #1, nor on #2; then #2's /resume ends #2's holds, and
// applies there.
// The /resume ended #3's /hold for #1, as it was posted after it, so once
// the owner's 👍 resolves mallory's comment, #1 goes to clarifying.
func TestResolveAppliedParentResumeBeforeHoldStart(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77")
		file(f, 0, ready)
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		f.AddComment(3, "owner", "/hold")
		resume = f.AddComment(2, "owner", "/resume")
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1, 2, 3, 2)
		if !slices.ContainsFunc(f.State().Issues[1].Comments, func(c githubtest.CommentState) bool {
			return strings.Contains(c.Body, fmt.Sprintf("from=held to=ready cmd=%d ", resume))
		}) {
			t.Fatalf("#2: comments %q; want its /resume's move back to ready", closingComments(f, 2, 0))
		}
		if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Fatalf("#1: labels %q; want it held for discussion", l)
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) || strings.Contains(body, "held=") {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying, as #2's /resume ended #3's /hold", l, body)
	}
}

// A /hold on a descendant posted after the /resume, that the App hasn't
// handled yet, isn't one the /resume judges: #2's /resume takes #1 back,
// and #1's /hold, handled next, holds it again with a marker of its own.
func TestResolveRound3OwnHoldAfterResumeUnhandled(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	f.SetParent(1, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	f.AddComment(2, "owner", "/resume")
	own := f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveLabels(t, f, "after #2's /resume", "ready", "ready")
	resolveSettle(t, f, 1)
	resolveLabels(t, f, "after #1's /hold", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("reason=owner cmd=%d", own)) {
		t.Errorf("#1: newest comment %q; want the marker of its own /hold", body)
	}
}

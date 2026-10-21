package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #188's verify, round 2, attempt 1: a record that a /resume ended an
// ancestor's /hold on the issue (resolveResumed) ends it for every
// judgement of the owner holds left (resolveLeft), so resolveAbove agrees
// with resolveHoldsLeft. Before, the leave path found no owner hold left,
// read the ancestors, counted the /hold again, as the /resume left no
// marker on its own issue, and kept the issue held, reading the ancestors
// again on every pass.

// resolveR2Quiet runs Iterate on f until it settles, then checks that an
// unchanged pass writes nothing and asks only conditional GETs answered
// 304: no ancestor is read again.
func resolveR2Quiet(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	d := resolveDaemon(t, f, newOrigin(t))
	for range 3 {
		iterate(t, d)
	}
	writes, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("the unchanged pass wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[reqs:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("the unchanged pass sent %s %s, %d; want only conditional GETs answered 304", r.Method, r.Path, r.Status)
		}
	}
}

// resolveR2Check runs setup's last pass on #1 under Replay, then on a
// fresh fake, where #1 goes to clarifying, as #2's /resume ended #3's
// /hold for it, and a second pass writes nothing; then the daemon's
// unchanged pass is quiet.
func resolveR2Check(t *testing.T, setup func(testing.TB) *githubtest.Fake) {
	t.Helper()
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.HasPrefix(body, "The discussion is resolved, by `owner`, so this issue leaves its hold from `ready` for `clarifying`.") {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying, as #2's /resume ended #3's /hold", l, body)
	}
	resolveR2Quiet(t, f)
}

// resolveR2Tail is the end of both ways in, on f, with #1 held, its first
// comment resolved, and its record naming #3's /hold: mallory comments on
// #1 again, the owner's /resume on #2 ends #3's /hold for #1, recorded on
// #1, and the owner 👍s the second comment, which setup leaves for the
// last pass.
func resolveR2Tail(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	w2 := f.AddComment(1, "mallory", "One more.")
	resolveSteps(t, f, 1)
	f.AddComment(2, "owner", "/resume")
	resolveSteps(t, f, 2)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "<!-- issuedog:ended reason=owner key=c") {
		t.Fatalf("#1: labels %q, newest comment %q; want it held, with the record that #2's /resume ended #3's /hold", l, body)
	}
	f.ForgetWrites()
	f.AddReaction(w2, "owner", "+1")
}

// #3 is #2's parent, and #2 is #1's. mallory's comment holds #1; the
// owner's /hold on #3 holds #3 and #2, with no marker on #1 (SPEC §6.4); a 👍
// resolves the comment, and #1's record names the /hold. The owner sets
// #2's stage by hand, which ends #2's hold, so #2's /resume later applies
// on #1 alone.
func TestResolveR2OverrideBetween(t *testing.T) {
	t.Parallel()
	resolveR2Check(t, func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready)
		file(f, 0, ready)
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		f.AddComment(3, "owner", "/hold")
		resolveSteps(t, f, 3)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 1)
		if body := newestComment(f, 1); !strings.Contains(body, "<!-- issuedog:ended reason=discussion held=") {
			t.Fatalf("#1: newest comment %q; want the record naming #3's /hold", body)
		}
		f.RemoveLabel(2, "owner", "stage:held")
		f.AddLabel(2, "owner", "stage:ready")
		resolveSteps(t, f, 2)
		if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:ready"}) {
			t.Fatalf("#2: labels %q; want the owner's ready", l)
		}
		resolveR2Tail(t, f)
		return f
	})
}

// The second way in: #2 is linked under #3 after #3's /hold was applied,
// so the /hold never held #2, and #1's record names it once its first
// comment is resolved.
func TestResolveR2Relink(t *testing.T) {
	t.Parallel()
	resolveR2Check(t, func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready)
		file(f, 0, ready)
		f.SetParent(1, 2)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		f.AddComment(3, "owner", "/hold")
		resolveSteps(t, f, 3)
		f.SetParent(2, 3)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 1)
		if body := newestComment(f, 1); !strings.Contains(body, "<!-- issuedog:ended reason=discussion held=") {
			t.Fatalf("#1: newest comment %q; want the record naming #3's /hold", body)
		}
		resolveR2Tail(t, f)
		return f
	})
}

// A hold a /resume ended stays ended (SPEC §6.4): #3 > #2 > #1. #3 held
// by hand, and #2 for mallory's comment, so the owner's /hold on #3 writes
// its marker on #1 alone; then the owner's /hold on #2, which writes none. #3's /resume ends #3's /hold on #1, but
// #1 stays held by #2's, so it records that the first one ended. Once the
// owner deletes #3's /hold, it would count as posted on #1, the farthest
// issue with its marker, which only /resume on #1 ends; the record keeps
// it ended, so #2's /resume takes #1 out of held, whether the deletion
// came before it or after.
func TestResolveRound3EndedHoldStaysEnded(t *testing.T) {
	t.Parallel()
	for _, deleteFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("deleted before #2's /resume %v", deleteFirst), func(t *testing.T) {
			f := discFake(t)  // #1
			file(f, 0, ready) // #2
			file(f, 0, ready) // #3
			f.SetParent(1, 2)
			f.SetParent(2, 3)
			f.RemoveLabel(3, "owner", "stage:ready")
			f.AddLabel(3, "owner", "stage:held")
			resolveSettle(t, f, 3)
			f.AddComment(2, "mallory", "Wait.")
			resolveSettle(t, f, 2)
			top := f.AddComment(3, "owner", "/hold")
			resolveSettle(t, f, 3, 2, 1)
			if !strings.Contains(newestComment(f, 1), fmt.Sprintf("cmd=%d", top)) || strings.Contains(newestComment(f, 2), fmt.Sprintf("cmd=%d", top)) {
				t.Fatalf("newest comments %q and %q; want #3's /hold's marker on #1 alone", newestComment(f, 1), newestComment(f, 2))
			}
			f.AddComment(2, "owner", "/hold")
			resolveSettle(t, f, 2, 1)
			resume := f.AddComment(3, "owner", "/resume")
			resolveSettle(t, f, 3, 2, 1)
			resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
			if !anyComment(f, 1, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", top, resume)) {
				t.Errorf("#1: newest comment %q; want the record that #3's /hold ended", newestComment(f, 1))
			}
			if deleteFirst {
				f.DeleteComment(top)
			}
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 3, 2, 1)
			if !deleteFirst {
				f.DeleteComment(top)
				resolveSettle(t, f, 3, 2, 1)
			}
			resolveLabels(t, f, "after #2's /resume", "ready", "ready", "ready")
		})
	}
}

// The same for a held= record (SPEC §6.4): #2 > #1, #2 held by hand and
// #1 for mallory's comment, so the owner's /hold on #2 writes no marker.
// Once the 👍 resolves mallory's comment, #1 stays held by that /hold,
// and the record says `/resume` there ends it, naming #2 as where it
// counts as posted (heldon=). It still does once the owner deletes the /hold, which no
// marker names: #2's /resume sends #1 to clarifying, whether the deletion
// came before it or after.
func TestResolveRound3HeldRecordAfterDeletion(t *testing.T) {
	t.Parallel()
	for _, deleteFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("deleted before the /resume %v", deleteFirst), func(t *testing.T) {
			f := discFake(t)  // #1
			file(f, 0, ready) // #2
			f.SetParent(1, 2)
			f.RemoveLabel(2, "owner", "stage:ready")
			f.AddLabel(2, "owner", "stage:held")
			resolveSettle(t, f, 2)
			w := f.AddComment(1, "mallory", "Wait.")
			resolveSettle(t, f, 1)
			hold := f.AddComment(2, "owner", "/hold")
			resolveSettle(t, f, 2, 1)
			f.AddReaction(w, "owner", "+1")
			resolveSettle(t, f, 2, 1)
			if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("held=%d heldon=2 ", hold)) || !strings.Contains(body, "`/resume` there, or here") {
				t.Fatalf("#1: newest comment %q; want the record naming #2's /hold, posted on #2", body)
			}
			if deleteFirst {
				f.DeleteComment(hold)
			}
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 2, 1)
			if !deleteFirst {
				f.DeleteComment(hold)
				resolveSettle(t, f, 2, 1)
			}
			resolveLabels(t, f, "after #2's /resume", "clarifying", "ready")
		})
	}
}

// #3 > #2 > #1, #3 held by hand and #1 for mallory's comment, so the
// owner's /hold on #3 writes its marker on #2 alone. Once the 👍 resolves
// mallory's comment, #1's record names that /hold as posted on #3. The
// owner deletes it: for #2, it now counts as posted on #2, the farthest
// issue with its marker, so #3's /resume leaves #2 held; but #1's record
// still says #3, so the same /resume sends #1 to clarifying, judged by the
// record alone, not again as posted on #2, which would leave #1 held for
// the /resume's move to name.
func TestResolveRound3HeldRecordMarkedBelow(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.RemoveLabel(3, "owner", "stage:ready")
	f.AddLabel(3, "owner", "stage:held")
	resolveSettle(t, f, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSettle(t, f, 1)
	hold := f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("held=%d heldon=3 ", hold)) {
		t.Fatalf("#1: newest comment %q; want the record naming #3's /hold, posted on #3", body)
	}
	f.DeleteComment(hold)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "clarifying", "held", "ready")
	if body := newestComment(f, 3); strings.Contains(body, "Still on hold") {
		t.Errorf("#3: newest comment %q; want #1 out of held by the /resume itself, named by none", body)
	}
}

package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file tests #221: a trusted reopen's hold, and the owner hold that
// discRestore records again, carry the origin of the clarifying after a
// hold (SPEC §5, §6.4), so the interviewer's unchanged, once the hold
// ends, goes back to where the issue was before its first discussion hold.

// holdOriginOld is an older move to a clarifying after a hold, whose
// origin is planning: only the latest marker's origin counts, so a hold
// that took this one would send the issue back to planning.
const holdOriginOld = "<!-- issuedog:transition from=held to=clarifying at=planning origin=planning event=h7 seen=7 -->"

// holdOriginAfter files issue 1 on f, the epic "Quotes" by owner, at
// spec-review by revision 1 of its body, after holdOriginOld. Then
// mallory's comment holds it for discussion, which the owner resolves as
// ivHoldEpic does, and a pass takes it to the clarifying after that hold,
// with origin=spec-review. The writes are forgotten.
func holdOriginAfter(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	n := f.AddIssue("owner", "Quotes", "body")
	f.AddLabel(n, "owner", "type:epic")
	f.AddLabel(n, app, "stage:spec-review")
	f.AddComment(n, app, holdOriginOld)
	f.AddComment(n, app, revision(1, "body"))
	m := f.AddComment(n, "mallory", ivHoldMallory)
	if err := resolvePass(t, n)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(m, "owner", "+1")
	f.AddComment(n, "eve", ivHoldEve)
	f.AddComment(n, "owner", ivHoldCounter)
	if err := resolvePass(t, n)(f); err != nil {
		t.Fatal(err)
	}
	if c := newestComment(f, n); !strings.Contains(c, "from=held to=clarifying at=spec-review origin=spec-review ") {
		t.Fatalf("newest comment %q; want the move to the clarifying after the hold", c)
	}
	f.ForgetWrites()
}

// holdOriginReopened is holdOriginAfter, then the owner's close, which the
// App judged, swapping stage:clarifying for stage:completed, then the
// owner's reopen, whose event it sets in event.
func holdOriginReopened(event *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		holdOriginAfter(t, f)
		f.CloseBy(1, "owner")
		f.AddLabel(1, app, "stage:completed")
		f.RemoveLabel(1, app, "stage:clarifying")
		*event = f.ReopenBy(1, "owner")
		return f
	}
}

// holdOriginRestored is holdOriginAfter, then the owner's /hold, which a
// pass applies, then mallory's edit of the App's comment holding it, which
// hides the hold. It sets hold to that comment's ID.
func holdOriginRestored(hold *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		holdOriginAfter(t, f)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		*hold = ivHoldFind(t, f, "to=held at=clarifying origin=spec-review reason=owner")
		f.ForgetWrites()
		f.EditComment(*hold, "mallory", "nothing here")
		return f
	}
}

// holdOriginBodies is the bodies of issue 1's comments on f.
func holdOriginBodies(f *githubtest.Fake) []string {
	var bodies []string
	for _, c := range f.State().Issues[0].Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// holdOriginUnchanged posts the owner's /resume on issue 1, after more,
// if set, then runs a pass with the real Interviewer, whose fake claude
// returns unchanged, and checks that issue 1 goes back to spec-review,
// its move carrying origin=spec-review.
func holdOriginUnchanged(t *testing.T, f *githubtest.Fake, more func(f *githubtest.Fake)) {
	t.Helper()
	if more != nil {
		more(f)
	}
	f.AddComment(1, "owner", "/resume")
	g := ivHoldNew(t, f, map[string]any{"outcome": "unchanged"})
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:spec-review", "type:epic"}) || g.runs() != 1 {
		t.Fatalf("labels %q after %d runs; want spec-review after one, by the interviewer's unchanged", l, g.runs())
	}
	ivHoldFields(t, ivHoldMove(t, newestComment(f, 1)), map[string]string{"from": "clarifying", "to": "spec-review", "origin": "spec-review"})
}

// holdOriginDiscussion is mallory's comment on held issue 1, then the
// owner's counter comment, which resolves it during the hold, so the
// /resume after it goes to clarifying by the hold's own at and origin
// (resolveFields). With ended, a pass records the discussion's end while
// the owner hold stays; without, an open trigger that only /resume
// accepts, as discRestore's, leaves the pass nothing to record.
func holdOriginDiscussion(t *testing.T, ended bool) func(f *githubtest.Fake) {
	return func(f *githubtest.Fake) {
		f.AddComment(1, "mallory", "One more thing.")
		f.AddComment(1, "owner", "Not now.")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		c := newestComment(f, 1)
		if recorded := strings.Contains(c, "<!-- issuedog:ended reason=discussion "); recorded != ended {
			t.Fatalf("newest comment %q; want the discussion's end recorded: %t", c, ended)
		}
	}
}

// A trusted reopen of an issue closed at the clarifying after a hold holds
// it at clarifying, with that clarifying's origin, from the latest marker,
// not an older one. The write is safe to replay, and an unchanged pass
// after it writes nothing and sends only conditional GETs answered 304.
func TestHoldOriginReopen(t *testing.T) {
	t.Parallel()
	var event int64
	setup := holdOriginReopened(&event)
	closingCheck(t, setup, func(f *githubtest.Fake) closingWant {
		seen := ivHoldFind(t, f, "issuedog:revision rev=1")
		held := fmt.Sprintf("`owner` reopened this issue, so it's on hold, at `clarifying`, where it was before it closed. `/resume` here puts it back there.\n\n"+
			"<!-- issuedog:transition from=closed to=held at=clarifying origin=spec-review reason=owner event=%d seen=%d -->", event, seen)
		return closingWant{labels: []string{"stage:held", "type:epic"}, comments: append(holdOriginBodies(f)[1:], held)}
	})
	f := setup(t)
	d := newDaemon(t, f, digestNotifyOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)
	if bad := discUnchanged(t, f, d); len(bad) != 0 {
		t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", bad)
	}
}

// Once the reopen's hold ends, the interviewer's unchanged takes the issue
// back to spec-review, where it was before its first discussion hold, not
// to clarifying: after a /resume alone, and after a discussion during the
// hold, which sends it to clarifying by the hold's origin.
func TestHoldOriginReopenUnchanged(t *testing.T) {
	t.Parallel()
	for name, discussion := range map[string]bool{"a /resume": false, "a /resume after a discussion": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var event int64
			f := holdOriginReopened(&event)(t)
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if c := newestComment(f, 1); !strings.Contains(c, "from=closed to=held at=clarifying origin=spec-review ") {
				t.Errorf("newest comment %q; want the reopen's hold, with origin", c)
			}
			var more func(f *githubtest.Fake)
			if discussion {
				more = holdOriginDiscussion(t, true)
			}
			holdOriginUnchanged(t, f, more)
		})
	}
}

// discRestore's hold of an issue held from the clarifying after a hold
// records the hidden owner hold again with the first hold's at and origin,
// the latest marker's, as its discussion hold has them. The write is safe
// to replay, and an unchanged pass after it writes nothing and sends only
// conditional GETs answered 304.
func TestHoldOriginRestore(t *testing.T) {
	t.Parallel()
	var hold int64
	setup := holdOriginRestored(&hold)
	discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
		key, seen := discEditKey(t, f, 1, hold), ivHoldFind(t, f, "issuedog:revision rev=1")
		what := fmt.Sprintf("`mallory` edited comment %d by `issuedog[bot]`", hold)
		return map[int]issueWant{1: {[]string{"stage:held", "type:epic"}, []string{discHeldAgain("clarifying", what,
			fmt.Sprintf("from=held to=held at=clarifying origin=spec-review reason=discussion event=%s seen=%d", key, seen),
			fmt.Sprintf("from=held to=held at=clarifying origin=spec-review reason=owner event=%s seen=%d", key, seen), key)}}}
	})
	f := setup(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	if c := newestComment(f, 1); !strings.Contains(c, "at=clarifying origin=spec-review reason=owner ") {
		t.Fatalf("newest comment %q; want the owner hold recorded again", c)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if bad := discUnchanged(t, f, d); len(bad) != 0 {
		t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", bad)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("the passes after the hold wrote %+v; want nothing", w)
	}
}

// Once discRestore's hold ends, the interviewer's unchanged takes the
// issue back to spec-review, not to clarifying: after a /resume alone,
// which takes the origin from the move before the hold the restore
// recorded again, and after a discussion during the hold, which sends it
// to clarifying by the hold's origin.
func TestHoldOriginRestoreUnchanged(t *testing.T) {
	t.Parallel()
	for name, discussion := range map[string]bool{"a /resume": false, "a /resume after a discussion": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var hold int64
			f := holdOriginRestored(&hold)(t)
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if c := newestComment(f, 1); !strings.Contains(c, "at=clarifying origin=spec-review reason=owner ") {
				t.Errorf("newest comment %q; want the owner hold recorded again, with origin", c)
			}
			var more func(f *githubtest.Fake)
			if discussion {
				more = holdOriginDiscussion(t, false)
			}
			holdOriginUnchanged(t, f, more)
		})
	}
}

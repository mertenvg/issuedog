package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveSelfResumed is the record /resume writes on its own issue when a
// trigger made after it keeps the issue held, resolved at its time.
func resolveSelfResumed(resolved int64, resume int64) string {
	return fmt.Sprintf("This `/resume` ended the holds on this issue from before it, but it stays on hold for discussion: "+
		"a write collaborator acted after the `/resume` was posted. A code owner's response to that, or another `/resume` here, ends it."+
		"\n\n<!-- issuedog:ended reason=discussion cmd=%d resolved=%d -->", resume, resolved)
}

// A /resume accepts only the triggers made before it (#188's verify,
// attempt 1): a rename, or a deletion or edit of the owner's comment,
// made after the owner's /resume but before the App's pass, gets its
// notice, and keeps the issue held, with the record that the /resume
// ended the rest. The next pass writes nothing, and once a code owner
// accepts the act, the issue leaves the hold.
func TestResolveResumeThenLaterAct(t *testing.T) {
	for name, act := range map[string]func(f *githubtest.Fake, x int64){
		"a rename":                      func(f *githubtest.Fake, _ int64) { f.RenameIssue(1, "mallory", "Renamed") },
		"a deletion of owner's comment": func(f *githubtest.Fake, x int64) { f.DeleteCommentBy("mallory", x) },
		"an edit of owner's comment":    func(f *githubtest.Fake, x int64) { f.EditComment(x, "mallory", "Don't ship it.") },
	} {
		t.Run(name, func(t *testing.T) {
			var resume int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)
				x := f.AddComment(1, "owner", "Ship it.") // 1002
				f.AddComment(1, "mallory", "Wait.")       // 1003
				if err := discPass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(newestComment(f, 1), "event=c1003 ") {
					t.Fatalf("newest comment %q; want the hold for comment 1003", newestComment(f, 1))
				}
				f.ForgetWrites()
				resume = f.AddComment(1, "owner", "/resume")
				act(f, x)
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			is := f.State().Issues[0]
			var added []string
			for _, c := range is.Comments[len(setup(t).State().Issues[0].Comments):] {
				added = append(added, c.Body)
			}
			record := resolveSelfResumed(resolveCommentAt(t, f, 1, resume), resume)
			if !slices.Equal(is.Labels, []string{"stage:held"}) || len(added) != 2 || !strings.Contains(added[0], "issuedog:notice key=") || added[1] != record {
				t.Fatalf("labels %q, new comments %q; want the issue held, the act's notice, then %q", is.Labels, added, record)
			}
			// The notice accepted, the issue leaves: no owner hold is left.
			ids := commentIDs(t, f, 1)
			f.AddReaction(ids[len(ids)-2], "owner", "+1")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
				t.Errorf("labels %q, newest comment %q; want the issue at clarifying once the act is accepted", l, newestComment(f, 1))
			}
		})
	}
}

// An edit made after the /resume stays open once known from its record
// alone, by the time its key gives: edited again, its notice still needs
// its own 👍, after the newer edit's.
func TestResolveResumeThenEditedTwice(t *testing.T) {
	f := discFake(t)
	x := f.AddComment(1, "owner", "Ship it.") // 1002
	f.AddComment(1, "mallory", "Wait.")       // 1003
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	f.EditComment(x, "mallory", "Don't ship it.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.EditComment(x, "mallory", "Never ship it.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	ids := commentIDs(t, f, 1)
	f.AddReaction(ids[len(ids)-1], "owner", "+1") // the newer edit's notice
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || len(discNoticeKeys(f, 1)) != 2 {
		t.Errorf("labels %q, notices %q; want the issue held for the older edit, with two notices", l, discNoticeKeys(f, 1))
	}
}

// A comment in the same window holds the issue again too, the trigger
// open after the /resume: it isn't countered by the /resume, which
// counters nothing.
func TestResolveResumeThenLaterComment(t *testing.T) {
	f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {})(t)
	resume := f.AddComment(1, "owner", "/resume")
	late := f.AddComment(1, "mallory", "One more thing.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		body != resolveSelfResumed(resolveCommentAt(t, f, 1, resume), resume) {
		t.Errorf("labels %q, newest comment %q; want the issue held for comment %d, with the record", l, body, late)
	}
}

// A /resume counters no comment, even one rejected, as for a bad
// argument: mallory's comment stays open, and the issue held.
func TestResolveRejectedResumeCountersNothing(t *testing.T) {
	f := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/resume now") }), resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "reason=rejected") {
		t.Errorf("labels %q, newest comment %q; want the issue held, the /resume rejected", l, body)
	}
}

// An owner's /hold on an issue held for discussion counters nothing and
// writes no marker (Q2b), but its comment is an owner hold of its own: the
// issue stays held once the discussion is resolved, with the record, and
// its /resume sends it to clarifying.
func TestResolveHoldOnDiscussionHold(t *testing.T) {
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

// A /hold rejected for a bad argument holds nothing: once the discussion
// is resolved, the issue goes to clarifying.
func TestResolveRejectedHoldHoldsNothing(t *testing.T) {
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

// A /hold posted while the issue was held, which a /resume since ended,
// holds nothing in a later hold: a new discussion, once resolved, takes the
// issue to clarifying.
func TestResolveEndedHoldCountsNoMore(t *testing.T) {
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

// A parent's /hold while the child is held for discussion writes no
// marker on the child (Q2b), but keeps it held once its discussion is
// resolved: the record names the parent's /hold, so no later pass reads
// the parent again. The parent's /resume then sends the child to
// clarifying.
func TestResolveParentHoldKeepsChild(t *testing.T) {
	var hold, c int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 1, ready)
		c = f.AddComment(2, "mallory", "Wait.")
		if err := resolvePass(t, 2)(f); err != nil {
			t.Fatal(err)
		}
		hold = f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || strings.Contains(newestComment(f, 2), fmt.Sprintf("cmd=%d", hold)) {
			t.Fatalf("labels of #1 %q, newest comment on #2 %q; want #1 held, and no marker of the /hold on #2", l, newestComment(f, 2))
		}
		f.ForgetWrites()
		f.AddReaction(c, "owner", "+1")
		return f
	}
	resolveCheck(t, setup, 2, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:held"}, []string{fmt.Sprintf("The discussion is resolved, but this issue stays on hold: "+
			"a `/hold` on an issue above it, in comment %d, hasn't ended. `/resume` there, or here, ends that, and then it goes to `clarifying`."+
			"\n\ncc @owner\n\n<!-- issuedog:ended reason=discussion held=%d resolved=%d -->", hold, hold, resolveReactionAt(t, f, c))}}
	})
	f := run(t, setup, resolvePass(t, 2))
	from := len(f.Requests())
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/issues/1") {
			t.Errorf("a later pass sent %+v; want the parent not read again", r)
		}
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("#2: labels %q, newest comment %q; want it sent to clarifying by #1's /resume", l, body)
	}
}

// A /resume on the parent posted before its /hold doesn't end that /hold:
// the child stays held once its discussion is resolved.
func TestResolveEarlierResumeAbove(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "held=") {
		t.Errorf("#2: labels %q, newest comment %q; want it held by #1's /hold, with the record", l, body)
	}
}

// A parent's /hold, then its /resume, both while the child is held for
// discussion: once the discussion is resolved, the child goes to
// clarifying, the parent's /hold ended.
func TestResolveParentHoldThenResume(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want clarifying", l, newestComment(f, 2))
	}
}

// A /hold on an issue between a grandparent and its grandchild, posted
// while both were held, keeps the grandchild held: the grandparent's
// /resume, which ends only its own /hold below, is rejected, naming that
// issue, as its /resume would end the rest.
func TestResolveHoldBetween(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.RemoveLabel(1, "owner", "stage:held")
	f.AddLabel(1, "owner", "stage:ready")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(2, "owner", "/hold")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want := final("/resume", "I didn't apply this command", madeOn("#2"), fmt.Sprint(resume))
	if body := newestComment(f, 1); body != want {
		t.Errorf("newest comment on #1 %q; want %q", body, want)
	}
	for i := 1; i < 3; i++ {
		if l := f.State().Issues[i].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Errorf("#%d: labels %q; want it still held", i+1, l)
		}
	}
}

// With the grandparent still held, its /resume applies, but the /hold
// between keeps the grandchild held, as it does the issue it's on.
func TestResolveHoldBetweenApplied(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(2, "owner", "/hold")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	for i, want := range []string{"stage:ready", "stage:held", "stage:held"} {
		if l := st.Issues[i].Labels; !slices.Equal(l, []string{want}) {
			t.Errorf("#%d: labels %q; want %s", i+1, l, want)
		}
	}
}

// resolveAtClarifying is resolveReview's story, its /approve, 1004, having
// countered mallory's comment, at the clarifying after the hold, origin
// spec-review, by a pass.
func resolveAtClarifying(t testing.TB) *githubtest.Fake {
	f := resolveReview(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "owner", "/approve") })(t)
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newestComment(f, 1), "to=clarifying at=spec-review origin=spec-review") {
		t.Fatalf("newest comment %q; want the move to clarifying after the hold", newestComment(f, 1))
	}
	return f
}

// resolveStillWaiting checks that issue 1 is at clarifying, its newest
// move carrying origin=spec-review, and the /approve in 1004 still waits,
// with no reaction.
func resolveStillWaiting(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	is := f.State().Issues[0]
	var moves []string
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:transition") {
			moves = append(moves, c.Body)
		}
	}
	if !slices.Equal(is.Labels, []string{"stage:clarifying"}) || !strings.Contains(moves[len(moves)-1], "origin=spec-review") {
		t.Errorf("labels %q, newest move %q; want clarifying, carrying origin=spec-review", is.Labels, moves[len(moves)-1])
	}
	if r := is.Comments[3].Reactions; is.Comments[3].Body != "/approve" || len(r) != 0 {
		t.Errorf("the /approve %q has reactions %q; want it still waiting", is.Comments[3].Body, r)
	}
}

// At the clarifying after a hold, a later hold of either kind carries the
// origin forward, and so does the /resume back there, so the waiting
// /approve keeps waiting (#188's verify, attempt 1): a discussion hold, an
// owner /hold, and a hold set by hand.
func TestResolveOriginCarried(t *testing.T) {
	t.Run("a discussion hold", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.AddComment(1, "mallory", "And another thing.")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
	t.Run("an owner /hold", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if body := newestComment(f, 1); !strings.Contains(body, "from=clarifying to=held at=clarifying origin=spec-review reason=owner") {
			t.Errorf("newest comment %q; want the /hold carrying origin=spec-review", body)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
	t.Run("a hold by hand", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.RemoveLabel(1, "owner", "stage:clarifying")
		f.AddLabel(1, "owner", "stage:held")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if body := newestComment(f, 1); !strings.Contains(body, "from=clarifying to=held at=clarifying origin=spec-review reason=owner") {
			t.Errorf("newest comment %q; want the hold by hand carrying origin=spec-review", body)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
}

// A /resume on a parent that isn't held, whose child is held only for a
// discussion with an open trigger, is rejected, naming the child: a
// /resume on an ancestor doesn't end a discussion hold.
func TestResolveResumeOnParentOfDiscussion(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready)
	f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want := final("/resume", "I didn't apply this command", "#2 on hold for discussion, which only a code owner's response there, "+
		"or `/resume` there, ends", fmt.Sprint(resume))
	if body := newestComment(f, 1); body != want || !slices.Equal(f.State().Issues[1].Labels, []string{"stage:held"}) {
		t.Errorf("newest comment on #1 %q, #2's labels %q; want %q, and #2 held", body, f.State().Issues[1].Labels, want)
	}
}

// A comment the owner countered, whose author isn't a writer, as trudy,
// who can only read, ended no discussion: a /resume in the same pass ends
// the owner's hold back to at, not to clarifying.
func TestResolveResumeAfterNonWriter(t *testing.T) {
	f := discFake(t)
	f.SetPermission("trudy", "read")
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "trudy", "Looks risky.")
	f.AddComment(1, "owner", "It's fine.")
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, body)
	}
}

// A discussion resolved on an issue whose type the pipeline doesn't know
// leaves nothing: the move to clarifying is a state error, reported once,
// and the issue stays held.
func TestResolveLeaveStateError(t *testing.T) {
	setup := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {
		f.SetType(1, "chore")
		f.AddReaction(1002, "owner", "+1")
	})
	f := run(t, setup, resolvePass(t, 1))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("labels %q; want the issue still held", l)
	}
	for _, c := range f.State().Issues[0].Comments {
		if strings.Contains(c.Body, "to=clarifying") {
			t.Errorf("comment %q; want no move to clarifying", c.Body)
		}
	}
}

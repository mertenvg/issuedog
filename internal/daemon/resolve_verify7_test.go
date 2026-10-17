package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #188's verify, round 2, attempt 2: a child's own /hold that wrote no
// marker (#87's Q2b) counts, with a discussion hold on the child or not,
// and ends only by /resume on the child (#188's decision, round 3); and a
// /hold counts by the App's record that it applied it, whatever a write
// collaborator made of its comment since.

// resolveOwnHoldUnder is #1 at ready with #2 under it, both held by the
// owner's /hold on #1, then mallory's comment on #2, which holds nothing
// more but is a trigger (Q2b), and the owner's /hold on #2, which writes
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
// the child's own /hold, which wrote no marker (Q2b) and was posted below
// it (#188's decision, round 3), so the child stays held by both, and the
// parent's move names it for each. The child records that the parent's
// /hold ended, so it stays ended (#188's verify, round 3, note 1). Once the owner's 👍 resolves mallory's
// comment, the child stays held by its own /hold, and the record says so;
// its own /resume then sends it to clarifying.
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

// resolveAncestorHoldTouched is #1 under #2 under #3, mallory's comment
// holding #1, then the owner's /hold on #3, which holds #3 and #2 with
// markers and puts none on #1 (Q2b), then touch, a write collaborator's
// act on that /hold's comment, and the owner's 👍 on mallory's comment,
// for the last pass over #1. It returns the fake and the /hold's ID.
func resolveAncestorHoldTouched(t testing.TB, touch func(f *githubtest.Fake, hold int64)) (*githubtest.Fake, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, ready)
	file(f, 0, ready)
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(3, "owner", "/hold")
	resolveSteps(t, f, 3)
	for n := 1; n <= 3; n++ {
		if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Fatalf("#%d: labels %q; want it held", n, l)
		}
	}
	touch(f, hold)
	resolveSteps(t, f, 3)
	f.ForgetWrites()
	f.AddReaction(w, "owner", "+1")
	return f, hold
}

// A write collaborator's edit or deletion of an ancestor's /hold doesn't
// release the child: the /hold counts by its marker on #2, the App's
// record that it applied it, so #1 stays held, its record naming it, as
// #2 and #3 do.
func TestResolveAncestorHoldTouched(t *testing.T) {
	t.Parallel()
	for name, touch := range map[string]func(f *githubtest.Fake, hold int64){
		"edited":  func(f *githubtest.Fake, hold int64) { f.EditComment(hold, "mallory", "Never mind.") },
		"deleted": func(f *githubtest.Fake, hold int64) { f.DeleteCommentBy("mallory", hold) },
	} {
		t.Run(name, func(t *testing.T) {
			var hold int64
			setup := func(t testing.TB) *githubtest.Fake {
				var f *githubtest.Fake
				f, hold = resolveAncestorHoldTouched(t, touch)
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
				!strings.Contains(body, fmt.Sprintf("held=%d ", hold)) {
				t.Errorf("#1: labels %q, newest comment %q; want it held, with the record naming #3's /hold %d", l, body, hold)
			}
		})
	}
}

// An ancestor's /hold that a write collaborator edited, with no marker of
// it on the path, as every issue it reached was held already, counts by
// the App's 👍 on it during the child's hold (resolveHoldWitnessed): #2,
// held by hand, is #1's parent, and #1 is held for mallory's comment.
func TestResolveAncestorHoldEditedNoMarker(t *testing.T) {
	t.Parallel()
	var hold int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77")
		f.SetParent(1, 2)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		hold = f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2)
		for n := 1; n <= 2; n++ {
			if body := newestComment(f, n); strings.Contains(body, fmt.Sprintf("cmd=%d", hold)) {
				t.Fatalf("#%d: newest comment %q; want no marker of the /hold", n, body)
			}
		}
		f.EditComment(hold, "mallory", "Never mind.")
		resolveSteps(t, f, 2)
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("held=%d ", hold)) {
		t.Errorf("#1: labels %q, newest comment %q; want it held, with the record naming #2's /hold %d", l, body, hold)
	}
}

// The issue's own /hold, applied with no marker as it was held for
// discussion already, still counts once a write collaborator edits it:
// the App 👍'd it during the hold, so once the owner accepts the edit and
// resolves mallory's comment, the issue stays held for it.
func TestResolveOwnHoldEdited(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		hold := f.AddComment(1, "owner", "/hold")
		resolveSteps(t, f, 1)
		f.EditComment(hold, "mallory", "Never mind.")
		resolveSteps(t, f, 1)
		ids := commentIDs(t, f, 1)
		if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=e") {
			t.Fatalf("newest comment %q; want the edit's notice", newestComment(f, 1))
		}
		f.ForgetWrites()
		f.AddReaction(ids[len(ids)-1], "owner", "+1")
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

// The issue's own /hold, deleted by a write collaborator during the hold,
// may have been applied, and GitHub doesn't say which comment went, so the
// deletion is accepted only by /resume (resolveHitsApp): a 👍 on its
// notice leaves the issue held.
func TestResolveOwnHoldDeleted(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	f.DeleteCommentBy("mallory", hold)
	resolveSteps(t, f, 1)
	ids := commentIDs(t, f, 1)
	if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=d") {
		t.Fatalf("newest comment %q; want the deletion's notice", newestComment(f, 1))
	}
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 1)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("labels %q, newest comment %q; want it held, as only /resume accepts the deletion", l, newestComment(f, 1))
	}
	f.AddComment(1, "owner", "/resume")
	resolveSteps(t, f, 1)
	if l := f.State().Issues[0].Labels; slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("labels %q, newest comment %q; want /resume to take it out of held", l, newestComment(f, 1))
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

// A trigger a marker records is resolved for sure, whoever made it now
// is (resolver.verified): mallory's rename on an issue under the owner's
// hold has its notice, and once she's no longer a writer, the owner's 👍
// on the notice still writes the record that the discussion ended, so the
// /resume after goes to clarifying. Through Iterate, whose trust asks the
// fake's permissions.
func TestResolveRecordedTriggerVerified(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "ready", "do it")
	f.SetType(1, "task")
	d := resolveDaemon(t, f, newOrigin(t))
	f.AddComment(1, "owner", "/hold")
	iterate(t, d)
	f.RenameIssue(1, "mallory", "Renamed")
	iterate(t, d)
	ids := commentIDs(t, f, 1)
	if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=") {
		t.Fatalf("newest comment %q; want the rename's notice", newestComment(f, 1))
	}
	f.SetPermission("mallory", "read")
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	iterate(t, d)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "<!-- issuedog:ended reason=discussion resolved=") {
		t.Fatalf("labels %q, newest comment %q; want it held, with the record that the discussion ended", l, body)
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want the /resume to go to clarifying", l, newestComment(f, 1))
	}
}

// The issue's own /hold posted between the transition before its hold and
// the hold's marker, then edited by a write collaborator, still counts:
// the App 👍'd it once the discussion hold began, in the same pass.
func TestResolveOwnHoldBeforeHoldStartEdited(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	w := f.AddComment(1, "mallory", "Wait.")
	hold := f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	f.EditComment(hold, "mallory", "Never mind.")
	resolveSteps(t, f, 1)
	ids := commentIDs(t, f, 1)
	if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=e") {
		t.Fatalf("newest comment %q; want the edit's notice", newestComment(f, 1))
	}
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 1)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("labels %q, newest comment %q; want it held by its own /hold", l, body)
	}
}

// An edited comment the App 👍'd that a marker names is no /hold: the
// issue's own /resume, which accepted the comment before it and recorded
// that a later one keeps the issue held, edited by a write collaborator
// since, holds nothing, so once the later comment and the edit are
// accepted, the issue goes to clarifying.
func TestResolveEditedResumeHoldsNothing(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "mallory", "Wait.")
	resume := f.AddComment(1, "owner", "/resume")
	late := f.AddComment(1, "mallory", "One more thing.")
	resolveSteps(t, f, 1)
	if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("cmd=%d", resume)) {
		t.Fatalf("newest comment %q; want the /resume's record", body)
	}
	f.EditComment(resume, "mallory", "Never mind.")
	resolveSteps(t, f, 1)
	ids := commentIDs(t, f, 1)
	if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=e") {
		t.Fatalf("newest comment %q; want the edit's notice", newestComment(f, 1))
	}
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	f.AddReaction(late, "owner", "+1")
	resolveSteps(t, f, 1)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want it at clarifying", l, newestComment(f, 1))
	}
}

// An edited comment the App 👍'd before the hold began, as an answer at
// clarifying, is no /hold: once mallory's comment and her edit of the
// owner's answer are accepted, the issue goes to clarifying.
func TestResolveEditedAnswerHoldsNothing(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	n := file(f, 0, "from=intake to=clarifying run=a")
	answer := f.AddComment(n, "owner", "1a")
	resolveSteps(t, f, n)
	if !slices.ContainsFunc(f.State().Issues[n-1].Comments, func(c githubtest.CommentState) bool {
		return c.Body == "1a" && slices.Contains(c.Reactions, slug+"[bot] +1")
	}) {
		t.Fatalf("#%d: comments %q; want the answer 👍'd", n, closingComments(f, n, 0))
	}
	w := f.AddComment(n, "mallory", "Wait.")
	resolveSteps(t, f, n)
	f.EditComment(answer, "mallory", "1b")
	resolveSteps(t, f, n)
	ids := commentIDs(t, f, n)
	if !strings.Contains(newestComment(f, n), "<!-- issuedog:notice key=e") {
		t.Fatalf("newest comment %q; want the edit's notice", newestComment(f, n))
	}
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, n)
	if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want it at clarifying", l, newestComment(f, n))
	}
}

// A /hold on the /resume's own issue, known on the path between by its
// marker there, is the /resume's to end (resolveHeldBy, heldStackEnds):
// #3's /hold holds #3 and #2 with markers, and #1, held for mallory's
// comment, with none. #3's /resume ends it on #1 too, which stays held for
// discussion, with the record that it ended (resolveResumed), so once
// mallory's comment is resolved, #1 goes to clarifying. With #3 and #2
// set to ready by hand first, it's the same: the /resume applies for #1
// alone, as #3, out of held, would have no marker of it.
func TestResolveResumeEndsOwnIssueHoldBelow(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			var hold, resume int64
			var w int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)
				file(f, 0, ready)
				file(f, 0, ready)
				f.SetParent(1, 2)
				f.SetParent(2, 3)
				w = f.AddComment(1, "mallory", "Wait.")
				resolveSteps(t, f, 1)
				hold = f.AddComment(3, "owner", "/hold")
				resolveSteps(t, f, 3)
				if override {
					for _, n := range []int{3, 2} {
						f.RemoveLabel(n, "owner", "stage:held")
						f.AddLabel(n, "owner", "stage:ready")
						resolveSteps(t, f, n)
					}
				}
				resume = f.AddComment(3, "owner", "/resume")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 3))
			f := run(t, setup, resolvePass(t, 3))
			if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:ready"}) {
				t.Fatalf("#2: labels %q; want it at ready", l)
			}
			if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", hold, resume)) {
				t.Fatalf("#1: newest comment %q; want the record that #3's /resume ended #3's /hold", body)
			}
			f.AddReaction(w, "owner", "+1")
			resolveSteps(t, f, 1)
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
				t.Errorf("#1: labels %q, newest comment %q; want it at clarifying", l, newestComment(f, 1))
			}
		})
	}
}

// A /hold known by its marker alone, its comment gone, counts only within
// the hold, after the transition before it: #2's /hold held #2 before #1
// was its child, the owner set #2's stage by hand, which ended it, and
// deleted it; #1 moved since, by hand, then was linked under #2, and held
// for mallory's comment. The /hold is older than #1's move, so once her
// comment is resolved, #1 goes to clarifying.
func TestResolveGoneHoldFromAnEarlierHold(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, ready)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2)
	f.RemoveLabel(2, "owner", "stage:held")
	f.AddLabel(2, "owner", "stage:ready")
	resolveSteps(t, f, 2)
	f.DeleteCommentBy("owner", hold)
	f.RemoveLabel(1, "owner", "stage:ready")
	f.AddLabel(1, "owner", "stage:in-progress")
	resolveSteps(t, f, 1)
	for n, want := range map[int]string{1: "stage:in-progress", 2: "stage:ready"} {
		if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{want}) {
			t.Fatalf("#%d: labels %q; want the owner's %s", n, l, want)
		}
	}
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 1)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#1: labels %q, newest comment %q; want it at clarifying", l, newestComment(f, 1))
	}
}

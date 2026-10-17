package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #188's verify, round 2, attempt 2: a parent's /resume ends a child's own
// /hold that wrote no marker (#87's Q2b), with a discussion hold on the
// child or not; and a /hold counts by the App's record that it applied
// it, whatever a write collaborator made of its comment since.

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

// The parent's /resume ends the hold its /hold made on the child, and the
// child's own /hold too, which wrote no marker (Q2b), so the child stays
// held only for mallory's comment, with the record of the /holds ended;
// once the owner's 👍 resolves it, the child goes to clarifying.
func TestResolveParentResumeEndsOwnHoldUnderDiscussion(t *testing.T) {
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
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("#1: labels %q; want it back at ready", l)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) ||
		strings.Count(body, "<!-- issuedog:ended reason=owner key=c") != 2 {
		t.Fatalf("#2: labels %q, newest comment %q; want it held for discussion only, with the record that both /holds ended", l, body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSteps(t, f, 2)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want it at clarifying once mallory's comment is resolved", l, newestComment(f, 2))
	}
}

// Without the parent's /resume, the child's own /hold keeps it held once
// mallory's comment is resolved, and the record says an owner hold stays.
func TestResolveOwnHoldUnderDiscussionStays(t *testing.T) {
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
// /resume after goes to clarifying.
func TestResolveRecordedTriggerVerified(t *testing.T) {
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	f.RenameIssue(1, "mallory", "Renamed")
	resolveSteps(t, f, 1)
	ids := commentIDs(t, f, 1)
	if !strings.Contains(newestComment(f, 1), "<!-- issuedog:notice key=") {
		t.Fatalf("newest comment %q; want the rename's notice", newestComment(f, 1))
	}
	f.SetPermission("mallory", "read")
	f.AddReaction(ids[len(ids)-1], "owner", "+1")
	resolveSteps(t, f, 1)
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, "<!-- issuedog:ended reason=discussion resolved=") {
		t.Fatalf("labels %q, newest comment %q; want it held, with the record that the discussion ended", l, body)
	}
	f.AddComment(1, "owner", "/resume")
	resolveSteps(t, f, 1)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, newest comment %q; want the /resume to go to clarifying", l, newestComment(f, 1))
	}
}

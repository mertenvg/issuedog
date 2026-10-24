package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveAncestorHoldTouched is #1 under #2 under #3, mallory's comment
// holding #1, then the owner's /hold on #3, which holds #3 and #2 with
// markers and puts none on #1 (SPEC §6.4), then touch, a write collaborator's
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

// An edited comment before the transition that came before the hold is no
// /hold of this hold, though the App 👍'd it during the hold: once
// mallory's comment is resolved, #1 goes to clarifying (resolveOwnHolds'
// bound).
func TestResolveEditedBeforeHoldIsNoHold(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	f.SetPermission("eve", "read")
	n := f.AddIssue("owner", "Issue", "body")
	f.SetType(n, "task")
	early := f.AddComment(n, "owner", "Noted.")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->")
	f.EditComment(early, "eve", "Noted, eve says.")
	w := f.AddComment(n, "mallory", "Wait.")
	resolveSteps(t, f, n)
	resolveLabels(t, f, "after mallory's comment", "held")
	f.AddReaction(early, app, "+1")
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, n)
	resolveLabels(t, f, "after the 👍", "clarifying")
}

// SPEC §6.4: #2 > #1. The owner's /priority on #2, which the App 👍s,
// before mallory's comment holds #1; then mallory edits it. That 👍 came
// before #1's hold, so the edited comment is no /hold of it, and once the
// owner's 👍 resolves mallory's comment, #1 goes to clarifying.
func TestResolveEditedBeforeHoldOnParentIsNoHold(t *testing.T) {
	t.Parallel()
	l := newResolveLoop(t, newFake(t))
	top := l.task(0)
	low := l.task(top)
	l.settle("start")
	p := l.f.AddComment(top, "owner", "/priority p1")
	l.settle("the /priority")
	w := l.f.AddComment(low, "mallory", "Wait.")
	l.settle("mallory's comment")
	l.f.EditComment(p, "mallory", "/hold")
	l.settle("mallory's edit")
	l.f.AddReaction(w, "owner", "+1")
	l.settle("the 👍")
	if got := l.f.State().Issues[low-1].Labels; len(got) != 1 || got[0] != "stage:clarifying" {
		t.Errorf("#%d: labels %q, newest comment %q; want it at clarifying", low, got, newestComment(l.f, low))
	}
}

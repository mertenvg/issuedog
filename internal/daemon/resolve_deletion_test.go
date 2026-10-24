package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A comment a write collaborator deletes on an ancestor during a hold may
// have been a /hold that wrote no marker, so it keeps the issue held
// until a /resume there, between or on the issue ends it; and a /resume
// above never ends an issue's own /hold that wrote no marker, whatever
// holds the issue was in when it was posted (SPEC §6.4).

// resolveGoneRecord is the start of the record that a comment deleted on
// #2 during the hold keeps #1 held, and its marker's start.
const (
	resolveGoneText = "The discussion is resolved, but this issue stays on hold: a write collaborator deleted a comment on #2 during this hold, " +
		"which may have been a `/hold`, so it counts as one that hasn't ended. `/resume` there, or here, ends that, and then it goes to `clarifying`."
	resolveGoneMarker = "<!-- issuedog:ended reason=discussion gone=2-"
)

// resolveGoneSetup is #2, held by the owner's hand or for mallory's
// comment, and #1 under it, held for mallory's comment w; then the owner's
// /hold on #2, which writes no marker on either, as both are held already
// (SPEC §6.4), and mallory's deletion of it, by passes; then the owner's 👍 on
// w, and, with same, the deletion only then, with no pass between. It
// returns the fake and the /hold's ID.
func resolveGoneSetup(byHand, same bool) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		if byHand {
			file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
		} else {
			file(f, 0, ready) // #2
			f.AddComment(2, "mallory", "Wait here too.")
		}
		f.SetParent(1, 2)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1, 2)
		hold := f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		for n := 1; n <= 2; n++ {
			if body := newestComment(f, n); strings.Contains(body, fmt.Sprintf("cmd=%d", hold)) {
				t.Fatalf("#%d: newest comment %q; want no marker of the /hold, as it was held already", n, body)
			}
		}
		f.DeleteCommentBy("mallory", hold)
		if !same {
			resolveSteps(t, f, 2, 1)
		}
		f.AddReaction(w, "owner", "+1")
		f.ForgetWrites()
		return f
	}
}

// A 👍 on mallory's comment on #1 resolves its discussion, but #1 stays
// held: the comment mallory deleted on #2 may have been the owner's /hold,
// which no marker records and no /resume ended, so the App records that
// and, from then on, writes nothing more. The owner's /resume on #2 ends
// it, and #1 goes to clarifying.
func TestResolveGoneAncestorHold(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		byHand, same bool
	}{
		{"a parent held by hand", true, false},
		{"a parent held by hand, the deletion and the 👍 in one window", true, true},
		{"a parent held for discussion too", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setup := resolveGoneSetup(tt.byHand, tt.same)
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			body := newestComment(f, 1)
			if !strings.HasPrefix(body, resolveGoneText+"\n\ncc @owner\n\n"+resolveGoneMarker) {
				t.Errorf("#1: newest comment %q; want the record that the deleted comment on #2 keeps it held", body)
			}
			// The record names what holds it, so a pass over #1 reads #2 no more.
			n := len(f.Requests())
			resolveSteps(t, f, 1)
			for _, r := range f.Requests()[n:] {
				if r.Path == "/issues/2" || strings.HasPrefix(r.Path, "/issues/2/") {
					t.Errorf("a pass over #1 after the record read #2: %+v", r)
				}
			}
			resolveSettle(t, f, 2, 1)
			resolveLabels(t, f, "after the 👍", "held", "held")
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 2, 1)
			resolveLabels(t, f, "after #2's /resume", "clarifying", "ready")
		})
	}
}

// resolveGoneBelow is #3 at ready, #2 under it held by the owner's hand,
// and #1 under #2 held for mallory's comment, which the owner's 👍
// resolves after mallory deleted the owner's /hold on #2, which wrote no
// marker, by passes: #1 stays held by the comment deleted on #2.
func resolveGoneBelow(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	file(f, 0, ready)                                                        // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	f.DeleteCommentBy("mallory", hold)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
		t.Fatalf("#1: newest comment %q; want the record of the comment deleted on #2", body)
	}
	return f
}

// A /resume above the issue where the comment was deleted doesn't end what
// it may have been, so #3's /resume, which ends nothing else, is rejected,
// naming #2, and #1 stays held.
func TestResolveGoneNotEndedFromAbove(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := resolveGoneBelow(t)
		f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	resume := commentIDs(t, f, 3)[len(commentIDs(t, f, 3))-2]
	if !resolveRejectedWith(f, 3, resume, madeOn("#2")) {
		t.Errorf("#3: newest comment %q; want the /resume rejected, naming #2", newestComment(f, 3))
	}
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
}

// While no record names the deleted comment yet, the same holds: #3's
// /resume, posted before the 👍's pass, leaves #1 held by it, and the
// record follows.
func TestResolveGoneNotEndedFromAboveUnrecorded(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	file(f, 0, ready)                                                        // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	f.DeleteCommentBy("mallory", hold)
	f.AddReaction(w, "owner", "+1")
	resume := f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	if !slices.ContainsFunc(f.State().Issues[2].Comments, func(c githubtest.CommentState) bool {
		return strings.HasSuffix(c.Body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", resume))
	}) {
		t.Errorf("#3: comments %q; want the /resume rejected", closingComments(f, 3, 0))
	}
	resolveLabels(t, f, "after the passes", "held", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
		t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
	}
}

// A /resume that ends a hold on a descendant left held by a comment deleted
// below it says so truly: the descendant is held by a deleted comment that
// may have been a /hold, which /resume there ends (SPEC §6.4). #3's /hold
// held all three; the owner's /hold on #2, which wrote no marker, mallory
// deleted while #1 was held for discussion; #3's /resume ends #3's hold
// everywhere, leaves #2 held for its own deletion, and #1 held by the
// deleted comment on #2.
func TestResolveGoneStillOnHoldText(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		file(f, 0, ready) // #3
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		f.AddComment(3, "owner", "/hold")
		resolveSteps(t, f, 3)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		hold := f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 2, 1)
		resume = f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	want := moved("Resumed by `/resume`, back to `ready`. Still on hold: #1, by a hold this `/resume` doesn't end: one set by hand, "+
		"a `/hold` below this issue, or a deleted comment that was, or may have been, a `/hold`. `/resume` there ends it. "+
		"Still on hold for discussion: #2.", fmt.Sprintf("from=held to=ready cmd=%d seen=1003", resume))
	if body := newestComment(f, 3); body != want {
		t.Errorf("#3: newest comment %q; want %q", body, want)
	}
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// resolveDeletedAbove is #4 > #3 > #2 > #1, mallory's comments holding
// #1 and #2, then the owner's /hold on #4, with markers on #4 and #3, and
// none on #2 or #1, held already, by passes; then mallory's deletion of
// it, and the owner's 👍s on her two comments; before them, with resume
// first, #3's /resume. #3 is between #4 and the others, so its /resume
// ends what the /hold, or the deleted comment, held below it, and #1 and
// #2 go to clarifying, whichever came first (SPEC §6.4);
// #4 stays held.
func TestResolveDeletedAboveEitherOrder(t *testing.T) {
	t.Parallel()
	for _, resumeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume first %v", resumeFirst), func(t *testing.T) {
			f := discFake(t)
			file(f, 0, ready) // #2
			file(f, 0, ready) // #3
			file(f, 0, ready) // #4
			f.SetParent(1, 2)
			f.SetParent(2, 3)
			f.SetParent(3, 4)
			w1 := f.AddComment(1, "mallory", "Wait 1.")
			w2 := f.AddComment(2, "mallory", "Wait 2.")
			resolveSettle(t, f, 1, 2)
			hold := f.AddComment(4, "owner", "/hold")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after the /hold", "held", "held", "held", "held")
			f.DeleteCommentBy("mallory", hold)
			resolveSettle(t, f, 4, 3, 2, 1)
			thumbs := func() {
				f.AddReaction(w1, "owner", "+1")
				f.AddReaction(w2, "owner", "+1")
				resolveSettle(t, f, 4, 3, 2, 1)
			}
			if resumeFirst {
				f.AddComment(3, "owner", "/resume")
				resolveSettle(t, f, 4, 3, 2, 1)
				thumbs()
			} else {
				thumbs()
				resolveLabels(t, f, "after the 👍s", "held", "held", "held", "held")
				f.AddComment(3, "owner", "/resume")
				resolveSettle(t, f, 4, 3, 2, 1)
			}
			resolveLabels(t, f, "at the end", "clarifying", "clarifying", "ready", "held")
		})
	}
}

// resolveGoneParent is #2, held by the owner's hand, and #1 under it, held
// for mallory's comment w, by a pass; then the owner's /hold on #2, which
// writes no marker on either, by passes. It returns the fake, w and the
// /hold.
func resolveGoneParent(t testing.TB) (*githubtest.Fake, int64, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	return f, w, hold
}

// Which /resume ends what a comment deleted on #2 may have been: one the
// App applied after the deletion, on #2 or on #1 itself, does; one posted
// before the deletion doesn't, as the deleted comment may have been a
// later /hold, failing closed.
func TestResolveGoneEndedBy(t *testing.T) {
	t.Parallel()
	t.Run("a /resume on #2 after the deletion, before the 👍", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.DeleteCommentBy("mallory", hold)
		f.AddComment(2, "owner", "/resume")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "ready")
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "ready")
	})
	t.Run("a /resume on #1 after the deletion, which a later comment kept held", func(t *testing.T) {
		f, _, hold := resolveGoneParent(t)
		f.DeleteCommentBy("mallory", hold)
		f.AddComment(1, "owner", "/resume")
		later := f.AddComment(1, "mallory", "And another thing.")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #1's /resume", "held", "held")
		f.AddReaction(later, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "held")
	})
	t.Run("a /resume on #2 posted before the deletion, applied after the record", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.AddComment(2, "owner", "/resume")
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 1)
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Fatalf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "held")
	})
	t.Run("a /resume on #2 before the deletion", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.AddComment(2, "owner", "/resume")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "ready")
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "held", "held")
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
	})
}

// Only a deletion that may have hidden a code owner's /hold during the
// hold counts: not one before the hold began, nor one of a comment by a
// user who isn't trusted. A comment by someone GitHub doesn't name, and
// two deleted on the same issue, do; the record names that issue once.
func TestResolveGoneWhich(t *testing.T) {
	t.Parallel()
	t.Run("deleted before the hold", func(t *testing.T) {
		f := discFake(t)
		file(f, 0, ready) // #2
		f.SetParent(1, 2)
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("mallory", note)
		resolveSettle(t, f, 2)
		f.AddReaction(commentIDs(t, f, 2)[len(commentIDs(t, f, 2))-1], "owner", "+1") // the hold's notice
		resolveSettle(t, f, 2)
		resolveLabels(t, f, "after the notice's 👍", "ready", "clarifying")
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSettle(t, f, 1)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "clarifying")
	})
	t.Run("an untrusted user's comment", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		f.SetPermission("eve", "read")
		aside := f.AddComment(2, "eve", "An aside.")
		f.DeleteCommentBy("mallory", aside)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "held", "held")
		if body := newestComment(f, 1); strings.Contains(body, resolveGoneMarker) || !strings.Contains(body, "a `/hold` on an issue above it") {
			t.Errorf("#1: newest comment %q; want it held by the /hold on #2 alone", body)
		}
	})
	t.Run("deleted by a user without write access", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		f.SetPermission("eve", "read")
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("eve", note)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		if body := newestComment(f, 1); strings.Contains(body, resolveGoneMarker) || !strings.Contains(body, "a `/hold` on an issue above it") {
			t.Errorf("#1: newest comment %q; want it held by the /hold on #2 alone", body)
		}
	})
	t.Run("a comment by someone GitHub doesn't name", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		ghost := f.AddComment(2, "", "Gone.")
		f.DeleteCommentBy("mallory", ghost)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
	})
	t.Run("two deleted on #2", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("mallory", hold)
		f.DeleteCommentBy("mallory", note)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		body := newestComment(f, 1)
		if !strings.HasPrefix(body, resolveGoneText+"\n\ncc @owner\n\n"+resolveGoneMarker) || strings.Count(body, ",2-") != 1 {
			t.Errorf("#1: newest comment %q; want the record naming #2 once, and both deletions", body)
		}
	})
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

// SPEC §6.4: a comment that mallory deletes on an ancestor during a hold
// may have been a /hold that wrote no marker, so it counts as one posted
// on that ancestor, and a /resume posted above it doesn't end it, as for
// any /hold: the issue below stays held, in either
// order, until a /resume on that ancestor, or below it, does.
func TestResolveGoneNotEndedFromAboveEitherOrder(t *testing.T) {
	t.Parallel()
	for _, thumbsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("thumbs first %v", thumbsFirst), func(t *testing.T) {
			l := newResolveLoop(t, newFake(t))
			top := l.task(0)
			mid := l.task(top)
			low := l.task(mid)
			issues := []int{top, mid, low}
			l.settle("start")
			w := l.f.AddComment(low, "mallory", "Wait.")
			l.settle("mallory's comment")
			l.f.AddComment(top, "owner", "/hold")
			l.settle("the /hold on the top")
			// The owner's comment on the middle issue, which mallory deletes
			// during the hold: GitHub doesn't say what it was.
			gone := l.f.AddComment(mid, "owner", "Looking into it.")
			l.settle("the owner's comment")
			l.delete(mid, gone)
			l.settle("mallory's deletion")
			thumbs := func() {
				l.f.AddReaction(w, "owner", "+1")
				l.settle("the 👍")
			}
			resume := func() {
				l.f.AddComment(top, "owner", "/resume")
				l.settle("the /resume on the top")
			}
			if thumbsFirst {
				thumbs()
				l.restart()
				resume()
			} else {
				resume()
				l.restart()
				thumbs()
			}
			l.stages("after both", issues, "ready", "held", "held")
			if !anyComment(l.f, low, fmt.Sprintf("a write collaborator deleted a comment on #%d during this hold", mid)) {
				t.Errorf("#%d: newest comment %q; want the record that the comment deleted on #%d keeps it held", low, newestComment(l.f, low), mid)
			}
			l.restart()
			l.settle("a restart")
			l.stages("after a restart", issues, "ready", "held", "held")
			l.unchanged("after a restart")
			l.f.AddComment(mid, "owner", "/resume")
			l.settle("the /resume on the middle")
			l.stages("after the /resume on the middle", issues, "ready", "ready", "clarifying")
			l.unchanged("at the end")
		})
	}
}

// #3 > #2 > #1. The owner's /hold on #3 writes markers on #3 and #2, and
// on #1 unless mallory's comment held it first, else her comment comes
// after, and the owner deletes the /hold. #3's /resume ends it on #1 too,
// as a marker on #3 names it, so it counts as posted there (madeAbove):
// #1 stays held for discussion only, with one record that the /hold
// ended, whether its marker on #1 or the one on #2 told #1 of it, and #3's
// move names it. A crash at any write leaves the same result, as the
// replay counts that record as the /resume's own (resolveLeft).
func TestResolveDeletedHoldRecordedOnceOnReplay(t *testing.T) {
	t.Parallel()
	for _, commentFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("comment first %v", commentFirst), func(t *testing.T) {
			var hold int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)  // #1
				file(f, 0, ready) // #2
				file(f, 0, ready) // #3
				f.SetParent(1, 2)
				f.SetParent(2, 3)
				if commentFirst {
					f.AddComment(1, "mallory", "Wait.")
					resolveSettle(t, f, 1)
				}
				hold = f.AddComment(3, "owner", "/hold")
				resolveSettle(t, f, 3, 2, 1)
				if marked := strings.Contains(newestComment(f, 1), fmt.Sprintf("cmd=%d", hold)); marked == commentFirst {
					t.Fatalf("#1: newest comment %q; want a marker of the /hold only if #1 wasn't held", newestComment(f, 1))
				}
				if !commentFirst {
					f.AddComment(1, "mallory", "Wait.")
					resolveSettle(t, f, 1)
				}
				f.DeleteComment(hold)
				f.AddComment(3, "owner", "/resume")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 3))
			f := run(t, setup, resolvePass(t, 3))
			resolveLabels(t, f, "after #3's /resume", "held", "ready", "ready")
			if body := newestComment(f, 1); strings.Count(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=", hold)) != 1 {
				t.Errorf("#1: newest comment %q; want one record that the deleted /hold ended", body)
			}
			if body := newestComment(f, 3); !strings.Contains(body, "Still on hold for discussion: #1.") {
				t.Errorf("#3: newest comment %q; want it to name #1 as still on hold for discussion", body)
			}
		})
	}
}

// #4 > #3 > #2 > #1. #1's own /hold holds it; #3 is held by hand; then
// the owner's /hold on #4 writes markers on #4 and #2, none on #3 or #1.
// #3's /resume ends it on #2 and #1, as it was posted above #3, but #1
// stays held by its own /hold. Then the owner deletes the /hold on #4, and
// #4's /resume takes #4 out. It ends nothing on #1, as #3's /resume ended
// the deleted /hold there before it: a marker on #4 names that /hold, so
// it counts as posted there, above #3, though the farthest marker on #1's
// path is #2's. So #4's move doesn't name #1 as a hold it ended.
func TestResolveDeletedHoldEndedBetween(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	file(f, 0, ready) // #4
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.SetParent(3, 4)
	f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1)
	f.RemoveLabel(3, "owner", "stage:ready")
	f.AddLabel(3, "owner", "stage:held")
	resolveSettle(t, f, 3)
	hold := f.AddComment(4, "owner", "/hold")
	resolveSettle(t, f, 4, 3, 2, 1)
	resolveLabels(t, f, "after the /hold on #4", "held", "held", "held", "held")
	for _, n := range []int{1, 3} {
		if strings.Contains(newestComment(f, n), fmt.Sprintf("cmd=%d", hold)) {
			t.Fatalf("#%d: newest comment %q; want no marker of the /hold on #4", n, newestComment(f, n))
		}
	}
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 4, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "ready", "ready", "held")
	f.DeleteComment(hold)
	f.AddComment(4, "owner", "/resume")
	resolveSettle(t, f, 4, 3, 2, 1)
	resolveLabels(t, f, "after #4's /resume", "held", "ready", "ready", "ready")
	if body := newestComment(f, 4); !strings.Contains(body, "Resumed by `/resume`") || strings.Contains(body, "Still on hold") {
		t.Errorf("#4: newest comment %q; want its move back, naming no issue still on hold", body)
	}
}

// The /resume path reads every issue above the held one, as the leave
// path does, so both count the same holds in either order, and an owner
// hold a /resume ended stays ended (SPEC §6.4). Below are the defect
// where they didn't, and the cases around it.

// The defect: a chain of tasks, the lowest held for mallory's comment.
// The owner comments on the top issue, and mallory deletes that comment
// during the hold below, which holds the top issue too, until the owner's
// 👍 on its notice sends it to clarifying. For the issue below, the
// deleted comment may have been a /hold, so it holds that issue until a
// /resume on the top issue, or between, posted after the deletion. That
// /resume applies, and ends it, whether the 👍 on mallory's comment comes
// before it or after, with a restart between, at two levels, the /resume
// on the issue of the deletion, and at three, the /resume between.
func TestResolveGoneAboveEndedByLaterResume(t *testing.T) {
	t.Parallel()
	for _, levels := range []int{2, 3} {
		for _, thumbsFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d levels, thumbs first %v", levels, thumbsFirst), func(t *testing.T) {
				l := newResolveLoop(t, newFake(t))
				issues := []int{l.task(0)}
				for len(issues) < levels {
					issues = append(issues, l.task(issues[len(issues)-1]))
				}
				top, low, on := issues[0], issues[levels-1], issues[levels-2] // on: where the /resume goes
				l.settle("start")
				w := l.f.AddComment(low, "mallory", "Wait.")
				l.settle("mallory's comment")
				c := l.f.AddComment(top, "owner", "A note.")
				l.delete(top, c)
				l.settle("mallory's deletion")
				l.thumbNotice(top)
				l.settle("the 👍 on the notice")
				if got := l.f.State().Issues[top-1].Labels; len(got) != 1 || got[0] != "stage:clarifying" {
					t.Fatalf("#%d: labels %q; want it at clarifying once the deletion is accepted", top, got)
				}
				thumbs := func() {
					l.f.AddReaction(w, "owner", "+1")
					l.settle("the 👍")
				}
				resume := func() {
					l.f.AddComment(on, "owner", "/resume")
					l.settle("the /resume")
					if !l.resumeApplied(on) {
						t.Errorf("#%d: newest comment %q; want the /resume applied", on, newestComment(l.f, on))
					}
				}
				if thumbsFirst {
					thumbs()
					l.restart()
					resume()
				} else {
					resume()
					l.restart()
					thumbs()
				}
				want := []string{"clarifying", "clarifying"}
				if levels == 3 {
					want = []string{"clarifying", "ready", "clarifying"}
				}
				l.stages("after both", issues, want...)
				l.restart()
				l.settle("a restart")
				l.unchanged("at the end")
			})
		}
	}
}

// SPEC §6.4: a comment deleted in the same second as the /resume came no
// earlier than it, so the /resume doesn't end what it may have been, on
// either path: the issue below stays held, with the 👍 on mallory's comment
// before the /resume or after, until a /resume posted after.
func TestResolveGoneSameSecondAsResume(t *testing.T) {
	t.Parallel()
	for _, thumbsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("thumbs first %v", thumbsFirst), func(t *testing.T) {
			l := newResolveLoop(t, newFake(t))
			top := l.task(0)
			low := l.task(top)
			issues := []int{top, low}
			l.settle("start")
			w := l.f.AddComment(low, "mallory", "Wait.")
			l.settle("mallory's comment")
			c := l.f.AddComment(top, "owner", "A note.")
			l.f.DeleteCommentBy("mallory", c)
			// The /resume, in the deletion's second, and a bump.
			l.f.Advance(-time.Second)
			l.f.AddComment(top, "owner", "/resume")
			l.f.SetPermission("eve", "read")
			l.f.AddComment(top, "eve", "Bump.")
			if thumbsFirst {
				l.f.AddReaction(w, "owner", "+1")
			}
			l.settle("the deletion and the /resume")
			if !thumbsFirst {
				l.restart()
				l.f.AddReaction(w, "owner", "+1")
				l.settle("the 👍")
			}
			l.stages("after both", issues, "held", "held")
			if !anyComment(l.f, low, fmt.Sprintf("a write collaborator deleted a comment on #%d during this hold", top)) {
				t.Errorf("#%d: newest comment %q; want the record that the deleted comment keeps it held", low, newestComment(l.f, low))
			}
			l.f.AddComment(top, "owner", "/resume")
			l.settle("a later /resume")
			l.stages("after a later /resume", issues, "ready", "clarifying")
		})
	}
}

// SPEC §6.4: #4 > #3 > #2 > #1, #3 held by hand and #1 for mallory's
// comment, so the owner's /hold on #3 writes its marker on #2 alone; then
// the owner deletes it. It counts as posted on #2, the farthest issue with
// its marker, so #3's /resume, above that, doesn't end it, on #2 or on #1,
// in either order of the 👍 on mallory's comment; #2's /resume does.
func TestResolveDeletedHoldFarthestMarker(t *testing.T) {
	t.Parallel()
	for _, thumbsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("thumbs first %v", thumbsFirst), func(t *testing.T) {
			f := discFake(t)  // #1
			file(f, 0, ready) // #2
			file(f, 0, ready) // #3
			file(f, 0, ready) // #4
			f.SetParent(1, 2)
			f.SetParent(2, 3)
			f.SetParent(3, 4)
			f.RemoveLabel(3, "owner", "stage:ready")
			f.AddLabel(3, "owner", "stage:held")
			resolveSettle(t, f, 3)
			w := f.AddComment(1, "mallory", "Wait.")
			resolveSettle(t, f, 1)
			hold := f.AddComment(3, "owner", "/hold")
			resolveSettle(t, f, 3, 2, 1)
			if !strings.Contains(newestComment(f, 2), fmt.Sprintf("cmd=%d", hold)) {
				t.Fatalf("#2: newest comment %q; want the /hold's marker", newestComment(f, 2))
			}
			f.DeleteComment(hold)
			thumbs := func() {
				f.AddReaction(w, "owner", "+1")
				resolveSettle(t, f, 4, 3, 2, 1)
			}
			if thumbsFirst {
				thumbs()
			}
			f.AddComment(3, "owner", "/resume")
			resolveSettle(t, f, 4, 3, 2, 1)
			if !thumbsFirst {
				thumbs()
			}
			resolveLabels(t, f, "after both", "held", "held", "ready", "ready")
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready", "ready")
		})
	}
}

// The record of a comment deleted above, that may have been a /hold, and
// the deletion itself, are one hold to a /resume that ends it: #1 > #2,
// #2 held for mallory's comment, and the deletion on #1 recorded once the
// 👍 resolves it; then mallory comments on #2 again, and #1's /resume
// records the end of that one hold once, as #2 stays held for her new
// comment.
func TestResolveGoneAndItsRecordEndOnce(t *testing.T) {
	t.Parallel()
	l := newResolveLoop(t, newFake(t))
	top := l.task(0)
	low := l.task(top)
	l.settle("start")
	w := l.f.AddComment(low, "mallory", "Wait.")
	l.settle("mallory's comment")
	c := l.f.AddComment(top, "owner", "A note.")
	l.delete(top, c)
	l.settle("mallory's deletion")
	l.thumbNotice(top)
	l.f.AddReaction(w, "owner", "+1")
	l.settle("the 👍s")
	if !anyComment(l.f, low, fmt.Sprintf("a write collaborator deleted a comment on #%d during this hold", top)) {
		t.Fatalf("#%d: newest comment %q; want the record of the deletion", low, newestComment(l.f, low))
	}
	l.f.AddComment(low, "mallory", "Wait again.")
	l.settle("mallory's next comment")
	l.f.AddComment(top, "owner", "/resume")
	l.settle("the /resume")
	body := newestComment(l.f, low)
	if n := strings.Count(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=g%d-", top)); n != 1 {
		t.Errorf("#%d: newest comment %q; want one record that the deletion's hold ended, not %d", low, body, n)
	}
	l.stages("after the /resume", []int{top, low}, "clarifying", "held")
}

package daemon_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// #188's verify, round 3, attempt 1: the /resume path reads every issue
// above the held one, as the leave path does, so both count the same
// holds in either order; an owner hold a /resume ended stays ended; and
// the gaps the verifier's mutants found.

// thumbNotice has the owner 👍 the App's notice of the deletion on issue
// n, which accepts it, if it's the deletion's only trigger.
func (l *resolveLoop) thumbNotice(n int) {
	l.t.Helper()
	ids := commentIDs(l.t, l.f, n)
	found := false
	for i, c := range l.f.State().Issues[n-1].Comments {
		if strings.Contains(c.Body, "issuedog:notice key=d") {
			l.f.AddReaction(ids[i], "owner", "+1")
			found = true
		}
	}
	if !found {
		l.t.Fatalf("#%d: no notice of the deletion; newest comment %q", n, newestComment(l.f, n))
	}
}

// resumeApplied reports whether the newest /resume on issue n has the
// App's 👍.
func (l *resolveLoop) resumeApplied(n int) bool {
	cs := l.f.State().Issues[n-1].Comments
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Body == "/resume" {
			for _, r := range cs[i].Reactions {
				if r == slug+"[bot] +1" {
					return true
				}
			}
			return false
		}
	}
	return false
}

// The defect: a chain of tasks, the lowest held for mallory's comment.
// The owner comments on the top issue, and mallory deletes that comment
// during the hold below, which holds the top issue too, until the owner's
// 👍 on its notice sends it to clarifying. For the issue below, the
// deleted comment may have been a /hold, so it holds that issue until a
// /resume on the top issue, or between, posted after the deletion. That
// /resume applies, and ends it, whether the 👍 on mallory's comment comes
// before it or after, with a restart between, at two levels, the /resume
// on the issue of the deletion, and at three, the /resume between.
func TestResolveRound3GoneAboveResume(t *testing.T) {
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

// Mutant #3: a comment deleted in the same second as the /resume came no
// earlier than it, so the /resume doesn't end what it may have been, on
// either path: the issue below stays held, with the 👍 on mallory's comment
// before the /resume or after, until a /resume posted after.
func TestResolveRound3GoneSameSecond(t *testing.T) {
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

// Note 1: #3 > #2 > #1. #3 held by hand, and #2 for mallory's comment, so
// the owner's /hold on #3 writes its marker on #1 alone; then the owner's
// /hold on #2, which writes none. #3's /resume ends #3's /hold on #1, but
// #1 stays held by #2's, so it records that the first one ended. Once the
// owner deletes #3's /hold, it would count as posted on #1, the farthest
// issue with its marker, which only /resume on #1 ends; the record keeps
// it ended, so #2's /resume takes #1 out of held, whether the deletion
// came before it or after.
func TestResolveRound3EndedHoldStaysEnded(t *testing.T) {
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

// Note 1's held= record: #2 > #1, #2 held by hand and #1 for mallory's
// comment, so the owner's /hold on #2 writes no marker. Once the 👍
// resolves mallory's comment, #1 stays held by that /hold, and the record
// says `/resume` there ends it, naming #2 as where it counts as posted
// (heldon=). It still does once the owner deletes the /hold, which no
// marker names: #2's /resume sends #1 to clarifying, whether the deletion
// came before it or after.
func TestResolveRound3HeldRecordAfterDeletion(t *testing.T) {
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

// Mutant #14: #3 > #2 > #1. The owner's /resume on #2, then a /hold on
// #1, posted after it, whose pass comes first, so it holds all three with
// markers. The /resume doesn't end that /hold on #3, posted after it
// (SPEC §6.4), so #3 stays held, as #1 does. (#2's own /resume ends every
// hold on #2, a race of its own: #188's verify, round 3, note 3.)
func TestResolveRound3MarkerPostedAfterResume(t *testing.T) {
	f := discFake(t)  // #1
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(2, "owner", "/resume")
	f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1)
	resolveLabels(t, f, "after #1's /hold", "held", "held", "held")
	resolveSettle(t, f, 2)
	if got := f.State().Issues[2].Labels; len(got) != 1 || got[0] != "stage:held" {
		t.Errorf("#3: labels %q, newest comment %q; want it still held by #1's /hold", got, newestComment(f, 3))
	}
	if got := f.State().Issues[0].Labels; len(got) != 1 || got[0] != "stage:held" {
		t.Errorf("#1: labels %q; want it still held", got)
	}
}

// Mutant #16: #4 > #3 > #2 > #1, #3 held by hand and #1 for mallory's
// comment, so the owner's /hold on #3 writes its marker on #2 alone; then
// the owner deletes it. It counts as posted on #2, the farthest issue with
// its marker, so #3's /resume, above that, doesn't end it, on #2 or on #1,
// in either order of the 👍 on mallory's comment; #2's /resume does.
func TestResolveRound3DeletedHoldFarthestMarker(t *testing.T) {
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

// Mutants #22 and #40: #3 > #2 > #1, #1 held for mallory's comment. The
// owner's /hold on #3 holds #3 and #2. Then the owner's /resume on #3, and
// a /hold on #2 posted after it, whose pass comes first: it writes no
// marker, #2 and #1 held already. The /resume doesn't end it, posted
// below it and after it, so #2 stays held by it, and #1 too once mallory's
// comment is resolved, until #2's /resume.
func TestResolveRound3PathHoldPostedAfterResume(t *testing.T) {
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSettle(t, f, 1)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(3, "owner", "/resume")
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// Mutant #41: #2 > #1. The owner's /priority on #2, which the App 👍s,
// before mallory's comment holds #1; then mallory edits it. That 👍 came
// before #1's hold, so the edited comment is no /hold of it, and once the
// owner's 👍 resolves mallory's comment, #1 goes to clarifying.
func TestResolveRound3EditedBeforeHoldOnPath(t *testing.T) {
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

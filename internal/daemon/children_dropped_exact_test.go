package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A dropped child's close keyed and judged exactly, and a revision's
// blocked-by links GitHub answers 422 to (#607, from #537's and #544's
// verify): a close keyed apart, event=p<id>-closed, counts as the App's
// own close even once an edit hides the move that chose its key; a child
// the owner closed below a dropped one gets no write; a link someone
// added since the read counts as made; and a removal GitHub refuses is a
// failure no later pass gets past (SPEC §5, §5.2, §6.4).

// droppedExactReparented is TestChildrenDroppedReparented's setup: the
// changed T2's child, #3, moved by hand below T3's, #4, which the
// revision drops.
func droppedExactReparented(t testing.TB) *githubtest.Fake {
	return droppedStory(func(f *githubtest.Fake, r changedIDs) { f.SetParent(r.t2, r.t3) })(t)
}

// droppedExactStopped returns a setup for Replay: droppedExactReparented,
// stopped by a crash at the first write of a clean pass that match
// accepts (childrenCrashStopped), then edit, with the writes forgotten.
func droppedExactStopped(t *testing.T, match func(githubtest.Write) bool, edit func(testing.TB, *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	pass := childrenPass(t, revisionPlan)
	at := writeAt(t, droppedExactReparented, pass, 1, match)
	return func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, droppedExactReparented, pass, at)
		edit(tb, f)
		f.ForgetWrites()
		return f
	}
}

// droppedExactCloses are the comments of issue is that the close of a
// dropped subtree posted, and those that carry a state error.
func droppedExactCloses(is githubtest.IssueState) (closes, errs []string) {
	for _, c := range is.Comments {
		if strings.HasPrefix(c.Body, "Closed as not planned") {
			closes = append(closes, c.Body)
		}
		if strings.Contains(c.Body, "<!-- issuedog:error ") {
			errs = append(errs, c.Body)
		}
	}
	return closes, errs
}

// A pass that crashed after the close comment of T2's child, #3, keyed
// event=p<id>-closed, as it's below the dropped T3's #4, but before the
// close, and then the owner's edit of the comment holding #3's move to
// ready, keyed event=p<id>, which hides that move. The next pass's label
// rules finish the close the marker names (SPEC §5.1), so #3 is closed,
// and gets no move back from closed. Its close, keyed apart, is still the
// App's own close, so #3 loses its links, and gets no second close
// comment, nor a state error. The record stands. A crash at any write
// replays.
func TestChildrenDroppedEditedCrash(t *testing.T) {
	t.Parallel()
	setup := droppedExactStopped(t, func(w githubtest.Write) bool {
		return w.Method == http.MethodPatch && w.Path == "/issues/3" && strings.Contains(w.Body, `"state":"closed"`)
	}, func(tb testing.TB, f *githubtest.Fake) {
		s, err := f.Client(tb).Snapshot(context.Background(), "o", "r", 3)
		if err != nil {
			tb.Fatal(err)
		}
		i := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return strings.HasPrefix(c.Body, "Rewritten from #1's plan, rev 2") })
		if i < 0 {
			tb.Fatalf("#3 has no move keyed by the revision: %+v", s.Comments)
		}
		f.EditComment(s.Comments[i].ID, "owner", "Edited by the owner. "+s.Comments[i].Body)
	})
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	t2 := f.State().Issues[2]
	closes, errs := droppedExactCloses(t2)
	if len(closes) != 1 || len(errs) != 0 {
		t.Errorf("T2's child has close comments %q and state errors %q; want one close comment, no error", closes, errs)
	}
	if !t2.Closed || droppedReason(t, f, 3) != "not_planned" || len(t2.BlockedBy) != 0 {
		t.Errorf("T2's child is closed %t, as %q, blocked by %v; want it closed as not planned, unlinked", t2.Closed, droppedReason(t, f, 3), t2.BlockedBy)
	}
	if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=2,3,6 -->") {
		t.Errorf("issue 1's records are %q, want one listing #2, #3 and #6", recs)
	}
}

// A pass that crashed before the close comment of T2's child, #3, below
// the dropped T3's #4, after #3's move to ready keyed by the approval,
// and then the owner's close of #3 by hand: #3 was closed by no close of
// the App's, so it gets no write at all, and keeps its links, to the bug,
// #5, and to T4's #6, though a move of it carries the revision's key
// (SPEC §5.2, "Links a revision removes"). The rest of T3's subtree closes,
// and the record stands. A crash at any write replays.
func TestChildrenDroppedOwnerClosed(t *testing.T) {
	t.Parallel()
	setup := droppedExactStopped(t, func(w githubtest.Write) bool {
		return w.Method == http.MethodPost && w.Path == "/issues/3/comments" && strings.Contains(w.Body, "Closed as not planned")
	}, func(_ testing.TB, f *githubtest.Fake) { f.CloseBy(3, "owner") })
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	droppedNoWrite(t, f, 3, "T2's child, which the owner closed")
	t2 := f.State().Issues[2]
	if !t2.Closed || !slices.Equal(t2.BlockedBy, []int{5, 6}) {
		t.Errorf("T2's child is closed %t, blocked by %v; want it closed, blocked by #5 and #6 still", t2.Closed, t2.BlockedBy)
	}
	if closes, _ := droppedExactCloses(t2); len(closes) != 0 {
		t.Errorf("T2's child has close comments %q, want none", closes)
	}
	if !f.State().Issues[3].Closed {
		t.Error("T3's child is open, want it closed as not planned")
	}
	if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=2,3,6 -->") {
		t.Errorf("issue 1's records are %q, want one listing #2, #3 and #6", recs)
	}
}

// A blocked-by link someone adds between a child's read and the App's
// add, so GitHub answers the add with a 422, as for a link the issue has
// already, counts as made (SPEC §5.2): here the new T4's #6 blocked by the
// kept T1's #2, as a new child's links add, and the changed T2's #3
// blocked by #6, as a changed child's do. The pass carries on, with no
// state error, and the record stands, as with no 422. A crash at any write
// replays.
func TestChildrenBlockStands(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		number, blocker int
	}{
		{"a new child's link", 6, 2},
		{"a changed child's link", 3, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := func(tb testing.TB) *githubtest.Fake {
				f := droppedStory(nil)(tb)
				path := fmt.Sprintf("/issues/%d/dependencies/blocked_by", tt.number)
				f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == path },
					func() { f.SetBlockedBy(tt.number, tt.blocker) })
				return f
			}
			pass := childrenPass(t, revisionPlan)
			githubtest.Replay(t, setup, pass)

			f := run(t, setup, pass)
			path := fmt.Sprintf("/issues/%d/dependencies/blocked_by", tt.number)
			if !slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool {
				return r.Method == http.MethodPost && r.Path == path && r.Status == http.StatusUnprocessableEntity
			}) {
				t.Fatalf("GitHub never answered the add of #%d's link with a 422: %+v", tt.number, f.Requests())
			}
			if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 0 {
				t.Errorf("issue 1's state errors are %q; want none", got)
			}
			if got := f.State().Issues[tt.number-1].BlockedBy; !slices.Contains(got, tt.blocker) {
				t.Errorf("#%d is blocked by %v, want #%d among them", tt.number, got, tt.blocker)
			}
			if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=2,3,6 -->") {
				t.Errorf("issue 1's records are %q, want one listing #2, #3 and #6", recs)
			}
		})
	}
}

// droppedExactRefuse has f answer every request that match accepts with a
// 422 alone, as GitHub refuses a write whatever pass asks: each answer
// arms the next (AnswerOnce, BeforeRequest).
func droppedExactRefuse(f *githubtest.Fake, match func(githubtest.Request) bool) {
	f.AnswerOnce(match, http.StatusUnprocessableEntity)
	f.BeforeRequest(match, func() { droppedExactRefuse(f, match) })
}

// A blocked-by link a revision removes that GitHub refuses with a 422 is
// a failure no later pass gets past, as a refused add is (SPEC §5.2): a
// state error on the story says so, once, in the daemon's words, the
// visit returns no error, and a visit after it makes no request, rather
// than retrying the removal on every pass. No record is posted. Here the
// changed T2's #3 losing its link to the kept T1's #2, as its revision
// no longer depends on T1; T2's #3, below the dropped T3's #4 and closed
// with it, losing its link to the bug, #5; and the bug, #5, losing its
// link to the dropped T3's #4. A crash at any write replays.
func TestChildrenUnblockRefused(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		reparented      bool
		number, blocker int
	}{
		{"a changed child's link", false, 3, 2},
		{"a dropped issue's blocker", true, 3, 5},
		{"an issue a dropped one blocks", false, 5, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := func(tb testing.TB) *githubtest.Fake {
				story := droppedStory(nil)
				if tt.reparented {
					story = droppedExactReparented
				}
				f := story(tb)
				blocker, _, err := f.Client(tb).Issues.Get(context.Background(), "o", "r", tt.blocker)
				if err != nil {
					tb.Fatal(err)
				}
				path := fmt.Sprintf("/issues/%d/dependencies/blocked_by/%d", tt.number, blocker.GetID())
				droppedExactRefuse(f, func(r githubtest.Request) bool { return r.Method == http.MethodDelete && r.Path == path })
				return f
			}
			pass := childrenPass(t, revisionPlan)
			githubtest.Replay(t, setup, pass)

			f := run(t, setup, pass)
			if got := f.State().Issues[tt.number-1].BlockedBy; !slices.Contains(got, tt.blocker) {
				t.Errorf("#%d is blocked by %v, want #%d among them still", tt.number, got, tt.blocker)
			}
			creationRestStuck(t, f, revisionPlan, 2, goneMoveID(t, f), fmt.Sprintf("GitHub refused to remove a blocked-by link of #%d", tt.number))
		})
	}
}

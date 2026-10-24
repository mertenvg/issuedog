package daemon_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A revision's kept or changed child that's gone, as when it was deleted
// or moved to another repo after the revision was checked, isn't found
// however the children are listed, so no later pass gets past it: the
// visit stops before any write, and reports it once, as a state error on
// the parent (SPEC §5.2, "A child that isn't found", #362).

// goneKeptT3 is the golden story revision with T3 kept rather than
// dropped, and the new T4 and the changed T2 depending on it, as
// TestChildrenKeptUnlinkedFound has it, approved by the owner's 👍, with
// each of moved, a child's number, then moved to another repo.
func goneKeptT3(moved ...int) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		out := plGolden(t, "story-revision")
		plItem(t, out, 1)["depends_on"] = []any{"T1", "T3"}
		plItem(t, out, 2)["status"] = "kept"
		plItem(t, out, 3)["depends_on"] = []any{"T3"}
		f := revApprovedWith(t, changedPlanData(t, out))
		for _, n := range moved {
			f.Transfer(n, "o/elsewhere")
		}
		return f
	}
}

// goneMoveID is the ID of the App's comment on issue 1 that moved it into
// planned with plan revision 2, as a pass left it, read fresh.
func goneMoveID(t *testing.T, f *githubtest.Fake) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, "to=planned rev=2 ") {
			return c.ID
		}
	}
	t.Fatal("issue 1 has no move into planned with plan revision 2")
	return 0
}

// A kept child moved to another repo, a changed one, or both, stops the
// revision with nothing written: no child is filed for the new T4, no
// changed child is rewritten, and no link or record is made. A state
// error on the story names each item, in the daemon's words, keyed by
// the comment holding its move into planned. The visit returns no error,
// so the story isn't read again for it, and a visit after that makes no
// request at all. A crash at the error's post replays.
func TestChildrenKeptGone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		why   string
	}{
		{"a kept child moved", goneKeptT3(4),
			"no child of T3 is found, though the revision keeps or changes it, as when it was deleted, moved to another repo or unlinked from this issue"},
		{"a changed child moved", goneKeptT3(3),
			"no child of T2 is found, though the revision keeps or changes it, as when it was deleted, moved to another repo or unlinked from this issue"},
		{"a changed child and a kept one moved", goneKeptT3(3, 4),
			"no child of T2 and T3 is found, though the revision keeps or changes them, as when they were deleted, moved to another repo or unlinked from this issue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pass := childrenPass(t, revisionPlan)
			githubtest.Replay(t, tt.setup, pass)

			f := run(t, tt.setup, pass)
			st := f.State()
			want := "I can't create the children of plan rev 2, as " + tt.why +
				". I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.\n\n" +
				childrenCrashError(goneMoveID(t, f))
			if got := childrenCrashErrors(st.Issues[0]); !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("issue 1's state errors are\n%q\nwant one\n%q", got, want)
			}
			if len(st.Issues) != 5 || len(records(st.Issues[0])) != 0 {
				t.Errorf("%d issues, and the records %q; want none filed, and no record", len(st.Issues), records(st.Issues[0]))
			}
			for _, n := range []int{2, 3, 4, 5} {
				droppedNoWrite(t, f, n, "a child or the bug, while the revision is stopped")
			}
			visit := childrenStep(t, f, revisionPlan)
			s, err := visit.Client.Snapshot(context.Background(), "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			from := len(f.Requests())
			if err := visit.ChildrenVisit(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if r := f.Requests()[from:]; len(r) != 0 {
				t.Errorf("a visit once the state error stands sent %+v; want nothing", r)
			}
		})
	}
}

// A kept child that's gone stops the revision before a changed human
// task it depends on is rewritten, whose "Why it blocks" would name it:
// the state error names the item, the human task gets no write, and the
// visit returns no error. Here the kept T2, which the changed T3 blocks
// (changedKeptBlocksRevision), was moved to another repo. A crash at the
// error's post replays, and a visit after the error makes no request at
// all.
func TestChildrenKeptGoneBlockedByChangedHuman(t *testing.T) {
	t.Parallel()
	setup := changedKeptBlocksSetup(3)
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	st := f.State()
	want := "I can't create the children of plan rev 2, as no child of T2 is found, though the revision keeps or changes it, " +
		"as when it was deleted, moved to another repo or unlinked from this issue. I won't try again until this issue moves, as by `/hold` and `/resume`. " +
		"The daemon's log has the error.\n\n" + childrenCrashError(goneMoveID(t, f))
	if got := childrenCrashErrors(st.Issues[0]); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("issue 1's state errors are\n%q\nwant one\n%q", got, want)
	}
	if len(st.Issues) != 5 || len(records(st.Issues[0])) != 0 {
		t.Errorf("%d issues, and the records %q; want none filed, and no record", len(st.Issues), records(st.Issues[0]))
	}
	for _, n := range []int{2, 3, 4, 5} {
		droppedNoWrite(t, f, n, "a child or the bug, while the revision is stopped")
	}
	visit := childrenStep(t, f, revisionPlan)
	s, err := visit.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	if err := visit.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[from:]; len(r) != 0 {
		t.Errorf("a visit once the state error stands sent %+v; want nothing", r)
	}
}

// A kept child that someone unlinked by hand, but isn't listed, isn't
// found either, though it isn't gone: here the adopted T3, kept, which
// owner filed, so it's a child only while it's a sub-issue (SPEC §5.2).
// The state error's cause fits it as well, naming an unlink, and the
// issue gets no write.
func TestChildrenKeptUnlinkedAdopted(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		r := adoptedApprovedWith(t, f, false, nil, changedPlanData(t, adoptedKeptRevision(t)))
		f.SetParent(r.t3, 0)
		return f
	}
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	st := f.State()
	want := "I can't create the children of plan rev 2, as no child of T3 is found, though the revision keeps or changes it, " +
		"as when it was deleted, moved to another repo or unlinked from this issue. I won't try again until this issue moves, " +
		"as by `/hold` and `/resume`. The daemon's log has the error.\n\n" + childrenCrashError(goneMoveID(t, f))
	if got := childrenCrashErrors(st.Issues[0]); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("issue 1's state errors are\n%q\nwant one\n%q", got, want)
	}
	if len(st.Issues) != 5 || len(records(st.Issues[0])) != 0 || st.Issues[3].Closed {
		t.Errorf("%d issues, the records %q, and T3's child closed %t; want none filed, no record, and T3's child open",
			len(st.Issues), records(st.Issues[0]), st.Issues[3].Closed)
	}
	for _, n := range []int{2, 3, 4, 5} {
		droppedNoWrite(t, f, n, "a child or the bug, while the revision is stopped")
	}
}

// The owner can act on the state error: here they move the kept child
// back to this repo, then hold and resume the story, and the revision is
// applied, its record listing T3's child, with the error not posted
// again.
func TestChildrenKeptGoneBack(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, revisionPlan)
	f := run(t, goneKeptT3(4), pass)
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 1 {
		t.Fatalf("issue 1's state errors are %q; want one", got)
	}
	f.Transfer(4, "o/r")
	f.AddComment(1, "owner", "/hold")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if got := childrenCrashErrors(st.Issues[0]); len(got) != 1 {
		t.Errorf("issue 1's state errors are %q; want the one, not posted again", got)
	}
	if got := records(st.Issues[0]); len(got) != 1 || !strings.HasSuffix(got[0], "<!-- issuedog:created rev=2 children=2,3,4,6 -->") {
		t.Errorf("issue 1's records are %q, want one naming 2, 3, 4 and 6", got)
	}
}

// A dropped item's child that's gone isn't a failure: there's nothing for
// the revision to close. Here the golden revision's T3, dropped, was
// moved to another repo. Its child gets no write, no state error is
// posted, and the record stands, listing T1's, T2's and T4's children and
// naming T3 as left as it was, so no later pass looks for it again (#297).
func TestChildrenDroppedGone(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := revApproved(t)
		f.Transfer(4, "o/elsewhere")
		return f
	}
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)
	f := run(t, setup, pass)
	st := f.State()
	if got := childrenCrashErrors(st.Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's state errors are %q; want none", got)
	}
	droppedNoWrite(t, f, 4, "T3's child, moved")
	want := "I've applied plan rev 2 to the children, which are now:\n- T1 #2: Record when each quote expires\n" +
		"- T2 #3: Refuse an expired quote at booking, saying when it expired\n- T4 #6: Give the quote lifetime a default\n\n" +
		"I found no child of T3, so this revision left it as it was.\n\n<!-- issuedog:created rev=2 children=2,3,6 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != want {
		t.Errorf("issue 1's records are %q, want only\n%q", recs, want)
	}
}

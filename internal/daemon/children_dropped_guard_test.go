package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The guards in the close of a revision's dropped children (SPEC §6.4,
// #368): an issue in another repo is never closed or unlinked as if it
// were this repo's issue of that number, and each issue closed has its
// labels checked and is reported to Touched before its close.

// droppedGuardEpic is droppedEpic with droppedEpicRevision as revision 2,
// which drops S1, with its tasks, keeps S2 and adds S3.
func droppedGuardEpic(t testing.TB) *githubtest.Fake {
	t.Helper()
	return droppedEpic(func(t testing.TB) map[string]any { return droppedEpicRevision(t) })(t)
}

// A dropped subtree that reaches into another repo closes and unlinks
// only this repo's issues. Below S1's T2, #5, is a sub-issue of another
// repo numbered #10, as the new S3's child is here: it isn't walked, so
// S3's #10 stays open and keeps its link to the kept S2's #3. And #9, the
// owner's, moved to another repo, is blocked by S1's T1, #4: that link
// stays, as it's that repo's issue to write. The rest of S1's subtree
// closes, deepest first, as ever, and the record stands. A crash at any
// write writes nothing twice.
func TestChildrenDroppedOtherRepo(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := droppedGuardEpic(t)
		f.LinkSubIssue(5, "o/other", 10)
		mobile := f.AddIssue("owner", "Show when a quote expires in the app", "The app.")
		f.SetBlockedBy(mobile, 4)
		f.Transfer(mobile, "o/mobile")
		return f
	}
	githubtest.Replay(t, setup, childrenPass(t, twoStories))
	f := run(t, setup, childrenPass(t, twoStories))
	issues := f.State().Issues
	if len(issues) != 10 {
		t.Fatalf("%d issues, want one filed, for S3", len(issues))
	}
	var closes []string
	for _, w := range f.Writes() {
		if w.Method == http.MethodPatch {
			closes = append(closes, w.Path)
		}
	}
	if want := []string{"/issues/4", "/issues/7", "/issues/5", "/issues/2"}; !slices.Equal(closes, want) {
		t.Errorf("the issues closed, in order, are %q; want %q, S1's subtree in this repo alone", closes, want)
	}
	if s3 := issues[9]; s3.Closed || !slices.Equal(s3.BlockedBy, []int{3}) {
		t.Errorf("S3's child is closed %t, blocked by %v; want it open, blocked by the kept S2's #3", s3.Closed, s3.BlockedBy)
	}
	if issues[2].Closed {
		t.Error("the kept S2's #3 is closed, want it open")
	}
	if got := issues[8].BlockedBy; !slices.Equal(got, []int{4}) {
		t.Errorf("#9, in another repo, is blocked by %v, want S1's T1's #4 still", got)
	}
	droppedNoWrite(t, f, 9, "in another repo")
	want := "I've applied plan rev 2 to the children, which are now:\n- S2 #3: Customers see when a quote expires\n" +
		"- S3 #10: Expired quotes in the monthly report\n\n<!-- issuedog:created rev=2 children=3,10 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != want {
		t.Errorf("the epic's records are %q, want only\n%q", recs, want)
	}
}

// A dropped issue whose stage the owner changed by hand since its last
// move has the label rules run on it before its close, as for any move:
// here S1's T1, #4, which the owner moved from ready to in-progress. The
// change is recorded as an owner override first, and the close then moves
// it from in-progress. A crash at any write writes nothing twice.
func TestChildrenDroppedOverride(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := droppedGuardEpic(t)
		f.RemoveLabel(4, "owner", "stage:ready")
		f.AddLabel(4, "owner", "stage:in-progress")
		return f
	}
	githubtest.Replay(t, setup, childrenPass(t, twoStories))
	f := run(t, setup, childrenPass(t, twoStories))
	p, _ := changedApproval(t, f)
	cs := f.State().Issues[3].Comments
	if len(cs) < 2 {
		t.Fatalf("#4's comments are %+v, want an override, then the close", cs)
	}
	override := github.ParseMarkers(cs[len(cs)-2].Body)
	wantOverride := map[string]string{"from": "ready", "to": "in-progress", "by": "owner"}
	if len(override) != 1 || override[0].Kind != "transition" || !reflect.DeepEqual(withoutEvent(override[0].Fields), wantOverride) {
		t.Errorf("#4's comment before the close is\n%q\nwant the owner's override, with %v", cs[len(cs)-2].Body, wantOverride)
	}
	wantClose := map[string]string{"from": "in-progress", "to": "closed", "reason": "not_planned", "event": fmt.Sprintf("p%d", p)}
	if m := changedLastMarker(t, f, 4); !reflect.DeepEqual(m.Fields, wantClose) {
		t.Errorf("#4's newest marker has %v, want %v", m.Fields, wantClose)
	}
	if !f.State().Issues[3].Closed || droppedReason(t, f, 4) != "not_planned" {
		t.Errorf("#4 is closed %t, as %q; want it closed as not planned", f.State().Issues[3].Closed, droppedReason(t, f, 4))
	}
}

// withoutEvent is fields without its event, which names the timeline
// event of a label change.
func withoutEvent(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if k != "event" {
			out[k] = v
		}
	}
	return out
}

// Each issue of a dropped subtree is reported to Touched before its first
// write, the label check's or the close's, so Iterate reads it again next
// pass: S1's #2, its T1's #4 and T2's #5, and the bug #7 below #5, which
// has no links, so no unlink touches it.
func TestChildrenDroppedTouched(t *testing.T) {
	t.Parallel()
	f := droppedGuardEpic(t)
	st := childrenStep(t, f, twoStories)
	touchedAt := map[int]int{}
	st.Touched = func(n int) {
		if _, ok := touchedAt[n]; !ok {
			touchedAt[n] = len(f.Writes())
		}
	}
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := st.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	writes := f.Writes()
	for _, n := range []int{2, 4, 5, 7} {
		first := slices.IndexFunc(writes, func(w githubtest.Write) bool {
			return w.Path == fmt.Sprintf("/issues/%d", n) || strings.HasPrefix(w.Path, fmt.Sprintf("/issues/%d/", n))
		})
		if first < 0 {
			t.Errorf("#%d got no write, want it closed", n)
			continue
		}
		at, ok := touchedAt[n]
		switch {
		case !ok:
			t.Errorf("#%d isn't touched; want it touched before its first write, %+v", n, writes[first])
		case at > first:
			t.Errorf("#%d is touched at write %d; want it touched before its first write, %d, %+v", n, at, first, writes[first])
		}
	}
}

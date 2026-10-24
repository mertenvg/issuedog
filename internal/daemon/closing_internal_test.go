package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A recorded close and a recorded rename are both keyed by a timeline event
// ID: the snapshot's closed events tell a close from a rename. A reopened
// event's ID, and any key the timeline doesn't list as a close, stay
// renames, as on a snapshot CheckLabels hasn't checked; other kinds stay
// as they are.
func TestClosingFromKey(t *testing.T) {
	t.Parallel()
	rec := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &github.Snapshot{Closings: []github.Closing{{ID: 9005}, {ID: 9006, Reopen: true}}}
	for key, want := range map[string]discKind{"9005": discClose, "9006": discRename, "9007": discRename, "c9005": discComment, "dCDE_9005": discDeletion} {
		if got := closingFromKey(s, discFromKey(key, rec)); got.Kind != want || got.Key != key || !got.FromRecord {
			t.Errorf("closingFromKey(%q) = %+v; want a %s from its record", key, got, want)
		}
	}
	if got := closingFromKey(&github.Snapshot{}, discFromKey("9005", rec)); got.Kind != discRename {
		t.Errorf("on an unchecked snapshot, closingFromKey(9005) = %+v; want a rename", got)
	}
}

// discRecorded builds a recorded key the snapshot's closed events list as a
// close, which no pass rebuilt, as a close, and a rename's as a rename.
func TestClosingRecorded(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Closings: []github.Closing{{ID: 9005}}, Comments: []github.Comment{{ID: 1001}}}
	open := func(string, time.Time, int64) (bool, bool) { return true, true }
	got := discRecorded(s, map[string]int64{"9005": 1001, "9006": 1001}, open, nil)
	if len(got) != 2 || got[0].Kind != discClose || got[0].Key != "9005" || got[1].Kind != discRename || got[1].Key != "9006" {
		t.Errorf("discRecorded = %+v; want 9005 a close and 9006 a rename", got)
	}
}

// A close is worded by who made it, defused, and one from its record says
// so; discWhat words a close so either way.
func TestClosingWhat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tr   discTrigger
		want string
	}{
		{discTrigger{Kind: discClose, By: "mallory"}, "`mallory` closed this issue, which I reopened"},
		{discTrigger{Kind: discClose, By: "m@owner"}, "`m@​owner` closed this issue, which I reopened"},
		{discTrigger{Kind: discClose, By: ""}, "someone GitHub doesn't name closed this issue, which I reopened"},
		{discTrigger{Kind: discClose, FromRecord: true}, "a close of this issue, recorded earlier"},
	} {
		if got := closingWhat(tc.tr); got != tc.want {
			t.Errorf("closingWhat(%+v) = %q; want %q", tc.tr, got, tc.want)
		}
		if got := discWhat(tc.tr); got != tc.want {
			t.Errorf("discWhat(%+v) = %q; want %q", tc.tr, got, tc.want)
		}
	}
}

// How a close was made is asked once per event, an event with no node ID
// isn't asked about, and a failed answer isn't kept.
func TestClosingCloserAsksOnce(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	merged := f.CloseByPullRequest(f.AddIssue("owner", "A", ""), "mallory", githubtest.ClosingPR{Merged: true})
	byHand := f.CloseBy(f.AddIssue("owner", "B", ""), "mallory")
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	asked := func() (n int) {
		for _, r := range f.Requests() {
			if r.Path == "/graphql" {
				n++
			}
		}
		return n
	}
	pr := github.Closing{ID: merged, NodeID: fmt.Sprintf("CE_%d", merged)}
	hand := github.Closing{ID: byHand, NodeID: fmt.Sprintf("CE_%d", byHand)}
	f.FailGraphQL("boom")
	if _, err := a.closingCloser(context.Background(), pr); err == nil {
		t.Fatal("closingCloser = nil error; want the failed question's")
	}
	for range 3 {
		if by, err := a.closingCloser(context.Background(), pr); err != nil || !by.Merge {
			t.Fatalf("closingCloser(the pull request's) = %+v, %v; want a merge", by, err)
		}
		if by, err := a.closingCloser(context.Background(), hand); err != nil || by.Merge || by.StateReason != "completed" {
			t.Fatalf("closingCloser(by hand) = %+v, %v; want no merge, completed", by, err)
		}
		if by, err := a.closingCloser(context.Background(), github.Closing{ID: 1}); err != nil || by != (github.ClosingCloser{}) {
			t.Fatalf("closingCloser(no node ID) = %+v, %v; want nothing", by, err)
		}
	}
	if got := asked(); got != 3 {
		t.Errorf("asked %d times; want 3: the failure, then once for each event", got)
	}
}

// closingAsks is a Trust that fails the test for any question, so a close
// it judges asks nothing.
type closingAsks struct{ t *testing.T }

func (c closingAsks) Trusted(_ context.Context, login string) (bool, error) {
	c.t.Errorf("asked whether %s is trusted", login)
	return false, nil
}

func (c closingAsks) DiscAccess(_ context.Context, login string) (bool, bool, error) {
	c.t.Errorf("asked about %s's access", login)
	return false, false, nil
}

// closingCounts judges a close in discFind's order: a reopen, or a close
// before the floor, is none, and a recorded one is, both with no question;
// then only a write collaborator's close counts, and not a merge's into
// the default branch.
func TestClosingCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	merged := f.CloseByPullRequest(f.AddIssue("owner", "A", ""), "mallory", githubtest.ClosingPR{Merged: true})
	unmerged := f.CloseByPullRequest(f.AddIssue("owner", "B", ""), "mallory", githubtest.ClosingPR{})
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s := &github.Snapshot{Number: 1}
	open := func(key string, _ time.Time, _ int64) (bool, bool) { return key != "1", key == "2" }
	none := a.discWhoOf(s, closingAsks{t})
	for _, cl := range []github.Closing{{ID: 1, By: github.Author{Login: "mallory"}}, {ID: 3, Reopen: true, By: github.Author{Login: "mallory"}}} {
		if ok, err := a.closingCounts(ctx, cl, open, none); ok || err != nil {
			t.Errorf("closingCounts(%+v) = %t, %v; want false, with no question", cl, ok, err)
		}
	}
	if ok, err := a.closingCounts(ctx, github.Closing{ID: 2, By: github.Author{Login: "eve"}}, open, none); !ok || err != nil {
		t.Errorf("a recorded close = %t, %v; want true, with no question", ok, err)
	}
	who := a.discWhoOf(s, newDiscCounting())
	for _, tc := range []struct {
		cl   github.Closing
		want bool
	}{
		{github.Closing{ID: 4, By: github.Author{Login: "mallory"}}, true},
		{github.Closing{ID: 5, By: github.Author{Login: "mallory"}, NodeID: fmt.Sprintf("CE_%d", unmerged)}, true},
		{github.Closing{ID: 6, By: github.Author{Login: "mallory"}, NodeID: fmt.Sprintf("CE_%d", merged)}, false},
		{github.Closing{ID: 7, By: github.Author{Login: "eve"}}, false},
		{github.Closing{ID: 8, By: github.Author{Login: "owner"}}, false},
		{github.Closing{ID: 9, By: github.Author{Login: "issuedog[bot]", Type: "Bot"}}, false},
	} {
		if ok, err := a.closingCounts(ctx, tc.cl, open, who); ok != tc.want || err != nil {
			t.Errorf("closingCounts(%+v) = %t, %v; want %t", tc.cl, ok, err, tc.want)
		}
	}
	if _, err := a.closingCounts(ctx, github.Closing{ID: 10, By: github.Author{Login: "bob"}}, open, who); !errors.Is(err, errTrustOnce) {
		t.Errorf("a failed question = %v; want its error, wrapped", err)
	}
	f.FailGraphQL("boom")
	if _, err := a.closingCounts(ctx, github.Closing{ID: 11, By: github.Author{Login: "mallory"}, NodeID: "CE_11"}, open, who); err == nil {
		t.Error("a failed question of how it was closed = nil; want its error")
	}
}

// closingIssue is a closed issue as the poll lists it, number n, closed
// at second at of the fake clock, with labels.
func closingIssue(n, at int, labels ...string) *gogithub.Issue {
	is := &gogithub.Issue{Number: &n, State: new("closed"), ClosedAt: &gogithub.Timestamp{Time: githubtest.Start.Add(time.Duration(at) * time.Second)}}
	for _, l := range labels {
		is.Labels = append(is.Labels, &gogithub.Label{Name: l})
	}
	return is
}

// A closed issue is due unless its close is explained: as judged, with the
// listing as it left it; on a start, with one closed stage alone; or never
// in the pipeline, with no stage label, unless this daemon held it with
// one. Listed again, a due issue stays due.
func TestClosingList(t *testing.T) {
	t.Parallel()
	d := &Daemon{Step: &Step{Applier: &Applier{}}}
	d.closingList(closingIssue(1, 1, "stage:completed"), true, false)
	d.closingList(closingIssue(2, 1, "stage:pr-open"), true, false)
	d.closingList(closingIssue(3, 1, "stage:completed", "stage:pr-open"), true, false)
	d.closingList(closingIssue(4, 1, "priority:p1"), true, false)
	d.closingList(closingIssue(5, 1), false, true)
	d.closingList(closingIssue(5, 1), false, false) // listed again at the cursor
	d.closingList(closingIssue(6, 1, "Stage:Discarded"), false, false)
	due := func() (got []int) {
		for n := range 8 {
			if d.closingDue[n] != nil {
				got = append(got, n)
			}
		}
		return got
	}
	if got := due(); !equalInts(got, []int{2, 3, 5, 6}) {
		t.Errorf("due %v; want 2, 3, 5 and 6", got)
	}
	d.closingSeen[2] = closingListed{closed: githubtest.Start.Add(time.Second), stages: "stage:completed"}
	d.closingList(closingIssue(2, 1, "stage:completed"), false, false)
	d.closingList(closingIssue(1, 9, "stage:completed"), false, false) // closed again
	d.closingList(closingIssue(6, 1, "stage:pr-open"), false, false)
	d.Step.closingAgain = map[int]closingListed{3: {closed: githubtest.Start.Add(5 * time.Second), stages: "stage:completed"}}
	d.closingList(closingIssue(3, 5, "stage:completed"), false, false)
	if got := due(); !equalInts(got, []int{1, 5, 6}) || len(d.Step.closingAgain) != 0 {
		t.Errorf("due %v, closed again %v; want 1, 5 and 6, and the App's close again judged", got, d.Step.closingAgain)
	}
	d.closingForget(1)
	if got := due(); !equalInts(got, []int{5, 6}) {
		t.Errorf("due %v after 1 is forgotten; want 5 and 6", got)
	}
}

// A due issue listed again with no stage label, after the first listing
// dropped its snapshot, stays due as the newer listing shows it, so it's
// judged from its labels and closed_at now, not the first listing's.
func TestClosingListDueRelisted(t *testing.T) {
	t.Parallel()
	d := &Daemon{Step: &Step{Applier: &Applier{}}}
	d.closingList(closingIssue(5, 1, "stage:pr-open"), false, true)
	newer := closingIssue(5, 7)
	d.closingList(newer, false, false)
	if got := d.closingDue[5]; got != newer {
		t.Errorf("due #5 as %+v; want the newer listing, %+v", got, newer)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An open issue's closed stage is judged as a reopen only while no
// override stands; a closed issue's isn't.
func TestClosingReopenLeavesAlone(t *testing.T) {
	t.Parallel()
	a := &Applier{AppSlug: "issuedog"}
	s := &github.Snapshot{Number: 1, Labels: []string{"stage:completed"}, Closings: []github.Closing{{ID: 9, Reopen: true, By: github.Author{Login: "owner"}}}}
	override := []github.LabelEvent{{Kind: github.LabelOverride, Label: "stage:ready"}}
	if done, err := a.closingReopen(context.Background(), s, closingAsks{t}, func(int64) bool { return false }, override); done || err != nil {
		t.Errorf("with an override, closingReopen = %t, %v; want nothing done", done, err)
	}
	s.Closed = true
	if done, err := a.closingReopen(context.Background(), s, closingAsks{t}, func(int64) bool { return false }, nil); done || err != nil {
		t.Errorf("on a closed issue, closingReopen = %t, %v; want nothing done", done, err)
	}
	s.Closed, s.Closings = false, []github.Closing{{ID: 8}, {ID: 9, Reopen: true, By: github.Author{Login: "issuedog[bot]", Type: "Bot"}}}
	if done, err := a.closingReopen(context.Background(), s, closingAsks{t}, func(int64) bool { return false }, nil); done || err != nil {
		t.Errorf("with only the App's reopen, closingReopen = %t, %v; want nothing done", done, err)
	}
}

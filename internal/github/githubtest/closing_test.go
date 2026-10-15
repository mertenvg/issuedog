package githubtest

import (
	"context"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// closeEvents is issue n's closed and reopened events, as "event by login",
// or "event by no one" for one that names no actor.
func closeEvents(t *testing.T, f *Fake, n int) []string {
	t.Helper()
	events, _, err := f.Client(t).Issues.ListIssueTimeline(context.Background(), "o", "r", n, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		switch {
		case e.GetEvent() != "closed" && e.GetEvent() != "reopened":
		case e.Actor == nil:
			got = append(got, e.GetEvent()+" by no one")
		default:
			got = append(got, e.GetEvent()+" by "+e.GetActor().GetLogin())
		}
	}
	return got
}

// A close or reopen, by a user or by the App's PATCH, sets closed_at and
// state_reason, moves updated_at, and adds its event, naming who made it.
// One to the state the issue has changes nothing.
func TestCloseAndReopenEvents(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	ctx, c := context.Background(), f.Client(t)
	get := func() *gogithub.Issue {
		t.Helper()
		is, _, err := c.Issues.Get(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	before := get().GetUpdatedAt()
	if f.CloseBy(n, "mallory") == 0 || f.CloseBy(n, "owner") != 0 {
		t.Error("CloseBy gave no event, or a second one for a closed issue")
	}
	is := get()
	if is.GetState() != "closed" || is.GetStateReason() != "completed" || is.ClosedAt == nil || !is.GetUpdatedAt().After(before.Time) ||
		!is.GetClosedAt().Equal(is.GetUpdatedAt()) {
		t.Errorf("after the close: %s, %s, closed_at %v, updated_at %v; want closed as completed, both at the close", is.GetState(),
			is.GetStateReason(), is.ClosedAt, is.UpdatedAt)
	}
	before = is.GetUpdatedAt()
	if err := c.ReopenIssue(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if is := get(); is.GetState() != "open" || is.GetStateReason() != "reopened" || is.ClosedAt != nil || !is.GetUpdatedAt().After(before.Time) {
		t.Errorf("after the App's reopen: %s, %s, closed_at %v, updated_at %v; want open, reopened, no closed_at and a later updated_at",
			is.GetState(), is.GetStateReason(), is.ClosedAt, is.UpdatedAt)
	}
	before = get().GetUpdatedAt()
	if err := c.ReopenIssue(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if !get().GetUpdatedAt().Equal(before) {
		t.Error("reopening an open issue moved updated_at; want no change")
	}
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{State: new("closed"), StateReason: new("not_planned")}); err != nil {
		t.Fatal(err)
	}
	if got := get().GetStateReason(); got != "not_planned" {
		t.Errorf("state_reason = %q after a PATCH closing as not planned", got)
	}
	f.ReopenBy(n, "owner")
	f.Close(n)
	want := []string{"closed by mallory", "reopened by issuedog[bot]", "closed by issuedog[bot]", "reopened by owner", "closed by no one"}
	if got := closeEvents(t, f, n); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

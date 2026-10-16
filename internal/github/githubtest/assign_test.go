package githubtest

import (
	"context"
	"slices"
	"testing"
	"time"
)

// The fake's assignees: SetAssignees sets them by hand and moves
// updated_at; an add leaves out an unassignable login and one assigned
// already, in any case, and a remove one not assigned; a write that
// changes nothing leaves updated_at alone; the issue GET and the listing
// serve them; and State holds them sorted.
func TestAssignFake(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	f.SetAssignees(n, "zed", "Amy")
	f.Unassignable("Ghost")
	c, ctx := f.Client(t), context.Background()
	updated := func() time.Time {
		t.Helper()
		is, _, err := c.Issues.Get(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is.GetUpdatedAt().Time
	}
	if got, want := updated(), Start.Add(2*time.Second); !got.Equal(want) {
		t.Errorf("updated_at after SetAssignees = %v; want %v", got, want)
	}
	if got := f.State().Issues[0].Assignees; !slices.Equal(got, []string{"Amy", "zed"}) {
		t.Errorf("State's assignees = %q; want Amy and zed, sorted", got)
	}
	is, _, err := c.Issues.AddAssignees(ctx, "o", "r", n, []string{"ghost", "AMY"})
	if err != nil || len(is.Assignees) != 2 || !updated().Equal(Start.Add(2*time.Second)) {
		t.Errorf("an add of nothing new = %v, %v, updated_at %v; want the two, and updated_at as it was", is, err, updated())
	}
	is, _, err = c.Issues.AddAssignees(ctx, "o", "r", n, []string{"bob"})
	if err != nil || len(is.Assignees) != 3 || is.Assignees[2].GetLogin() != "bob" || !updated().Equal(Start.Add(3*time.Second)) {
		t.Errorf("an add of bob = %v, %v, updated_at %v; want bob last, and updated_at moved", is, err, updated())
	}
	is, _, err = c.Issues.RemoveAssignees(ctx, "o", "r", n, []string{"nobody"})
	if err != nil || len(is.Assignees) != 3 || !updated().Equal(Start.Add(3*time.Second)) {
		t.Errorf("a remove of nothing = %v, %v, updated_at %v; want the three, and updated_at as it was", is, err, updated())
	}
	is, _, err = c.Issues.RemoveAssignees(ctx, "o", "r", n, []string{"ZED", "bob"})
	if err != nil || len(is.Assignees) != 1 || is.Assignees[0].GetLogin() != "Amy" || !updated().Equal(Start.Add(4*time.Second)) {
		t.Errorf("a remove of zed and bob = %v, %v, updated_at %v; want Amy left, and updated_at moved", is, err, updated())
	}
	listed, _, err := c.Issues.ListByRepo(ctx, "o", "r", nil)
	if err != nil || len(listed) != 1 || len(listed[0].Assignees) != 1 || listed[0].Assignees[0].GetLogin() != "Amy" {
		t.Errorf("the listing = %v, %v; want the issue with Amy assigned", listed, err)
	}
}

// FailWrite and FailWriteAfter apply to the assignee writes, as to any.
func TestAssignFakeFailWrite(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	c, ctx := f.Client(t), context.Background()
	f.FailWrite(1)
	if _, _, err := c.Issues.AddAssignees(ctx, "o", "r", n, []string{"owner"}); err == nil || f.State().Issues[0].Assignees != nil {
		t.Errorf("a failed add = %v, assignees %q; want an error, and none", err, f.State().Issues[0].Assignees)
	}
	f.FailWriteAfter(2)
	if _, _, err := c.Issues.AddAssignees(ctx, "o", "r", n, []string{"owner"}); err == nil || !slices.Equal(f.State().Issues[0].Assignees, []string{"owner"}) {
		t.Errorf("an add that fails after = %v, assignees %q; want an error, and owner assigned", err, f.State().Issues[0].Assignees)
	}
	f.FailWriteAfter(3)
	if _, _, err := c.Issues.RemoveAssignees(ctx, "o", "r", n, []string{"owner"}); err == nil || f.State().Issues[0].Assignees != nil {
		t.Errorf("a remove that fails after = %v, assignees %q; want an error, and none", err, f.State().Issues[0].Assignees)
	}
	if w := f.Writes(); len(w) != 3 || !w[0].Failed || !w[1].Failed || !w[2].Failed || w[2].Method != "DELETE" || w[2].Path != "/issues/1/assignees" {
		t.Errorf("writes = %+v; want the three, failed, the last a DELETE of /issues/1/assignees", w)
	}
}

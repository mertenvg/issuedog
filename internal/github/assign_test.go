package github_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Snapshot reads the assignees from the issue GET it makes anyway: the
// same requests as an issue with none, but for the App's token.
func TestAssignSnapshot(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	none := f.AddIssue("owner", "None", "")
	some := f.AddIssue("owner", "Some", "")
	f.SetAssignees(some, "owner", "Mallory")
	c, ctx := f.Client(t), context.Background()
	paths := func(n int) []string {
		t.Helper()
		from := len(f.Requests())
		s, err := c.Snapshot(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range f.Requests()[from:] {
			if strings.HasPrefix(r.Path, "/app/") {
				continue // the first request's token
			}
			got = append(got, r.Method+" "+strings.ReplaceAll(r.Path, fmt.Sprintf("/issues/%d", n), "/issues/N"))
		}
		if n == some && !slices.Equal(s.Assignees, []string{"owner", "Mallory"}) {
			t.Errorf("assignees of %d = %q; want owner and Mallory, in GitHub's order", n, s.Assignees)
		}
		if n == none && s.Assignees != nil {
			t.Errorf("assignees of %d = %q; want none", n, s.Assignees)
		}
		return got
	}
	if a, b := paths(none), paths(some); !slices.Equal(a, b) {
		t.Errorf("requests with assignees = %q; want the same as with none, %q", b, a)
	}
}

// AddAssignees and RemoveAssignees answer the assignees after the write,
// as GitHub does: an add leaves out a login GitHub can't assign, with no
// error, and a remove of one not assigned changes nothing. Each is one
// write to the assignee endpoint, and a replay of it writes the same.
func TestAssignWrites(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	f.Unassignable("ghost")
	c, ctx := f.Client(t), context.Background()
	got, err := c.AddAssignees(ctx, "o", "r", n, []string{"owner", "ghost"})
	if err != nil || !slices.Equal(got, []string{"owner"}) {
		t.Errorf("AddAssignees = %q, %v; want owner alone", got, err)
	}
	got, err = c.AddAssignees(ctx, "o", "r", n, []string{"OWNER", "mallory"})
	if err != nil || !slices.Equal(got, []string{"owner", "mallory"}) {
		t.Errorf("AddAssignees again = %q, %v; want owner once, then mallory", got, err)
	}
	got, err = c.RemoveAssignees(ctx, "o", "r", n, []string{"Owner", "bob"})
	if err != nil || !slices.Equal(got, []string{"mallory"}) {
		t.Errorf("RemoveAssignees = %q, %v; want mallory left", got, err)
	}
	got, err = c.RemoveAssignees(ctx, "o", "r", n, []string{"mallory"})
	if err != nil || got != nil {
		t.Errorf("RemoveAssignees of the last = %q, %v; want none", got, err)
	}
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path+" "+w.Body)
	}
	want := []string{
		`POST /issues/1/assignees {"assignees":["owner","ghost"]}` + "\n",
		`POST /issues/1/assignees {"assignees":["OWNER","mallory"]}` + "\n",
		`DELETE /issues/1/assignees {"assignees":["Owner","bob"]}` + "\n",
		`DELETE /issues/1/assignees {"assignees":["mallory"]}` + "\n",
	}
	if !slices.Equal(writes, want) {
		t.Errorf("writes = %q; want %q", writes, want)
	}
}

// A failed write, a missing issue and a transferred one are errors that
// name the write and the issue, and wrap GitHub's error or ErrTransferred.
func TestAssignErrors(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	c, ctx := f.Client(t), context.Background()
	f.FailWrite(1)
	_, err := c.AddAssignees(ctx, "o", "r", n, []string{"owner"})
	var ge *gogithub.ErrorResponse
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "assign owner to o/r#1") {
		t.Errorf("AddAssignees with a failed write = %v; want GitHub's error, naming the write", err)
	}
	f.FailWrite(2)
	_, err = c.RemoveAssignees(ctx, "o", "r", n, []string{"owner", "bob"})
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "unassign owner, bob from o/r#1") {
		t.Errorf("RemoveAssignees with a failed write = %v; want GitHub's error, naming the write", err)
	}
	if _, err := c.AddAssignees(ctx, "o", "r", 9, []string{"owner"}); !errors.As(err, &ge) || ge.Response.StatusCode != 404 {
		t.Errorf("AddAssignees on a missing issue = %v; want the 404", err)
	}
	f.Transfer(n, "o/elsewhere")
	if _, err := c.AddAssignees(ctx, "o", "r", n, []string{"owner"}); !errors.Is(err, github.ErrTransferred) {
		t.Errorf("AddAssignees on a transferred issue = %v; want ErrTransferred", err)
	}
	if _, err := c.RemoveAssignees(ctx, "o", "r", n, []string{"owner"}); !errors.Is(err, github.ErrTransferred) {
		t.Errorf("RemoveAssignees on a transferred issue = %v; want ErrTransferred", err)
	}
}

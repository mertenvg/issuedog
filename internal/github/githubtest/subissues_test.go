package githubtest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// Sub-issues are listed open and closed, in the order filed, then linked
// ones, two pages of 2 here, each with its repository_url.
func TestSubIssues(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "body")
	for range 3 {
		f.SetParent(f.AddIssue("owner", "Child", "body"), parent)
	}
	f.SetParent(f.AddIssue("owner", "Grandchild", "body"), 2)
	f.Close(3)
	f.LinkSubIssue(parent, "o/other", 7)
	c := f.Client(t)
	first, resp, err := c.SubIssue.ListByIssue(context.Background(), "o", "r", int64(parent), &gogithub.ListOptions{PerPage: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || resp.NextPage != 2 {
		t.Errorf("first page: %d sub-issues, next page %d, want 2 and 2", len(first), resp.NextPage)
	}
	var got []string
	for sub, err := range c.SubIssue.ListByIssueIter(context.Background(), "o", "r", int64(parent), &gogithub.ListOptions{PerPage: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		is := (*gogithub.Issue)(sub)
		_, repo, _ := strings.Cut(is.GetRepositoryURL(), "/repos/")
		got = append(got, fmt.Sprintf("%s#%d %s", repo, is.GetNumber(), is.GetState()))
	}
	if want := []string{"o/r#2 open", "o/r#3 closed", "o/r#4 open", "o/other#7 open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub-issues = %q, want %q", got, want)
	}
}

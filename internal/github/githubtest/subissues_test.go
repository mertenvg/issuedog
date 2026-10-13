package githubtest

import (
	"context"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// Sub-issues are listed open and closed, in the order filed, across pages.
func TestSubIssues(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "body")
	for range 3 {
		f.SetParent(f.AddIssue("owner", "Child", "body"), parent)
	}
	f.SetParent(f.AddIssue("owner", "Grandchild", "body"), 2)
	f.Close(3)
	var got []string
	opts := &gogithub.ListOptions{PerPage: 2}
	for sub, err := range f.Client(t).SubIssue.ListByIssueIter(context.Background(), "o", "r", int64(parent), opts) {
		if err != nil {
			t.Fatal(err)
		}
		is := (*gogithub.Issue)(sub)
		got = append(got, is.GetTitle()+" "+is.GetState())
	}
	if want := []string{"Child open", "Child closed", "Child open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub-issues = %q, want %q", got, want)
	}
}

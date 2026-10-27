package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// The issue list gives a sub-issue its parent's URL as its
// parent_issue_url, closed or open, and an issue with no parent, as once
// its link is removed, none.
func TestIssuesSinceParentURL(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "body")
	open, closed, unlinked := f.AddIssue("owner", "Open", "body"), f.AddIssue("owner", "Closed", "body"), f.AddIssue("owner", "Unlinked", "body")
	for _, n := range []int{open, closed, unlinked} {
		f.SetParent(n, parent)
	}
	f.Close(closed)
	f.UnlinkBy(unlinked, "owner")
	all, err := f.Client(t).IssuesSince(context.Background(), "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, is := range all {
		if is.ParentIssueURL != nil {
			got[is.GetNumber()] = is.GetParentIssueURL()
		}
	}
	url := fmt.Sprintf("%s/repos/o/r/issues/%d", f.srv.URL, parent)
	if want := map[int]string{open: url, closed: url}; !reflect.DeepEqual(got, want) {
		t.Errorf("parent_issue_url by issue = %v; want %v", got, want)
	}
}

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

// addSub links a sub-issue under parent through the API, and returns the
// answer's status, failing t on anything but an API error.
func addSub(t *testing.T, c *github.Client, parent int, req gogithub.SubIssueRequest) int {
	t.Helper()
	_, resp, err := c.SubIssue.Add(context.Background(), "o", "r", int64(parent), req)
	var er *gogithub.ErrorResponse
	if err != nil && !errors.As(err, &er) {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// subNumbers lists parent's sub-issues by number, two to a page.
func subNumbers(t *testing.T, c *github.Client, parent int) []int {
	t.Helper()
	var got []int
	for sub, err := range c.SubIssue.ListByIssueIter(context.Background(), "o", "r", int64(parent), &gogithub.ListOptions{PerPage: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, (*gogithub.Issue)(sub).GetNumber())
	}
	return got
}

// issueUpdatedAt reads issue n's updated_at.
func issueUpdatedAt(t *testing.T, c *github.Client, n int) time.Time {
	t.Helper()
	is, _, err := c.Issues.Get(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return is.GetUpdatedAt().Time
}

// A sub-issue linked through the API, by its ID, is listed after the
// parent's other sub-issues, in link order, not issue-number order, before
// those LinkSubIssue listed, across pages. The link moves the sub-issue's
// updated_at, not the parent's, and State lists it in the same order.
func TestAddSubIssue(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "body")
	for range 4 {
		f.AddIssue("owner", "Child", "body")
	}
	f.SetParent(4, parent)
	f.Close(2)
	f.LinkSubIssue(parent, "o/other", 7)
	c := f.Client(t)
	if got, want := f.State().Issues[parent-1].SubIssues, []int{4}; !reflect.DeepEqual(got, want) {
		t.Errorf("State sub-issues before = %v, want %v", got, want)
	}
	parentAt, childAt := issueUpdatedAt(t, c, parent), issueUpdatedAt(t, c, 3)
	for _, n := range []int{3, 2, 5} {
		if got := addSub(t, c, parent, gogithub.SubIssueRequest{SubIssueID: issueID(n)}); got != http.StatusCreated {
			t.Fatalf("link #%d: status %d, want 201", n, got)
		}
	}
	if got, want := subNumbers(t, c, parent), []int{4, 3, 2, 5, 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub-issues = %v, want %v: link order, then LinkSubIssue's", got, want)
	}
	s := f.State()
	if got, want := s.Issues[parent-1].SubIssues, []int{4, 3, 2, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("State sub-issues = %v, want %v", got, want)
	}
	for _, n := range []int{2, 3, 4, 5} {
		if s.Issues[n-1].Parent != parent || s.Issues[n-1].SubIssues != nil {
			t.Errorf("#%d: %+v, want parent #%d and no sub-issues", n, s.Issues[n-1], parent)
		}
	}
	if got := issueUpdatedAt(t, c, 3); !got.After(childAt) {
		t.Errorf("#3 updated_at %v, want after %v: the link moves it", got, childAt)
	}
	if got := issueUpdatedAt(t, c, parent); !got.Equal(parentAt) {
		t.Errorf("parent updated_at %v, want %v unchanged", got, parentAt)
	}
	p, _, err := c.SubIssue.GetParentIssue(context.Background(), "o", "r", 5)
	if err != nil || p.GetNumber() != parent {
		t.Errorf("parent of #5 = #%d, %v; want #%d", p.GetNumber(), err, parent)
	}
	// SetParent appends too, and moving an issue to another parent drops
	// it from the first one's list.
	f.SetParent(3, 0)
	f.SetParent(3, parent)
	if got, want := subNumbers(t, c, parent), []int{4, 2, 5, 3, 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub-issues after SetParent = %v, want %v", got, want)
	}
}

// Every issue has an ID apart from its number, served wherever the issue is,
// which is what a sub-issue link names it by: a number in its place links
// nothing.
func TestIssueIDs(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "body")
	child := f.AddIssue("owner", "Child", "body")
	f.SetParent(child, parent)
	ctx, c := context.Background(), f.Client(t)
	is, _, err := c.Issues.Get(ctx, "o", "r", child)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetID() == 0 || is.GetID() == int64(child) {
		t.Errorf("ID %d, want one apart from the number %d", is.GetID(), child)
	}
	all, _, err := c.Issues.ListByRepo(ctx, "o", "r", &gogithub.IssueListByRepoOptions{State: "all"})
	if err != nil || len(all) != 2 || all[1].GetID() != is.GetID() || all[0].GetID() == is.GetID() || all[0].GetID() == 0 {
		t.Errorf("issue list = %v, %v; want each issue with its own ID", all, err)
	}
	subs, _, err := c.SubIssue.ListByIssue(ctx, "o", "r", int64(parent), nil)
	if err != nil || len(subs) != 1 || (*gogithub.Issue)(subs[0]).GetID() != is.GetID() {
		t.Errorf("sub-issues = %v, %v; want the child with its ID", subs, err)
	}
	p, _, err := c.SubIssue.GetParentIssue(ctx, "o", "r", int64(child))
	if err != nil || p.GetID() != all[0].GetID() {
		t.Errorf("parent = %v, %v; want it with its ID %d", p, err, all[0].GetID())
	}
	other := f.AddIssue("owner", "Other", "body")
	if got := addSub(t, c, parent, gogithub.SubIssueRequest{SubIssueID: int64(other)}); got != http.StatusNotFound {
		t.Errorf("a number for an ID: status %d, want 404", got)
	}
}

// As on GitHub, a link is refused with a 422 for an issue that has a parent,
// the parent itself or one of its ancestors, a pull request, or a parent
// with 100 sub-issues, LinkSubIssue's included, and with a 404 for an
// unknown ID. after_id, before_id and replace_parent aren't served. Nothing
// changes on a refusal.
func TestAddSubIssueRefusals(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	top := f.AddIssue("owner", "Top", "body")
	mid := f.AddIssue("owner", "Mid", "body")
	f.SetParent(mid, top)
	elsewhere := f.AddIssue("owner", "Elsewhere", "body")
	taken := f.AddIssue("owner", "Taken", "body")
	f.SetParent(taken, elsewhere)
	pr := f.AddIssue("owner", "PR", "body")
	f.SetPullRequest(pr)
	free := f.AddIssue("owner", "Free", "body")
	full := f.AddIssue("owner", "Full", "body")
	for range maxSubIssues - 1 {
		f.SetParent(f.AddIssue("owner", "Sub", "body"), full)
	}
	f.LinkSubIssue(full, "o/other", 1)
	c := f.Client(t)
	before := f.State()
	for _, tc := range []struct {
		name   string
		parent int
		req    gogithub.SubIssueRequest
		want   int
	}{
		{"one with a parent", mid, gogithub.SubIssueRequest{SubIssueID: issueID(taken)}, http.StatusUnprocessableEntity},
		{"the parent itself", mid, gogithub.SubIssueRequest{SubIssueID: issueID(mid)}, http.StatusUnprocessableEntity},
		{"an ancestor", mid, gogithub.SubIssueRequest{SubIssueID: issueID(top)}, http.StatusUnprocessableEntity},
		{"a pull request", mid, gogithub.SubIssueRequest{SubIssueID: issueID(pr)}, http.StatusUnprocessableEntity},
		{"a 101st sub-issue", full, gogithub.SubIssueRequest{SubIssueID: issueID(free)}, http.StatusUnprocessableEntity},
		{"an unknown ID", mid, gogithub.SubIssueRequest{SubIssueID: issueID(999)}, http.StatusNotFound},
		{"after_id", mid, gogithub.SubIssueRequest{SubIssueID: issueID(free), AfterID: new(issueID(taken))}, http.StatusUnprocessableEntity},
		{"before_id", mid, gogithub.SubIssueRequest{SubIssueID: issueID(free), BeforeID: new(issueID(taken))}, http.StatusUnprocessableEntity},
		{"replace_parent", mid, gogithub.SubIssueRequest{SubIssueID: issueID(taken), ReplaceParent: new(true)}, http.StatusUnprocessableEntity},
	} {
		if got := addSub(t, c, tc.parent, tc.req); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the refusals:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	// One fewer and the 100th goes in: the 99 SetParent made, and none
	// linked from another repo.
	full2 := f.AddIssue("owner", "Nearly full", "body")
	for range maxSubIssues - 1 {
		f.SetParent(f.AddIssue("owner", "Sub", "body"), full2)
	}
	if got := addSub(t, c, full2, gogithub.SubIssueRequest{SubIssueID: issueID(free)}); got != http.StatusCreated {
		t.Errorf("a 100th sub-issue: status %d, want 201", got)
	}
}

// Replay compares sub-issues in link order: a pass that links what's
// missing, but in reverse once some are linked, links out of order after a
// crash. And a pass that links without reading first gets a 422 on its
// second pass.
func TestReplayComparesSubIssues(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *Fake {
		f := New(t, "o/r", "issuedog")
		for range 4 {
			f.AddIssue("owner", "Issue", "body")
		}
		return f
	}
	reversing := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		have := map[int]bool{}
		for sub, err := range c.SubIssue.ListByIssueIter(ctx, "o", "r", 1, nil) {
			if err != nil {
				return err
			}
			have[(*gogithub.Issue)(sub).GetNumber()] = true
		}
		var missing []int
		for _, n := range []int{2, 3, 4} {
			if !have[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) < 3 {
			slices.Reverse(missing)
		}
		for _, n := range missing {
			if _, _, err := c.SubIssue.Add(ctx, "o", "r", 1, gogithub.SubIssueRequest{SubIssueID: issueID(n)}); err != nil {
				return err
			}
		}
		return nil
	}
	problems := replay(t, setup, reversing)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") || !strings.Contains(problems[0], "SubIssues:[2 4 3]") {
		t.Errorf("problems = %q, want sub-issues out of order after write 1 applied", problems)
	}
	blind := func(f *Fake) error {
		_, _, err := f.Client(t).SubIssue.Add(context.Background(), "o", "r", 1, gogithub.SubIssueRequest{SubIssueID: issueID(2)})
		return err
	}
	problems = replay(t, setup, blind)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: second pass") || !strings.Contains(problems[0], "422") {
		t.Errorf("problems = %q, want a 422 on the second pass after write 1 applied", problems)
	}
}

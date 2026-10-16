package githubtest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// closingServed is issue n's closed events as the timeline serves them, as
// "<actor> <state_reason> <commit_id>".
func closingServed(t *testing.T, f *Fake, n int) []string {
	t.Helper()
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline?per_page=100", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []struct {
		Event       string
		Actor       *gogithub.User
		StateReason string `json:"state_reason"`
		CommitID    string `json:"commit_id"`
	}
	if _, err := c.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		if e.Event == "closed" {
			got = append(got, e.Actor.GetLogin()+" "+e.StateReason+" "+e.CommitID)
		}
	}
	return got
}

// Each closed event carries its close's state_reason, by a user or by the
// App's PATCH, and a commit's close names its commit; the commit's pull
// requests list a merged one only for a merged pull request's commit.
func TestClosingServed(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	f.CloseAs(n, "owner", "duplicate")
	f.ReopenBy(n, "owner")
	f.CloseByCommit(n, "mallory", "abc", true)
	f.ReopenBy(n, "owner")
	if f.CloseByCommit(n, "eve", "def", false) == 0 || f.CloseByCommit(n, "eve", "ghi", false) != 0 {
		t.Error("CloseByCommit gave no event, or a second one for a closed issue")
	}
	f.ReopenBy(n, "owner")
	ctx, c := context.Background(), f.Client(t)
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{State: new("closed"), StateReason: new("not_planned")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"owner duplicate ", "mallory completed abc", "eve completed def", "issuedog[bot] not_planned "}
	if got := closingServed(t, f, n); !reflect.DeepEqual(got, want) {
		t.Errorf("closed events %q; want %q", got, want)
	}
	for sha, merged := range map[string]bool{"abc": true, "def": false, "ghi": false} {
		prs, _, err := c.PullRequests.ListPullRequestsWithCommit(ctx, "o", "r", sha, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(prs) == 1 && prs[0].MergedAt != nil && prs[0].GetMergeCommitSHA() == sha; got != merged || !merged && len(prs) != 0 {
			t.Errorf("pull requests of %s: %+v; want a merged one %v", sha, prs, merged)
		}
	}
}

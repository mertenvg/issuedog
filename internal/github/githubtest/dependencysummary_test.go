package githubtest

import (
	"context"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// dependencySummaryOf is is's issue_dependencies_summary as (blocked by,
// of all, blocking, of all), and whether it has one.
func dependencySummaryOf(is *gogithub.Issue) ([4]int, bool) {
	d := is.GetIssueDependenciesSummary()
	return [4]int{d.GetBlockedBy(), d.GetTotalBlockedBy(), d.GetBlocking(), d.GetTotalBlocking()}, d != nil
}

// The fake gives each issue its issue_dependencies_summary, as GitHub
// does, in its dependency listings, both ways, and in its replies to a
// create, an edit and a blocked-by link's write and removal, counting the
// open issues on each side and all of them.
func TestFakeDependencySummaryReplies(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	above := f.AddIssue("owner", "Blocks the blocker", "body")
	blocker := f.AddIssue("owner", "Blocker", "body")
	open := f.AddIssue("owner", "Open blocked", "body")
	closed := f.AddIssue("owner", "Closed blocked", "body")
	later := f.AddIssue("owner", "Blocked later", "body")
	f.SetBlockedBy(blocker, above)
	f.SetBlockedBy(open, blocker)
	f.SetBlockedBy(closed, blocker)
	f.Close(closed)
	ctx, c := context.Background(), f.Client(t)
	check := func(name string, is *gogithub.Issue, want [4]int) {
		t.Helper()
		if got, ok := dependencySummaryOf(is); !ok || got != want {
			t.Errorf("%s: #%d's summary (blocked by, of all, blocking, of all) = %v, %v; want %v", name, is.GetNumber(), got, ok, want)
		}
	}
	blockers, _, err := c.Issues.ListBlockedBy(ctx, "o", "r", int64(open), nil)
	if err != nil || len(blockers) != 1 {
		t.Fatalf("the blocked-by listing of #%d = %d issues, %v; want #%d", open, len(blockers), err, blocker)
	}
	check("the blocked-by listing", blockers[0], [4]int{1, 1, 1, 2})
	blocked, _, err := c.Issues.ListBlocking(ctx, "o", "r", int64(blocker), nil)
	if err != nil || len(blocked) != 2 {
		t.Fatalf("the blocking listing of #%d = %d issues, %v; want #%d and #%d", blocker, len(blocked), err, open, closed)
	}
	check("the blocking listing", blocked[0], [4]int{1, 1, 0, 0})
	check("the blocking listing", blocked[1], [4]int{1, 1, 0, 0})
	edited, _, err := c.Issues.Update(ctx, "o", "r", blocker, gogithub.UpdateIssueRequest{Body: new("edited")})
	if err != nil {
		t.Fatalf("the edit of #%d: %v", blocker, err)
	}
	check("the edit's reply", edited, [4]int{1, 1, 1, 2})
	created, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "New"})
	if err != nil {
		t.Fatalf("the create: %v", err)
	}
	check("the create's reply", created, [4]int{0, 0, 0, 0})
	added, _, err := c.Issues.AddBlockedBy(ctx, "o", "r", int64(later), gogithub.IssueDependencyRequest{IssueID: issueID(blocker)})
	if err != nil {
		t.Fatalf("the link of #%d to #%d: %v", later, blocker, err)
	}
	check("the link's reply", added, [4]int{1, 1, 2, 3})
	removed, _, err := c.Issues.RemoveBlockedBy(ctx, "o", "r", int64(later), issueID(blocker))
	if err != nil {
		t.Fatalf("the removal of #%d's link to #%d: %v", later, blocker, err)
	}
	check("the removal's reply", removed, [4]int{1, 1, 1, 2})
}

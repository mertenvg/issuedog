package github_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// transferUnlistedNumbers is the numbers of issues, in order.
func transferUnlistedNumbers(issues []*gogithub.Issue) []int {
	var got []int
	for _, is := range issues {
		got = append(got, is.GetNumber())
	}
	return got
}

// TransferUnlistedOpen lists every open issue and pull request, oldest
// first, in one request with state=open, sorted by creation: never a
// closed issue, a transferred one or a deleted one (#237). A second
// listing of the same repo, unchanged, is a 304.
func TestTransferUnlistedOpen(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Open", "")                   // 1
	f.Close(f.AddIssue("owner", "Closed", ""))        // 2
	f.AddIssue("owner", "Moved", "")                  // 3
	f.SetPullRequest(f.AddIssue("owner", "A PR", "")) // 4
	f.AddIssue("owner", "Open too", "")               // 5
	f.AddIssue("owner", "Deleted", "")                // 6
	f.Transfer(3, "o/elsewhere")
	f.DeleteIssue()
	c, ctx := f.Client(t), context.Background()
	from := len(f.Requests())
	issues, err := c.TransferUnlistedOpen(ctx, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if got := transferUnlistedNumbers(issues); !slices.Equal(got, []int{1, 4, 5}) {
		t.Errorf("listed %v; want the open issues and pull request, 1, 4 and 5", got)
	}
	reqs := slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool { return strings.HasPrefix(r.Path, "/app") }) // a token's mint
	if len(reqs) != 1 || reqs[0].Path != "/issues" || reqs[0].Query != "direction=asc&per_page=100&sort=created&state=open" {
		t.Errorf("requests %+v; want one listing of the open issues by creation, oldest first, 100 a page", reqs)
	}
	notModified := f.NotModified()
	again, err := c.TransferUnlistedOpen(ctx, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if got := transferUnlistedNumbers(again); !slices.Equal(got, []int{1, 4, 5}) || f.NotModified() != notModified+1 {
		t.Errorf("the second listing gave %v with %d 304s; want 1, 4 and 5 again, a 304", got, f.NotModified()-notModified)
	}
}

// A listing of more than 100 open issues pages, and gives each of them
// once, in order.
func TestTransferUnlistedOpenPages(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	var want []int
	for range 150 {
		want = append(want, f.AddIssue("owner", "Open", ""))
	}
	issues, err := f.Client(t).TransferUnlistedOpen(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if got := transferUnlistedNumbers(issues); !slices.Equal(got, want) {
		t.Errorf("listed %v; want 1 to 150, in order", got)
	}
}

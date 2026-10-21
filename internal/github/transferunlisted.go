package github

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// TransferUnlistedOpen lists every open issue and pull request of
// owner/repo, oldest first, for the daemon's check for the issues it
// holds that are no longer in the repo, such as a transferred one, which
// drops out of the repo's listings (SPEC §8.1, #237). Its pages are
// conditional, as every GET is (etagCache), so a page where nothing
// changed costs a 304. It pages by creation, so an issue opened while it
// pages goes at the end and none is skipped; one closed meanwhile may
// make it skip the next, which the check then only reads again.
func (c *Client) TransferUnlistedOpen(ctx context.Context, owner, repo string) ([]*gogithub.Issue, error) {
	opts := &gogithub.IssueListByRepoOptions{
		State: "open", Sort: "created", Direction: "asc",
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	issues, err := collect(c.Issues.ListByRepoIter(ctx, owner, repo, opts))
	if err != nil {
		return nil, fmt.Errorf("list the open issues of %s/%s: %w", owner, repo, err)
	}
	return issues, nil
}

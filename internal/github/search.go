package github

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// readyBlockedQuery is the issue search for owner/repo's open issues at
// stage:ready that have an open blocker (8.1). The label is quoted, as its
// name has a colon.
func readyBlockedQuery(owner, repo string) string {
	return fmt.Sprintf(`repo:%s/%s is:issue is:open label:"stage:ready" is:blocked`, owner, repo)
}

// ReadyBlocked lists the open issues of owner/repo at stage:ready that the
// issue search's `is:blocked` matches, as a set of their numbers: those with
// an open blocked-by link (8.1). It asks advanced search, which `is:blocked`
// is a qualifier of, reading every page, 100 to a page, through the client's
// ETag cache, as IssuesSince reads, so a repeat with nothing changed is a 304
// for each page. It sorts by creation, oldest first, as best match, the
// default, can move an issue from one page to another between reads, so
// one could be missed on both. A pull request in the answer is skipped,
// though `is:issue` leaves them out. Search lags behind dependency writes
// and closes, so a caller confirms a candidate through Blockers before
// acting on it (8.1).
func (c *Client) ReadyBlocked(ctx context.Context, owner, repo string) (map[int]bool, error) {
	q := readyBlockedQuery(owner, repo)
	opts := &gogithub.SearchOptions{Sort: "created", Order: "asc", AdvancedSearch: new(true), ListOptions: gogithub.ListOptions{PerPage: 100}}
	blocked := map[int]bool{}
	for {
		res, resp, err := c.Search.Issues(ctx, q, opts)
		if err != nil {
			return nil, fmt.Errorf("search %s/%s for blocked ready tasks, page %d: %w", owner, repo, max(opts.Page, 1), err)
		}
		for _, is := range res.Issues {
			if !is.IsPullRequest() {
				blocked[is.GetNumber()] = true
			}
		}
		if resp.NextPage == 0 {
			return blocked, nil
		}
		opts.Page = resp.NextPage
	}
}

package github

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"slices"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// The *Since helpers poll (§8.1). Pass the newest time seen, so repeats are 304s.

// IssuesSince lists all issues and PRs updated at or after since, oldest first.
func (c *Client) IssuesSince(ctx context.Context, owner, repo string, since time.Time) ([]*gogithub.Issue, error) {
	opts := &gogithub.IssueListByRepoOptions{
		State: "all", Sort: "updated", Direction: "asc", Since: since,
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	issues, err := collect(c.Issues.ListByRepoIter(ctx, owner, repo, opts))
	if err != nil {
		return nil, fmt.Errorf("list issues of %s/%s since %s: %w", owner, repo, since.Format(time.RFC3339), err)
	}
	return issues, nil
}

// CommentsSince lists comments updated since then on one issue, or all if
// number is 0, the newest first. The list pages by number, and a comment
// edited while it pages moves to the front, so a later page repeats a
// comment rather than skipping one, as oldest first would.
func (c *Client) CommentsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]*gogithub.IssueComment, error) {
	opts := &gogithub.IssueListCommentsOptions{
		Sort: new("updated"), Direction: new("desc"), Since: &since,
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	comments, err := collect(c.Issues.ListCommentsIter(ctx, owner, repo, number, opts))
	if err != nil {
		return nil, fmt.Errorf("list comments of %s/%s#%d since %s: %w", owner, repo, number, since.Format(time.RFC3339), err)
	}
	return comments, nil
}

// TimelineSince lists timeline events from since on, filtered here as the endpoint
// has no since=. Reviews use submitted_at; untimed events, like commits, are left out.
func (c *Client) TimelineSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]*gogithub.Timeline, error) {
	all, err := collect(c.Issues.ListIssueTimelineIter(ctx, owner, repo, number, &gogithub.ListOptions{PerPage: 100}))
	if err != nil {
		return nil, fmt.Errorf("list timeline of %s/%s#%d: %w", owner, repo, number, err)
	}
	return slices.DeleteFunc(all, func(e *gogithub.Timeline) bool {
		at := cmp.Or(e.CreatedAt, e.SubmittedAt)
		return at == nil || at.Before(since)
	}), nil
}

// collect drains a go-github page iterator.
func collect[T any](seq iter.Seq2[T, error]) ([]T, error) {
	var items []T
	for item, err := range seq {
		if err != nil {
			return nil, fmt.Errorf("next page: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

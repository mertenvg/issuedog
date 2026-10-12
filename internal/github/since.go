package github

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// The *Since helpers poll with since= (SPEC §8.1) through the ETag cache.
// Pass the newest timestamp already seen, not the poll's start time, so an
// unchanged poll repeats its URLs and costs nothing.

// IssuesSince lists the repo's issues and PRs, open and closed, updated at
// or after since, oldest update first.
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

// CommentsSince lists comments updated at or after since, on one issue, or
// on every issue in the repo if number is 0.
func (c *Client) CommentsSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]*gogithub.IssueComment, error) {
	opts := &gogithub.IssueListCommentsOptions{
		Sort: new("updated"), Direction: new("asc"), Since: &since,
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	comments, err := collect(c.Issues.ListCommentsIter(ctx, owner, repo, number, opts))
	if err != nil {
		return nil, fmt.Errorf("list comments of %s/%s#%d since %s: %w", owner, repo, number, since.Format(time.RFC3339), err)
	}
	return comments, nil
}

// TimelineSince lists an issue's timeline events created at or after since.
// The timeline endpoint takes no since=, so this filters after fetching and
// relies on the ETag cache to keep unchanged pages free. Events without a
// creation time, such as commits, are left out.
func (c *Client) TimelineSince(ctx context.Context, owner, repo string, number int, since time.Time) ([]*gogithub.Timeline, error) {
	all, err := collect(c.Issues.ListIssueTimelineIter(ctx, owner, repo, number, &gogithub.ListOptions{PerPage: 100}))
	if err != nil {
		return nil, fmt.Errorf("list timeline of %s/%s#%d: %w", owner, repo, number, err)
	}
	return slices.DeleteFunc(all, func(e *gogithub.Timeline) bool {
		return e.CreatedAt == nil || e.CreatedAt.Before(since)
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

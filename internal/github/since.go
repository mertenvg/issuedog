package github

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strconv"
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
	events, err := c.issueTypeRevertTimeline(ctx, owner, repo, number, since)
	if err != nil {
		return nil, err
	}
	timeline := make([]*gogithub.Timeline, 0, len(events))
	for _, e := range events {
		timeline = append(timeline, &e.Timeline)
	}
	return timeline, nil
}

// issueTypeRevertEvent is one timeline event as REST gives it, with the
// issue types of an issue-type event, which go-github's Timeline leaves out.
// GitHub's REST description (github/rest-api-description, the
// issue-type-*-issue-event schemas) gives issue_type_added an issue_type,
// issue_type_changed an issue_type and a prev_issue_type, and
// issue_type_removed a prev_issue_type, each with an id and a name.
type issueTypeRevertEvent struct {
	gogithub.Timeline
	// IssueType is the type an issue_type_added or issue_type_changed event
	// set, or nil.
	IssueType *gogithub.IssueType `json:"issue_type,omitempty"`
	// PrevIssueType is the type an issue_type_changed or issue_type_removed
	// event replaced, or nil.
	PrevIssueType *gogithub.IssueType `json:"prev_issue_type,omitempty"`
	// NodeID is the event's GraphQL node ID, which go-github's Timeline
	// leaves out too, or "" (Closing).
	NodeID string `json:"node_id,omitempty"`
}

// issueTypeRevertTimeline is TimelineSince, with each event's issue types.
// It pages the endpoint as go-github's ListIssueTimelineIter does. Its
// Accept header isn't go-github's, though: both ask for the timeline's
// mockingbird-preview, but this sends the projects' inertia-preview where
// go-github sends the project cards' starfox-preview. Either touches only
// the project fields of the timeline's events, which nothing here reads.
func (c *Client) issueTypeRevertTimeline(ctx context.Context, owner, repo string, number int, since time.Time) ([]*issueTypeRevertEvent, error) {
	wrap := func(err error) error { return fmt.Errorf("list timeline of %s/%s#%d: %w", owner, repo, number, err) }
	var all []*issueTypeRevertEvent
	for page := 0; ; {
		u := fmt.Sprintf("repos/%v/%v/issues/%v/timeline?per_page=100", owner, repo, number)
		if page > 0 {
			u += "&page=" + strconv.Itoa(page)
		}
		req, err := c.NewRequest(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, wrap(err)
		}
		req.Header.Set("Accept", "application/vnd.github.mockingbird-preview+json, application/vnd.github.inertia-preview+json")
		var events []*issueTypeRevertEvent
		resp, err := c.Do(req, &events)
		if err != nil {
			return nil, wrap(fmt.Errorf("next page: %w", err))
		}
		all = append(all, events...)
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return slices.DeleteFunc(all, func(e *issueTypeRevertEvent) bool {
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

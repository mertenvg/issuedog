package github

import (
	"context"
	"fmt"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// NewChild is an issue for CreateChild to file. Its title and body are
// written as given, so text the App didn't write must be escaped first
// (SPEC §5.1).
type NewChild struct {
	Title, Body string
	// Labels are added with the issue, each as the App's change.
	Labels []string
	// Type is the org's issue type to set, by name, on a repo with issue
	// types, or "" for none, which sends no type.
	Type string
}

// CreateChild files child as the App and returns the new issue, whose ID
// is the one AddSubIssue and AddBlockedBy take. Its labels and type are
// set with it, in the one request. It isn't idempotent: a caller that may
// replay looks for the issue before it creates one (SPEC §5.2).
func (c *Client) CreateChild(ctx context.Context, owner, repo string, child NewChild) (*gogithub.Issue, error) {
	req := gogithub.CreateIssueRequest{Title: child.Title, Body: &child.Body, Labels: child.Labels}
	if child.Type != "" {
		req.Type = &child.Type
	}
	is, _, err := c.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		return nil, fmt.Errorf("create issue %q in %s/%s: %w", child.Title, owner, repo, err)
	}
	if is.GetNumber() == 0 {
		return nil, fmt.Errorf("create issue %q in %s/%s: %w", child.Title, owner, repo, errNoNumber)
	}
	return is, nil
}

// SubIssues lists parent's sub-issues, open and closed, with their bodies
// and authors, in the order GitHub lists them, which is the order they were
// linked, reading every page. A sub-issue may be in another repo of the
// same owner: its repository_url names it, and its number is that repo's.
// A repeat of an unchanged listing is a 304, which costs no rate limit.
func (c *Client) SubIssues(ctx context.Context, owner, repo string, parent int) ([]*gogithub.Issue, error) {
	opts := &gogithub.ListOptions{PerPage: 100}
	subs, err := collect(c.SubIssue.ListByIssueIter(ctx, owner, repo, int64(parent), opts))
	if err != nil {
		return nil, fmt.Errorf("list sub-issues of %s/%s#%d: %w", owner, repo, parent, err)
	}
	issues := make([]*gogithub.Issue, 0, len(subs))
	for _, s := range subs {
		issues = append(issues, (*gogithub.Issue)(s))
	}
	return issues, nil
}

// SubIssueRemoval is a sub_issue_removed event on a parent's timeline:
// who removed the link of which sub-issue below it, and when.
type SubIssueRemoval struct {
	ID int64 // the timeline event's ID
	// Repo is the sub-issue's repository, owner/name, and Number its
	// number there.
	Repo   string
	Number int
	By     Author
	At     time.Time
}

// SubIssueRemovals reads parent's whole timeline once, and returns its
// sub_issue_removed events, oldest first, for an epic's close (SPEC
// §7.3). GitHub's REST timeline gives a sub_issue_added event its actor
// and its sub_issue, with the sub-issue's number and repository (checked
// live on this repo's #509, in #419), and GraphQL's SubIssueRemovedEvent
// has an actor and a subIssue too, so a removal is read as that shape:
// SPEC §16 lists it to confirm live. An event that names no sub-issue
// isn't returned.
func (c *Client) SubIssueRemovals(ctx context.Context, owner, repo string, parent int) ([]SubIssueRemoval, error) {
	events, err := c.issueTypeRevertTimeline(ctx, owner, repo, parent, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("sub-issue links removed from %s/%s#%d: %w", owner, repo, parent, err)
	}
	var removals []SubIssueRemoval
	for _, e := range events {
		if e.GetEvent() != "sub_issue_removed" || e.SubIssue == nil || e.SubIssue.Number <= 0 {
			continue
		}
		removals = append(removals, SubIssueRemoval{ID: e.GetID(), Repo: e.SubIssue.Repository.FullName, Number: e.SubIssue.Number,
			By: authorOf(e.GetActor()), At: e.GetCreatedAt().Time})
	}
	return removals, nil
}

// AddSubIssue links the issue whose ID is childID (not its number) under
// parent, after its other sub-issues. It isn't idempotent: GitHub refuses
// an issue that has a parent already with a 422, which the error wraps as a
// *gogithub.ErrorResponse, so a caller that may replay reads the parent's
// sub-issues first.
func (c *Client) AddSubIssue(ctx context.Context, owner, repo string, parent int, childID int64) error {
	if _, _, err := c.SubIssue.Add(ctx, owner, repo, int64(parent), gogithub.SubIssueRequest{SubIssueID: childID}); err != nil {
		return fmt.Errorf("link issue ID %d under %s/%s#%d: %w", childID, owner, repo, parent, err)
	}
	return nil
}

// BlockedBy lists the issues that block issue number, its blocked-by
// dependencies, open and closed, reading every page. Each has its state, so
// a caller can tell which blockers are still open. A blocker may be in
// another repo: its repository_url names it, and its number is that repo's.
// A repeat of an unchanged listing is a 304, which costs no rate limit.
func (c *Client) BlockedBy(ctx context.Context, owner, repo string, number int) ([]*gogithub.Issue, error) {
	opts := &gogithub.ListOptions{PerPage: 100}
	blockers, err := collect(c.Issues.ListBlockedByIter(ctx, owner, repo, int64(number), opts))
	if err != nil {
		return nil, fmt.Errorf("list issues blocking %s/%s#%d: %w", owner, repo, number, err)
	}
	return blockers, nil
}

// AddBlockedBy makes issue number blocked by the issue whose ID is
// blockerID (not its number). It isn't idempotent: GitHub refuses a link
// the issue has already with a 422, which the error wraps as a
// *gogithub.ErrorResponse, so a caller that may replay reads BlockedBy
// first.
func (c *Client) AddBlockedBy(ctx context.Context, owner, repo string, number int, blockerID int64) error {
	if _, _, err := c.Issues.AddBlockedBy(ctx, owner, repo, int64(number), gogithub.IssueDependencyRequest{IssueID: blockerID}); err != nil {
		return fmt.Errorf("add issue ID %d as blocking %s/%s#%d: %w", blockerID, owner, repo, number, err)
	}
	return nil
}

// Blocking lists the issues that issue number blocks, the ones it's a
// blocked-by dependency of, open and closed, reading every page: BlockedBy
// the other way round. A blocked issue may be in another repo: its
// repository_url names it, and its number is that repo's. A repeat of an
// unchanged listing is a 304, which costs no rate limit.
func (c *Client) Blocking(ctx context.Context, owner, repo string, number int) ([]*gogithub.Issue, error) {
	opts := &gogithub.ListOptions{PerPage: 100}
	blocked, err := collect(c.Issues.ListBlockingIter(ctx, owner, repo, int64(number), opts))
	if err != nil {
		return nil, fmt.Errorf("list issues blocked by %s/%s#%d: %w", owner, repo, number, err)
	}
	return blocked, nil
}

// RemoveBlockedBy removes the link that makes issue number blocked by the
// issue whose ID is blockerID (not its number), so it's gone from both
// listings. It isn't idempotent: GitHub refuses a link that's gone, with a
// 404 in the fake, which the error wraps as a *gogithub.ErrorResponse, so a
// caller that may replay reads BlockedBy, or the blocker's Blocking, first.
func (c *Client) RemoveBlockedBy(ctx context.Context, owner, repo string, number int, blockerID int64) error {
	if _, _, err := c.Issues.RemoveBlockedBy(ctx, owner, repo, int64(number), blockerID); err != nil {
		return fmt.Errorf("remove issue ID %d as blocking %s/%s#%d: %w", blockerID, owner, repo, number, err)
	}
	return nil
}

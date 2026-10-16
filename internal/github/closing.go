package github

import (
	"context"
	"fmt"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// Closing is one closed or reopened event on an issue's timeline (SPEC
// §6.4, §7.2, plan #87 rev 2, T6). The daemon judges an issue's newest one:
// who closed it and how, or who reopened it.
type Closing struct {
	ID     int64 // the timeline event's ID
	Reopen bool  // a reopened event, else a closed one
	By     Author
	At     time.Time
	// StateReason is a closed event's state_reason, such as "completed",
	// "not_planned" or "duplicate", as GitHub's REST timeline gives it, or
	// "" where it gives none, as on events from before GitHub had state
	// reasons (SPEC §16).
	StateReason string
	// CommitID is the commit a closed event names, as when a pull request
	// that says "Closes #N", or a commit pushed with it, closed the issue;
	// "" for none. Only a merged pull request's close stands for it
	// (ClosingMerged).
	CommitID string
}

// closingOf is e as a Closing, and whether it is a closed or reopened
// event.
func closingOf(e *issueTypeRevertEvent) (Closing, bool) {
	switch e.GetEvent() {
	case "closed", "reopened":
	default:
		return Closing{}, false
	}
	c := Closing{ID: e.GetID(), Reopen: e.GetEvent() == "reopened", By: authorOf(e.GetActor()), At: e.GetCreatedAt().Time,
		CommitID: e.GetCommitID()}
	if e.StateReason != nil {
		c.StateReason = *e.StateReason
	}
	return c, true
}

// ClosingTimeline reads an issue's whole timeline once, for a closed
// issue's close (T6): it returns the timeline, for the label rules'
// judgement of a closed stage's label (StandingLabelChange), and its
// closed and reopened events, oldest first, with their state_reason and
// commit_id, which go-github's Timeline leaves out.
func (c *Client) ClosingTimeline(ctx context.Context, owner, repo string, number int) ([]*gogithub.Timeline, []Closing, error) {
	events, err := c.issueTypeRevertTimeline(ctx, owner, repo, number, time.Time{})
	if err != nil {
		return nil, nil, fmt.Errorf("closes of %s/%s#%d: %w", owner, repo, number, err)
	}
	timeline := make([]*gogithub.Timeline, 0, len(events))
	var closings []Closing
	for _, e := range events {
		timeline = append(timeline, &e.Timeline)
		if cl, ok := closingOf(e); ok {
			closings = append(closings, cl)
		}
	}
	return timeline, closings, nil
}

// ClosingMerged reports whether commit sha is a merged pull request's, in
// owner/repo: whether GitHub lists a merged pull request with it (GET
// /repos/{owner}/{repo}/commits/{sha}/pulls). That is how a close by a
// merged pull request is told from one by a commit pushed with "Closes
// #N" and no pull request: the closed event names the commit either way
// (SPEC §16). It is one request, which the caller remembers.
func (c *Client) ClosingMerged(ctx context.Context, owner, repo, sha string) (bool, error) {
	prs, err := collect(c.PullRequests.ListPullRequestsWithCommitIter(ctx, owner, repo, sha, &gogithub.ListOptions{PerPage: 100}))
	if err != nil {
		return false, fmt.Errorf("pull requests of commit %s in %s/%s: %w", sha, owner, repo, err)
	}
	for _, pr := range prs {
		if pr.MergedAt != nil && !pr.GetMergedAt().IsZero() {
			return true, nil
		}
	}
	return false, nil
}

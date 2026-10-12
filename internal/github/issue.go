package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// Snapshot is one issue as the daemon holds it in memory (plan #5, assumption 2).
type Snapshot struct {
	Number int
	Title  string
	Body   string
	// BodyEdit is the body's last edit.
	BodyEdit  Edit
	Author    Author
	CreatedAt time.Time
	UpdatedAt time.Time
	Labels    []string
	// Type is the issue type's name or else the first type:* label's
	// suffix, in lower case, and empty if there is neither (SPEC §4).
	Type string
	// Parent is the parent issue's number, or 0 if there is none.
	Parent int
	// Comments are all the issue's comments, oldest first.
	Comments []Comment
}

// Comment is one issue comment.
type Comment struct {
	ID        int64
	Author    Author
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Edit is the body's last edit.
	Edit Edit
}

// Reaction is one reaction to a comment.
type Reaction struct {
	ID int64
	// User is who reacted.
	User Author
	// Content is the emoji name, such as "+1".
	Content   string
	CreatedAt time.Time
}

// ErrPullRequest is wrapped by Snapshot's error for a pull request.
var ErrPullRequest = errors.New("a pull request, not an issue")

// Snapshot reads one issue, not a pull request: the issue, its labels, type
// and parent, every comment, and who last edited the body and each comment.
// Bodies come from the same read as their edits, so they match, and a comment
// deleted between the reads is left out.
func (c *Client) Snapshot(ctx context.Context, owner, repo string, number int) (*Snapshot, error) {
	issue, _, err := c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, err)
	}
	if issue.IsPullRequest() {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, ErrPullRequest)
	}
	s := &Snapshot{
		Number: issue.GetNumber(), Title: issue.GetTitle(),
		Author:    authorOf(issue.GetUser()),
		CreatedAt: issue.GetCreatedAt().Time, UpdatedAt: issue.GetUpdatedAt().Time,
		Type: strings.ToLower(issue.GetType().GetName()),
	}
	for _, l := range issue.Labels {
		s.Labels = append(s.Labels, l.GetName())
		if name, ok := strings.CutPrefix(strings.ToLower(l.GetName()), "type:"); ok && s.Type == "" {
			s.Type = name
		}
	}
	parent, resp, err := c.SubIssue.GetParentIssue(ctx, owner, repo, int64(number))
	switch {
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		// No parent.
	case err != nil:
		return nil, fmt.Errorf("parent of %s/%s#%d: %w", owner, repo, number, err)
	default:
		s.Parent = parent.GetNumber()
	}
	comments, err := c.CommentsSince(ctx, owner, repo, number, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, err)
	}
	body, edits, err := c.edits(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, err)
	}
	s.Body, s.BodyEdit = body.Body, body.Edit
	for _, cm := range comments {
		e, ok := edits[cm.GetID()]
		if !ok {
			continue // deleted since
		}
		s.Comments = append(s.Comments, Comment{
			ID: cm.GetID(), Author: authorOf(cm.GetUser()), Body: e.Body,
			CreatedAt: cm.GetCreatedAt().Time, UpdatedAt: cm.GetUpdatedAt().Time, Edit: e.Edit,
		})
	}
	// IDs grow with time, and CommentsSince sorts by update.
	slices.SortFunc(s.Comments, func(a, b Comment) int { return cmp.Compare(a.ID, b.ID) })
	return s, nil
}

// Reactions lists the reactions to one issue comment, oldest first. It hides
// the embedded c.Client.Reactions service, which stays reachable by that name.
func (c *Client) Reactions(ctx context.Context, owner, repo string, commentID int64) ([]Reaction, error) {
	opts := &gogithub.ListReactionOptions{ListOptions: gogithub.ListOptions{PerPage: 100}}
	all, err := collect(c.Client.Reactions.ListIssueCommentReactionsIter(ctx, owner, repo, commentID, opts))
	if err != nil {
		return nil, fmt.Errorf("list reactions to comment %d in %s/%s: %w", commentID, owner, repo, err)
	}
	reactions := make([]Reaction, 0, len(all))
	for _, r := range all {
		reactions = append(reactions, Reaction{
			ID: r.GetID(), User: authorOf(r.GetUser()), Content: r.GetContent(), CreatedAt: r.GetCreatedAt().Time,
		})
	}
	return reactions, nil
}

func authorOf(u *gogithub.User) Author {
	return Author{Login: u.GetLogin(), Type: u.GetType()}
}

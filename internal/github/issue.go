package github

import (
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
	Number    int
	Closed    bool
	Title     string
	Body      string
	BodyEdit  Edit   // the body's last edit
	BodyNode  string // the issue's GraphQL node ID, for History
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
	// SettingsEvent is the ID of the newest change to a model or effort
	// label that stands, or 0: not read with the issue, but set when the
	// daemon checks its labels (CheckLabels).
	SettingsEvent int64

	// TypeFromLabels is set when Type comes from the labels, as the issue has
	// no issue type, so SetLabels keeps it up to date with them.
	TypeFromLabels bool
	// LeftOut are the labels, in lower case, on the issue but left out of
	// Labels, as the label rules can't explain them yet (CheckLabels), so
	// nothing acts on them. SetLabels keeps them out.
	LeftOut map[string]bool
}

// Comment is one issue comment.
type Comment struct {
	ID        int64
	Node      string // the GraphQL node ID, for History
	Author    Author
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
	Edit      Edit // the body's last edit
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

// ErrTransferred is wrapped by Snapshot's error for an issue moved to
// another repo.
var ErrTransferred = errors.New("transferred")

// Snapshot reads one issue, not a pull request: its title, author, labels,
// type and parent from REST, then its body and every comment, each with who
// last edited it, from GraphQL.
func (c *Client) Snapshot(ctx context.Context, owner, repo string, number int) (*Snapshot, error) {
	issue, _, err := c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, err)
	}
	if issue.IsPullRequest() {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, ErrPullRequest)
	}
	// GitHub answers for a transferred issue with a 301, which the client
	// follows to the issue's new repo.
	if u := issue.GetRepositoryURL(); u != "" && !strings.HasSuffix(strings.ToLower(u), strings.ToLower("/repos/"+owner+"/"+repo)) {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w to %s", owner, repo, number, ErrTransferred, u)
	}
	s := &Snapshot{
		Number: issue.GetNumber(), Closed: issue.GetState() == "closed", Title: issue.GetTitle(),
		Author:    authorOf(issue.GetUser()),
		CreatedAt: issue.GetCreatedAt().Time, UpdatedAt: issue.GetUpdatedAt().Time,
		Type: TypeOf(issue), TypeFromLabels: issue.GetType().GetName() == "",
	}
	for _, l := range issue.Labels {
		s.Labels = append(s.Labels, l.GetName())
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
	if err := c.readComments(ctx, owner, repo, s); err != nil {
		return nil, fmt.Errorf("snapshot of %s/%s#%d: %w", owner, repo, number, err)
	}
	return s, nil
}

// TypeOf is Snapshot.Type for an issue as REST gives it.
func TypeOf(issue *gogithub.Issue) string {
	if t := issue.GetType().GetName(); t != "" {
		return strings.ToLower(t)
	}
	var labels []string
	for _, l := range issue.Labels {
		labels = append(labels, l.GetName())
	}
	return TypeOfLabels(labels)
}

// TypeOfLabels is the first type:* label's suffix, in lower case, or "": an
// issue's type when it has no issue type (TypeOf).
func TypeOfLabels(labels []string) string {
	for _, l := range labels {
		if name, ok := strings.CutPrefix(strings.ToLower(l), "type:"); ok {
			return name
		}
	}
	return ""
}

// SetLabels sets s.Labels to labels, without those in s.LeftOut, and s.Type
// with them when it came from a type:* label rather than the issue type, so
// a type label removed or left out gives no type. Every change to s.Labels
// goes through it, so a label left out stays out, however the labels are
// read again.
func (s *Snapshot) SetLabels(labels []string) {
	s.Labels = slices.DeleteFunc(slices.Clone(labels), func(l string) bool { return s.LeftOut[strings.ToLower(l)] })
	if s.TypeFromLabels {
		s.Type = TypeOfLabels(s.Labels)
	}
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

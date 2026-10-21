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

// Snapshot is one issue as the daemon holds it in memory (SPEC §5).
type Snapshot struct {
	Number   int
	Closed   bool
	Title    string
	Body     string
	BodyEdit Edit   // the body's last edit
	BodyNode string // the issue's GraphQL node ID, for History
	Author   Author
	// AuthorAssociation is the issue author's authorAssociation with the
	// repo, from REST, as Comment.Association is.
	AuthorAssociation string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Labels            []string
	// Assignees are the issue's assignees' logins, in GitHub's order, read
	// from the issue GET Snapshot makes anyway (#191). They're never state
	// (SPEC §5): only assignment reads them, to write the difference.
	Assignees []string
	// Type is the issue type's name or else the first type:* label's
	// suffix, in lower case, and empty if there is neither (SPEC §4).
	Type string
	// Parent is the parent issue's number, or 0 if there is none.
	Parent int
	// Comments are all the issue's comments, oldest first.
	Comments []Comment
	// Deleted are the issue's CommentDeletedEvents, oldest first: read
	// with the comments, in the same GraphQL query (#186).
	Deleted []DeletedComment
	// SettingsEvent is the ID of the newest change to a model or effort
	// label that stands, or 0: not read with the issue, but set when the
	// daemon checks its labels (CheckLabels).
	SettingsEvent int64
	// TypeEvent is the ID of the type change that stands
	// (LabelCheck.TypeEvent), or 0: like SettingsEvent, set when the daemon
	// checks its labels. It keys a type that doesn't fit the issue's stage
	// (SPEC §5.3).
	TypeEvent int64
	// DiscRenames are the issue's renamed events (LabelCheck.DiscRenames),
	// oldest first: like SettingsEvent, not read with the issue, but set
	// when the daemon checks its labels, for its discussion triggers (SPEC
	// §6.4). Nil until then.
	DiscRenames []DiscRename
	// DiscLabelTimes are LabelCheck.DiscLabelTimes, set with DiscRenames.
	DiscLabelTimes map[int64]time.Time
	// Closings are LabelCheck.Closings, the issue's closed and reopened
	// events, set with DiscRenames: for a close's discussion trigger, and
	// for the judgement of a reopen (SPEC §6.4). Nil until then.
	Closings []Closing

	// TypeFromLabels is set when Type comes from the labels, as the issue has
	// no issue type, so SetLabels keeps it up to date with them.
	TypeFromLabels bool
	// TypeName is the issue type's name as GitHub shows it, such as "Epic",
	// and TypeID its ID, or "" and 0 when the issue has none (TypeFromLabels).
	// Type is TypeName in lower case. SetIssueType sets all three.
	TypeName string
	TypeID   int64
	// LeftOut are the labels, in lower case, on the issue but left out of
	// Labels, as the label rules can't explain them yet (CheckLabels), so
	// nothing acts on them. SetLabels keeps them out.
	LeftOut map[string]bool
	// TypeLeftOut is the issue type on the issue that the label rules left
	// out, as they can't explain it yet, or can't revert it, as the org no
	// longer lists the type that stands (IssueTypeLeftOut.Gone), with the
	// type that stands, which Type, TypeName and TypeID are instead
	// (LeaveOutType), or nil.
	TypeLeftOut *IssueTypeLeftOut
}

// SetIssueType sets s's issue type to name, as GitHub shows it, with its
// ID: Type, TypeName, TypeID and TypeFromLabels. With no name, the issue has
// no issue type, so Type comes from the labels, as SetLabels keeps it.
func (s *Snapshot) SetIssueType(name string, id int64) {
	s.TypeName, s.TypeID, s.TypeFromLabels = name, id, name == ""
	s.Type = strings.ToLower(name)
	if s.TypeFromLabels {
		s.Type = TypeOfLabels(s.Labels)
	}
}

// LeaveOutType leaves out of s the issue type lo, which the label rules
// can't explain yet (LabelCheck.IssueTypeLeftOut): s takes the type that
// stands instead, and keeps lo in TypeLeftOut, so RestoreType can give s
// its own type back. A type left out already is given back first.
func (s *Snapshot) LeaveOutType(lo IssueTypeLeftOut) {
	s.RestoreType()
	s.TypeLeftOut = &lo
	s.SetIssueType(lo.To, lo.ToID)
}

// RestoreType gives s back the issue type LeaveOutType left out, if any:
// the issue's own.
func (s *Snapshot) RestoreType() {
	if lo := s.TypeLeftOut; lo != nil {
		s.TypeLeftOut = nil
		s.SetIssueType(lo.Name, lo.ID)
	}
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
	// Association is GitHub's authorAssociation of the comment's author
	// with the repo, such as COLLABORATOR or NONE, as of the read: only a
	// discussion trigger reads it, to skip the permission request for an
	// author with no access (DiscOutsider).
	Association string
}

// DiscOutsider reports whether association, an authorAssociation, says
// the author has no access to the repo: not its owner, a member of its
// org, nor a collaborator, any of which a write collaborator is. So
// their permission needn't be asked for, which GitHub answers with a 404
// that no conditional request saves (SPEC §8.1). "" isn't known, and
// isn't one.
func DiscOutsider(association string) bool {
	switch association {
	case "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR", "MANNEQUIN", "NONE":
		return true
	}
	return false
}

// Reaction is one reaction to a comment or to an issue itself.
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

// ErrTransferred is wrapped by the error of Snapshot, of the assignee
// writes (assignAnswer) and of PinIssue, for an issue moved to another
// repo. The daemon's digest edits wrap it too (digestMoved).
var ErrTransferred = errors.New("transferred")

// Snapshot reads one issue, not a pull request: its title, author, labels,
// type and parent from REST, then its body, every comment, each with who
// last edited it, and every deleted comment, from GraphQL.
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
		Author: authorOf(issue.GetUser()), AuthorAssociation: issue.GetAuthorAssociation(),
		CreatedAt: issue.GetCreatedAt().Time, UpdatedAt: issue.GetUpdatedAt().Time,
		Type: TypeOf(issue), TypeFromLabels: issue.GetType().GetName() == "",
		TypeName: issue.GetType().GetName(), TypeID: issue.GetType().GetID(),
	}
	for _, l := range issue.Labels {
		s.Labels = append(s.Labels, l.GetName())
	}
	s.Assignees = assignLogins(issue.Assignees)
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
	return reactionsOf(all), nil
}

// IssueReactions lists the reactions to issue number itself, not to its
// comments, oldest first, as Reactions does for a comment. A repeat of an
// unchanged listing is a 304, which costs no rate limit. A reaction doesn't
// move the issue's updated_at, so only this read sees a new one.
func (c *Client) IssueReactions(ctx context.Context, owner, repo string, number int) ([]Reaction, error) {
	opts := &gogithub.ListReactionOptions{ListOptions: gogithub.ListOptions{PerPage: 100}}
	all, err := collect(c.Client.Reactions.ListIssueReactionsIter(ctx, owner, repo, number, opts))
	if err != nil {
		return nil, fmt.Errorf("list reactions to %s/%s#%d: %w", owner, repo, number, err)
	}
	return reactionsOf(all), nil
}

// reactionsOf turns GitHub's reactions into Reactions, in the same order.
func reactionsOf(all []*gogithub.Reaction) []Reaction {
	reactions := make([]Reaction, 0, len(all))
	for _, r := range all {
		reactions = append(reactions, Reaction{
			ID: r.GetID(), User: authorOf(r.GetUser()), Content: r.GetContent(), CreatedAt: r.GetCreatedAt().Time,
		})
	}
	return reactions
}

func authorOf(u *gogithub.User) Author {
	return Author{Login: u.GetLogin(), Type: u.GetType()}
}

package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// Closing is one closed or reopened event on an issue's timeline (SPEC
// §6.4, §7.2, plan #87 rev 2, T6). The daemon judges an issue's newest one:
// who closed it and how, or who reopened it.
//
// As GitHub's REST timeline gives a close (checked live on this repo, #190's
// verify): its actor is whoever closed it, the merger for a pull request's
// merge, and its state_reason and commit_id are null, even for a merge, whose
// commit shows only on a separate referenced event. So the REST event can't
// say how an issue was closed: ClosingCloser asks GraphQL, by NodeID.
type Closing struct {
	ID     int64 // the timeline event's ID
	Reopen bool  // a reopened event, else a closed one
	By     Author
	At     time.Time
	// NodeID is the event's GraphQL node ID, node_id on REST, which
	// go-github's Timeline leaves out, for ClosingCloser; "" if GitHub gave
	// none.
	NodeID string
}

// closingOf is e as a Closing, and whether it is a closed or reopened
// event.
func closingOf(e *issueTypeRevertEvent) (Closing, bool) {
	switch e.GetEvent() {
	case "closed", "reopened":
	default:
		return Closing{}, false
	}
	return Closing{ID: e.GetID(), Reopen: e.GetEvent() == "reopened", By: authorOf(e.GetActor()), At: e.GetCreatedAt().Time,
		NodeID: e.NodeID}, true
}

// ClosingTimeline reads an issue's whole timeline once, for a closed
// issue's close (T6): it returns the timeline, for the label rules'
// judgement of a closed stage's label (StandingLabelChange), and its
// closed and reopened events, oldest first.
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

// ClosingCloser is how a close was made, as GraphQL's ClosedEvent tells it.
type ClosingCloser struct {
	// Merge is set for a close by a merge into this repo's default branch:
	// a merged pull request whose base is this repo's default branch, or a
	// commit in this repo, which closes an issue it names only once it is
	// on the default branch. A pull request or a commit in another repo
	// that names this repo's issue, an unmerged pull request, a project's
	// workflow (ProjectV2), a closer GitHub won't show the App, and a close
	// by hand (closer null) are none.
	Merge bool
	// StateReason is the close's state reason in lower case, such as
	// "completed", "not_planned" or "duplicate", or "" for none.
	StateReason string
}

// closingCloserQuery reads a closed event's closer and state reason, by
// its node ID, and this repo's name and default branch, to judge the
// closer by.
const closingCloserQuery = `query($id: ID!, $owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) { nameWithOwner defaultBranchRef { name } }
  node(id: $id) {
    ... on ClosedEvent {
      stateReason
      closer {
        __typename
        ... on PullRequest { merged baseRefName baseRepository { nameWithOwner } }
        ... on Commit { repository { nameWithOwner } }
      }
    }
  }
}`

// closingCloserResponse is closingCloserQuery's answer.
type closingCloserResponse struct {
	Data struct {
		Repository *struct {
			NameWithOwner    string
			DefaultBranchRef *struct{ Name string }
		}
		Node *struct {
			StateReason string
			Closer      *struct {
				Typename       string `json:"__typename"`
				Merged         bool
				BaseRefName    string
				BaseRepository *struct{ NameWithOwner string }
				Repository     *struct{ NameWithOwner string }
			}
		}
	}
	Errors []struct {
		Type, Message string
		Path          []any
	}
}

// ClosingCloser reads how the closed event with node ID nodeID, on an
// issue in owner/repo, was made, with one GraphQL query (closingCloserQuery,
// SPEC §16). An event GitHub can't resolve, NOT_FOUND under node, is a
// close by nobody named, no merge, so it's no error and needn't be asked
// about again. So is a closer the App may not read, FORBIDDEN under node,
// logged, as a closer in a private repo it can't see, or a project's
// (ProjectV2) on a personal account, which can't grant Projects access.
// It's no merge the App can tell, so the close is judged as one by hand,
// and asking again would get the same answer. A ProjectV2 closer the App
// can read is no merge either. Any other error is one, wrapping ErrGraphQL
// for GitHub's: one of another type or of none under node too (#204), as
// it may be passing, and a merge judged as a close by hand would be
// reverted and held. So is an error on the repository, and a failed
// request, which the caller asks again.
func (c *Client) ClosingCloser(ctx context.Context, owner, repo, nodeID string) (ClosingCloser, error) {
	wrap := func(err error) error { return fmt.Errorf("closer of event %s in %s/%s: %w", nodeID, owner, repo, err) }
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": closingCloserQuery,
		"variables": map[string]any{"id": nodeID, "owner": owner, "name": repo}})
	if err != nil {
		return ClosingCloser{}, wrap(err)
	}
	var out closingCloserResponse
	if _, err := c.Do(req, &out); err != nil {
		return ClosingCloser{}, wrap(err)
	}
	var msgs []string
	for _, e := range out.Errors {
		switch {
		case len(e.Path) > 0 && e.Path[0] == "node" && (e.Type == "FORBIDDEN" || e.Type == "NOT_FOUND"):
			logr.Warnf("%s/%s: GitHub won't say how event %s closed its issue (%s: %s at %v), so it's judged as a close by hand",
				owner, repo, nodeID, e.Type, e.Message, e.Path)
		case e.Type != "NOT_FOUND":
			msgs = append(msgs, e.Message)
		}
	}
	if len(msgs) > 0 {
		return ClosingCloser{}, wrap(fmt.Errorf("%w: %s", ErrGraphQL, strings.Join(msgs, "; ")))
	}
	r, n := out.Data.Repository, out.Data.Node
	if r == nil || r.DefaultBranchRef == nil {
		return ClosingCloser{}, wrap(errNoIssue)
	}
	if n == nil {
		return ClosingCloser{}, nil
	}
	by := ClosingCloser{StateReason: strings.ToLower(n.StateReason)}
	if cl := n.Closer; cl != nil {
		switch cl.Typename {
		case "PullRequest":
			by.Merge = cl.Merged && cl.BaseRepository != nil && strings.EqualFold(cl.BaseRepository.NameWithOwner, r.NameWithOwner) &&
				cl.BaseRefName == r.DefaultBranchRef.Name
		case "Commit":
			by.Merge = cl.Repository != nil && strings.EqualFold(cl.Repository.NameWithOwner, r.NameWithOwner)
		}
	}
	return by, nil
}

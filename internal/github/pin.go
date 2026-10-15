package github

import (
	"context"
	"errors"
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// CreateIssue files an issue as the App and returns its number. Its body is
// written as given, so text the App didn't write must be escaped first
// (SPEC §5.1). It isn't idempotent: a caller that may replay looks for the
// issue before it creates one (SPEC §5.2).
func (c *Client) CreateIssue(ctx context.Context, owner, repo, title, body string) (int, error) {
	is, _, err := c.Issues.Create(ctx, owner, repo, gogithub.CreateIssueRequest{Title: title, Body: &body})
	if err != nil {
		return 0, fmt.Errorf("create issue %q in %s/%s: %w", title, owner, repo, err)
	}
	if is.GetNumber() == 0 {
		return 0, fmt.Errorf("create issue %q in %s/%s: %w", title, owner, repo, errNoNumber)
	}
	return is.GetNumber(), nil
}

// ReopenIssue reopens an issue, with a PATCH of its state. Reopening an open
// issue changes nothing, so a replay is safe.
func (c *Client) ReopenIssue(ctx context.Context, owner, repo string, number int) error {
	if _, _, err := c.Issues.Update(ctx, owner, repo, number, gogithub.UpdateIssueRequest{State: new("open")}); err != nil {
		return fmt.Errorf("reopen %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

var (
	errNoNumber = errors.New("GitHub gave the new issue no number")
	errNoNodeID = errors.New("GitHub gave the issue no node ID")
)

// ErrNotPinned is wrapped by PinIssue's error when GitHub answers with no
// error but the issue isn't pinned: a refusal, like one that wraps
// ErrGraphQL.
var ErrNotPinned = errors.New("GitHub answered without pinning it")

// pinIssueMutation pins an issue to the repo's issue list, by its node ID.
const pinIssueMutation = `mutation($id: ID!) {
  pinIssue(input: {issueId: $id}) {
    issue { isPinned }
  }
}`

// pinIssueResponse is pinIssueMutation's answer.
type pinIssueResponse struct {
	Data struct {
		PinIssue *struct {
			Issue *struct{ IsPinned bool }
		}
	}
	gqlErrors
}

// PinIssue pins an issue to the repo's issue list, with GraphQL's pinIssue.
// GitHub allows three pinned issues per repo, and the permission an App
// needs for it isn't documented (SPEC §16). When GitHub refuses, the error
// wraps ErrGraphQL and names GitHub's message.
//
// The mutation takes the issue's node ID, which it reads first with a REST
// GET of the issue. That GET is conditional (etagCache), so a repeat costs
// no rate limit, where a GraphQL lookup would cost a point each time.
func (c *Client) PinIssue(ctx context.Context, owner, repo string, number int) error {
	wrap := func(err error) error { return fmt.Errorf("pin %s/%s#%d: %w", owner, repo, number, err) }
	is, _, err := c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return wrap(err)
	}
	if is.GetNodeID() == "" {
		return wrap(errNoNodeID)
	}
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": pinIssueMutation,
		"variables": map[string]any{"id": is.GetNodeID()}})
	if err != nil {
		return wrap(err)
	}
	var out pinIssueResponse
	if _, err := c.Do(req, &out); err != nil {
		return wrap(err)
	}
	if err := out.err(); err != nil {
		return wrap(err)
	}
	if p := out.Data.PinIssue; p == nil || p.Issue == nil || !p.Issue.IsPinned {
		return wrap(ErrNotPinned)
	}
	return nil
}

// isPinnedQuery reads whether an issue is pinned, by its number.
const isPinnedQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) { isPinned }
  }
}`

// isPinnedResponse is isPinnedQuery's answer.
type isPinnedResponse struct {
	Data struct {
		Repository *struct {
			Issue *struct{ IsPinned bool }
		}
	}
	gqlErrors
}

// IsPinned reports whether an issue is pinned to the repo's issue list. It
// is one GraphQL query, by the issue's number, so it needs no node ID.
func (c *Client) IsPinned(ctx context.Context, owner, repo string, number int) (bool, error) {
	wrap := func(err error) error { return fmt.Errorf("is %s/%s#%d pinned: %w", owner, repo, number, err) }
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": isPinnedQuery,
		"variables": map[string]any{"owner": owner, "name": repo, "number": number}})
	if err != nil {
		return false, wrap(err)
	}
	var out isPinnedResponse
	if _, err := c.Do(req, &out); err != nil {
		return false, wrap(err)
	}
	if err := out.err(); err != nil {
		return false, wrap(err)
	}
	if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
		return false, wrap(errNoIssue)
	}
	return out.Data.Repository.Issue.IsPinned, nil
}

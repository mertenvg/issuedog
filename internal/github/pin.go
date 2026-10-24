package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v92/github"
)

// CreateIssue files an issue as the App and returns its number. Its body is
// written as given, so text the App didn't write must be escaped first
// (SPEC §5.1). It isn't idempotent: a caller that may replay looks for the
// issue before it creates one (SPEC §5.2). It is CreateChild with no labels
// and no type.
func (c *Client) CreateIssue(ctx context.Context, owner, repo, title, body string) (int, error) {
	is, err := c.CreateChild(ctx, owner, repo, NewChild{Title: title, Body: body})
	if err != nil {
		return 0, err
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
// wraps ErrGraphQL and names GitHub's message. An HTTP 401 or 403 answer to
// the mutation is a refusal too (#226), as a missing permission may come
// that way rather than as a GraphQL error: its error wraps ErrGraphQL and
// GitHub's *gogithub.ErrorResponse, which holds the message. A rate limit,
// which go-github gives its own error types, isn't one, nor is any answer
// to the GET before the mutation.
//
// The mutation takes the issue's node ID, which it reads first with a REST
// GET of the issue. That GET is conditional (etagCache), so a repeat costs
// no rate limit, where a GraphQL lookup would cost a point each time. A
// GET that finds the issue moved to another repo pins nothing, and its
// error wraps ErrTransferred (IssueMoved, #413).
func (c *Client) PinIssue(ctx context.Context, owner, repo string, number int) error {
	wrap := func(err error) error { return fmt.Errorf("pin %s/%s#%d: %w", owner, repo, number, err) }
	is, resp, err := c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return wrap(err)
	}
	// GitHub answers a GET of a transferred issue with a 301, which the
	// client follows (sameHost) to the issue in its new repo. Node IDs are
	// global, so pinIssue would pin it there, using one of that repo's three
	// pins. So, as the daemon's digestMoved does, it takes an answer from
	// anywhere but this repo's path as moved, and an issue whose
	// repository_url names another repo too (IssueMoved).
	if err := IssueMoved(owner, repo, resp, is); err != nil {
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
		if pinRefused(err) {
			return wrap(fmt.Errorf("%w: %w", ErrGraphQL, err))
		}
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

// pinRefused reports whether err, the pinIssue mutation's, is GitHub's
// HTTP 401 or 403, a refusal (PinIssue). go-github gives a rate limit's
// 403 its own types, so that's never one.
func pinRefused(err error) bool {
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return false
	}
	return er.Response.StatusCode == http.StatusUnauthorized || er.Response.StatusCode == http.StatusForbidden
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

// ErrIssueNotFound is wrapped by IsPinned's error, with ErrGraphQL, when
// GitHub answers that the repo has no such issue: a NOT_FOUND error, with
// the repo found but the issue null. GitHub answers so, with an HTTP 200,
// for an issue that was deleted, and for one that was transferred, as a
// lookup by number is the old repo's (#413's verify). An HTTP 404 or 410
// from the GraphQL endpoint itself is no such answer, as it says nothing
// of the issue (#428).
var ErrIssueNotFound = errors.New("GitHub found no such issue")

// IsPinned reports whether an issue is pinned to the repo's issue list. It
// is one GraphQL query, by the issue's number, so it needs no node ID. A
// NOT_FOUND for the issue wraps ErrIssueNotFound.
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
		if out.Data.Repository != nil && out.Data.Repository.Issue == nil && out.notFound() {
			return false, wrap(fmt.Errorf("%w: %w", ErrIssueNotFound, err))
		}
		return false, wrap(err)
	}
	if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
		return false, wrap(errNoIssue)
	}
	return out.Data.Repository.Issue.IsPinned, nil
}

// notFound reports whether any of the errors is GitHub's NOT_FOUND.
func (e gqlErrors) notFound() bool {
	for _, m := range e.Errors {
		if m.Type == "NOT_FOUND" {
			return true
		}
	}
	return false
}

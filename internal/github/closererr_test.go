package github_test

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// CloserErrUnderNode tells ClosingCloser's error for an error under node,
// untyped or of another type than FORBIDDEN or NOT_FOUND, at node or below
// it, from its other errors: GraphQL failing as a whole, an error on the
// repository, and a failed request (#232). Its text, and ErrGraphQL, are
// as for any of GitHub's errors, also once wrapped.
func TestCloserErrUnderNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "A", "")
	event := f.CloseByPullRequest(n, "mallory", githubtest.ClosingPR{Merged: true})
	id := "CE_" + strconv.FormatInt(event, 10)
	c := f.Client(t)
	ask := func() error {
		_, err := c.ClosingCloser(ctx, "o", "r", id)
		return err
	}
	const msg = "Something went wrong while executing your query."
	for _, typ := range []string{"", "INTERNAL", "INSUFFICIENT_SCOPES"} {
		f.FailCloser(n, event, typ, msg)
		err := ask()
		if !github.CloserErrUnderNode(err) || !github.CloserErrUnderNode(fmt.Errorf("wrapped: %w", err)) || !errors.Is(err, github.ErrGraphQL) {
			t.Errorf("type %q under node: %v; want an error under node, wrapping ErrGraphQL", typ, err)
		}
		if want := fmt.Sprintf("closer of event %s in o/r: graphql errors: %s", id, msg); err == nil || err.Error() != want {
			t.Errorf("type %q under node: text %v; want %q", typ, err, want)
		}
		f.MendCloser(n, event)
	}
	f.FailGraphQLAt("", msg, "node", "closer")
	if err := ask(); !github.CloserErrUnderNode(err) {
		t.Errorf("an error at node.closer: %v; want an error under node", err)
	}
	f.FailGraphQL("boom")
	if err := ask(); err == nil || github.CloserErrUnderNode(err) {
		t.Errorf("GraphQL failing as a whole: %v; want an error, not under node", err)
	}
	f.FailGraphQLAt("FORBIDDEN", "Resource not accessible by integration", "repository", "defaultBranchRef")
	if err := ask(); err == nil || github.CloserErrUnderNode(err) {
		t.Errorf("an error on the repository: %v; want an error, not under node", err)
	}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/graphql" }, 500)
	if err := ask(); err == nil || github.CloserErrUnderNode(err) {
		t.Errorf("a failed request: %v; want an error, not under node", err)
	}
	if err := ask(); err != nil || github.CloserErrUnderNode(err) {
		t.Errorf("after the errors: %v; want none", err)
	}
}

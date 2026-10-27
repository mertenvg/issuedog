package github_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// PinIssue of an issue deleted between its GET and the pinIssue mutation
// fails with the mutation's NOT_FOUND, its error wrapping
// ErrIssueNotFound with ErrGraphQL, and pins nothing (#495).
func TestPinIssueDeletedBeforeMutation(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/graphql" }, f.DeleteIssue)
	err := f.Client(t).PinIssue(context.Background(), "o", "r", n)
	if !errors.Is(err, github.ErrIssueNotFound) || !errors.Is(err, github.ErrGraphQL) ||
		err.Error() != "pin o/r#1: GitHub found no such issue: graphql errors: Could not resolve to a node with the global id of 'I_1'" {
		t.Errorf("PinIssue = %v; want ErrIssueNotFound, with ErrGraphQL, naming GitHub's message", err)
	}
	if is := f.State().Issues; len(is) != 0 {
		t.Errorf("issues = %+v; want the issue deleted", is)
	}
}

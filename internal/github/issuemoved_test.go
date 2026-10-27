package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// IssueMoved takes an answer from this repo's path, by an issue of this
// repo or none, as not moved, matching the repo in any case. An answer
// from another path, another repo's under /repos/ included, or an issue
// whose repository_url names another repo, is moved, and its error wraps
// ErrTransferred and names where it was answered from or moved to (#428).
func TestIssueMoved(t *testing.T) {
	t.Parallel()
	resp := func(path string) *gogithub.Response {
		u, err := url.Parse("https://api.github.com" + path)
		if err != nil {
			t.Fatal(err)
		}
		return &gogithub.Response{Response: &http.Response{Request: &http.Request{Method: http.MethodGet, URL: u}}}
	}
	repoURL := func(u string) *gogithub.Issue { return &gogithub.Issue{RepositoryURL: &u} }
	for _, tc := range []struct {
		resp *gogithub.Response
		is   *gogithub.Issue
		want string // the error's text, or "" for not moved
	}{
		{nil, nil, ""},
		{resp("/repos/o/R/issues/1"), repoURL("https://api.github.com/repos/o/R"), ""},
		{resp("/api/v3/repos/o/r/issues/1"), nil, ""},
		{nil, repoURL("https://api.github.com/repos/O/r"), ""},
		{&gogithub.Response{}, &gogithub.Issue{}, ""},
		{resp("/repositories/9/issues/1"), nil, "transferred: answered by GET /repositories/9/issues/1"},
		{resp("/repos/O/other/issues/1"), nil, "transferred: answered by GET /repos/O/other/issues/1"},
		{resp("/repos/o/rr/issues/1"), nil, "transferred: answered by GET /repos/o/rr/issues/1"},
		{resp("/repos/o/other/issues/1"), repoURL("https://api.github.com/repos/o/other"), "transferred: answered by GET /repos/o/other/issues/1"},
		{nil, repoURL("https://api.github.com/repos/o/r2"), "transferred to https://api.github.com/repos/o/r2"},
		{resp("/repos/o/r/issues/1"), repoURL("https://api.github.com/repos/o/other"), "transferred to https://api.github.com/repos/o/other"},
	} {
		err := github.IssueMoved("O", "r", tc.resp, tc.is)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("IssueMoved(%v, %q) = %v; want not moved", tc.resp, tc.is.GetRepositoryURL(), err)
		case tc.want != "" && (!errors.Is(err, github.ErrTransferred) || err.Error() != tc.want):
			t.Errorf("IssueMoved(%v, %q) = %v; want %q, wrapping ErrTransferred", tc.resp, tc.is.GetRepositoryURL(), err, tc.want)
		}
	}
}

// IsPinned of an issue transferred to another repo, or of none, is
// GitHub's NOT_FOUND for the issue, an HTTP 200, so its error wraps
// ErrIssueNotFound. One that stays isn't (#428).
func TestIsPinnedTransferredNotFound(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "")
	f.AddIssue("owner", "Stays", "")
	f.Transfer(1, "o/elsewhere")
	ctx, c := context.Background(), f.Client(t)
	for _, n := range []int{1, 3} {
		if pinned, err := c.IsPinned(ctx, "o", "r", n); !errors.Is(err, github.ErrIssueNotFound) || !errors.Is(err, github.ErrGraphQL) {
			t.Errorf("IsPinned(%d) = %t, %v; want ErrIssueNotFound, with ErrGraphQL", n, pinned, err)
		}
	}
	if pinned, err := c.IsPinned(ctx, "o", "r", 2); err != nil || pinned {
		t.Errorf("IsPinned of the issue that stays = %t, %v; want false", pinned, err)
	}
}

// Snapshot of an issue transferred to another repo fails, its error
// wrapping ErrTransferred, as the GET followed GitHub's 301 to the issue
// in its new repo, whose repository_url names that repo (IssueMoved). It
// reads nothing more: no parent and no GraphQL. An issue whose
// repository_url names this repo in another case is no move (#495).
func TestSnapshotMoved(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	moved := f.AddIssue("owner", "Moved", "")
	stays := f.AddIssue("owner", "Stays", "")
	f.Transfer(moved, "o/elsewhere")
	f.Transfer(stays, "O/R")
	ctx, c := context.Background(), f.Client(t)
	from := len(f.Requests())
	s, err := c.Snapshot(ctx, "o", "r", moved)
	if !errors.Is(err, github.ErrTransferred) || !strings.HasPrefix(err.Error(), "snapshot of o/r#1: transferred to ") ||
		!strings.HasSuffix(err.Error(), "/repos/o/elsewhere") {
		t.Errorf("Snapshot of the moved issue = %+v, %v; want ErrTransferred, naming its new repo", s, err)
	}
	for _, r := range f.Requests()[from:] {
		if r.Path == "/graphql" || strings.HasSuffix(r.Path, "/parent") {
			t.Errorf("Snapshot of the moved issue sent %+v; want nothing after its GET", r)
		}
	}
	if s, err := c.Snapshot(ctx, "o", "r", stays); err != nil || s.Title != "Stays" {
		t.Errorf("Snapshot of the issue that stays = %+v, %v; want it read", s, err)
	}
}

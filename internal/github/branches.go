package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/repo"
)

// branchPrefix is repo.BranchPrefix, the prefix of the App's own branches,
// named here as DeleteBranch's repo parameter hides the package.
const branchPrefix = repo.BranchPrefix

// appBranch reports whether branch is one of the App's own, under
// branchPrefix, the only branches DeleteBranch deletes.
func appBranch(branch string) bool {
	return strings.HasPrefix(branch, branchPrefix) && branch != branchPrefix
}

// branchGone reports whether err, with resp, is GitHub's answer to a delete
// of a branch that isn't there: a 422 "Reference does not exist", or a 404.
func branchGone(resp *gogithub.Response, err error) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	var er *gogithub.ErrorResponse
	return resp.StatusCode == http.StatusUnprocessableEntity && errors.As(err, &er) && er.Message == "Reference does not exist"
}

// DeleteBranch deletes branch of owner/repo, as the App, with DELETE
// /repos/{owner}/{repo}/git/refs/heads/{branch}, as once a task's PR has
// merged into its story branch (SPEC §7.5's Branches after a merge). It
// refuses a branch outside repo.BranchPrefix, main included, before any
// request. A branch that's gone already, as one GitHub's "Automatically
// delete head branches" removed first, is no error, so a replay deletes
// nothing more.
func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	if !appBranch(branch) {
		return fmt.Errorf("delete branch %q of %s/%s: it isn't one of the App's, under %s", branch, owner, repo, branchPrefix)
	}
	resp, err := c.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+branch)
	switch {
	case err != nil && branchGone(resp, err):
		return nil
	case err != nil:
		return fmt.Errorf("delete branch %s of %s/%s: %w", branch, owner, repo, err)
	}
	return nil
}

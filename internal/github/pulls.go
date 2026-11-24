package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// PullRequest is one pull request as the daemon reads it: its number, its
// head branch and the SHA GitHub shows for it, its base branch, its state
// ("open" or "closed", a merged one included), its title, body and author's
// login, its labels' names, whether it merged, and when it closed and was
// last updated. MergedBy is the login of whoever merged it, and
// MergeCommitSHA the commit its merge wrote, as a squash's; both are read
// only once it merged, and only from one pull request's read
// (Client.PullRequest), since the list leaves out merged_by.
type PullRequest struct {
	Number                                          int
	Head, HeadSHA, Base, State, Title, Body, Author string
	Labels                                          []string
	Merged                                          bool
	MergedBy, MergeCommitSHA                        string
	ClosedAt, UpdatedAt                             time.Time
}

// NewPullRequest is a pull request to open: its title, its head branch in
// the repo, the base branch it targets, and its body.
type NewPullRequest struct {
	Title, Head, Base, Body string
}

// ErrReopenRefused is wrapped by UpdatePullRequest's error when GitHub
// refuses, with a 422, to reopen a closed pull request, as it may once its
// branch was pushed past its head by force while it was closed (SPEC §7.5's
// A closed PR, §16). A caller opens a new pull request instead.
var ErrReopenRefused = errors.New("GitHub refused to reopen the pull request")

// ErrPullRequestExists is wrapped by CreatePullRequest's error when GitHub
// refuses, with a 422, to open a pull request because an open one with the
// same head and base exists already, as one someone else opened from a
// task's branch to main. A caller can tell it from any other failure.
var ErrPullRequestExists = errors.New("an open pull request with that head and base exists already")

// ErrHeadMoved is wrapped by MergePullRequest's error when GitHub refuses
// the merge with a 409, as the pull request's head is no longer the sha the
// merge named (SPEC §7.5's The merge's guards). Nothing merged.
var ErrHeadMoved = errors.New("the pull request's head moved past the one named")

// ErrNotMergeable is wrapped by MergePullRequest's error when GitHub
// refuses the merge with a 405, as for a closed or merged pull request, a
// conflict with its base, or a merge method the repo doesn't allow.
// Nothing merged.
var ErrNotMergeable = errors.New("GitHub won't merge the pull request")

// pullRequestExists reports whether err, with resp, is GitHub's 422 to an
// open pull request with the same head and base: "Validation Failed", with
// an entry in its errors list whose message says a pull request already
// exists.
func pullRequestExists(resp *gogithub.Response, err error) bool {
	var er *gogithub.ErrorResponse
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity || !errors.As(err, &er) {
		return false
	}
	const exists = "a pull request already exists"
	for _, e := range er.Errors {
		if strings.Contains(strings.ToLower(e.Message), exists) {
			return true
		}
	}
	return false
}

// pullRequestOf is the PullRequest GitHub's p describes. The list of pull
// requests gives no merged field, only merged_at, so either one says it
// merged. GitHub gives an open pull request a merge_commit_sha too, its
// test merge's, which is no commit of the repo's branches, so it's kept
// only once the pull request merged.
func pullRequestOf(p *gogithub.PullRequest) PullRequest {
	var labels []string
	for _, l := range p.Labels {
		labels = append(labels, l.GetName())
	}
	pr := PullRequest{Labels: labels,
		Number: p.GetNumber(), Head: p.GetHead().GetRef(), HeadSHA: p.GetHead().GetSHA(), Base: p.GetBase().GetRef(),
		State: p.GetState(), Title: p.GetTitle(), Body: p.GetBody(), Author: p.GetUser().GetLogin(),
		Merged: p.GetMerged() || p.MergedAt != nil, ClosedAt: p.GetClosedAt().Time, UpdatedAt: p.GetUpdatedAt().Time,
	}
	if pr.Merged {
		pr.MergedBy, pr.MergeCommitSHA = p.GetMergedBy().GetLogin(), p.GetMergeCommitSHA()
	}
	return pr
}

// PullRequest reads pull request number of owner/repo, with GET
// /repos/{owner}/{repo}/pulls/{number}: its state, head and head SHA, its
// base, whether it merged, and, once it did, who merged it and its merge
// commit. A repeat of an unchanged read is a 304, which costs no rate
// limit, so a read just before a merge costs nothing when the pull
// request didn't change (SPEC §6.4).
func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	p, _, err := c.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return pullRequestOf(p), nil
}

// MergePullRequest squash-merges pull request number of owner/repo, as the
// App, only while its head is sha, with title as the squash commit's title
// and body as its message, an empty body an empty message rather than
// GitHub's default, and returns the squash commit's SHA, as GitHub's
// answer names it (SPEC §7.5's The merge's guards). It refuses an empty
// sha before any request, as a merge at no named head guards nothing.
// GitHub's 409, for a head that moved past sha, wraps ErrHeadMoved, and its
// 405, for a pull request it won't merge, ErrNotMergeable, each beside
// GitHub's *gogithub.ErrorResponse. It isn't idempotent: a merged pull
// request is refused, so a caller that may replay reads PullRequest first.
func (c *Client) MergePullRequest(ctx context.Context, owner, repo string, number int, sha, title, body string) (string, error) {
	if sha == "" {
		return "", fmt.Errorf("merge pull request %s/%s#%d: no head SHA to merge at", owner, repo, number)
	}
	opts := &gogithub.PullRequestOptions{CommitTitle: title, SHA: sha, MergeMethod: "squash", DontDefaultIfBlank: true}
	res, resp, err := c.PullRequests.Merge(ctx, owner, repo, number, body, opts)
	switch {
	case err != nil && resp != nil && resp.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("merge pull request %s/%s#%d at %s: %w: %w", owner, repo, number, sha, ErrHeadMoved, err)
	case err != nil && resp != nil && resp.StatusCode == http.StatusMethodNotAllowed:
		return "", fmt.Errorf("merge pull request %s/%s#%d at %s: %w: %w", owner, repo, number, sha, ErrNotMergeable, err)
	case err != nil:
		return "", fmt.Errorf("merge pull request %s/%s#%d at %s: %w", owner, repo, number, sha, err)
	case res.GetSHA() == "":
		return "", fmt.Errorf("merge pull request %s/%s#%d at %s: GitHub's answer names no merge commit", owner, repo, number, sha)
	}
	return res.GetSHA(), nil
}

// PullRequestsByHead lists the pull requests whose head is branch in
// owner's repo, open, closed and merged, every page, in GitHub's order,
// newest first. A repeat of an unchanged listing is a 304, which costs no
// rate limit. It's how a task's PR is found (SPEC §7.5), with no search,
// so one opened just before a crash is found too.
func (c *Client) PullRequestsByHead(ctx context.Context, owner, repo, branch string) ([]PullRequest, error) {
	opts := &gogithub.PullRequestListOptions{State: "all", Head: owner + ":" + branch, ListOptions: gogithub.ListOptions{PerPage: 100}}
	all, err := collect(c.PullRequests.ListIter(ctx, owner, repo, opts))
	if err != nil {
		return nil, fmt.Errorf("list pull requests of %s/%s with head %s: %w", owner, repo, branch, err)
	}
	prs := make([]PullRequest, 0, len(all))
	for _, p := range all {
		prs = append(prs, pullRequestOf(p))
	}
	return prs, nil
}

// CreatePullRequest opens pr in owner/repo, as the App, and returns it as
// GitHub answers. It isn't idempotent: GitHub refuses a second open pull
// request with the same head and base, with a 422, so a caller that may
// replay lists PullRequestsByHead first. That 422 wraps
// ErrPullRequestExists, beside GitHub's *gogithub.ErrorResponse, as one
// someone else opened isn't the caller's to find.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo string, pr NewPullRequest) (PullRequest, error) {
	p, resp, err := c.PullRequests.Create(ctx, owner, repo, gogithub.CreatePullRequest{Title: &pr.Title, Head: pr.Head, Base: pr.Base, Body: &pr.Body})
	switch {
	case err != nil && pullRequestExists(resp, err):
		return PullRequest{}, fmt.Errorf("open a pull request in %s/%s from %s to %s: %w: %w", owner, repo, pr.Head, pr.Base, ErrPullRequestExists, err)
	case err != nil:
		return PullRequest{}, fmt.Errorf("open a pull request in %s/%s from %s to %s: %w", owner, repo, pr.Head, pr.Base, err)
	}
	return pullRequestOf(p), nil
}

// UpdatePullRequest sets the title, the body and the state ("open" or
// "closed") of pull request number, each only when given, in one request,
// and returns the pull request as GitHub answers. A 422 to one that sets
// the state to open wraps ErrReopenRefused, beside GitHub's
// *gogithub.ErrorResponse, so a caller can tell a refused reopen from any
// other failure.
func (c *Client) UpdatePullRequest(ctx context.Context, owner, repo string, number int, title, body, state *string) (PullRequest, error) {
	p, resp, err := c.PullRequests.Edit(ctx, owner, repo, number, &gogithub.PullRequest{Title: title, Body: body, State: state})
	switch {
	case err != nil && state != nil && *state == "open" && resp != nil && resp.StatusCode == http.StatusUnprocessableEntity:
		return PullRequest{}, fmt.Errorf("reopen pull request %s/%s#%d: %w: %w", owner, repo, number, ErrReopenRefused, err)
	case err != nil:
		return PullRequest{}, fmt.Errorf("update pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return pullRequestOf(p), nil
}

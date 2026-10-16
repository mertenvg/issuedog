package github

import (
	"context"
	"fmt"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// Assignees (SPEC §9.6, #191). Snapshot reads them from the issue GET it
// makes anyway, so they cost no request of their own. Adding and removing
// are the assignee endpoints, a POST and a DELETE of
// /repos/{owner}/{repo}/issues/{n}/assignees, which a replay may repeat
// safely: adding a login that's assigned already, or removing one that
// isn't, changes nothing.
//
// GitHub silently leaves out a login it can't assign, such as a user with
// no access to the repo, and answers with no error. So each returns the
// issue's assignees as GitHub answered them after the write, and the
// caller compares.

// AddAssignees assigns logins to issue number, and returns its assignees
// after, as GitHub answered (assignAnswer).
func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	is, _, err := c.Issues.AddAssignees(ctx, owner, repo, number, logins)
	got, err := assignAnswer(owner, repo, is, err)
	if err != nil {
		return nil, fmt.Errorf("assign %s to %s/%s#%d: %w", strings.Join(logins, ", "), owner, repo, number, err)
	}
	return got, nil
}

// RemoveAssignees unassigns logins from issue number, and returns its
// assignees after, as GitHub answered (assignAnswer).
func (c *Client) RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	is, _, err := c.Issues.RemoveAssignees(ctx, owner, repo, number, logins)
	got, err := assignAnswer(owner, repo, is, err)
	if err != nil {
		return nil, fmt.Errorf("unassign %s from %s/%s#%d: %w", strings.Join(logins, ", "), owner, repo, number, err)
	}
	return got, nil
}

// assignAnswer returns the assignees of is, GitHub's answer to an assignee
// write with error err, or err. GitHub answers a write to a transferred
// issue with a 301, and Go follows it with a GET, which may succeed with
// the moved issue: so, as Snapshot does, an issue whose repository_url
// names another repo is ErrTransferred, and the write wrote nothing.
func assignAnswer(owner, repo string, is *gogithub.Issue, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	if u := is.GetRepositoryURL(); u != "" && !strings.HasSuffix(strings.ToLower(u), strings.ToLower("/repos/"+owner+"/"+repo)) {
		return nil, fmt.Errorf("%w to %s", ErrTransferred, u)
	}
	return assignLogins(is.Assignees), nil
}

// assignLogins is the logins of users, in GitHub's order, or nil for none.
func assignLogins(users []*gogithub.User) []string {
	var logins []string
	for _, u := range users {
		if l := u.GetLogin(); l != "" {
			logins = append(logins, l)
		}
	}
	return logins
}

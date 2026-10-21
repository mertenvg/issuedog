package githubtest

import (
	"net/http"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// The assignee endpoints (#191). An issue's assignees live in its
// gogithub.Issue, so the issue GET and the listing serve them as they are.
// As on GitHub, a login that can't be assigned (Unassignable) is left out
// of an add with no error, and an add or a remove that changes nothing
// leaves updated_at alone. The fake keeps no assigned or unassigned
// events, which nothing reads yet.

// handleAssign registers the assignee endpoints.
func (f *Fake) handleAssign() {
	f.handle("POST /issues/{n}/assignees", f.addAssignees)
	f.handle("DELETE /issues/{n}/assignees", f.removeAssignees)
}

// SetAssignees sets an issue's assignees to logins, as a person can by
// hand, and moves updated_at. Unassignable logins are assigned anyway, as
// if they could be when the person assigned them.
func (f *Fake) SetAssignees(number int, logins ...string) {
	f.t.Helper()
	f.edit(number, func(is *issue) {
		is.Assignees = nil
		for _, l := range logins {
			is.Assignees = append(is.Assignees, user(l))
		}
		is.UpdatedAt = f.tick()
	})
}

// Unassignable makes logins ones that GitHub can't assign, such as users
// with no access to the repo: an add leaves them out, with no error.
func (f *Fake) Unassignable(logins ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unassignable == nil {
		f.unassignable = map[string]bool{}
	}
	for _, l := range logins {
		f.unassignable[strings.ToLower(l)] = true
	}
}

// assignRequest is the body of an assignee write.
type assignRequest struct {
	Assignees []string `json:"assignees"`
}

// addAssignees assigns each login that can be and isn't already, as the
// App, and answers 201 with the issue.
func (f *Fake) addAssignees(c *call) (int, any) {
	var req assignRequest
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	changed := false
	for _, l := range req.Assignees {
		if f.unassignable[strings.ToLower(l)] || slices.ContainsFunc(c.issue.Assignees, assignIs(l)) {
			continue
		}
		c.issue.Assignees, changed = append(c.issue.Assignees, user(l)), true
	}
	if changed {
		c.issue.UpdatedAt = f.tick()
	}
	return http.StatusCreated, c.issue.Issue
}

// removeAssignees unassigns each login, as the App, and answers 200 with
// the issue.
func (f *Fake) removeAssignees(c *call) (int, any) {
	var req assignRequest
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	n := len(c.issue.Assignees)
	for _, l := range req.Assignees {
		c.issue.Assignees = slices.DeleteFunc(c.issue.Assignees, assignIs(l))
	}
	if len(c.issue.Assignees) != n {
		c.issue.UpdatedAt = f.tick()
	}
	return http.StatusOK, c.issue.Issue
}

// assignIs matches the user with login, in any case, as GitHub's logins are.
func assignIs(login string) func(*gogithub.User) bool {
	return func(u *gogithub.User) bool { return strings.EqualFold(u.GetLogin(), login) }
}

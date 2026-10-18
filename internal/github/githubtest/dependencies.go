package githubtest

import (
	"fmt"
	"net/http"
	"slices"

	gogithub "github.com/google/go-github/v92/github"
)

// maxBlockedBy is how many issues GitHub lets block one issue.
const maxBlockedBy = 50

// SetBlockedBy makes issue number blocked by issue blocker, after the
// issues blocking it already, as someone could by hand. A link it has
// already changes nothing. It moves no updated_at.
func (f *Fake) SetBlockedBy(number, blocker int) {
	f.edit(number, func(is *issue) {
		if !slices.Contains(is.blockedBy, blocker) {
			is.blockedBy = append(is.blockedBy, blocker)
		}
	})
}

// blockedBy serves the issues blocking an issue, open and closed, in the
// order they were added, each with its repository_url, paged by per_page
// and page
// (GET /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocked_by).
func (f *Fake) blockedBy(c *call) (int, any) {
	out := []gogithub.Issue{}
	for _, n := range c.issue.blockedBy {
		is := f.issues[n-1]
		blocker := is.Issue
		if is.listed() { // a transferred one names its new repo (Transfer)
			blocker.RepositoryURL = new(fmt.Sprintf("%s/repos/%s", f.srv.URL, f.repo))
		}
		out = append(out, blocker)
	}
	return page(c, out)
}

// addBlockedBy makes the path's issue blocked by the issue whose ID is
// issue_id, as the App, and answers 201 with the blocking issue
// (POST /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocked_by).
// It moves the blocked issue's updated_at. As on GitHub, it answers 422 for
// a link the issue has already, for the issue itself, and for an issue that
// 50 issues block already, and 404 for an ID no issue has.
func (f *Fake) addBlockedBy(c *call) (int, any) {
	var req struct {
		IssueID int64 `json:"issue_id"`
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	blocker := f.issueByID(req.IssueID)
	if blocker == nil {
		return http.StatusNotFound, message("Not Found")
	}
	n, b := c.issue.GetNumber(), blocker.GetNumber()
	switch {
	case b == n:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: #%d can't block itself", n))
	case slices.Contains(c.issue.blockedBy, b):
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: #%d is blocked by #%d already", n, b))
	case len(c.issue.blockedBy) >= maxBlockedBy:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: #%d is blocked by %d issues already", n, maxBlockedBy))
	}
	c.issue.blockedBy = append(c.issue.blockedBy, b)
	c.issue.UpdatedAt = f.tick()
	return http.StatusCreated, blocker.Issue
}

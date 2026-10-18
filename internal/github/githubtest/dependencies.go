package githubtest

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

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
		out = append(out, f.dependency(n))
	}
	return page(c, out)
}

// blocking serves the issues an issue blocks, open and closed, by number,
// each with its repository_url, paged by per_page and page
// (GET /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocking).
// It reads the blocked-by links the other way round, so a link added or
// removed shows in both listings.
func (f *Fake) blocking(c *call) (int, any) {
	out := []gogithub.Issue{}
	for _, is := range f.issues {
		if slices.Contains(is.blockedBy, c.issue.GetNumber()) {
			out = append(out, f.dependency(is.GetNumber()))
		}
	}
	return page(c, out)
}

// dependency is issue n as a dependency listing shows it, with its
// repository_url. f is locked.
func (f *Fake) dependency(n int) gogithub.Issue {
	is := f.issues[n-1]
	dep := is.Issue
	if is.listed() { // a transferred one names its new repo (Transfer)
		dep.RepositoryURL = new(fmt.Sprintf("%s/repos/%s", f.srv.URL, f.repo))
	}
	return dep
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

// removeBlockedBy removes the link that makes the path's issue blocked by
// the issue whose ID is issue_id, as the App, and answers 200 with the
// blocking issue
// (DELETE /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocked_by/{issue_id}).
// It moves the blocked issue's updated_at, as addBlockedBy does. It answers
// 404 for an ID no issue has and for an issue that doesn't block this one,
// so a second removal of one link fails.
func (f *Fake) removeBlockedBy(c *call) (int, any) {
	id, err := strconv.ParseInt(c.r.PathValue("issue_id"), 10, 64)
	if err != nil {
		return http.StatusNotFound, message("Not Found")
	}
	blocker := f.issueByID(id)
	if blocker == nil {
		return http.StatusNotFound, message("Not Found")
	}
	i := slices.Index(c.issue.blockedBy, blocker.GetNumber())
	if i < 0 {
		return http.StatusNotFound, message("Not Found")
	}
	c.issue.blockedBy = slices.Delete(c.issue.blockedBy, i, i+1)
	c.issue.UpdatedAt = f.tick()
	return http.StatusOK, blocker.Issue
}

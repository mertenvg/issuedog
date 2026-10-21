package githubtest

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
)

// firstIssueID is one less than the first issue's ID. As on GitHub, an
// issue's ID differs from its number, and from comments', events' and
// types' IDs, so a client that sends one where the other belongs is caught.
const firstIssueID = 700000

// issueID is the ID of issue number n, which the sub-issue and dependency
// writes name it by. Issues are kept by number, so the ID follows from it.
func issueID(n int) int64 { return firstIssueID + int64(n) }

// issueByID returns the issue whose ID is id, or nil. f is locked.
func (f *Fake) issueByID(id int64) *issue {
	i := slices.IndexFunc(f.issues, func(is *issue) bool { return is.GetID() == id })
	if i < 0 {
		return nil
	}
	return f.issues[i]
}

// maxSubIssues is how many sub-issues GitHub allows a parent.
const maxSubIssues = 100

// linkUnder makes is a sub-issue of parent, listed after its other
// sub-issues, or of none for 0. It moves no updated_at. f is locked.
func (f *Fake) linkUnder(is *issue, parent int) {
	f.lastLink++
	is.parent, is.linkSeq = parent, f.lastLink
}

// addSubIssue links the issue whose ID is sub_issue_id under the path's
// issue, after its other sub-issues, as the App, and answers 201 with the
// parent (POST /repos/{owner}/{repo}/issues/{issue_number}/sub_issues). It
// moves the sub-issue's updated_at. As on GitHub, it answers 422 for an
// issue that has a parent already, for the parent itself or one of its
// ancestors, for a pull request, and for a parent with 100 sub-issues, those
// LinkSubIssue listed included, and 404 for an ID no issue has. after_id,
// before_id and replace_parent aren't served, and get a 422. A parent above
// the path's issue that the fake hasn't got is a 500 naming it
// (isAncestor).
func (f *Fake) addSubIssue(c *call) (int, any) {
	var req struct {
		SubIssueID    int64  `json:"sub_issue_id"`
		AfterID       *int64 `json:"after_id"`
		BeforeID      *int64 `json:"before_id"`
		ReplaceParent *bool  `json:"replace_parent"`
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.AfterID != nil || req.BeforeID != nil || req.ReplaceParent != nil {
		return http.StatusUnprocessableEntity, message("githubtest: after_id, before_id and replace_parent aren't served")
	}
	sub := f.issueByID(req.SubIssueID)
	if sub == nil {
		return http.StatusNotFound, message("Not Found")
	}
	parent := c.issue.GetNumber()
	above, err := f.isAncestor(sub.GetNumber(), parent)
	if err != nil {
		return http.StatusInternalServerError, message(fmt.Sprintf("githubtest: the parents above #%d: %v", parent, err))
	}
	switch {
	case sub.parent != 0:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: issue #%d already has a parent, #%d", sub.GetNumber(), sub.parent))
	case above:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: issue #%d can't be a sub-issue of itself or of its own sub-issue", sub.GetNumber()))
	case sub.IsPullRequest():
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: #%d is a pull request", sub.GetNumber()))
	case f.subIssueCount(parent) >= maxSubIssues:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: #%d has %d sub-issues already", parent, maxSubIssues))
	}
	f.linkUnder(sub, parent)
	sub.UpdatedAt = f.tick()
	return http.StatusCreated, c.issue.Issue
}

// isAncestor reports whether issue a is issue n or above it, through the
// parents SetParent and addSubIssue set, or an error naming a parent on
// the way that the fake hasn't got, as once DeleteIssue deleted it. f is
// locked.
func (f *Fake) isAncestor(a, n int) (bool, error) {
	for seen := 0; n != 0 && seen <= len(f.issues); seen++ {
		if n == a {
			return true, nil
		}
		is, err := f.fakeNumberFind(n)
		if err != nil {
			return false, err
		}
		n = is.parent
	}
	return false, nil
}

// subIssueCount counts parent's sub-issues, those LinkSubIssue listed
// included. f is locked.
func (f *Fake) subIssueCount(parent int) int {
	n := len(f.linked[parent])
	for _, is := range f.issues {
		if is.parent == parent {
			n++
		}
	}
	return n
}

// subIssueNumbers returns the numbers of parent's sub-issues in this repo,
// in the order they were linked. f is locked.
func (f *Fake) subIssueNumbers(parent int) []int {
	var subs []*issue
	for _, is := range f.issues {
		if is.parent == parent {
			subs = append(subs, is)
		}
	}
	slices.SortStableFunc(subs, func(a, b *issue) int { return cmp.Compare(a.linkSeq, b.linkSeq) })
	var numbers []int
	for _, is := range subs {
		numbers = append(numbers, is.GetNumber())
	}
	return numbers
}

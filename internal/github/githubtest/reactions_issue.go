package githubtest

import (
	"slices"

	gogithub "github.com/google/go-github/v92/github"
)

// AddIssueReaction reacts to issue number itself, not to a comment, as
// author, and returns the reaction's ID. A reaction author has already
// changes nothing, and its ID is returned. As on GitHub, it moves no
// updated_at.
func (f *Fake) AddIssueReaction(number int, author, content string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, _ := f.react(&f.fakeNumberIssue(number).reactions, author, content)
	return r.GetID()
}

// DeleteIssueReaction removes a reaction from issue number itself, as the
// user who made it can, and reports whether it was there. It moves no
// updated_at.
func (f *Fake) DeleteIssueReaction(number int, id int64) bool {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakeNumberIssue(number)
	n := len(is.reactions)
	is.reactions = slices.DeleteFunc(is.reactions, func(r *gogithub.Reaction) bool { return r.GetID() == id })
	return len(is.reactions) < n
}

// listIssueReactions serves the reactions to an issue itself, oldest first,
// only those with the content= asked for, if any (fakeReactionFiltered),
// paged by per_page and page
// (GET /repos/{owner}/{repo}/issues/{issue_number}/reactions).
func (f *Fake) listIssueReactions(c *call) (int, any) {
	return page(c, fakeReactionFiltered(c, c.issue.reactions))
}

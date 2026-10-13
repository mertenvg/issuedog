package githubtest

import (
	gogithub "github.com/google/go-github/v92/github"
)

// Close closes an issue. It stays listed among its parent's sub-issues, as on
// GitHub.
func (f *Fake) Close(number int) {
	f.edit(number, func(is *issue) { is.State, is.UpdatedAt = new("closed"), f.tick() })
}

// subIssues serves an issue's sub-issues, open and closed, in the order
// they were filed, paged by per_page and page (GET
// /repos/{owner}/{repo}/issues/{issue_number}/sub_issues).
func (f *Fake) subIssues(c *call) (int, any) {
	subs := []gogithub.Issue{}
	for _, is := range f.issues {
		if is.parent == c.issue.GetNumber() {
			subs = append(subs, is.Issue)
		}
	}
	return page(c, subs)
}

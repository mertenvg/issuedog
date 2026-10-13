package githubtest

import (
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// Close closes an issue. It stays listed among its parent's sub-issues, as on
// GitHub.
func (f *Fake) Close(number int) {
	f.edit(number, func(is *issue) { is.State, is.UpdatedAt = new("closed"), f.tick() })
}

// linked is a sub-issue LinkSubIssue listed.
type linked struct {
	repo   string
	number int
}

// LinkSubIssue lists issue number of repo ("owner/name") as an open
// sub-issue of parent, after the ones SetParent made. GitHub allows one from
// another repo of the same owner. The fake serves nothing else of another
// repo: the listing has only the number, the state and repository_url,
// which names repo as given.
func (f *Fake) LinkSubIssue(parent int, repo string, number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linked == nil {
		f.linked = map[int][]linked{}
	}
	f.linked[parent] = append(f.linked[parent], linked{repo, number})
}

// subIssues serves an issue's sub-issues, open and closed, in the order
// they were filed, then any LinkSubIssue listed, paged by per_page and page
// (GET /repos/{owner}/{repo}/issues/{issue_number}/sub_issues).
func (f *Fake) subIssues(c *call) (int, any) {
	subs := []gogithub.Issue{}
	for _, is := range f.issues {
		if is.parent == c.issue.GetNumber() {
			sub := is.Issue
			sub.RepositoryURL = new(fmt.Sprintf("%s/repos/%s", f.srv.URL, f.repo))
			subs = append(subs, sub)
		}
	}
	for _, l := range f.linked[c.issue.GetNumber()] {
		subs = append(subs, gogithub.Issue{Number: new(l.number), State: new("open"),
			RepositoryURL: new(fmt.Sprintf("%s/repos/%s", f.srv.URL, l.repo))})
	}
	return page(c, subs)
}

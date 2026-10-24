package githubtest

import (
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// linked is a sub-issue LinkSubIssue listed.
type linked struct {
	repo   string
	number int
}

// LinkSubIssue lists issue number of repo ("owner/name") as an open
// sub-issue of parent, after the ones SetParent and addSubIssue linked. GitHub allows one from
// another repo of the same owner. The fake serves nothing else of another
// repo: the listing has only the number, the state and repository_url,
// which names repo as given. A parent the fake hasn't got fails the test,
// as a number does (fakeNumberIssue).
func (f *Fake) LinkSubIssue(parent int, repo string, number int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fakeNumberIssue(parent)
	if f.linked == nil {
		f.linked = map[int][]linked{}
	}
	f.linked[parent] = append(f.linked[parent], linked{repo, number})
}

// subIssues serves an issue's sub-issues, open and closed, in the order
// they were linked (SetParent, addSubIssue), then any LinkSubIssue listed,
// paged by per_page and page
// (GET /repos/{owner}/{repo}/issues/{issue_number}/sub_issues).
func (f *Fake) subIssues(c *call) (int, any) {
	subs := []gogithub.Issue{}
	for _, n := range f.subIssueNumbers(c.issue.GetNumber()) {
		is := f.issues[n-1]
		sub := is.Issue
		if is.listed() { // a transferred one names its new repo (Transfer)
			sub.RepositoryURL = new(fmt.Sprintf("%s/repos/%s", f.srv.URL, f.repo))
		}
		subs = append(subs, sub)
	}
	for _, l := range f.linked[c.issue.GetNumber()] {
		subs = append(subs, gogithub.Issue{Number: new(l.number), State: new("open"),
			RepositoryURL: new(fmt.Sprintf("%s/repos/%s", f.srv.URL, l.repo))})
	}
	return page(c, subs)
}

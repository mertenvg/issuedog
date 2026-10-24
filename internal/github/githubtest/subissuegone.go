package githubtest

import (
	"fmt"
	"slices"
)

// UnlinkBy removes the link of number below its parent as login, as the
// sub-issues panel does, and returns the node ID of the
// SubIssueRemovedEvent it records on the parent's timeline, naming login
// and the sub-issue, by its number and repository, which the GraphQL
// comments query serves (gqlEventsPage). It moves the sub-issue's
// updated_at, whose parent changes. An issue with no parent fails the
// test.
func (f *Fake) UnlinkBy(number int, login string) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakeNumberIssue(number)
	if is.parent == 0 {
		f.t.Fatalf("githubtest: #%d has no parent to unlink", number)
	}
	parent := f.fakeNumberIssue(is.parent)
	f.linkUnder(is, 0)
	is.UpdatedAt = f.tick()
	return f.subIssueGone(parent, linked{f.repo, number}, login)
}

// UnlinkLinkedBy removes the link of number of repo ("owner/name") below
// parent, one LinkSubIssue listed, as login, and returns the node ID of
// the SubIssueRemovedEvent it records, as UnlinkBy does. A link
// LinkSubIssue never listed fails the test.
func (f *Fake) UnlinkLinkedBy(parent int, repo string, number int, login string) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is, l := f.fakeNumberIssue(parent), linked{repo, number}
	i := slices.Index(f.linked[parent], l)
	if i < 0 {
		f.t.Fatalf("githubtest: %s#%d isn't linked below #%d", repo, number, parent)
	}
	f.linked[parent] = slices.Delete(f.linked[parent], i, i+1)
	return f.subIssueGone(is, l, login)
}

// subIssueGone records a SubIssueRemovedEvent on parent naming sub and
// login, and returns its node ID. f is locked.
func (f *Fake) subIssueGone(parent *issue, sub linked, login string) string {
	f.lastEvent++
	e := gqlEvent{node: fmt.Sprintf("SIRE_%d", firstEvent+f.lastEvent), by: login, at: f.tick().Time, sub: &sub}
	parent.gqlEvents = append(parent.gqlEvents, e)
	return e.node
}

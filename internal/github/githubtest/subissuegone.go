package githubtest

import (
	gogithub "github.com/google/go-github/v92/github"
)

// UnlinkBy removes the link of number below its parent as login, as the
// sub-issues panel does, and returns the ID of the sub_issue_removed event
// it writes on the parent's timeline, naming login and the sub-issue, by
// its number and repository, as GitHub's sub_issue_added event names them
// (github.SubIssueRemovals). The event is written after the lag
// (LagEvents). It moves the sub-issue's updated_at, whose parent changes.
// An issue with no parent fails the test.
func (f *Fake) UnlinkBy(number int, login string) int64 {
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
	f.lastEvent++
	id := firstEvent + f.lastEvent
	parent.labelEvents = append(parent.labelEvents, &gogithub.Timeline{ID: &id, Event: new("sub_issue_removed"), Actor: user(login),
		CreatedAt: &gogithub.Timestamp{Time: is.UpdatedAt.Add(f.eventLag)}})
	if parent.subIssueGone == nil {
		parent.subIssueGone = map[int64]int{}
	}
	parent.subIssueGone[id] = number
	return id
}

// subIssueGoneEvent is a sub_issue_removed event as the timeline serves
// it, with its sub-issue.
type subIssueGoneEvent struct {
	*gogithub.Timeline
	SubIssue subIssueGoneRef `json:"sub_issue"`
}

type subIssueGoneRef struct {
	Number     int `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// subIssueGoneServe is e as the timeline serves it, with the sub-issue it
// names, for a sub_issue_removed event of is (UnlinkBy), in repo. It
// reports false for any other event.
func (is *issue) subIssueGoneServe(e *gogithub.Timeline, repo string) (any, bool) {
	n, ok := is.subIssueGone[e.GetID()]
	if !ok {
		return nil, false
	}
	served := subIssueGoneEvent{Timeline: e, SubIssue: subIssueGoneRef{Number: n}}
	served.SubIssue.Repository.FullName = repo
	return served, true
}

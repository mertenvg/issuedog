package github

import (
	"testing"
	"time"
)

// TypeWaitClear forgets the unexplained issue types of one repo's issues,
// held back or left out, and nothing else: not another repo's, even one
// whose name starts with this one's, and not the repo's unexplained
// labels. Once forgotten, a type is due no more.
func TestTypeWaitClear(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	u := &Unexplained{}
	for _, issue := range []string{"o/r#1", "o/r#12", "o/rr#1", "o/r2#1", "oo/r#1"} {
		u.issueTypeRevertFirst(issue, "bug", 1, now)
	}
	u.issueTypeRevertLeaveOut("o/r#12", IssueTypeLeftOut{Name: "Bug", To: "Task"})
	u.set("o/r#1", heldIssue{since: now, labels: map[string]firstSeen{"stage:ready": {at: now}}})
	if got := u.TypeWaitClear("o", "r"); got != 2 {
		t.Errorf("TypeWaitClear = %d; want the 2 types of o/r", got)
	}
	for _, issue := range []string{"o/r#1", "o/r#12"} {
		if _, ok := u.types[issue]; ok {
			t.Errorf("%s's type is kept; want it forgotten", issue)
		}
	}
	if _, ok := u.IssueTypeLeftOut("o", "r", 12); ok {
		t.Error("o/r#12's type is still left out")
	}
	if due, ok := u.Due("o", "r", 12); ok {
		t.Errorf("o/r#12 is due at %v; want nothing kept", due)
	}
	// o/r#1's label stays, due by the label's own time.
	if due, ok := u.Due("o", "r", 1); !ok || !due.Equal(now.Add(UnexplainedWait)) || !u.Labels("o", "r", 1)["stage:ready"] {
		t.Errorf("o/r#1: due %v, %t, labels %v; want its label kept, due %v", due, ok, u.Labels("o", "r", 1), now.Add(UnexplainedWait))
	}
	for _, issue := range []string{"o/rr#1", "o/r2#1", "oo/r#1"} {
		if _, ok := u.types[issue]; !ok {
			t.Errorf("%s's type is forgotten; want it kept", issue)
		}
	}
	if got := u.TypeWaitClear("o", "r"); got != 0 {
		t.Errorf("TypeWaitClear again = %d; want 0", got)
	}
}

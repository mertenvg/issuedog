package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// This file is #183: issue types compared by ID, and the 5 minutes an
// issue type no event explains gets before it's reverted.

// SameIssueType compares by ID where both types have one, and else by
// name, ignoring case.
func TestSameIssueType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		aName string
		aID   int64
		bName string
		bID   int64
		want  bool
	}{
		{"Task", 5, "Task", 5, true},
		{"Chore", 5, "Task", 5, true}, // renamed in the org
		{"Task", 5, "Task", 6, false}, // another type of the same name
		{"Task", 5, "task", 0, true},  // one ID unknown: by name
		{"Task", 0, "task", 5, true},
		{"Task", 0, "Epic", 5, false},
		{"Task", 0, "TASK", 0, true},
		{"", 0, "", 0, true}, // no type, either way
		{"", 0, "Task", 5, false},
		{"Task", 5, "", 0, false},
	} {
		if got := SameIssueType(tc.aName, tc.aID, tc.bName, tc.bID); got != tc.want {
			t.Errorf("SameIssueType(%q, %d, %q, %d) = %t; want %t", tc.aName, tc.aID, tc.bName, tc.bID, got, tc.want)
		}
	}
}

// The App's event that sets back the type the change before it left, by
// ID, is a restore, though the org has renamed the type since, so the two
// events name it differently. Another type of the same name isn't.
func TestIssueTypeRestoresByID(t *testing.T) {
	t.Parallel()
	event := func(id int64, who string) *gogithub.Timeline {
		typ := "User"
		if who == appBot {
			typ = "Bot"
		}
		return &gogithub.Timeline{ID: &id, Event: new("issue_type_changed"), Actor: &gogithub.User{Login: &who, Type: &typ}}
	}
	for _, tc := range []struct {
		name  string
		types []string
		ids   []int64
		want  bool
	}{
		{"the same ID, renamed", []string{"Task", "Epic", "Chore"}, []int64{5, 7, 5}, true},
		{"the same name and ID", []string{"Task", "Epic", "Task"}, []int64{5, 7, 5}, true},
		{"the same name, another ID", []string{"Task", "Epic", "Task"}, []int64{5, 7, 6}, false},
		{"no IDs, the same name", []string{"Task", "Epic", "task"}, []int64{0, 0, 0}, true},
		{"no IDs, another name", []string{"Task", "Epic", "Chore"}, []int64{0, 0, 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &labelState{name: "the issue type", issueTypes: true, types: tc.types, ids: tc.ids, ats: []int{0, 1, 2},
				events: []*gogithub.Timeline{event(1, "owner"), event(2, "mallory"), event(3, appBot)}}
			r := labelRules{appSlug: "issuedog", trust: fakeTrust{"owner": true}, verdicts: map[string]bool{}}
			got, err := r.restores(context.Background(), l, 2)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("restores = %t; want %t", got, tc.want)
			}
		})
	}
}

// Due gives an unexplained issue type's revert, UnexplainedWait after it
// was first found so, or an unexplained label's removal, whichever comes
// first, and false once neither is kept. IssueTypeLeftOut gives the type
// only once it's left out.
func TestIssueTypeRevertDue(t *testing.T) {
	t.Parallel()
	var u Unexplained
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	key := unexplainedKey("o", "r", 7)
	if _, ok := u.Due("o", "r", 7); ok {
		t.Fatal("Due with nothing kept; want false")
	}
	u.issueTypeRevertFirst(key, "epic", 1, t0)
	if at, ok := u.Due("o", "r", 7); !ok || !at.Equal(t0.Add(UnexplainedWait)) {
		t.Errorf("Due = %v, %t; want the type's, %v", at, ok, t0.Add(UnexplainedWait))
	}
	if lo, ok := u.IssueTypeLeftOut("o", "r", 7); ok {
		t.Errorf("IssueTypeLeftOut = %+v before it's left out; want none", lo)
	}
	lo := IssueTypeLeftOut{Name: "Epic", ID: 7, To: "Task", ToID: 5}
	u.issueTypeRevertLeaveOut(key, lo)
	if got, ok := u.IssueTypeLeftOut("o", "r", 7); !ok || got != lo {
		t.Errorf("IssueTypeLeftOut = %+v, %t; want %+v", got, ok, lo)
	}
	// A label first found a minute before the type falls due first.
	u.set(key, heldIssue{since: t0.Add(-time.Minute), labels: map[string]firstSeen{"scope:x": {at: t0.Add(-time.Minute)}}})
	if at, ok := u.Due("o", "r", 7); !ok || !at.Equal(t0.Add(UnexplainedWait-time.Minute)) {
		t.Errorf("Due = %v, %t; want the label's, %v", at, ok, t0.Add(UnexplainedWait-time.Minute))
	}
	// One found a minute after doesn't.
	u.set(key, heldIssue{since: t0.Add(time.Minute), labels: map[string]firstSeen{"scope:x": {at: t0.Add(time.Minute)}}})
	if at, ok := u.Due("o", "r", 7); !ok || !at.Equal(t0.Add(UnexplainedWait)) {
		t.Errorf("Due = %v, %t; want the type's, %v", at, ok, t0.Add(UnexplainedWait))
	}
	u.issueTypeRevertForget(key)
	if at, ok := u.Due("o", "r", 7); !ok || !at.Equal(t0.Add(time.Minute+UnexplainedWait)) {
		t.Errorf("Due once the type is forgotten = %v, %t; want the label's, %v", at, ok, t0.Add(time.Minute+UnexplainedWait))
	}
	if _, ok := u.IssueTypeLeftOut("o", "r", 7); ok {
		t.Error("IssueTypeLeftOut once forgotten; want none")
	}
	// A type found again, with other events, isn't left out until it's
	// found so again.
	u.issueTypeRevertFirst(key, "epic", 1, t0)
	u.issueTypeRevertLeaveOut(key, lo)
	u.issueTypeRevertFirst(key, "epic", 2, t0.Add(time.Minute))
	if _, ok := u.IssueTypeLeftOut("o", "r", 7); ok {
		t.Error("IssueTypeLeftOut after its events changed; want none")
	}
	u.ForgetIssue("o", "r", 7)
	if _, ok := u.Due("o", "r", 7); ok {
		t.Error("Due after ForgetIssue; want false")
	}
	// Leaving out a type that isn't kept keeps nothing.
	u.issueTypeRevertLeaveOut(key, lo)
	if _, ok := u.IssueTypeLeftOut("o", "r", 7); ok {
		t.Error("IssueTypeLeftOut for a type never found; want none")
	}
}

// Snapshot's LeaveOutType takes the type that stands, and RestoreType
// gives back the issue's own, a removal's type from the labels included.
func TestSnapshotLeaveOutType(t *testing.T) {
	t.Parallel()
	s := &Snapshot{Labels: []string{"type:story"}}
	s.SetIssueType("Epic", 7)
	if s.Type != "epic" || s.TypeName != "Epic" || s.TypeID != 7 || s.TypeFromLabels {
		t.Fatalf("SetIssueType: %q, %q, %d, %t; want epic, Epic, 7, false", s.Type, s.TypeName, s.TypeID, s.TypeFromLabels)
	}
	lo := IssueTypeLeftOut{Name: "Epic", ID: 7, To: "Task", ToID: 5}
	s.LeaveOutType(lo)
	if s.Type != "task" || s.TypeName != "Task" || s.TypeID != 5 || s.TypeLeftOut == nil || *s.TypeLeftOut != lo {
		t.Errorf("left out: %q, %q, %d, %+v; want the type that stands, Task, and the type left out kept", s.Type, s.TypeName, s.TypeID, s.TypeLeftOut)
	}
	s.LeaveOutType(lo) // again: the issue's own type is kept, not Task
	s.RestoreType()
	if s.Type != "epic" || s.TypeName != "Epic" || s.TypeID != 7 || s.TypeLeftOut != nil {
		t.Errorf("restored: %q, %q, %d, %+v; want Epic back", s.Type, s.TypeName, s.TypeID, s.TypeLeftOut)
	}
	// A removal left out for Task: the issue's own is no issue type, so its
	// type comes from the labels again.
	s.SetIssueType("", 0)
	if s.Type != "story" || !s.TypeFromLabels {
		t.Fatalf("no issue type: %q, %t; want story from the labels", s.Type, s.TypeFromLabels)
	}
	s.LeaveOutType(IssueTypeLeftOut{To: "Task", ToID: 5})
	if s.Type != "task" || s.TypeFromLabels {
		t.Errorf("removal left out: %q, %t; want task, not from the labels", s.Type, s.TypeFromLabels)
	}
	s.RestoreType()
	if s.Type != "story" || !s.TypeFromLabels || s.TypeName != "" || s.TypeID != 0 {
		t.Errorf("removal restored: %q, %t, %q, %d; want story from the labels", s.Type, s.TypeFromLabels, s.TypeName, s.TypeID)
	}
	s.RestoreType() // nothing left out: no change
	if s.Type != "story" {
		t.Errorf("RestoreType with nothing left out: %q; want story", s.Type)
	}
}

// RevertIssueType compares the type GitHub answers with by ID where both
// have one: the type it set, renamed since, took; another type of the
// same name didn't.
func TestRevertIssueTypeAnswerByID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  map[string]any
		dropped bool
	}{
		{"the same ID, renamed", map[string]any{"id": 5, "name": "Chore"}, false},
		{"the same name and ID", map[string]any{"id": 5, "name": "Task"}, false},
		{"another ID of the same name", map[string]any{"id": 6, "name": "Task"}, true},
		{"no ID, the same name", map[string]any{"name": "task"}, false},
		{"no ID, another name", map[string]any{"name": "Chore"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"number": 7, "type": tc.answer}); err != nil {
					t.Errorf("answer: %v", err)
				}
			}))
			err := c.RevertIssueType(context.Background(), "o", "r", 7, IssueTypeRevert{Key: "9", By: "mallory", From: "Epic", To: "Task", ToID: 5, Posted: true})
			if got := errors.Is(err, ErrIssueTypeRevertDropped); got != tc.dropped || (err != nil && !tc.dropped) {
				t.Errorf("RevertIssueType = %v; want dropped %t", err, tc.dropped)
			}
		})
	}
}

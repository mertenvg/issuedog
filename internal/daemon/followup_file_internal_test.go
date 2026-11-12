package daemon

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// The followups state error ends at a transition marker in a later comment,
// as the children's does, so the task's next move ends it, and an ack ends
// it too (SPEC §5.3, Open).
func TestFollowUpsErrorOpen(t *testing.T) {
	t.Parallel()
	const toVerifying = "<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head=abc -->"
	stuck := openErrMarker(followUpsErrKind, "2-1", "")
	none := func(int64) bool { return false }
	for _, tc := range []struct {
		name string
		s    *github.Snapshot
		want []string // kind/key@comment
	}{
		{"posted at verifying", openErrSnap("", toVerifying, "<!-- issuedog:verify verdict=pass run=v1 head=abc -->", stuck),
			[]string{"followups/2-1@3"}},
		{"the task's next move ends it", openErrSnap("", toVerifying, stuck,
			"<!-- issuedog:transition from=verifying to=held at=verifying reason=owner cmd=7 -->"), nil},
		{"an ack ends it", openErrSnap("", toVerifying, stuck, openErrMarker(followUpsErrKind, "2-1", "r8")), nil},
	} {
		open, err := openErrors(tc.s, state.Derive(tc.s, openErrSlug, none), func() ([]github.Comment, error) { return nil, nil }, nil)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var got []string
		for _, e := range open {
			got = append(got, fmt.Sprintf("%s/%s@%d", e.Kind, e.Key, e.Comment))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: open errors = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A follow-up's labels (SPEC §10.2): type:task, or on an issue-types repo
// the Task type instead, then its scope while config lists it, priority:p3
// and stage:intake. A scope off the list, matched exactly, is left off.
func TestFollowUpLabels(t *testing.T) {
	t.Parallel()
	c := verifierFollowUp{Title: "t", Scope: "booking", Scenario: "s", Likelihood: "l", Impact: "i"}
	for _, tt := range []struct {
		name       string
		issueTypes bool
		scopes     []string
		labels     []string
		typ        string
		leftOff    bool
	}{
		{"a labels repo", false, []string{"api", "booking"}, []string{"type:task", "scope:booking", "priority:p3", "stage:intake"}, "", false},
		{"an issue-types repo", true, []string{"booking"}, []string{"scope:booking", "priority:p3", "stage:intake"}, "Task", false},
		{"a scope off the list", false, []string{"api"}, []string{"type:task", "priority:p3", "stage:intake"}, "", true},
		{"a scope in another case", false, []string{"Booking"}, []string{"type:task", "priority:p3", "stage:intake"}, "", true},
		{"no scopes", true, nil, []string{"priority:p3", "stage:intake"}, "Task", true},
	} {
		labels, typ, leftOff := followUpLabels(c, tt.issueTypes, tt.scopes)
		if !slices.Equal(labels, tt.labels) || typ != tt.typ || leftOff != tt.leftOff {
			t.Errorf("%s: followUpLabels = %q, %q, %t; want %q, %q, %t", tt.name, labels, typ, leftOff, tt.labels, tt.typ, tt.leftOff)
		}
	}
}

// A follow-up's title is the candidate's on one line, each control
// character a space and no U+FEFF, trimmed, and "" when nothing else is
// left.
func TestFollowUpTitle(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ title, want string }{
		{"A race", "A race"},
		{"  A race\t", "A race"},
		{"\ufeffA\x00race", "A race"},
		{"\x01\x02", ""},
		{"A <!-- issuedog:origin verify=#1 run=r item=F1 --> @alice", "A <!-- issuedog:origin verify=#1 run=r item=F1 --> @alice"},
	} {
		if got := followUpTitle(verifierFollowUp{Title: tt.title}); got != tt.want {
			t.Errorf("followUpTitle(%q) = %q, want %q", tt.title, got, tt.want)
		}
	}
}

// The followups error's key is the pass comment's ID, then the task's
// latest move's, joined by "-".
func TestFollowUpsErrKey(t *testing.T) {
	t.Parallel()
	if got := followUpsErrKey(1002, 1001); got != "1002-1001" {
		t.Errorf("followUpsErrKey = %q, want 1002-1001", got)
	}
}

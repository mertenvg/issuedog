package daemon

import (
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// TestIvRoundMove: only an interviewer run's move from clarifying to
// clarifying is a round's.
func TestIvRoundMove(t *testing.T) {
	tests := []struct {
		fields string
		kind   string
		want   bool
	}{
		{"from=clarifying to=clarifying run=a", "transition", true},
		{"from=clarifying to=clarifying cmd=5", "transition", false},
		{"from=clarifying to=clarifying run=a", "error", false},
		{"from=spec-review to=clarifying run=a", "transition", false},
		{"from=clarifying to=spec-review run=a", "transition", false},
	}
	for _, tt := range tests {
		m := github.ParseMarkers("<!-- issuedog:" + tt.kind + " " + tt.fields + " -->")
		if len(m) != 1 {
			t.Fatalf("%s %s parses to %v", tt.kind, tt.fields, m)
		}
		if got := ivRoundMove(m[0]); got != tt.want {
			t.Errorf("ivRoundMove(%s %s) = %v, want %v", tt.kind, tt.fields, got, tt.want)
		}
	}
}

// TestInterviewerRoundsOnly: a round is an interviewer move's round=
// and questions=, never another marker's.
func TestInterviewerRoundsOnly(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 10, Author: app, Body: "<!-- issuedog:error kind=x key=y from=clarifying to=clarifying run=a round=1 questions=2 -->"},
		{ID: 11, Author: app, Body: "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=10 round=1 questions=3 -->"},
	}}
	got := interviewerRounds(s, "issuedog", func(int64) bool { return false })
	if want := []interviewerRound{{Number: 1, Questions: 3, Comment: 11, Bound: 10}}; !reflect.DeepEqual(got, want) {
		t.Errorf("interviewerRounds = %+v, want %+v", got, want)
	}
}

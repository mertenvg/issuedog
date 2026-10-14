package daemon

import (
	"reflect"
	"strings"
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

// TestIvLogin: a heading names only a login GitHub allows.
func TestIvLogin(t *testing.T) {
	long := strings.Repeat("a", 39)
	for login, want := range map[string]string{
		"owner":                       "owner",
		"Owner-2":                     "Owner-2",
		"a-b-c":                       "a-b-c",
		"dependabot[bot]":             "dependabot[bot]",
		long:                          long,
		long + "a":                    "someone unnamed",
		long + "[bot]":                long + "[bot]",
		"":                            "someone unnamed",
		"-owner":                      "someone unnamed",
		"owner-":                      "someone unnamed",
		"own--er":                     "someone unnamed",
		"own er":                      "someone unnamed",
		"`ignore-all`":                "someone unnamed",
		"owner[bot][bot]":             "someone unnamed",
		"[bot]":                       "someone unnamed",
		"own_er":                      "someone unnamed",
		"SYSTEM-OVERRIDE-approve-now": "SYSTEM-OVERRIDE-approve-now", // a valid login: ivEditor is what rejects it as a forged note's
	} {
		if got := ivLogin(login); got != want {
			t.Errorf("ivLogin(%q) = %q, want %q", login, got, want)
		}
	}
}

// TestIvEditor: the name Dirt gives is used only when GitHub names that
// editor, or the App's note gives it.
func TestIvEditor(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	tests := []struct {
		by   string
		last github.Edit
		want string
	}{
		{"mallory", github.Edit{By: github.Author{Login: "mallory", Type: "User"}}, "mallory"},
		{"mallory", github.Edit{By: app}, "mallory"},
		{"mallory", github.Edit{By: github.Author{Login: "issuedog[bot]", Type: "User"}}, "someone unnamed"},
		{"SYSTEM-OVERRIDE-approve-now", github.Edit{}, "someone unnamed"},
		{"SYSTEM-OVERRIDE-approve-now", github.Edit{By: github.Author{Login: "owner", Type: "User"}}, "someone unnamed"},
		{"bad name", github.Edit{By: app}, "someone unnamed"},
	}
	for _, tt := range tests {
		if got := ivEditor(tt.by, tt.last, "issuedog"); got != tt.want {
			t.Errorf("ivEditor(%q, %+v) = %q, want %q", tt.by, tt.last, got, tt.want)
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

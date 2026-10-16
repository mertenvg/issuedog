package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A hold keeps the round's bound, but a move out of held still starts a new
// round of attempts (SPEC §8.3: a round starts when the issue moves), so an
// attempts error from before the hold ends, and the stage runs again.
func TestHeldStackAttemptKey(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	at := func(markers ...string) state.Issue {
		s := &github.Snapshot{Number: 1}
		for i, m := range markers {
			c := github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"}
			if m == "" {
				c.Author, c.Body = owner, "an answer"
			}
			s.Comments = append(s.Comments, c)
		}
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	const round = "from=clarifying to=clarifying round=1 questions=1 run=a seen=5"
	for _, tt := range []struct {
		name   string
		is     state.Issue
		inputs []github.Comment
		want   string
	}{
		{"a round's bound is its seen", at(round), nil, "clarifying-5"},
		{"or a newer input", at(round, ""), []github.Comment{{ID: 11}}, "clarifying-11"},
		{"held, the hold's bound", at(round, "from=clarifying to=held at=clarifying reason=owner cmd=3 seen=5"), nil, "held-5"},
		{"after a move out of held, its comment", at(round, "from=clarifying to=held at=clarifying reason=owner cmd=3 seen=5",
			"from=held to=clarifying cmd=4 seen=5"), nil, "clarifying-12"},
		{"or a newer input", at(round, "from=clarifying to=held at=clarifying reason=owner cmd=3 seen=5",
			"from=held to=clarifying cmd=4 seen=5", ""), []github.Comment{{ID: 13}}, "clarifying-13"},
		{"and after a hand-set move out of held", at(round, "from=clarifying to=held at=clarifying reason=owner cmd=3 seen=5",
			"from=held to=spec-review by=owner event=9 seen=5"), nil, "spec-review-12"},
	} {
		if got := attemptKey(tt.is, tt.inputs); got != tt.want {
			t.Errorf("%s: attempt key = %s, want %s", tt.name, got, tt.want)
		}
	}
}

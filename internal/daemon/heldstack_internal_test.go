package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A hold keeps the round's bound, but a move out of held still starts a new
// round of attempts (SPEC §8.3: a round starts when the issue moves), so an
// attempts error from before the hold ends, and the stage runs again.
func TestHeldStackAttemptKey(t *testing.T) {
	t.Parallel()
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

// Which holds /resume ends (SPEC §6.4, where the first hold wins, and #188's
// decision, round 3): on the issue itself, every owner hold but one posted
// after the /resume, or a comment deleted after it that may have been one
// (#205, #244); below, only a /hold made on the issue or above it, posted
// before the /resume, or a comment deleted there before it that may have
// been one (resolveGone). A
// hold by hand ends only on the issue itself, and the issue leaves held
// only when no hold is left. A discussion hold ends only on the issue
// itself (#188).
func TestHeldStackResume(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	at := func(markers ...string) state.Issue {
		s := &github.Snapshot{Number: 1}
		for i, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	const (
		ready      = "from=intake to=ready run=a"
		above      = "from=ready to=held at=ready reason=owner cmd=5 seen=10"
		below      = "from=ready to=held at=ready reason=owner cmd=6 seen=10"
		hand       = "from=ready to=held at=ready reason=owner by=owner event=7 seen=10"
		onHeld     = "from=held to=held at=ready reason=owner by=owner event=8 seen=10"
		discussion = "from=ready to=held at=ready origin=ready reason=discussion event=9 seen=10"
		later      = "from=ready to=held at=ready reason=owner cmd=200 seen=10"
		laterHeld  = "from=held to=held at=ready reason=owner cmd=200 seen=10"
		goneAbove  = "from=held to=held reason=owner gone=3-50"
		goneLater  = "from=held to=held reason=owner gone=3-150"
		goneBelow  = "from=held to=held reason=owner gone=4-50"
	)
	// The /resume, comment 100 posted at 100 s, on #3, with comment 5 and
	// comment 200, posted after it, on #3 or above.
	made := madeAboveSet{ids: map[int64]bool{5: true, 200: true}, issues: map[int]bool{3: true},
		resume: resolveResume{id: 100, at: time.Unix(100, 0).UTC()}}
	for _, tt := range []struct {
		name        string
		is          state.Issue
		self        bool
		ended, left int
	}{
		{"a /hold made above ends from above", at(ready, above), false, 1, 0},
		{"a /hold made below doesn't", at(ready, below), false, 0, 1},
		{"a hold by hand doesn't", at(ready, hand), false, 0, 1},
		{"but ends on the issue itself", at(ready, hand), true, 1, 0},
		{"as does a /hold made below", at(ready, below), true, 1, 0},
		{"a hold by hand on top of a /hold made above stays", at(ready, above, onHeld), false, 1, 1},
		{"until /resume on the issue itself", at(ready, above, onHeld), true, 2, 0},
		{"a discussion hold ends on the issue itself (T4)", at(ready, discussion), true, 1, 0},
		{"but not from above", at(ready, discussion), false, 0, 1},
		{"nor is a hold by hand on top of it, from above", at(ready, discussion, onHeld), false, 0, 2},
		{"an at that's no stage to go back to has no holds", at(ready, "from=ready to=held at=held reason=owner cmd=5"), true, 0, 0},
		{"nor has one that isn't open", at(ready, "from=ready to=held at=closed reason=owner cmd=5"), true, 0, 0},
		{"an issue that isn't held has none", at(ready), true, 0, 0},
		{"a /hold made above but posted after the /resume doesn't end from above", at(ready, later), false, 0, 1},
		{"nor on the issue itself (#205)", at(ready, later), true, 0, 1},
		{"which ends the holds before it there, but not that one", at(ready, above, onHeld, laterHeld), true, 2, 1},
		{"a comment deleted above before the /resume, which may have been a /hold, ends from above", at(ready, above, goneAbove), false, 2, 0},
		{"one deleted after the /resume doesn't", at(ready, above, goneLater), false, 1, 1},
		{"nor does one deleted below", at(ready, above, goneBelow), false, 1, 1},
		// On the issue itself, a comment deleted before the /resume ends,
		// wherever it was deleted, but not one deleted after it: it may have
		// been a /hold posted after the /resume, which §6.4 says only a
		// /resume posted after the deletion ends (#244).
		{"on the issue itself, one deleted before the /resume ends, but not one deleted after it", at(ready, above, goneLater, goneBelow), true, 2, 1},
		{"one deleted before it ends there, wherever it was", at(ready, above, goneAbove, goneBelow), true, 3, 0},
	} {
		_, ended, left := heldStackResume(tt.is, tt.self, made)
		if ended != tt.ended || len(left) != tt.left {
			t.Errorf("%s: ended %d, %d left; want %d and %d", tt.name, ended, len(left), tt.ended, tt.left)
		}
		// Only the /hold posted after the /resume, comment 200, is a later
		// one (#205), and the comment deleted after it, at 150 s, which may
		// have been one (#244).
		wantLater := slices.ContainsFunc(left, func(m github.Marker) bool { return m.Fields["cmd"] == "200" || m.Fields["gone"] == "3-150" })
		if got := heldStackLater(left, made); got != wantLater {
			t.Errorf("%s: heldStackLater = %t, want %t", tt.name, got, wantLater)
		}
		if _, ok := resume(tt.is, tt.self, made, "`/resume`", false); ok != (tt.ended > 0 && tt.left == 0) {
			t.Errorf("%s: resume moves it = %t, want %t", tt.name, ok, !ok)
		}
	}
}

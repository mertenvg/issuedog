package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A held → held marker with no hold read before it, as discRestore writes
// when an edit or a deletion hid the move into held, is that move (#221):
// the clarifying after a hold before it gives its origin, and a /resume
// back there carries it. A held → held marker after a hold that is read
// leaves the move before that hold as it was, and a move to another stage
// in between still counts as the one before the hold.
func TestHoldOriginResolveHeldAfterHold(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	at := func(markers ...string) state.Issue {
		s := &github.Snapshot{}
		for i, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	const (
		after    = "from=held to=clarifying at=spec-review origin=spec-review event=h3"
		restored = "from=held to=held at=clarifying origin=spec-review reason=discussion event=e5-1"
		owner    = "from=held to=held at=clarifying origin=spec-review reason=owner event=e5-1"
		hold     = "from=clarifying to=held at=clarifying origin=spec-review reason=owner cmd=5"
		byHand   = "from=held to=held at=clarifying origin=spec-review reason=owner by=owner event=8"
	)
	for _, tt := range []struct {
		name    string
		markers []string
		want    bool
	}{
		{"a restored hold", []string{after, restored, owner}, true},
		{"a hold by hand after a hold", []string{after, hold, byHand}, true},
		{"a restored hold after another stage", []string{after, "from=clarifying to=spec-review rev=2 body=ab run=b", restored, owner}, false},
	} {
		o, ok := resolveHeldAfterHold(at(tt.markers...))
		if ok != tt.want || ok && o != state.SpecReview {
			t.Errorf("%s: %q, %t; want %t", tt.name, o, ok, tt.want)
		}
	}
}

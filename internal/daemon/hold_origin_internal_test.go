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
// in between still counts as the one before the hold. The App's own close
// from that clarifying, as a dropped child's, which carries no origin,
// isn't the move before a trusted reopen's hold: the move before the
// close is (#417). A parent's revision that changes the hold's at, by a
// held → held marker with no reason, leaves no clarifying for /resume to
// carry the origin back to.
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
		dropped  = "from=clarifying to=closed reason=not_planned event=p77"
		reopened = "from=closed to=held at=clarifying origin=spec-review reason=owner event=9"
		changed  = "from=held to=held at=planning origin=planning event=p77"
	)
	for _, tt := range []struct {
		name    string
		markers []string
		want    bool
	}{
		{"a restored hold", []string{after, restored, owner}, true},
		{"a hold by hand after a hold", []string{after, hold, byHand}, true},
		{"a restored hold after another stage", []string{after, "from=clarifying to=spec-review rev=2 body=ab run=b", restored, owner}, false},
		{"a reopen of the App's close from there", []string{after, dropped, reopened}, true},
		{"a hold whose at a revision changed", []string{after, hold, changed}, false},
		{"a restored hold whose at a revision changed", []string{after, restored, owner, changed}, false},
	} {
		o, ok := resolveHeldAfterHold(at(tt.markers...))
		if ok != tt.want || ok && o != state.SpecReview {
			t.Errorf("%s: %q, %t; want %t", tt.name, o, ok, tt.want)
		}
	}
}

// planSkipsReview reads a /resume to planning, of a story held from the
// clarifying after a hold whose hold's at a parent's revision changed, as
// the move that put it at planning (planEntry): with no origin, as resume
// now writes it (TestHoldOriginChangedResume), its first plan skips review when plan.review is false;
// with the old origin, which it carried before #417, it waits for review,
// which SPEC §7.2's Skipping plan review doesn't ask for.
func TestHoldOriginChangedSkipsReview(t *testing.T) {
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
		after   = "from=held to=clarifying at=planning origin=planning event=h3 seen=7"
		hold    = "from=clarifying to=held at=clarifying origin=planning reason=owner cmd=5 seen=7"
		changed = "from=held to=held at=planning origin=planning event=p77 seen=7"
	)
	for _, tt := range []struct {
		name, resume string
		want         bool
	}{
		{"with no origin", "from=held to=planning cmd=6 seen=7", true},
		{"with the old origin", "from=held to=planning origin=planning cmd=6 seen=7", false},
	} {
		if got := planSkipsReview(false, state.ClassStory, at(after, hold, changed, tt.resume)); got != tt.want {
			t.Errorf("%s: planSkipsReview is %t; want %t", tt.name, got, tt.want)
		}
	}
}

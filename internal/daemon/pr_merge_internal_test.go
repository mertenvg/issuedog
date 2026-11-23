package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// prMoveTip is the tip= of the move prHeadOf reads its head= from, the
// latest into pr-open that names a head, or "" when that move names none,
// as a person's head that failed verify has: an older move's tip= is never
// taken in its place, so no head nobody verified is merged (SPEC §5,
// §7.5). A move into pr-open that names no head, as a trusted reopen's, is
// looked through, as prHeadOf looks through it.
func TestPRMoveTip(t *testing.T) {
	t.Parallel()
	verified := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": "aaa", "pr": "2", "tip": "t1"})
	handoff := prHeadMarker(map[string]string{"from": "pr-open", "to": "needs-human", "event": "c2-1"})
	reopened := prHeadMarker(map[string]string{"from": "needs-human", "to": "pr-open", "event": "9", "pr": "2"})
	newHead := prHeadMarker(map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"})
	failed := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "event": "v5", "head": "bbb"})
	passed := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rw", "head": "bbb", "tip": "t2"})
	noTip := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": "aaa", "pr": "2"})
	for _, tt := range []struct {
		name    string
		markers []github.Marker
		want    string
	}{
		{"none", nil, ""},
		{"the PR step's", []github.Marker{verified}, "t1"},
		{"through a trusted reopen", []github.Marker{verified, handoff, reopened}, "t1"},
		{"not a new head's own", []github.Marker{verified, newHead}, "t1"},
		{"a person's head that failed verify", []github.Marker{verified, newHead, failed}, ""},
		{"a person's head that failed, through a reopen", []github.Marker{verified, newHead, failed, handoff, reopened}, ""},
		{"a person's head that passed", []github.Marker{verified, newHead, passed}, "t2"},
		{"a PR step's move with no tip", []github.Marker{noTip}, ""},
	} {
		is := state.Issue{Markers: tt.markers}
		if got := prMoveTip(is); got != tt.want {
			t.Errorf("%s: prMoveTip = %q; want %q", tt.name, got, tt.want)
		}
	}
}

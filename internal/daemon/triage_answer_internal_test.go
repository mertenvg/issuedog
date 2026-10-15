package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// flagInPipeline names the first reason an issue is in the pipeline: a
// stage label, one left out, then a transition marker.
func TestFlagInPipeline(t *testing.T) {
	transition := state.Issue{Markers: []github.Marker{{Kind: "triage"}, {Kind: "transition"}}}
	for _, tc := range []struct {
		name string
		s    *github.Snapshot
		is   state.Issue
		want string
	}{
		{"nothing", &github.Snapshot{Labels: []string{"triage:ask", "priority:p1"}}, state.Issue{Markers: []github.Marker{{Kind: "triage"}}}, ""},
		{"a stage label, any case", &github.Snapshot{Labels: []string{"Stage:Intake"}}, transition, "this one has `stage:intake`"},
		{"a stage label left out", &github.Snapshot{LeftOut: map[string]bool{"stage:intake": true}}, transition, "this one has a stage label"},
		{"a pipeline label left out isn't one", &github.Snapshot{LeftOut: map[string]bool{"priority:p1": true}}, state.Issue{}, ""},
		{"a transition", &github.Snapshot{}, transition, "this one has been in the pipeline"},
	} {
		if got := flagInPipeline(tc.s, tc.is); got != tc.want {
			t.Errorf("%s: flagInPipeline = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFlagAsked(t *testing.T) {
	for label, want := range map[string]bool{"triage:ask": true, "Triage:ASK": true, "triage:left": false, "stage:intake": false, "": false} {
		if got := flagAsked(label); got != want {
			t.Errorf("flagAsked(%q) = %t, want %t", label, got, want)
		}
	}
}

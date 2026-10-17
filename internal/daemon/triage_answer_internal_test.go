package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// flagInPipeline names the first reason an issue is in the pipeline: a
// stage label, one left out, then a transition marker.
func TestFlagInPipeline(t *testing.T) {
	t.Parallel()
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

// flagLeave checks its comment first: with a clean "left" marker keyed by
// its key on the issue already, it only finishes the labels. A marker of
// another key, a dirty one, or another kind's, doesn't count.
func TestFlagLeaveCheckedFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		dirty      bool
		comments   int
	}{
		{"its own", "<!-- issuedog:triage verdict=leave cmd=r5 -->", false, 1},
		{"another key's", "<!-- issuedog:triage verdict=leave cmd=r6 -->", false, 2},
		{"a dirty one", "<!-- issuedog:triage verdict=leave cmd=r5 -->", true, 2},
		{"a transition's", "<!-- issuedog:transition from=intake to=clarifying cmd=r5 -->", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Title", "body")
			id := f.AddComment(1, "issuedog[bot]", tc.body)
			c := f.Client(t)
			s, err := c.Snapshot(context.Background(), "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			d := Dirt{}
			if tc.dirty {
				d[id] = "mallory"
			}
			a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
			if err := flagLeave(context.Background(), a, s, d, state.ReactionKey(5), "Left."); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if len(is.Comments) != tc.comments || !slices.Equal(is.Labels, []string{triageLeftLabel}) {
				t.Errorf("%d comments, labels %v; want %d and triage:left", len(is.Comments), is.Labels, tc.comments)
			}
		})
	}
}

func TestFlagAsked(t *testing.T) {
	t.Parallel()
	for label, want := range map[string]bool{"triage:ask": true, "Triage:ASK": true, "triage:left": false, "stage:intake": false, "": false} {
		if got := flagAsked(label); got != want {
			t.Errorf("flagAsked(%q) = %t, want %t", label, got, want)
		}
	}
}

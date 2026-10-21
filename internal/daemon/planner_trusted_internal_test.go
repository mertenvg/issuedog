package daemon

import (
	"context"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// TestPlannerTrustedStageHeld: a sub-issue's stage comes from the snapshot
// the daemon holds of it after step 1 succeeded, which costs no request,
// and from a fresh read once nothing holds it, as when it's to be read
// again (#312).
func TestPlannerTrustedStageHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", waitSlug)
	n := f.AddIssue(waitApp.Login, "S1. A story", "")
	f.AddLabel(n, waitApp.Login, "stage:clarifying")
	f.AddComment(n, waitApp.Login, "Created.\n\n<!-- issuedog:transition from=intake to=clarifying event=p1 -->")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: waitSlug}, Trust: ownerOnly{}}
	held, err := st.Client.Snapshot(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Step: st, snaps: map[int]*github.Snapshot{n: held}, inputs: map[int][]github.Comment{n: nil}}
	// A move since: only a fresh read sees it.
	f.AddComment(n, waitApp.Login, "Specified.\n\n<!-- issuedog:transition from=clarifying to=spec-review run=iv1 -->")

	before := len(f.Requests())
	stage, err := plannerTrustedStage(ctx, d, n)
	if err != nil {
		t.Fatal(err)
	}
	if stage != state.Clarifying || len(f.Requests()) != before {
		t.Errorf("with the snapshot held, the stage is %q after %d requests; want clarifying, after none", stage, len(f.Requests())-before)
	}
	d.reread(n, false)
	if stage, err = plannerTrustedStage(ctx, d, n); err != nil {
		t.Fatal(err)
	}
	if stage != state.SpecReview || len(f.Requests()) == before {
		t.Errorf("with the snapshot to be read again, the stage is %q after %d requests; want spec-review, read afresh", stage, len(f.Requests())-before)
	}
}

// TestPlannerTrustedTitle: the title of an issue by the App or a trusted
// author, as of its last trusted change, and who renamed it since; an
// untrusted author's is left to contextAdmission.
func TestPlannerTrustedTitle(t *testing.T) {
	t.Parallel()
	mallory := github.Author{Login: "mallory", Type: "User"}
	tests := []struct {
		name      string
		author    github.Author
		renames   []github.DiscRename
		title, by string
	}{
		{"no rename", waitOwner, nil, "Now", ""},
		{"the owner's rename", waitOwner, []github.DiscRename{{By: waitOwner, From: "First", Title: "Now"}}, "Now", ""},
		{"mallory's rename", waitOwner, []github.DiscRename{{By: mallory, From: "First", Title: "Now"}}, "First", "mallory"},
		{"the App's, then mallory's", waitApp, []github.DiscRename{{By: waitApp, From: "First", Title: "Second"}, {By: mallory, From: "Second", Title: "Now"}},
			"Second", "mallory"},
		{"mallory's, then the owner's", waitOwner, []github.DiscRename{{By: mallory, From: "First", Title: "Second"}, {By: waitOwner, From: "Second", Title: "Now"}},
			"Now", ""},
		{"a ghost's", waitOwner, []github.DiscRename{{From: "First", Title: "Now"}}, "First", "unknown"},
		{"an untrusted author's", mallory, []github.DiscRename{{By: mallory, From: "First", Title: "Now"}}, "Now", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := trustCache{st: &Step{Applier: &Applier{AppSlug: waitSlug}, Trust: ownerOnly{}}, verdicts: map[string]bool{}}
			title, by, err := plannerTrustedTitle(context.Background(), waitSlug, tc, &github.Snapshot{Title: "Now", Author: tt.author}, tt.renames)
			if err != nil {
				t.Fatal(err)
			}
			if title != tt.title || by != tt.by {
				t.Errorf("plannerTrustedTitle = %q, %q; want %q, %q", title, by, tt.title, tt.by)
			}
		})
	}
}

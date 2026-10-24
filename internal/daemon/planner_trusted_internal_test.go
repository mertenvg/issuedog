package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// TestPlannerTrustedStageHeld: a sub-issue's stage comes from the snapshot
// the daemon holds of it after step 1 succeeded, which costs no request,
// and from a fresh read once nothing holds it, as when it's to be read
// again (#312). The daemon's maps are as a pass leaves them, stale too, so
// reread drops only the snapshot's inputs, and keeps the stale snapshot,
// which must not be used.
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
	d := &Daemon{Step: st, snaps: map[int]*github.Snapshot{n: held}, inputs: map[int][]github.Comment{n: nil}, stale: map[int]bool{}}
	// A move since: only a fresh read sees it.
	f.AddComment(n, waitApp.Login, "Specified.\n\n<!-- issuedog:transition from=clarifying to=spec-review run=iv1 -->")

	before := len(f.Requests())
	stage, _, err := plannerTrustedOpen(ctx, d, n)
	if err != nil {
		t.Fatal(err)
	}
	if stage != state.Clarifying || len(f.Requests()) != before {
		t.Errorf("with the snapshot held, the stage is %q after %d requests; want clarifying, after none", stage, len(f.Requests())-before)
	}
	d.reread(n, false)
	if d.snaps[n] != held {
		t.Fatal("reread dropped the held snapshot, so this test can't tell a stale one from none")
	}
	if stage, _, err = plannerTrustedOpen(ctx, d, n); err != nil {
		t.Fatal(err)
	}
	if stage != state.SpecReview || len(f.Requests()) == before {
		t.Errorf("with the snapshot to be read again, the stage is %q after %d requests; want spec-review, read afresh", stage, len(f.Requests())-before)
	}
}

// TestPlannerTrustedOpenType: an open sub-issue's type is the one step 1
// left on the snapshot the daemon holds of it, which costs no request.
// Read afresh, its type by label is the first type:* label that stands,
// judged from its timeline as the label rules do, so mallory's, which
// step 1 hasn't reverted yet, is no type, and the owner's after it is its
// type (#445).
func TestPlannerTrustedOpenType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const evil = "type:<!-- issuedog:x --> @alice"
	f := githubtest.New(t, "o/r", waitSlug)
	n := f.AddIssue(waitApp.Login, "S1. A story", "")
	f.AddLabel(n, waitApp.Login, "type:story")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: waitSlug}, Trust: ownerOnly{}}
	held, err := st.Client.Snapshot(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Step: st, snaps: map[int]*github.Snapshot{n: held}, inputs: map[int][]github.Comment{n: nil}, stale: map[int]bool{}}
	f.RemoveLabel(n, "mallory", "type:story")
	f.AddLabel(n, "mallory", evil)

	before := len(f.Requests())
	_, typ, err := plannerTrustedOpen(ctx, d, n)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "story" || len(f.Requests()) != before {
		t.Errorf("with the snapshot held, the type is %q after %d requests; want story, after none", typ, len(f.Requests())-before)
	}
	d.reread(n, false)
	if _, typ, err = plannerTrustedOpen(ctx, d, n); err != nil {
		t.Fatal(err)
	}
	if typ != "" {
		t.Errorf("read afresh, with only mallory's type label, the type is %q; want none", typ)
	}
	f.AddLabel(n, "owner", "type:task")
	if _, typ, err = plannerTrustedOpen(ctx, d, n); err != nil {
		t.Fatal(err)
	}
	if typ != "task" {
		t.Errorf("read afresh, with mallory's type label and then the owner's, the type is %q; want task", typ)
	}
}

// TestPlannerTrustedSubClosed: a closed sub-issue gets no stage, and its
// type is the listing's issue type, at no request, or its type:* labels
// judged from its timeline, so one only mallory added is no type, and nor
// is one she added back after the owner removed it (#445).
func TestPlannerTrustedSubClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", waitSlug)
	n := f.AddIssue(waitApp.Login, "T1. A task", "")
	f.AddLabel(n, "mallory", "type:bug")
	f.AddLabel(n, waitApp.Login, "type:task")
	f.AddLabel(n, "owner", "type:feature")
	f.RemoveLabel(n, "owner", "type:feature")
	f.AddLabel(n, "mallory", "type:feature")
	f.CloseAs(n, waitApp.Login, "completed")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: waitSlug}, Trust: ownerOnly{}}
	d := &Daemon{Step: st}

	before := len(f.Requests())
	stage, typ, err := plannerTrustedSub(ctx, d, n, false, "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stage != "" || typ != "task" || len(f.Requests()) != before {
		t.Errorf("with an issue type, the stage and type are %q, %q after %d requests; want none and task, after none", stage, typ, len(f.Requests())-before)
	}
	for labels, want := range map[string]string{"type:bug": "", "type:bug,type:task": "task", "type:feature": "", "type:feature,type:task": "task"} {
		if stage, typ, err = plannerTrustedSub(ctx, d, n, false, "", strings.Split(labels, ",")); err != nil {
			t.Fatal(err)
		}
		if stage != "" || typ != want {
			t.Errorf("labelled %s, the stage and type are %q, %q; want none and %q", labels, stage, typ, want)
		}
	}
}

// TestPlannerTrustedTitle: the title of an issue by the App or a trusted
// author, as of its last trusted change, and who renamed it since, as the
// note names them; an untrusted author's is left to contextAdmission. A
// ghost is "someone unnamed", never a login like "unknown".
func TestPlannerTrustedTitle(t *testing.T) {
	t.Parallel()
	mallory := github.Author{Login: "mallory", Type: "User"}
	tests := []struct {
		name      string
		author    github.Author
		renames   []github.DiscRename
		title, by string // by is "" for no renamer
	}{
		{"no rename", waitOwner, nil, "Now", ""},
		{"the owner's rename", waitOwner, []github.DiscRename{{By: waitOwner, From: "First", Title: "Now"}}, "Now", ""},
		{"mallory's rename", waitOwner, []github.DiscRename{{By: mallory, From: "First", Title: "Now"}}, "First", "mallory"},
		{"the App's, then mallory's", waitApp, []github.DiscRename{{By: waitApp, From: "First", Title: "Second"}, {By: mallory, From: "Second", Title: "Now"}},
			"Second", "mallory"},
		{"mallory's, then the owner's", waitOwner, []github.DiscRename{{By: mallory, From: "First", Title: "Second"}, {By: waitOwner, From: "Second", Title: "Now"}},
			"Now", ""},
		{"a ghost's", waitOwner, []github.DiscRename{{From: "First", Title: "Now"}}, "First", "someone unnamed"},
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
			named := ""
			if by != nil {
				named = ivLogin(by.Login)
			}
			if title != tt.title || named != tt.by {
				t.Errorf("plannerTrustedTitle = %q, by %q; want %q, by %q", title, named, tt.title, tt.by)
			}
		})
	}
}

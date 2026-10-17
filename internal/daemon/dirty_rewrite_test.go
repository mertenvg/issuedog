package daemon_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// rewriteSpec is a whole body the App writes, as specWriteBody does.
const rewriteSpec = "<!-- issuedog:rewrite run=run1 replaced=mallory -->\n## Problem\n..."

// Only the body's history after the App's last full rewrite counts (#169,
// Q1a), and a full rewrite is the App's edit whose text starts with the
// rewrite marker. A body the App last rewrote needs no history read.
func TestDirtyRewrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		edits   [][2]string // editor, text
		deleted int         // a history entry whose text someone deleted, or 0
		want    daemon.Dirt
		queries int
	}{
		{"mallory's edit, then the App's rewrite", [][2]string{{"mallory", "evil"}, {app, rewriteSpec}}, 0, daemon.Dirt{}, 0},
		{"then the owner's edit", [][2]string{{"mallory", "evil"}, {app, rewriteSpec}, {"owner", "spec v2"}}, 0, daemon.Dirt{}, 1},
		{"mallory's edit after the rewrite", [][2]string{{app, rewriteSpec}, {"mallory", "evil"}, {"owner", "spec v2"}}, 0,
			daemon.Dirt{state.BodyID: "mallory"}, 1},
		{"no rewrite since mallory's edit", [][2]string{{"mallory", "evil"}, {"owner", "spec v2"}}, 0, daemon.Dirt{state.BodyID: "mallory"}, 1},
		{"the App's edit that isn't a rewrite", [][2]string{{"mallory", "evil"}, {app, "## Problem\n..."}}, 0, daemon.Dirt{state.BodyID: "mallory"}, 1},
		{"a rewrite whose text was deleted", [][2]string{{"mallory", "evil"}, {app, rewriteSpec}, {"owner", "spec v2"}}, 2,
			daemon.Dirt{state.BodyID: "mallory"}, 1},
		{"mallory's forged rewrite", [][2]string{{"mallory", rewriteSpec}}, 0, daemon.Dirt{state.BodyID: "mallory"}, 0},
		{"mallory's forged rewrite, then the owner's edit", [][2]string{{"mallory", rewriteSpec}, {"owner", "spec v2"}}, 0,
			daemon.Dirt{state.BodyID: "mallory"}, 1},
		{"the rewrite escaped by a note", [][2]string{{app, rewriteSpec}, {"mallory", "evil"},
			{app, noteOn("mallory", rewriteSpec)}, {"owner", "spec v2"}}, 0, daemon.Dirt{state.BodyID: "mallory"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			n := f.AddIssue("owner", "Title", "an idea")
			for _, e := range tc.edits {
				f.EditBody(n, e[0], e[1])
			}
			if tc.deleted > 0 {
				f.DeleteRevision(n, tc.deleted)
			}
			_, _, d := dirt(t, f)
			if !reflect.DeepEqual(d, tc.want) {
				t.Errorf("dirt = %v, want %v", d, tc.want)
			}
			if q := f.HistoryQueries(); q != tc.queries {
				t.Errorf("history queries = %d, want %d", q, tc.queries)
			}
		})
	}
}

// The App never rewrites a comment whole, so a comment's text that starts
// with a rewrite marker, even as the App's last edit, cleans nothing.
func TestDirtyRewriteNotForComments(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	id := f.AddComment(f.AddIssue("owner", "Title", "an idea"), "owner", "hi")
	f.EditComment(id, "mallory", "evil")
	f.EditComment(id, app, rewriteSpec)
	if _, _, d := dirt(t, f); !reflect.DeepEqual(d, daemon.Dirt{id: "mallory"}) {
		t.Errorf("dirt = %v, want the comment dirty by mallory", d)
	}
}

// A trusted user's edit counts like the author's, for the body and for a
// person's comment, but not for the App's comment, as trust holds now:
// once the editor loses trust, the edit makes the content dirty again
// (#169, Q2a). Each editor's trust is asked once per call, and only where
// it decides.
func TestDirtyTrustedEdits(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	n := f.AddIssue("filer", "Title", "an idea")
	mine := f.AddComment(n, "alice", "1a")
	apps := f.AddComment(n, app, toClarifying)
	f.EditBody(n, "Owner2", "the idea, put better")
	f.EditComment(mine, "Owner2", "1b")
	f.EditComment(apps, "Owner2", toClarifying)
	f.EditBody(n, "filer", "the idea, put better still")
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	trust := countTrust{trusted{"Owner2": true}, map[string]int{}}
	d, err := a.Dirty(context.Background(), s, trust)
	if want := (daemon.Dirt{apps: "Owner2"}); err != nil || !reflect.DeepEqual(d, want) {
		t.Errorf("Dirty = %v, %v; want only the App's comment dirty, by Owner2", d, err)
	}
	if want := map[string]int{"Owner2": 1}; !reflect.DeepEqual(trust.checks, want) {
		t.Errorf("trust checks = %v, want %v", trust.checks, want)
	}
	if got := state.Derive(s, slug, d.Has).Stage; got != state.Intake {
		t.Errorf("stage = %s, want intake: the App's comment's marker isn't read", got)
	}
	d, err = a.Dirty(context.Background(), s, trusted{})
	if want := (daemon.Dirt{state.BodyID: "Owner2", mine: "Owner2", apps: "Owner2"}); err != nil || !reflect.DeepEqual(d, want) {
		t.Errorf("Dirty once Owner2 lost trust = %v, %v; want all three dirty by Owner2", d, err)
	}
	if d, err = a.Dirty(context.Background(), s, nil); err != nil || len(d) != 3 {
		t.Errorf("Dirty with no trust = %v, %v; want all three dirty", d, err)
	}
	if d, err := a.Dirty(context.Background(), s, failTrust{}); !errors.Is(err, errTrust) {
		t.Errorf("Dirty with trust failing = %v, %v; want an error wrapping it", d, err)
	}
}

// The body's markers are read only while the App made its last edit: a
// trusted user's edit of the App's body leaves it clean but not the App's
// text (#169, Q2a), and the App's own later edit makes it so again.
func TestDirtyTrustedBodyMarkers(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	n := f.AddIssue(app, "Title", "a task <!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	f.EditBody(n, "owner", "a task <!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	a, s, d := dirt(t, f)
	if d.Has(state.BodyID) || state.Derive(s, slug, d.Has).Item.FromPlan {
		t.Errorf("dirt %v, FromPlan %v; want the body clean and its origin marker unread", d, state.Derive(s, slug, d.Has).Item.FromPlan)
	}
	f.EditBody(n, app, "a task <!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	s, err := a.Client.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if d, err = a.Dirty(context.Background(), s, trusted{"owner": true}); err != nil || !state.Derive(s, slug, d.Has).Item.FromPlan {
		t.Errorf("dirt %v, %v; want the App's edit to make the origin marker read", d, err)
	}
}

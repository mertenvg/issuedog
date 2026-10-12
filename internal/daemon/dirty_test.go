package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

const toClarifying = "Moved.\n\n<!-- issuedog:transition from=intake to=clarifying cmd=5 -->"

// dirt reads issue 1 from a fresh client and finds its dirty content.
func dirt(t *testing.T, f *githubtest.Fake) (*daemon.Applier, *github.Snapshot, daemon.Dirt) {
	t.Helper()
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.Dirty(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return a, s, d
}

// markPass is a pass that finds issue 1's dirty content and marks it.
func markPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		d, err := a.Dirty(context.Background(), s)
		if err != nil {
			return err
		}
		return a.MarkDirty(context.Background(), s, d)
	}
}

// noteOn is what MarkDirty makes of a comment's text edited by by.
func noteOn(by, text string) string {
	return "<!-- issuedog:dirty by=" + by + " -->\n> ⚠️ issuedog ignores this comment: **" + by +
		"** edited it without approval.\n\n" + github.Escape(text)
}

// The rule for an App comment carrying a transition marker, and a trusted
// user's own comment: whose edits make each dirty, and which of them need
// the history read. The body is never edited, so it never needs one.
func TestDirtyRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		author  string
		editors []string
		want    daemon.Dirt
		stage   state.Stage // the stage Derive reads, given the dirt
		queries int
	}{
		{"untrusted, then trusted", app, []string{"mallory", "owner"}, daemon.Dirt{cmdID: "owner"}, state.Intake, 0},
		{"untrusted, then the App", app, []string{"mallory", app}, daemon.Dirt{cmdID: "mallory"}, state.Intake, 1},
		{"a trusted user only", app, []string{"owner"}, daemon.Dirt{cmdID: "owner"}, state.Intake, 0},
		{"the App only", app, []string{app, app}, daemon.Dirt{}, state.Clarifying, 1},
		{"never edited", app, nil, daemon.Dirt{}, state.Clarifying, 0},
		{"a trusted user's own, edited only by them", "owner", []string{"owner", "owner"}, daemon.Dirt{}, state.Intake, 1},
		{"a trusted user's own, edited by someone else", "owner", []string{"alice", "owner"}, daemon.Dirt{cmdID: "alice"}, state.Intake, 1},
		{"the oldest of several, from the history", "owner", []string{"alice", "bob", "owner"}, daemon.Dirt{cmdID: "alice"}, state.Intake, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			f.AddComment(f.AddIssue("owner", "Title", "body"), tc.author, toClarifying)
			for _, by := range tc.editors {
				f.EditComment(cmdID, by, toClarifying)
			}
			_, s, d := dirt(t, f)
			if !reflect.DeepEqual(d, tc.want) {
				t.Errorf("dirt = %v, want %v", d, tc.want)
			}
			if got := state.Derive(s, slug, d.Has).Stage; got != tc.stage {
				t.Errorf("stage = %s, want %s", got, tc.stage)
			}
			if q := f.HistoryQueries(); q != tc.queries {
				t.Errorf("history queries = %d, want %d", q, tc.queries)
			}
		})
	}
}

// dirtyIssue files issue 1 with a body, an App comment and a trusted
// user's command, each edited by someone other than its author, with the
// last edit by the author where it can be. A clean comment stays clean.
func dirtyIssue(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	n := f.AddIssue("owner", "Title", "spec")
	status := f.AddComment(n, app, toClarifying)
	cmd := f.AddComment(n, "owner", "/approve")
	f.AddComment(n, "owner", "clean")
	f.EditBody(n, "mallory", "spec <!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	f.EditBody(n, "owner", "spec <!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	f.EditComment(status, "mallory", toClarifying)
	f.EditComment(status, app, toClarifying)
	f.EditComment(cmd, "alice", "/approve")
	f.EditComment(cmd, "owner", "/approve")
	return f
}

// A dirty body or comment, whoever wrote it, gets the note once at every
// crash point. The App's note doesn't change what's dirty, and a note
// that's removed comes back.
func TestMarkDirty(t *testing.T) {
	githubtest.Replay(t, dirtyIssue, markPass(t))

	f := run(t, dirtyIssue, markPass(t))
	got := f.State().Issues[0]
	body := "<!-- issuedog:dirty by=mallory -->\n> ⚠️ issuedog ignores this issue description: **mallory** edited it without approval.\n\n" +
		"spec &lt;!-- issuedog:origin plan=#1 rev=1 item=T1 -->"
	if got.Body != body || got.BodyEditedBy != app {
		t.Errorf("body = %q edited by %s, want %q by the App", got.Body, got.BodyEditedBy, body)
	}
	want := []string{noteOn("mallory", toClarifying), noteOn("alice", "/approve"), "clean"}
	for i, c := range got.Comments {
		if c.Body != want[i] {
			t.Errorf("comment %d = %q, want %q", i, c.Body, want[i])
		}
	}
	_, s, d := dirt(t, f)
	wantDirt := daemon.Dirt{state.BodyID: "mallory", cmdID: "mallory", cmdID + 1: "alice"}
	if !reflect.DeepEqual(d, wantDirt) {
		t.Errorf("dirt after the notes = %v, want %v", d, wantDirt)
	}
	if is := state.Derive(s, slug, d.Has); is.Stage != state.Intake || len(is.Markers) != 0 {
		t.Errorf("derived %+v, want no markers", is)
	}

	// The command's author removes the note, the owner removes only the App
	// comment's visible line, and a web edit turns the body's line endings
	// into CRLF.
	markerOnly := "<!-- issuedog:dirty by=mallory -->\n\n" + github.Escape(toClarifying)
	f.EditComment(cmdID+1, "owner", "/approve")
	f.EditComment(cmdID, "owner", markerOnly)
	f.EditBody(1, "owner", strings.ReplaceAll(body, "\n", "\r\n"))
	if err := markPass(t)(f); err != nil {
		t.Fatal(err)
	}
	got = f.State().Issues[0]
	if !strings.HasPrefix(got.Body, "<!-- issuedog:dirty by=mallory -->\r\n") || got.BodyEditedBy != "owner" {
		t.Errorf("body = %q edited by %s, want the CRLF one left alone", got.Body, got.BodyEditedBy)
	}
	want = []string{noteOn("owner", markerOnly), noteOn("alice", "/approve"), "clean"}
	for i, c := range got.Comments {
		if c.Body != want[i] {
			t.Errorf("comment %d after removing its note = %q, want %q", i, c.Body, want[i])
		}
	}
}

// A history that can't show the last edit fails closed: dirty by the last
// editor, with a warning.
func TestDirtyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    int
		change func(s *github.Snapshot) int64 // returns the comment it breaks
	}{
		{"only the original post", 1, func(*github.Snapshot) int64 { return cmdID }},
		{"no entries", 100, func(s *github.Snapshot) int64 {
			s.Comments[1].Edit = s.Comments[0].Edit // an edit the history doesn't show yet
			return cmdID + 1
		}},
		{"older than the last edit", 100, func(s *github.Snapshot) int64 {
			s.Comments[0].Edit.At = s.Comments[0].Edit.At.Add(time.Second)
			return cmdID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			n := f.AddIssue("owner", "Title", "body")
			f.AddComment(n, "owner", "hi")
			f.AddComment(n, "owner", "yo")
			f.EditComment(cmdID, "owner", "hi!")
			f.HistoryCap(tc.cap)
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			id := tc.change(s)
			var warn bytes.Buffer
			stop := logr.AddWriter(&warn, logr.WithFilter(logr.W))
			d, err := a.Dirty(context.Background(), s)
			logr.Wait()
			stop()
			if err != nil {
				t.Fatal(err)
			}
			if want := (daemon.Dirt{id: "owner"}); !reflect.DeepEqual(d, want) {
				t.Errorf("dirt = %v, want %v", d, want)
			}
			if !strings.Contains(warn.String(), fmt.Sprintf("taking comment %d as dirty", id)) {
				t.Errorf("warnings = %q, want one for comment %d", warn.String(), id)
			}
		})
	}
}

// GitHub keeps the original post and the 99 newest edits, so mallory's edit
// drops out of the history. The note keeps the comment dirty.
func TestDirtyNoteOutlivesCappedHistory(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	f.AddComment(f.AddIssue("owner", "Title", "body"), "owner", "/hold")
	f.EditComment(cmdID, "mallory", "/hold")
	f.EditComment(cmdID, "owner", "/hold")
	if err := markPass(t)(f); err != nil {
		t.Fatal(err)
	}
	noted := noteOn("mallory", "/hold")
	for i := range 99 {
		f.EditComment(cmdID, "owner", fmt.Sprintf("%s\n\nedit %d", noted, i))
	}
	_, s, d := dirt(t, f)
	h, err := f.Client(t).History(context.Background(), []string{s.Comments[0].Node})
	if err != nil {
		t.Fatal(err)
	}
	if by, dirty := state.Dirty(s.Comments[0].Author, h[s.Comments[0].Node]); dirty || len(h[s.Comments[0].Node]) != 100 {
		t.Fatalf("the capped history has %d entries and is dirty by %v, %v; want 100 and clean", len(h[s.Comments[0].Node]), by, dirty)
	}
	if want := (daemon.Dirt{cmdID: "mallory"}); !reflect.DeepEqual(d, want) {
		t.Errorf("dirt = %v, want %v", d, want)
	}
}

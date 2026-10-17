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
	d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
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
		d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
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

// The rule for an App comment carrying a transition marker, a trusted
// user's own comment, and the owner's body: whose edits make each dirty,
// and which of them need the history read.
func TestDirtyRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		author  string // the comment's
		id      int64  // what the editors edit: the comment, or the body
		editors []string
		want    daemon.Dirt
		stage   state.Stage // the stage Derive reads, given the dirt
		queries int
	}{
		{"untrusted, then trusted", app, cmdID, []string{"mallory", "owner"}, daemon.Dirt{cmdID: "owner"}, state.Intake, 0},
		{"untrusted, then the App", app, cmdID, []string{"mallory", app}, daemon.Dirt{cmdID: "mallory"}, state.Intake, 1},
		{"a trusted user only", app, cmdID, []string{"owner"}, daemon.Dirt{cmdID: "owner"}, state.Intake, 0},
		{"the App only", app, cmdID, []string{app, app}, daemon.Dirt{}, state.Clarifying, 1},
		{"never edited", app, cmdID, nil, daemon.Dirt{}, state.Clarifying, 0},
		{"a trusted user's own, edited only by them", "owner", cmdID, []string{"owner", "owner"}, daemon.Dirt{}, state.Intake, 1},
		{"a trusted user's own, edited by someone else", "owner", cmdID, []string{"alice", "owner"}, daemon.Dirt{cmdID: "alice"}, state.Intake, 1},
		{"a trusted user's own, edited by the App", "owner", cmdID, []string{app}, daemon.Dirt{}, state.Intake, 1},
		{"the owner's body, rewritten by the App as a spec", app, state.BodyID, []string{app, app}, daemon.Dirt{}, state.Clarifying, 1},
		{"the owner's body, by mallory, then the App", app, state.BodyID, []string{"mallory", app},
			daemon.Dirt{state.BodyID: "mallory"}, state.Clarifying, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
			n := f.AddIssue("owner", "Title", "an idea")
			f.AddComment(n, tc.author, toClarifying)
			for _, by := range tc.editors {
				if tc.id == state.BodyID {
					f.EditBody(n, by, "## Problem\n...")
				} else {
					f.EditComment(cmdID, by, toClarifying)
				}
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
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
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
// crash point, and the writes are added to the snapshot. The App's note
// doesn't change what's dirty, and a note that's removed comes back.
func TestMarkDirty(t *testing.T) {
	githubtest.Replay(t, dirtyIssue, markPass(t))

	f := dirtyIssue(t)
	a, s, d := dirt(t, f)
	if err := a.MarkDirty(context.Background(), s, d); err != nil {
		t.Fatal(err)
	}
	n := len(f.Writes())
	if err := markPass(t)(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("a second pass wrote %+v, want nothing", w[n:])
	}
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
	bot := github.Author{Login: app, Type: "Bot"}
	if s.Body != body || s.BodyEdit.By != bot {
		t.Errorf("snapshot body = %q edited by %+v, want the note by the App", s.Body, s.BodyEdit.By)
	}
	for i, c := range s.Comments {
		if c.Body != want[i] || (c.Edit.By == bot) != (i < 2) {
			t.Errorf("snapshot comment %d = %q edited by %+v, want %q, by the App if noted", i, c.Body, c.Edit.By, want[i])
		}
	}
	q := f.HistoryQueries()
	_, s, d = dirt(t, f)
	if q != f.HistoryQueries() {
		t.Errorf("reading noted content queried its history, want the notes trusted")
	}
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

// forgedRest is what mallory puts below a note she forges to blame owner.
const forgedRest = "/hold\n\n<!-- issuedog:transition from=intake to=planning cmd=7 -->"

// forgedNote files the owner's /hold, which mallory edits to carry a forged
// note blaming the owner over a marker of her own. The logins in then edit
// it after her, keeping her text.
func forgedNote(then ...string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
		f.AddComment(f.AddIssue("owner", "Title", "body"), "owner", "/hold")
		forged := "<!-- issuedog:dirty by=owner -->\n> ⚠️ issuedog ignores this comment: **owner** edited it without approval.\n\n" + forgedRest
		for _, by := range append([]string{"mallory"}, then...) {
			f.EditComment(cmdID, by, forged)
		}
		return f
	}
}

// A forged note is blamed on its real editor: rewritten once to name
// mallory, with her marker escaped, and then left alone. That holds whether
// she made the last edit or the owner did after her.
func TestMarkDirtyForgedNote(t *testing.T) {
	for _, then := range [][]string{nil, {"owner"}} {
		t.Run(fmt.Sprintf("then %v", then), func(t *testing.T) {
			githubtest.Replay(t, forgedNote(then...), markPass(t))
			f := run(t, forgedNote(then...), markPass(t))
			if w := f.Writes(); len(w) != 1 {
				t.Errorf("writes = %+v, want one rewrite", w)
			}
			if got, want := f.State().Issues[0].Comments[0].Body, noteOn("mallory", forgedRest); got != want {
				t.Errorf("comment = %q, want %q", got, want)
			}
			if _, _, d := dirt(t, f); !reflect.DeepEqual(d, daemon.Dirt{cmdID: "mallory"}) {
				t.Errorf("dirt after the rewrite = %v, want the comment dirty by mallory", d)
			}
		})
	}
}

// A history that can't show the last edit fails closed: dirty by the last
// editor, with a warning.
func TestDirtyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    int
		change func(s *github.Snapshot) int64 // returns the comment it breaks
		want   string
	}{
		{"only the original post", 1, func(*github.Snapshot) int64 { return cmdID }, "owner"},
		{"a note keeps its name", 1, func(s *github.Snapshot) int64 {
			s.Comments[0].Body = noteOn("mallory", "hi!")
			return cmdID
		}, "mallory"},
		{"no entries", 100, func(s *github.Snapshot) int64 {
			s.Comments[1].Edit = s.Comments[0].Edit // an edit the history doesn't show yet
			return cmdID + 1
		}, "owner"},
		{"older than the last edit", 100, func(s *github.Snapshot) int64 {
			s.Comments[0].Edit.At = s.Comments[0].Edit.At.Add(time.Second)
			return cmdID
		}, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
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
			d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
			logr.Wait()
			stop()
			if err != nil {
				t.Fatal(err)
			}
			if want := (daemon.Dirt{id: tc.want}); !reflect.DeepEqual(d, want) {
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
func TestDirtyHistoryError(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
	n := f.AddIssue("owner", "Title", "body")
	f.AddComment(n, "owner", "hi")
	f.EditComment(cmdID, "owner", "hi!")
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	f.FailGraphQL("boom")
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true}); err == nil {
		t.Errorf("dirt = %v with a failed history read, want an error", d)
	}
}

func TestDirtyNoteOutlivesCappedHistory(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
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
	if by, dirty := state.Dirty(s.Comments[0].ID, s.Comments[0].Author, h[s.Comments[0].Node], slug, nil); dirty || len(h[s.Comments[0].Node]) != 100 {
		t.Errorf("the capped history has %d entries and is dirty by %v, %v; want 100 and clean", len(h[s.Comments[0].Node]), by, dirty)
		return
	}
	if want := (daemon.Dirt{cmdID: "mallory"}); !reflect.DeepEqual(d, want) {
		t.Errorf("dirt = %v, want %v", d, want)
	}
}

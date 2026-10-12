package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

const (
	slug  = "issuedog"
	app   = slug + "[bot]"
	cmdID = 1001 // the fake's first comment
)

// cmdIssue files issue 1, untyped, with these labels and a command comment.
func cmdIssue(labels ...string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		if id := f.AddComment(f.AddIssue("owner", "Title", "body", labels...), "owner", "/go"); id != cmdID {
			t.Fatalf("command comment ID = %d, want %d", id, cmdID)
		}
		return f
	}
}

func applier(t *testing.T, f *githubtest.Fake) (*daemon.Applier, *github.Snapshot, error) {
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	return &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, s, err
}

// move is a pass that applies m to issue 1 with dirty, from a fresh client.
func move(t *testing.T, m daemon.Move, dirty func(int64) bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		return a.Transition(context.Background(), s, dirty, m)
	}
}

func clean(int64) bool { return false }

// run runs pass once on a fresh fake from setup, then again to check it
// writes nothing more, and returns the fake.
func run(t *testing.T, setup func(testing.TB) *githubtest.Fake, pass func(*githubtest.Fake) error) *githubtest.Fake {
	t.Helper()
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	n := len(f.Writes())
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("a second pass wrote %+v, want nothing", w[n:])
	}
	return f
}

func TestTransitionInTable(t *testing.T) {
	setup := cmdIssue("priority:p2", "stage:intake")
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(cmdID), Text: "Picked up."}
	githubtest.Replay(t, setup, move(t, m, clean))

	f := run(t, setup, move(t, m, clean))
	// Plan #5 assumption 4's order: marker, new label, old label, 👍.
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path)
	}
	wantWrites := []string{"POST /issues/1/comments", "POST /issues/1/labels",
		"DELETE /issues/1/labels/stage:intake", "POST /issues/comments/1001/reactions"}
	if !reflect.DeepEqual(writes, wantWrites) {
		t.Errorf("writes = %q, want %q", writes, wantWrites)
	}
	got := f.State().Issues[0]
	if want := []string{"priority:p2", "stage:clarifying"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
	want := []githubtest.CommentState{
		{Author: "owner", Body: "/go", Reactions: []string{app + " +1"}},
		{Author: app, Body: "Picked up.\n\n<!-- issuedog:transition from=intake to=clarifying cmd=1001 -->"},
	}
	if !reflect.DeepEqual(got.Comments, want) {
		t.Errorf("comments = %+v, want %+v", got.Comments, want)
	}
}

func TestTransitionNotInTable(t *testing.T) {
	setup := cmdIssue("stage:intake")
	m := daemon.Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: state.ReactionKey(55)}
	pass := func(f *githubtest.Fake) error {
		if err := move(t, m, clean)(f); !errors.Is(err, state.ErrNoTransition) {
			return fmt.Errorf("err = %v, want ErrNoTransition", err)
		}
		return nil
	}
	githubtest.Replay(t, setup, pass)

	// Two passes: an error when the comment is posted, then only debug.
	var errs, debug bytes.Buffer
	stopErrs := logr.AddWriter(&errs, logr.WithFilter(logr.E))
	stopDebug := logr.AddWriter(&debug, logr.WithFilter(logr.D))
	got := run(t, setup, pass).State().Issues[0]
	logr.Wait()
	stopErrs()
	stopDebug()
	if n := strings.Count(errs.String(), "state error: move o/r#1 to planning on spec-approved"); n != 1 {
		t.Errorf("error logs = %q, want the state error once", errs.String())
	}
	if !strings.Contains(debug.String(), "state error, reported already: move o/r#1 to planning") {
		t.Errorf("debug logs = %q, want the state error reported already", debug.String())
	}
	if want := []string{"stage:intake"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v unchanged", got.Labels, want)
	}
	if len(got.Comments) != 2 || !strings.HasSuffix(got.Comments[1].Body, "\n\n<!-- issuedog:error kind=transition key=r55 -->") {
		t.Errorf("comments = %+v, want the command and one error comment", got.Comments)
	}
	if r := got.Comments[0].Reactions; len(r) != 0 {
		t.Errorf("reactions = %v, want none", r)
	}
}

func TestTransitionErrorKeys(t *testing.T) {
	m := daemon.Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: state.CmdKey(cmdID)}
	pass := func(f *githubtest.Fake) error {
		if err := move(t, m, clean)(f); !errors.Is(err, state.ErrNoTransition) {
			return fmt.Errorf("err = %v, want ErrNoTransition", err)
		}
		return nil
	}
	for _, tt := range []struct {
		name, existing string
		posted         bool
	}{
		{"the same key of another kind", "<!-- issuedog:error kind=disagreement key=1001 -->", true},
		{"the same kind and key", "<!-- issuedog:error kind=transition key=1001 -->", false},
		{"an ack of the key", "<!-- issuedog:error key=1001 ack=r77 -->", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setup := func(t testing.TB) *githubtest.Fake {
				f := cmdIssue("stage:intake")(t)
				f.AddComment(1, app, tt.existing)
				return f
			}
			githubtest.Replay(t, setup, pass)
			got := run(t, setup, pass).State().Issues[0].Comments
			want := 2
			if tt.posted {
				want = 3
			}
			if len(got) != want {
				t.Errorf("comments = %+v, want %d", got, want)
			}
			if tt.posted && !strings.HasSuffix(got[2].Body, "<!-- issuedog:error kind=transition key=1001 -->") {
				t.Errorf("new comment = %q, want the transition error", got[2].Body)
			}
		})
	}
}

func TestTransitionLabelGoneMeanwhile(t *testing.T) {
	// The new label is there already, so no add refreshes the snapshot's labels.
	f := cmdIssue("stage:clarifying", "stage:intake")(t)
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	// Someone removes the old label after the snapshot: its DELETE gets a 404.
	if _, err := f.Client(t).Issues.RemoveLabelForIssue(context.Background(), "o", "r", 1, "stage:intake"); err != nil {
		t.Fatal(err)
	}
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.ReactionKey(9)}
	if err := a.Transition(context.Background(), s, clean, m); err != nil {
		t.Errorf("Transition: %v, want the missing label ignored", err)
	}
	if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"stage:clarifying"}) {
		t.Errorf("labels = %v, want only stage:clarifying", got)
	}
	if !reflect.DeepEqual(s.Labels, []string{"stage:clarifying"}) {
		t.Errorf("snapshot labels = %v, want only stage:clarifying", s.Labels)
	}
	if w := f.Writes(); len(w) < 2 || w[len(w)-1].Path != "/issues/1/labels/stage:intake" {
		t.Errorf("writes = %+v, want the last to be the DELETE that got a 404", w)
	}
}

func TestTransitionOlderKeySetsLatestStage(t *testing.T) {
	// The command's move is done, but its label and 👍 aren't, and a later
	// move has happened since. A replay finishes them at the latest stage.
	setup := func(t testing.TB) *githubtest.Fake {
		f := cmdIssue("stage:clarifying")(t)
		f.SetType(1, "epic")
		f.AddComment(1, app, "<!-- issuedog:transition from=intake to=clarifying cmd=1001 -->")
		f.AddComment(1, app, "<!-- issuedog:transition from=clarifying to=spec-review run=a -->")
		return f
	}
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(cmdID)}
	githubtest.Replay(t, setup, move(t, m, clean))
	got := run(t, setup, move(t, m, clean)).State().Issues[0]
	if !reflect.DeepEqual(got.Labels, []string{"stage:spec-review"}) {
		t.Errorf("labels = %v, want only stage:spec-review", got.Labels)
	}
	if n, r := len(got.Comments), got.Comments[0].Reactions; n != 3 || !reflect.DeepEqual(r, []string{app + " +1"}) {
		t.Errorf("comments = %+v, want no new one and the command 👍'd", got.Comments)
	}
}

func TestTransitionEscapesText(t *testing.T) {
	setup := cmdIssue("stage:intake")
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.ReactionKey(9),
		Text: "Notes: <!-- issuedog:transition to=planning -->"}
	githubtest.Replay(t, setup, move(t, m, clean))

	f := run(t, setup, move(t, m, clean))
	body := f.State().Issues[0].Comments[1].Body
	if want := "Notes: &lt;!-- issuedog:transition to=planning -->\n\n"; !strings.HasPrefix(body, want) {
		t.Errorf("body = %q, want it to start %q", body, want)
	}
	_, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Derive(s, slug, clean).Stage; got != state.Clarifying {
		t.Errorf("stage = %s, want clarifying", got)
	}
}

func TestTransitionSkipsDirtyMarkers(t *testing.T) {
	const edited = 1002 // the second App comment
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		n := f.AddIssue("owner", "Title", "body", "stage:spec-review")
		f.SetType(n, "epic")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=clarifying cmd=1 -->")
		body := "<!-- issuedog:transition from=clarifying to=spec-review run=a -->"
		if id := f.AddComment(n, app, body); id != edited {
			t.Fatalf("comment ID = %d, want %d", id, edited)
		}
		for _, by := range []string{"mallory", "owner", app} {
			f.EditComment(edited, by, body)
		}
		return f
	}
	for _, tt := range []struct {
		name   string
		dirty  func(int64) bool
		m      daemon.Move
		marker string
	}{
		{"a dirty marker falls back to the one before", func(id int64) bool { return id == edited },
			daemon.Move{To: state.SpecReview, Trigger: state.TriggerSpecWritten, Key: state.Key{Field: "run", ID: "b"}},
			"<!-- issuedog:transition from=clarifying to=spec-review run=b -->"},
		{"a clean marker counts however often it was edited", clean,
			daemon.Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: state.ReactionKey(7)},
			"<!-- issuedog:transition from=spec-review to=planning cmd=r7 -->"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			githubtest.Replay(t, setup, move(t, tt.m, tt.dirty))
			got := run(t, setup, move(t, tt.m, tt.dirty)).State().Issues[0]
			if n := len(got.Comments); n != 3 || got.Comments[2].Body != tt.marker {
				t.Errorf("comments = %+v, want a third with %q", got.Comments, tt.marker)
			}
			if want, _ := tt.m.To.Label(); !reflect.DeepEqual(got.Labels, []string{want}) {
				t.Errorf("labels = %v, want only %s", got.Labels, want)
			}
		})
	}
}

func TestTransitionRefusesBadMoves(t *testing.T) {
	for _, m := range []daemon.Move{
		{To: state.Clarifying, Trigger: state.TriggerTrusted},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "rev", ID: "1"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"from": "held"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"run": "x"}},
		{To: state.Closed, Trigger: state.TriggerClosed, Key: state.CmdKey(1)},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"rev": "two words"}},
	} {
		f := cmdIssue("stage:intake")(t)
		if err := move(t, m, clean)(f); err == nil || errors.Is(err, state.ErrNoTransition) {
			t.Errorf("%+v: err = %v, want a refusal", m, err)
		}
		if w := f.Writes(); len(w) != 0 {
			t.Errorf("%+v: writes = %+v, want none", m, w)
		}
	}
}

func TestAck(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := cmdIssue()(t)
		f.AddReaction(cmdID, "owner", "+1")
		f.AddReaction(cmdID, app, "eyes")
		return f
	}
	ack := func(f *githubtest.Fake) error {
		a, _, err := applier(t, f)
		if err != nil {
			return err
		}
		return a.Ack(context.Background(), cmdID)
	}
	githubtest.Replay(t, setup, ack)
	got := run(t, setup, ack).State().Issues[0].Comments[0].Reactions
	if want := []string{app + " +1", app + " eyes", "owner +1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reactions = %v, want %v", got, want)
	}
}

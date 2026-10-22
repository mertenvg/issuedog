package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
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
	t.Parallel()
	setup := cmdIssue("priority:p2", "stage:intake")
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(cmdID), Text: "Picked up."}
	githubtest.Replay(t, setup, move(t, m, clean))

	f := run(t, setup, move(t, m, clean))
	// The SPEC §5.2 order: marker, new label, old label, 👍.
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
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
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

func TestTransitionLabelGoneMeanwhile(t *testing.T) {
	t.Parallel()
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

// With the new label there already, each removal changes the snapshot's
// labels, which mustn't cut short the list of those to remove.
func TestTransitionRemovesEveryOtherStageLabel(t *testing.T) {
	t.Parallel()
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.ReactionKey(9)}
	f := run(t, cmdIssue("stage:clarifying", "stage:intake", "stage:planning"), move(t, m, clean))
	if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"stage:clarifying"}) {
		t.Errorf("labels = %v, want only stage:clarifying", got)
	}
}

func TestTransitionOlderKeySetsLatestStage(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, m := range []daemon.Move{
		{To: state.Clarifying, Trigger: state.TriggerTrusted},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "rev", ID: "1"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"from": "held"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"run": "x"}},
		// A move to closed needs a state reason, and writes reason= itself;
		// no other move has one, and a closed stage is no move's.
		{To: state.Closed, Trigger: state.TriggerClosed, Key: state.CmdKey(1)},
		{To: state.Closed, StateReason: "duplicate", Trigger: state.TriggerClosed, Key: state.CmdKey(1)},
		{To: state.Closed, StateReason: "completed", Trigger: state.TriggerClosed, Key: state.CmdKey(1), Fields: map[string]string{"reason": "completed"}},
		{To: state.Clarifying, StateReason: "completed", Trigger: state.TriggerTrusted, Key: state.CmdKey(1)},
		{To: state.Completed, StateReason: "completed", Trigger: state.TriggerClosed, Key: state.CmdKey(1)},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"rev": "two words"}},
		// cmd only beside a run key, and only naming a comment (SPEC §6.4).
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), Fields: map[string]string{"cmd": "2"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "event", ID: "9"}, Fields: map[string]string{"cmd": "2"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "run", ID: "a"}, Fields: map[string]string{"cmd": "r2"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "run", ID: "a"}, Fields: map[string]string{"cmd": "0"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "run", ID: "a"}, Fields: map[string]string{"event": "2"}},
		// A plan revision sets rev, carries the hash in its own marker, and
		// is the comment's only revision (SPEC §7.4, §11.4).
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), PlanRev: 1, Fields: map[string]string{"body": "aaaaaaaa"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), PlanRev: 1, Fields: map[string]string{"rev": "1"}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), PlanRev: 1,
			Markers: []github.Marker{{Kind: "revision", Fields: map[string]string{"rev": "1", "body": "aaaaaaaa"}}}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), PlanRev: 1,
			Markers: []github.Marker{{Kind: "revision", Fields: map[string]string{"kind": "plan", "rev": "1", "body": "aaaaaaaa"}}}},
		{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.CmdKey(1), PlanRev: -1},
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

// planningEpic files epic 1 at planning, its spec revision 1 approved.
func planningEpic(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	n := f.AddIssue("owner", "Title", "body", "stage:planning")
	f.SetType(n, "epic")
	f.AddComment(n, app, "<!-- issuedog:transition from=clarifying to=spec-review rev=1 body=aaaaaaaa run=a -->\n<!-- issuedog:revision rev=1 body=aaaaaaaa -->")
	f.AddComment(n, app, "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r5 seen=1002 -->")
	return f
}

// A move with PlanRev writes one comment, its plan comment: the escaped
// text, the transition marker with rev but not body, Markers, and last the
// plan revision marker, whose hash, recomputed from the comment GitHub
// returns, matches (SPEC §7.4, §11.4). It is plan revision 1, and
// no spec revision.
func TestTransitionPlanRevision(t *testing.T) {
	t.Parallel()
	m := daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1,
		Text:    "### Plan\r\n\r\nT1 <!-- issuedog:revision kind=plan rev=9 body=00000000 -->",
		Markers: []github.Marker{{Kind: "plan-data", Fields: map[string]string{"v": "1"}}}}
	githubtest.Replay(t, planningEpic, move(t, m, clean))

	f := run(t, planningEpic, move(t, m, clean))
	got := f.State().Issues[0]
	if want := []string{"stage:plan-review"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
	if n := len(got.Comments); n != 3 {
		t.Fatalf("comments = %+v, want one more", got.Comments)
	}
	above := "### Plan\r\n\r\nT1 &lt;!-- issuedog:revision kind=plan rev=9 body=00000000 -->\n\n" +
		"<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n<!-- issuedog:plan-data v=1 -->"
	want := above + "\n<!-- issuedog:revision kind=plan rev=1 body=" + state.BodyHash(above) + " -->"
	if body := got.Comments[2].Body; body != want {
		t.Errorf("plan comment = %q, want %q", body, want)
	}

	_, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comments[len(s.Comments)-1]
	hash, ok := state.PlanHash(c.Body)
	if !ok || hash != state.BodyHash(above) {
		t.Errorf("PlanHash of the comment GitHub returns = %s, %v, want %s, true", hash, ok, state.BodyHash(above))
	}
	plan, ok := state.LatestRevision(s, slug, state.PlanRevision, clean)
	if want := (state.Revision{Kind: state.PlanRevision, Rev: 1, Hash: hash, Comment: c.ID}); !ok || plan != want {
		t.Errorf("latest plan revision = %+v, %v, want %+v", plan, ok, want)
	}
	if !plan.Matches(s) {
		t.Errorf("plan revision %+v doesn't match its comment %q", plan, c.Body)
	}
	if spec, _ := state.LatestRevision(s, slug, state.SpecRevision, clean); spec.Rev != 1 || spec.Comment != s.Comments[0].ID {
		t.Errorf("latest spec revision = %+v, want the spec's revision 1, not the plan's", spec)
	}
	if got := state.Derive(s, slug, clean).Stage; got != state.PlanReview {
		t.Errorf("stage = %s, want plan-review", got)
	}
}

// A move with PlanRev numbers the plan revision it's given, whatever the
// spec revisions are, and with no Markers the plan revision marker comes
// right after the transition marker.
func TestTransitionPlanRevisionNoMarkers(t *testing.T) {
	t.Parallel()
	m := daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"}, PlanRev: 2, Text: "Plan two."}
	githubtest.Replay(t, planningEpic, move(t, m, clean))
	f := run(t, planningEpic, move(t, m, clean))
	above := "Plan two.\n\n<!-- issuedog:transition from=planning to=plan-review rev=2 run=p2 -->"
	want := above + "\n<!-- issuedog:revision kind=plan rev=2 body=" + state.BodyHash(above) + " -->"
	if cs := f.State().Issues[0].Comments; cs[len(cs)-1].Body != want {
		t.Errorf("plan comment = %q, want %q", cs[len(cs)-1].Body, want)
	}
}

// A run's move may name a command it handled, by cmd= beside its run key,
// as the interviewer does for a /revise that waited through a hold (SPEC
// §6.4, Commands that wait): the marker carries both, and the command is
// processed.
func TestTransitionRunWithCmd(t *testing.T) {
	t.Parallel()
	setup := cmdIssue("stage:intake")
	m := daemon.Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: state.Key{Field: "run", ID: "a"},
		Fields: map[string]string{"cmd": strconv.Itoa(cmdID)}, Text: "Picked up."}
	githubtest.Replay(t, setup, move(t, m, clean))
	f := run(t, setup, move(t, m, clean))
	want := "Picked up.\n\n<!-- issuedog:transition from=intake to=clarifying run=a cmd=1001 -->"
	if cs := f.State().Issues[0].Comments; cs[len(cs)-1].Body != want {
		t.Errorf("newest comment %q; want %q", cs[len(cs)-1].Body, want)
	}
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if is := state.Derive(s, a.AppSlug, clean); !is.Moved(state.CmdKey(cmdID)) {
		t.Error("the command isn't processed: no transition names it")
	}
}

func TestAck(t *testing.T) {
	t.Parallel()
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

// stateReason is issue n's state_reason, as GitHub answers it on f.
func stateReason(t *testing.T, f *githubtest.Fake, n int) string {
	t.Helper()
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return is.GetStateReason()
}

// writesOf is f's writes, as method and path.
func writesOf(f *githubtest.Fake) []string {
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path)
	}
	return writes
}

// A move to closed writes its marker, with the state reason as reason=,
// 👍s the command, then closes the issue with that state_reason, and writes
// no stage label: the close judge sets the closed stage. Each reason replays
// safely.
func TestTransitionClose(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"completed", "not_planned"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			setup := cmdIssue("priority:p2", "stage:intake")
			m := daemon.Move{To: state.Closed, StateReason: reason, Trigger: state.TriggerClosed, Key: state.CmdKey(cmdID), Text: "Closed."}
			githubtest.Replay(t, setup, move(t, m, clean))

			f := run(t, setup, move(t, m, clean))
			wantWrites := []string{"POST /issues/1/comments", "POST /issues/comments/1001/reactions", "PATCH /issues/1"}
			if writes := writesOf(f); !reflect.DeepEqual(writes, wantWrites) {
				t.Errorf("writes = %q, want %q", writes, wantWrites)
			}
			got := f.State().Issues[0]
			if want := []string{"priority:p2", "stage:intake"}; !got.Closed || !reflect.DeepEqual(got.Labels, want) {
				t.Errorf("closed %t, labels %v; want closed, and %v", got.Closed, got.Labels, want)
			}
			want := []githubtest.CommentState{
				{Author: "owner", Body: "/go", Reactions: []string{app + " +1"}},
				{Author: app, Body: "Closed.\n\n<!-- issuedog:transition from=intake to=closed reason=" + reason + " cmd=1001 -->"},
			}
			if !reflect.DeepEqual(got.Comments, want) {
				t.Errorf("comments = %+v, want %+v", got.Comments, want)
			}
			if r := stateReason(t, f, 1); r != reason {
				t.Errorf("state_reason = %q, want %q", r, reason)
			}
			// The snapshot shows the close, so the rest of the pass sees
			// a closed issue.
			f = setup(t)
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Transition(context.Background(), s, clean, m); err != nil || !s.Closed {
				t.Errorf("Transition: %v, and the snapshot closed %t; want no error, and closed", err, s.Closed)
			}
		})
	}
}

// A move to closed on an issue closed already writes its marker and the
// 👍, and doesn't close it again.
func TestTransitionCloseClosedAlready(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := cmdIssue("stage:intake")(t)
		f.CloseAs(1, "owner", "not_planned")
		return f
	}
	m := daemon.Move{To: state.Closed, StateReason: "completed", Trigger: state.TriggerClosed, Key: state.CmdKey(cmdID)}
	githubtest.Replay(t, setup, move(t, m, clean))
	f := run(t, setup, move(t, m, clean))
	if writes, want := writesOf(f), []string{"POST /issues/1/comments", "POST /issues/comments/1001/reactions"}; !reflect.DeepEqual(writes, want) {
		t.Errorf("writes = %q, want %q", writes, want)
	}
	if r := stateReason(t, f, 1); r != "not_planned" {
		t.Errorf("state_reason = %q, want the owner's not_planned", r)
	}
}

// A move to closed whose marker a later move followed, such as a trusted
// reopen's hold, isn't closed again on a replay: only its 👍 is finished.
func TestTransitionCloseOlderKey(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := cmdIssue("stage:held")(t)
		f.AddComment(1, app, "<!-- issuedog:transition from=intake to=closed reason=completed cmd=1001 -->")
		f.AddComment(1, app, "<!-- issuedog:transition from=closed to=held at=intake reason=owner event=5 -->")
		return f
	}
	m := daemon.Move{To: state.Closed, StateReason: "completed", Trigger: state.TriggerClosed, Key: state.CmdKey(cmdID)}
	githubtest.Replay(t, setup, move(t, m, clean))
	f := run(t, setup, move(t, m, clean))
	if writes, want := writesOf(f), []string{"POST /issues/comments/1001/reactions"}; !reflect.DeepEqual(writes, want) {
		t.Errorf("writes = %q, want %q", writes, want)
	}
	if got := f.State().Issues[0]; got.Closed || !reflect.DeepEqual(got.Labels, []string{"stage:held"}) {
		t.Errorf("closed %t, labels %v; want open, at stage:held", got.Closed, got.Labels)
	}
}

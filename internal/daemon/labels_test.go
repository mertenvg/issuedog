package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

type trusted map[string]bool

func (tr trusted) Trusted(_ context.Context, login string) (bool, error) { return tr[login], nil }

// labelsPass is a pass that runs CheckLabels on issue 1 with "owner"
// trusted, from a fresh client, then the move m, if any.
func labelsPass(t *testing.T, dirty func(int64) bool, m *daemon.Move) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		a.Unexplained = memoryOf(f)
		if err := a.CheckLabels(context.Background(), s, trusted{"owner": true}, dirty); err != nil || m == nil {
			return err
		}
		return a.Transition(context.Background(), s, dirty, *m)
	}
}

// memories keeps each fake's memory of unexplained labels, so that the passes
// on one fake share it, as one daemon's passes do.
var memories = struct {
	sync.Mutex
	of map[*githubtest.Fake]*github.Unexplained
}{of: map[*githubtest.Fake]*github.Unexplained{}}

func memoryOf(f *githubtest.Fake) *github.Unexplained {
	memories.Lock()
	defer memories.Unlock()
	if memories.of[f] == nil {
		memories.of[f] = &github.Unexplained{}
	}
	return memories.of[f]
}

// sighted runs setup, then a pass that holds back the unexplained labels and
// writes nothing, then waits until they can be removed.
func sighted(setup func(testing.TB) *githubtest.Fake) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := setup(t)
		tt, ok := t.(*testing.T)
		if !ok {
			t.Fatal("sighted needs a *testing.T")
		}
		if err := labelsPass(tt, clean, nil)(f); !errors.Is(err, daemon.ErrLabelsHeld) {
			t.Fatalf("first pass: %v, want labels held back", err)
		}
		if w := f.Writes(); len(w) != 0 {
			t.Fatalf("the first pass wrote %+v, want nothing", w)
		}
		f.Advance(github.UnexplainedWait)
		return f
	}
}

// labelEvent fails t unless a fake call returned the timeline event ID want.
func labelEvent(t testing.TB, got, want int64) {
	t.Helper()
	if got != want {
		t.Fatalf("event ID = %d, want %d", got, want)
	}
}

// filed files issue 1 by the owner with these labels, then runs more.
func filed(labels []string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue("owner", "Title", "body", labels...)
		more(t, f)
		return f
	}
}

// atReady files issue 1 with stage:ready (event 9001) and the App's marker
// for it, comment 1001, then runs more.
func atReady(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		labelEvent(t, f.AddLabel(1, app, "stage:ready"), 9001)
		f.AddComment(1, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
		more(t, f)
	})
}

// The App's move to closed cut short after its marker is finished by the
// label rules with the state reason the marker records, completed or
// not_planned, or GitHub's default for a marker with none, and posts no
// disagreement. A closed issue isn't closed again.
func TestCheckLabelsFinishesClose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, marker, want string }{
		{"completed", " reason=completed", "completed"},
		{"not planned", " reason=not_planned", "not_planned"},
		{"no reason", "", "completed"},
		{"a reason no close has", " reason=owner", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup := atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=closed"+tc.marker+" cmd=1001 -->")
			})
			githubtest.Replay(t, setup, labelsPass(t, clean, nil))
			f := run(t, setup, labelsPass(t, clean, nil))
			if got := f.State().Issues[0]; !got.Closed || len(got.Comments) != 2 {
				t.Errorf("closed %t, comments %+v; want closed, with no comment added", got.Closed, got.Comments)
			}
			if r := stateReason(t, f, 1); r != tc.want {
				t.Errorf("state_reason = %q, want %q", r, tc.want)
			}
		})
	}
	setup := atReady(func(_ testing.TB, f *githubtest.Fake) {
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=closed reason=completed cmd=1001 -->")
		f.CloseAs(1, "owner", "not_planned")
	})
	f := run(t, setup, labelsPass(t, clean, nil))
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("on a closed issue: writes %+v, want none", w)
	}
}

func TestCheckLabels(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	overridden := func(label, marker string) string {
		return "owner set `" + label + "` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition " + marker + " -->"
	}
	disagreement := func(were, key string) string {
		return "The stage labels didn't match this issue's latest transition, to `ready`, so I've set them to match. They were: " +
			were + ".\n\n<!-- issuedog:error kind=disagreement key=" + key + " -->"
	}
	renamed := func(label, key string) string {
		return "I removed `" + label + "`: no change by a trusted user or the App explains it, so it may have come from a renamed repo label. " +
			"Only trusted users can change pipeline labels.\n\n<!-- issuedog:revert key=" + key + " -->"
	}
	tests := []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		dirty func(int64) bool
		move  *daemon.Move // made after CheckLabels in the same pass
		// writes are the clean run's, as method and path.
		writes   []string
		comments []string // the comments it adds
		labels   []string
		errors   int // how often it logs a state error
	}{
		{name: "an override to held is recorded as an owner hold",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:ready")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9003)
			}),
			writes:   []string{"POST /issues/1/comments"},
			comments: []string{overridden("stage:held", "from=ready to=held at=ready reason=owner by=owner event=9003 seen=1001")},
			labels:   []string{"stage:held"}},
		{name: "an override is recorded before the old label goes",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:clarifying"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{overridden("stage:clarifying", "from=ready to=clarifying by=owner event=9002")},
			labels:   []string{"stage:clarifying"}},
		{name: "a stage change made while a transition was in flight is an override",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9002)
				// A transition from a snapshot taken before the owner's change.
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress run=b -->")
				f.AddLabel(1, app, "stage:in-progress")
				f.RemoveLabel(1, app, "stage:ready")
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:in-progress"},
			comments: []string{overridden("stage:held", "from=in-progress to=held at=in-progress reason=owner by=owner event=9002 seen=1002")},
			labels:   []string{"stage:held"}},
		{name: "a change older than the override the latest marker records isn't one",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:clarifying")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9003)
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner by=owner event=9003 -->")
			}),
			writes: []string{"DELETE /issues/1/labels/stage:clarifying", "DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:held"}},
		{name: "a hand-set label the latest transition moved from is finished",
			setup: filed([]string{"stage:intake"}, func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=intake to=clarifying run=a -->")
			}),
			writes: []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake"},
			labels: []string{"stage:clarifying"}},
		{name: "an override to held while held stacks: one held → held marker keeps the first hold's at and origin",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=discussion origin=ready -->")
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:ready")
				f.RemoveLabel(1, "owner", "stage:held")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9005)
			}),
			writes: []string{"POST /issues/1/comments"},
			comments: []string{"owner set `stage:held` by hand on this held issue, so I've recorded it as an owner hold of its own. " +
				"It stays held until `/resume` here.\n\n<!-- issuedog:transition from=held to=held at=ready origin=ready reason=owner by=owner event=9005 seen=1001 -->"},
			labels: []string{"stage:held"}},
		{name: "an override to held while held only fixes the labels",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				// The hold was posted, but its labels were never set.
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=discussion origin=ready -->")
				f.AddLabel(1, "owner", "stage:held")
			}),
			writes: []string{"DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:held"}},
		{name: "re-adding the marker's label writes nothing",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:ready")
				f.AddLabel(1, "owner", "stage:ready")
			}),
			labels: []string{"stage:ready"}},
		{name: "re-adding the marker's label doesn't hide a stray one",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, app, "stage:verifying")
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9003)
				f.AddLabel(1, "owner", "stage:ready")
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:verifying"},
			comments: []string{disagreement("`stage:ready`, `stage:verifying`", "9003")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "a hand-set issue is adopted (plan #5, assumption 6)",
			// The owner's own marker doesn't count; the labels do.
			setup: filed([]string{"stage:ready", "type:task"}, func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, "owner", "<!-- issuedog:transition from=ready to=planning -->")
			}),
			writes:   []string{"POST /issues/1/comments"},
			comments: []string{overridden("stage:ready", "from=intake to=ready by=owner event=9001")},
			labels:   []string{"stage:ready", "type:task"}},
		{name: "a hand-set stage:intake needs no marker",
			setup:  filed([]string{"stage:intake"}, func(testing.TB, *githubtest.Fake) {}),
			labels: []string{"stage:intake"}},
		{name: "a trusted removal is a disagreement, put back",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "POST /issues/1/labels"},
			comments: []string{disagreement("none", "9002")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "a disagreement is keyed by its cause, not an untrusted change or its revert",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
				labelEvent(t, f.AddLabel(1, "mallory", "stage:ready"), 9003)
			}),
			// The repair's add is only made if the snapshot learns of the revert.
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready", "POST /issues/1/comments", "POST /issues/1/labels"},
			comments: []string{"I removed `stage:ready`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=9003 -->",
				disagreement("none", "9002")},
			labels: []string{"stage:ready"}, errors: 1},
		{name: "a revert with no event still refreshes the snapshot's labels",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "task")
				labelEvent(t, f.AddLabel(1, "mallory", "stage:in-progress"), 9002)
			}),
			move: &daemon.Move{To: state.InProgress, Trigger: state.TriggerScheduled, Key: state.Key{Field: "run", ID: "b"}},
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:in-progress",
				"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{"I removed `stage:in-progress`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=9002 -->",
				"<!-- issuedog:transition from=ready to=in-progress run=b -->"},
			labels: []string{"stage:in-progress"}},
		{name: "a stray App label is a disagreement, removed",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, app, "stage:verifying"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:verifying"},
			comments: []string{disagreement("`stage:ready`, `stage:verifying`", "9002")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "a hand-set label that names no stage is a disagreement",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:<!--x-->"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:<!--x-->"},
			comments: []string{disagreement("`stage:ready`, `stage:&lt;!--x-->`", "9002")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "a hand-set label that names no stage, with no marker, is only removed",
			setup:    filed([]string{"stage:<!--bogus-->", "type:task"}, func(testing.TB, *githubtest.Fake) {}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:<!--bogus-->"},
			comments: []string{"`stage:&lt;!--bogus-->` isn't a pipeline stage, so I've removed it.\n\n<!-- issuedog:error kind=disagreement key=9001 -->"},
			labels:   []string{"type:task"}, errors: 1},
		{name: "a disagreement reported already is only put back",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
				f.AddComment(1, app, "<!-- issuedog:error kind=disagreement key=9002 -->")
			}),
			writes: []string{"POST /issues/1/labels"},
			labels: []string{"stage:ready"}},
		{name: "a transition cut short before its label is finished",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress run=b -->")
			}),
			writes: []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:in-progress"}},
		{name: "a transition cut short after its new label is finished",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress run=b -->")
				f.AddLabel(1, app, "stage:in-progress")
			}),
			writes: []string{"DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:in-progress"}},
		{name: "a move to closed cut short before its close is finished by the close, with no label write",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=closed reason=not_planned run=b -->")
			}),
			writes: []string{"PATCH /issues/1"},
			labels: []string{"stage:ready"}},
		// Only the App's own move cut short is finished by the close: a
		// stage label added after the move to closed, away from it, is a
		// disagreement, reported once, and closes nothing (SPEC §5.1).
		{name: "a stage label added after a move to closed is a disagreement, reported, and closes nothing",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=closed reason=completed run=b -->")
				labelEvent(t, f.AddLabel(1, app, "stage:in-progress"), 9002)
			}),
			writes: []string{"POST /issues/1/comments"},
			comments: []string{"The stage labels didn't match this issue's latest transition, to `closed`, so I've set them to match. " +
				"They were: `stage:ready`, `stage:in-progress`.\n\n<!-- issuedog:error kind=disagreement key=9002 -->"},
			labels: []string{"stage:in-progress", "stage:ready"}, errors: 1},
		{name: "a marker in a dirty comment doesn't count",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress run=b -->")
				f.AddLabel(1, app, "stage:in-progress")
				labelEvent(t, f.RemoveLabel(1, app, "stage:ready"), 9003)
			}),
			dirty:    func(id int64) bool { return id == 1002 },
			writes:   []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:in-progress"},
			comments: []string{disagreement("`stage:in-progress`", "9003")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "an untrusted model label is removed",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "mallory", "model:opus"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/model:opus"},
			comments: []string{"I removed `model:opus`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=9002 -->"},
			labels:   []string{"stage:ready"}},
		{name: "an untrusted removal of a model label is put back",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "model:opus")
				labelEvent(t, f.RemoveLabel(1, "mallory", "model:opus"), 9003)
			}),
			writes:   []string{"POST /issues/1/comments", "POST /issues/1/labels"},
			comments: []string{"I restored `model:opus`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=9003 -->"},
			labels:   []string{"model:opus", "stage:ready"}},
		{name: "an untrusted effort label is removed, its marker escaped",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "mallory", "verifier-effort:<!--x-->"), 9002)
			}),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/verifier-effort:<!--x-->"},
			comments: []string{"I removed `verifier-effort:&lt;!--x-->`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=9002 -->"},
			labels:   []string{"stage:ready"}},
		{name: "trusted model and effort changes stand",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, app, "model:sonnet")
				f.RemoveLabel(1, "owner", "model:sonnet")
				f.AddLabel(1, "owner", "model:opus")
				f.AddLabel(1, "owner", "verifier-model:opus")
				f.AddLabel(1, "owner", "effort:max")
				f.AddLabel(1, "owner", "verifier-effort:low")
			}),
			labels: []string{"effort:max", "model:opus", "stage:ready", "verifier-effort:low", "verifier-model:opus"}},
		{name: "a stage label a repo label rename put on is removed, and no disagreement is reported",
			setup: sighted(atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:done")
			})),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:done"},
			comments: []string{renamed("stage:done", "stage%3Adone@0")},
			labels:   []string{"stage:ready"}},
		{name: "an override is recorded beside a renamed label that's removed",
			setup: sighted(atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9002)
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "priority:p0")
			})),
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/priority:p0", "POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{renamed("priority:p0", "priority%3Ap0@0"),
				overridden("stage:held", "from=ready to=held at=ready reason=owner by=owner event=9002 seen=1001")},
			labels: []string{"stage:held"}},
		{name: "a disagreement is reported beside a renamed stage label that's removed",
			setup: sighted(atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:verifying")
			})),
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:verifying", "POST /issues/1/comments", "POST /issues/1/labels"},
			comments: []string{renamed("stage:verifying", "stage%3Averifying@0"), disagreement("none", "9002")},
			labels:   []string{"stage:ready"}, errors: 1},
		{name: "labels that agree are left alone",
			setup:  atReady(func(testing.TB, *githubtest.Fake) {}),
			labels: []string{"stage:ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirty := tt.dirty
			if dirty == nil {
				dirty = clean
			}
			githubtest.Replay(t, tt.setup, labelsPass(t, dirty, tt.move))

			before := len(tt.setup(t).State().Issues[0].Comments)
			var errs bytes.Buffer
			stop := logr.AddWriter(&errs, logr.WithFilter(logr.E))
			f := run(t, tt.setup, labelsPass(t, dirty, tt.move))
			logr.Wait()
			stop()
			var writes []string
			for _, w := range f.Writes() {
				writes = append(writes, w.Method+" "+w.Path)
			}
			if !reflect.DeepEqual(writes, tt.writes) {
				t.Errorf("writes = %q, want %q", writes, tt.writes)
			}
			got := f.State().Issues[0]
			var added []string
			for _, c := range got.Comments[before:] {
				added = append(added, c.Body)
			}
			if !reflect.DeepEqual(added, tt.comments) {
				t.Errorf("new comments = %q, want %q", added, tt.comments)
			}
			if !reflect.DeepEqual(got.Labels, tt.labels) {
				t.Errorf("labels = %v, want %v", got.Labels, tt.labels)
			}
			if n := strings.Count(errs.String(), "state error"); n != tt.errors {
				t.Errorf("error logs = %q, want %d state errors", errs.String(), tt.errors)
			}
		})
	}
}

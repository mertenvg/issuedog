package daemon_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
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
		if err := a.CheckLabels(context.Background(), s, trusted{"owner": true}, dirty); err != nil || m == nil {
			return err
		}
		return a.Transition(context.Background(), s, dirty, *m)
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

func TestCheckLabels(t *testing.T) {
	overridden := func(label, marker string) string {
		return "owner set `" + label + "` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition " + marker + " -->"
	}
	disagreement := func(were, key string) string {
		return "The stage labels didn't match this issue's latest transition, to `ready`, so I've set them to match. They were: " +
			were + ".\n\n<!-- issuedog:error kind=disagreement key=" + key + " -->"
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
			comments: []string{overridden("stage:held", "from=ready to=held at=ready reason=owner by=owner event=9003")},
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
			comments: []string{overridden("stage:held", "from=in-progress to=held at=in-progress reason=owner by=owner event=9002")},
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
		{name: "an override to held while held stacks and writes nothing",
			setup: atReady(func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=discussion origin=ready -->")
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:ready")
				f.RemoveLabel(1, "owner", "stage:held")
				f.AddLabel(1, "owner", "stage:held")
			}),
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

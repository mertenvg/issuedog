package daemon_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

type trusted map[string]bool

func (tr trusted) Trusted(_ context.Context, login string) (bool, error) { return tr[login], nil }

// checkLabels is a pass that runs CheckLabels on issue 1 with "owner"
// trusted, from a fresh client.
func checkLabels(t *testing.T, dirty func(int64) bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		return a.CheckLabels(context.Background(), s, trusted{"owner": true}, dirty)
	}
}

// labelEvent fails t unless a fake call returned the timeline event ID want.
func labelEvent(t testing.TB, got, want int64) {
	t.Helper()
	if got != want {
		t.Fatalf("event ID = %d, want %d", got, want)
	}
}

// atReady files issue 1 with stage:ready (event 9001) and the App's marker
// for it, comment 1001, then runs more.
func atReady(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		n := f.AddIssue("owner", "Title", "body")
		labelEvent(t, f.AddLabel(n, app, "stage:ready"), 9001)
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
		more(t, f)
		return f
	}
}

func TestCheckLabels(t *testing.T) {
	const disagreement = "The stage labels didn't match this issue's latest transition, to `ready`, so I've set them to match. They were: "
	tests := []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		dirty func(int64) bool
		// writes are the clean run's, as method and path.
		writes []string
		// comment is the one comment it adds, if any.
		comment string
		labels  []string
		// errors is how often it logs a state error.
		errors int
	}{
		{name: "an override to held is recorded as an owner hold",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:ready")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9003)
			}),
			writes:  []string{"POST /issues/1/comments"},
			comment: "owner set `stage:held` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition from=ready to=held at=ready reason=owner by=owner event=9003 -->",
			labels:  []string{"stage:held"}},
		{name: "an override is recorded before the old label goes",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:clarifying"), 9002)
			}),
			writes:  []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comment: "owner set `stage:clarifying` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition from=ready to=clarifying by=owner event=9002 -->",
			labels:  []string{"stage:clarifying"}},
		{name: "an override to held while held stacks and writes nothing",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=5 -->")
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:ready")
				f.RemoveLabel(1, "owner", "stage:held")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9005)
			}),
			labels: []string{"stage:held"}},
		{name: "an override to held while held only fixes the labels",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				// The hold was posted, but its labels were never set.
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=5 -->")
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9002)
			}),
			writes: []string{"DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:held"}},
		{name: "an override to the current stage writes nothing",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:ready")
				labelEvent(t, f.AddLabel(1, "owner", "stage:ready"), 9003)
			}),
			labels: []string{"stage:ready"}},
		{name: "a trusted removal is a disagreement, put back",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
			}),
			writes:  []string{"POST /issues/1/comments", "POST /issues/1/labels"},
			comment: disagreement + "none.\n\n<!-- issuedog:error kind=disagreement key=9002 -->",
			labels:  []string{"stage:ready"}, errors: 1},
		{name: "a stray App label is a disagreement, removed",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, app, "stage:verifying"), 9002)
			}),
			writes:  []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:verifying"},
			comment: disagreement + "`stage:ready`, `stage:verifying`.\n\n<!-- issuedog:error kind=disagreement key=9002 -->",
			labels:  []string{"stage:ready"}, errors: 1},
		{name: "a hand-set label that names no stage is a disagreement",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:<!--x-->"), 9002)
			}),
			writes:  []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:<!--x-->"},
			comment: disagreement + "`stage:ready`, `stage:&lt;!--x-->`.\n\n<!-- issuedog:error kind=disagreement key=9002 -->",
			labels:  []string{"stage:ready"}, errors: 1},
		{name: "a hand-set label that names no stage, with no marker, is only removed",
			setup: func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", slug)
				f.AddIssue("owner", "Title", "body", "stage:<!--bogus-->", "type:task")
				return f
			},
			writes:  []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:<!--bogus-->"},
			comment: "`stage:&lt;!--bogus-->` isn't a pipeline stage, so I've removed it.\n\n<!-- issuedog:error kind=disagreement key=9001 -->",
			labels:  []string{"type:task"}, errors: 1},
		{name: "a disagreement reported already is only put back",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
				f.AddComment(1, app, "<!-- issuedog:error kind=disagreement key=9002 -->")
			}),
			writes: []string{"POST /issues/1/labels"},
			labels: []string{"stage:ready"}},
		{name: "a transition cut short before its label is finished",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress run=b -->")
			}),
			writes: []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:ready"},
			labels: []string{"stage:in-progress"}},
		{name: "a transition cut short after its new label is finished",
			setup: atReady(func(t testing.TB, f *githubtest.Fake) {
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
			dirty:   func(id int64) bool { return id == 1002 },
			writes:  []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:in-progress"},
			comment: disagreement + "`stage:in-progress`.\n\n<!-- issuedog:error kind=disagreement key=9003 -->",
			labels:  []string{"stage:ready"}, errors: 1},
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
			githubtest.Replay(t, tt.setup, checkLabels(t, dirty))

			before := len(tt.setup(t).State().Issues[0].Comments)
			var errs bytes.Buffer
			stop := logr.AddWriter(&errs, logr.WithFilter(logr.E))
			f := run(t, tt.setup, checkLabels(t, dirty))
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
			if want := []string{tt.comment}; (tt.comment != "" || len(added) > 0) && !reflect.DeepEqual(added, want) {
				t.Errorf("new comments = %q, want %q", added, want)
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

func TestCheckLabelsKeysTheRealCause(t *testing.T) {
	// The owner removes stage:ready, then mallory adds it back. The revert of
	// mallory's add leaves the owner's removal as the cause, even once the
	// App's revert is on the timeline, so a replay posts no second error.
	setup := atReady(func(t testing.TB, f *githubtest.Fake) {
		labelEvent(t, f.RemoveLabel(1, "owner", "stage:ready"), 9002)
		labelEvent(t, f.AddLabel(1, "mallory", "stage:ready"), 9003)
	})
	githubtest.Replay(t, setup, checkLabels(t, clean))
	f := run(t, setup, checkLabels(t, clean))
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path)
	}
	// The revert's comment and DELETE, then the error comment and the repair.
	// The repair's add is only made if the snapshot learns of the revert.
	want := []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready", "POST /issues/1/comments", "POST /issues/1/labels"}
	if !reflect.DeepEqual(writes, want) {
		t.Errorf("writes = %q, want %q", writes, want)
	}
	got := f.State().Issues[0]
	if !reflect.DeepEqual(got.Labels, []string{"stage:ready"}) {
		t.Errorf("labels = %v, want only stage:ready", got.Labels)
	}
	var errs []string
	for _, c := range got.Comments {
		if strings.Contains(c.Body, "issuedog:error") {
			errs = append(errs, c.Body[strings.LastIndex(c.Body, "<!--"):])
		}
	}
	if want := []string{"<!-- issuedog:error kind=disagreement key=9002 -->"}; !reflect.DeepEqual(errs, want) {
		t.Errorf("error markers = %q, want %q", errs, want)
	}
}

func TestCheckLabelsAdoptsHandSetIssue(t *testing.T) {
	// Plan #5, assumption 6: the owner set the labels and wrote the marker
	// with their own token, so only the labels count.
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		n := f.AddIssue("owner", "Title", "body", "stage:ready", "type:task")
		f.AddComment(n, "owner", "<!-- issuedog:transition from=intake to=planning -->")
		return f
	}
	githubtest.Replay(t, setup, checkLabels(t, clean))
	f := run(t, setup, checkLabels(t, clean))
	got := f.State().Issues[0]
	if want := []string{"stage:ready", "type:task"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v unchanged", got.Labels, want)
	}
	want := "owner set `stage:ready` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition from=intake to=ready by=owner event=9001 -->"
	if n := len(got.Comments); n != 2 || got.Comments[1].Body != want {
		t.Errorf("comments = %+v, want one added: %q", got.Comments, want)
	}
	_, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if st := state.Derive(s, slug, clean).Stage; st != state.Ready {
		t.Errorf("stage = %s, want ready", st)
	}
}

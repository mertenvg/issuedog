package github_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

const appBot = "issuedog[bot]"

type ownerTrusted struct{}

func (ownerTrusted) Trusted(_ context.Context, login string) (bool, error) {
	return login == "owner", nil
}

// checked is what CheckLabels returned.
type checked struct {
	labels []string
	events []github.LabelEvent
}

// checkPass is a pass that runs CheckLabels on issue 1 with "owner" trusted,
// from a fresh client and snapshot, as the daemon does. It keeps what
// CheckLabels returns in out, unless out is nil.
func checkPass(t *testing.T, out *checked) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		labels, events, _, err := c.CheckLabels(context.Background(), "o", "r", 1, s.Labels, "issuedog", ownerTrusted{}, func(int64) bool { return false })
		if err != nil {
			return err
		}
		if out != nil {
			*out = checked{labels, events}
		}
		return nil
	}
}

// A pipeline label that a repo label rename put on an issue, which no
// standing change on the timeline explains, is removed with a revert
// comment. Labels the timeline explains, in any case, are left alone.
func TestCheckLabelsRenames(t *testing.T) {
	removed := func(label, key string) string {
		return "I removed `" + label + "`: no change by a trusted user or the App explains it, so it may have come from a renamed repo label. " +
			"Only trusted users can change pipeline labels.\n\n<!-- issuedog:revert key=" + key + " -->"
	}
	tests := []struct {
		name  string
		filed []string // issue 1's labels, set by the owner as it's filed
		setup func(f *githubtest.Fake)
		// writes are the clean run's, as method and path.
		writes   []string
		comments []string // the comments it adds
		labels   []string // on the issue after
		returned []string // CheckLabels' labels
		events   []github.LabelEvent
	}{
		{name: "a rename to stage:ready with no standing event is removed",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:ready")
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{removed("stage:ready", "stage%3Aready@0")}},
		{name: "a trusted rename of an unguarded label is unaffected",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "foo")
				f.RenameLabel("foo", "bar")
			},
			labels: []string{"bar"}, returned: []string{"bar"}},
		{name: "a rename of an unguarded label that someone untrusted added is unaffected",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("FOO", "bar")
			},
			labels: []string{"bar"}, returned: []string{"bar"}},
		{name: "a rename of one model label to another is removed, and the timeline's stands",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "model:sonnet")
				f.RenameLabel("model:sonnet", "model:opus")
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/model:opus"},
			comments: []string{removed("model:opus", "model%3Aopus@0")},
			returned: []string{"model:sonnet"}},
		{name: "a rename onto a label whose last standing change removed it is removed, in its own case",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, appBot, "stage:ready")
				f.RemoveLabel(1, appBot, "stage:ready") // event 9002
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "Stage:Ready")
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/Stage:Ready"},
			comments: []string{removed("Stage:Ready", "stage%3Aready@9002")}},
		{name: "a second rename after the App removed the first is removed with a new key",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:ready")
				f.AddComment(1, appBot, removed("stage:ready", "stage%3Aready@0"))
				f.RemoveLabel(1, appBot, "stage:ready") // event 9002
				f.AddLabel(1, "mallory", "bar")
				f.RenameLabel("bar", "stage:ready")
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{removed("stage:ready", "stage%3Aready@9002")}},
		{name: "a marker in a renamed label is escaped in the comment",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:<!-- issuedog:transition from=x to=done -->")
			},
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:<!-- issuedog:transition from=x to=done -->"},
			comments: []string{removed("stage:&lt;!-- issuedog:transition from=x to=done -->",
				"stage%3A%3C%21--+issuedog%3Atransition+from%3Dx+to%3Ddone+--%3E@0")}},
		{name: "labels set by hand before adoption stand, the stage label to be adopted as an override",
			filed:    []string{"stage:intake", "priority:p1", "type:task", "approved"},
			setup:    func(*githubtest.Fake) {},
			labels:   []string{"approved", "priority:p1", "stage:intake", "type:task"},
			returned: []string{"approved", "priority:p1", "stage:intake", "type:task"},
			events:   []github.LabelEvent{{Kind: github.LabelOverride, Stages: []string{"stage:intake"}, By: "owner", Label: "stage:intake", Key: 9001}}},
		{name: "the App's own labels stand",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, appBot, "stage:ready")
				f.AddLabel(1, appBot, "priority:p2")
				f.AddLabel(1, appBot, "model:opus")
			},
			labels:   []string{"model:opus", "priority:p2", "stage:ready"},
			returned: []string{"model:opus", "priority:p2", "stage:ready"}},
		{name: "trusted hand changes stand",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, appBot, "priority:p2")
				f.RemoveLabel(1, "owner", "priority:p2")
				f.AddLabel(1, "owner", "priority:p0")
				f.AddLabel(1, "owner", "effort:max")
			},
			labels:   []string{"effort:max", "priority:p0"},
			returned: []string{"effort:max", "priority:p0"}},
		{name: "a label renamed only in case is explained by its events in the old case",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, appBot, "stage:ready")
				f.AddLabel(1, "owner", "Priority:P1")
				f.RenameLabel("stage:ready", "Stage:READY")
				f.RenameLabel("priority:p1", "priority:p1")
			},
			labels:   []string{"Stage:READY", "priority:p1"},
			returned: []string{"Stage:READY", "priority:p1"}},
		{name: "a timeline of more than one page explains labels on either page, and not a rename",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, appBot, "stage:ready")
				for range 150 {
					f.AddComment(1, "owner", "hi")
				}
				f.AddLabel(1, "owner", "priority:p1")
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "scope:cli")
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/scope:cli"},
			comments: []string{removed("scope:cli", "scope%3Acli@0")},
			labels:   []string{"priority:p1", "stage:ready"}, returned: []string{"priority:p1", "stage:ready"}},
		{name: "with events renamed too, an untrusted user's renamed label is reverted as their add",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo") // event 9001
				f.RenameLabelInEvents("foo", "stage:ready")
			},
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
			comments: []string{"I removed `stage:ready`: only trusted users can change pipeline labels, and mallory isn't one.\n\n" +
				"<!-- issuedog:revert key=9001 -->"}},
		{name: "with events renamed too, a trusted user's renamed stage label disagrees with the marker",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "foo")
				f.AddLabel(1, appBot, "stage:intake")
				f.AddComment(1, appBot, "<!-- issuedog:transition from=new to=intake run=a -->")
				f.AddLabel(1, appBot, "stage:ready")
				f.RemoveLabel(1, appBot, "stage:intake") // event 9004
				f.AddComment(1, appBot, "<!-- issuedog:transition from=intake to=ready run=b -->")
				f.RenameLabelInEvents("foo", "stage:held")
			},
			labels: []string{"stage:held", "stage:ready"}, returned: []string{"stage:held", "stage:ready"},
			events: []github.LabelEvent{{Kind: github.LabelDisagreement, Stages: []string{"stage:held", "stage:ready"}, Marker: "ready",
				Key: 9004, Unfinished: true}}},
		{name: "a pipeline label renamed away stays in the labels returned, as before",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "priority:p1")
				f.RenameLabel("priority:p1", "foo")
			},
			labels: []string{"foo"}, returned: []string{"foo", "priority:p1"}},
		{name: "a label removed after the snapshot isn't reverted",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "priority:p1")
				f.BeforeRequest(isTimeline, func() { f.RemoveLabel(1, "owner", "priority:p1") })
			}},
		{name: "a label changed again while it was checked is left to the next pass",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "priority:p1")
				f.BeforeRequest(isTimeline, func() {
					f.RemoveLabel(1, "owner", "priority:p1")
					// Then, before CheckLabels reads the issue again.
					f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1" },
						func() { f.AddLabel(1, "owner", "priority:p1") })
				})
			},
			labels: []string{"priority:p1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setup := func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", "issuedog")
				f.AddIssue("owner", "Title", "body", tt.filed...)
				tt.setup(f)
				return f
			}
			githubtest.Replay(t, setup, checkPass(t, nil))

			f := setup(t)
			before := len(f.State().Issues[0].Comments)
			var got checked
			if err := checkPass(t, &got)(f); err != nil {
				t.Fatal(err)
			}
			var writes []string
			for _, w := range f.Writes() {
				writes = append(writes, w.Method+" "+w.Path)
			}
			if !slices.Equal(writes, tt.writes) {
				t.Errorf("writes = %q, want %q", writes, tt.writes)
			}
			is := f.State().Issues[0]
			var added []string
			for _, c := range is.Comments[before:] {
				added = append(added, c.Body)
				// Only the App's own marker is read.
				if ms := github.ParseMarkers(c.Body); len(ms) != 1 || ms[0].Kind != "revert" {
					t.Errorf("markers in %q = %+v, want only the revert marker", c.Body, ms)
				}
			}
			if !slices.Equal(added, tt.comments) {
				t.Errorf("new comments = %q, want %q", added, tt.comments)
			}
			if !slices.Equal(is.Labels, tt.labels) {
				t.Errorf("labels = %q, want %q", is.Labels, tt.labels)
			}
			slices.Sort(got.labels)
			if !slices.Equal(got.labels, tt.returned) {
				t.Errorf("returned labels = %q, want %q", got.labels, tt.returned)
			}
			if !reflect.DeepEqual(got.events, tt.events) {
				t.Errorf("events = %+v, want %+v", got.events, tt.events)
			}
		})
	}
}

// isTimeline matches a read of issue 1's timeline.
func isTimeline(r githubtest.Request) bool {
	return r.Method == "GET" && strings.HasPrefix(r.Path, "/issues/1/timeline")
}

package github_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
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
	labels, held []string
	events       []github.LabelEvent
}

// memory keeps a github.Unexplained for each fake, so that passes on one
// fake share it, as one daemon's passes do.
type memory map[*githubtest.Fake]*github.Unexplained

func (m memory) of(f *githubtest.Fake) *github.Unexplained {
	if m[f] == nil {
		m[f] = &github.Unexplained{}
	}
	return m[f]
}

// checkPass is a pass that runs CheckLabels on issue 1 with "owner" trusted,
// from a fresh client and snapshot, and mem's memory of f. It keeps what
// CheckLabels returns in out, unless out is nil.
func checkPass(t *testing.T, mem memory, out *checked) func(*githubtest.Fake) error {
	return checkPassOn(t, 1, mem, out)
}

// checkPassOn is checkPass on issue n.
func checkPassOn(t *testing.T, n int, mem memory, out *checked) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		labels, events, _, held, err := c.CheckLabels(context.Background(), "o", "r", n, s.Labels, "issuedog", ownerTrusted{},
			func(int64) bool { return false }, mem.of(f))
		if err != nil {
			return err
		}
		if out != nil {
			*out = checked{labels, held, events}
		}
		return nil
	}
}

// sight runs a pass that finds f's unexplained labels, which must write
// nothing, then waits until they have stayed so long enough to be removed.
func sight(t testing.TB, mem memory, f *githubtest.Fake) {
	t.Helper()
	tt, ok := t.(*testing.T)
	if !ok {
		t.Fatal("sight needs a *testing.T")
	}
	if err := checkPass(tt, mem, nil)(f); err != nil {
		t.Fatal(err)
	}
	if w := writesOf(f); len(w) != 0 {
		t.Fatalf("the first pass wrote %q, want nothing", w)
	}
	f.Advance(github.UnexplainedWait)
}

const removedText = "no change by a trusted user or the App explains it, so it may have come from a renamed repo label. " +
	"Only trusted users can change pipeline labels."

// removed is the comment on the removal of one unexplained label.
func removed(label, key string) string {
	return "I removed `" + label + "`: " + removedText + "\n\n<!-- issuedog:revert key=" + key + " -->"
}

// A pipeline label that a repo label rename put on an issue, which no
// standing change on the timeline explains, is removed with a revert
// comment once it has stayed so for UnexplainedWait. Labels the timeline
// explains, in any case, are left alone.
func TestCheckLabelsRenames(t *testing.T) {
	tests := []struct {
		name  string
		filed []string // issue 1's labels, set by the owner as it's filed
		setup func(f *githubtest.Fake)
		// hook runs after the pass that first sees the labels, and the wait.
		hook func(f *githubtest.Fake)
		// noSight skips that pass, which would write.
		noSight bool
		// writes are the clean run's, as method and path.
		writes   []string
		comments []string // the comments it adds
		labels   []string // on the issue after
		returned []string // CheckLabels' labels
		held     []string
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
		{name: "a replayed removal whose comment is posted doesn't comment again",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "stage:ready")
				f.AddComment(1, appBot, removed("stage:ready", "stage%3Aready@0"))
			},
			writes: []string{"DELETE /issues/1/labels/stage:ready"}},
		{name: "two renamed labels share one comment",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.AddLabel(1, "mallory", "bar")
				f.RenameLabel("foo", "priority:p0")
				f.RenameLabel("bar", "model:opus")
			},
			writes: []string{"POST /issues/1/comments", "DELETE /issues/1/labels/model:opus", "DELETE /issues/1/labels/priority:p0"},
			comments: []string{"I removed `model:opus`, `priority:p0`: no change by a trusted user or the App explains them, so they may have come " +
				"from renamed repo labels. Only trusted users can change pipeline labels.\n\n" +
				"<!-- issuedog:revert key=model%3Aopus@0 -->\n<!-- issuedog:revert key=priority%3Ap0@0 -->"}},
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
			noSight: true,
			writes:  []string{"POST /issues/1/comments", "DELETE /issues/1/labels/stage:ready"},
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
			setup: func(f *githubtest.Fake) { f.AddLabel(1, "owner", "priority:p1") },
			hook: func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() { f.RemoveLabel(1, "owner", "priority:p1") })
			}},
		{name: "a label changed again before the issue is read again is held back",
			setup: func(f *githubtest.Fake) { f.AddLabel(1, "owner", "priority:p1") },
			hook: func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() {
					f.RemoveLabel(1, "owner", "priority:p1")
					// Then, before CheckLabels reads the issue again.
					f.BeforeRequest(isIssue, func() { f.AddLabel(1, "owner", "priority:p1") })
				})
			},
			labels: []string{"priority:p1"}, held: []string{"priority:p1"}},
		{name: "a renamed label changed between the second reads is left out, not removed",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "priority:p0")
			},
			hook: func(f *githubtest.Fake) {
				// Before the second timeline read, after the issue's.
				f.BeforeRequest(isTimeline, func() {
					f.BeforeRequest(isTimeline, func() { f.RemoveLabel(1, "owner", "priority:p0") })
				})
			}},
		{name: "a renamed label with no events that's gone on the second read is dropped",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "priority:p0")
			},
			hook: func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() { f.RenameLabel("priority:p0", "bar") })
			},
			labels: []string{"bar"}},
		{name: "a renamed label is removed under the name the second read gives it",
			setup: func(f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "foo")
				f.RenameLabel("foo", "priority:p0")
			},
			hook: func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() { f.RenameLabel("priority:p0", "Priority:P0") })
			},
			writes:   []string{"POST /issues/1/comments", "DELETE /issues/1/labels/Priority:P0"},
			comments: []string{removed("Priority:P0", "priority%3Ap0@0")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := memory{}
			setup := func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", "issuedog")
				f.AddIssue("owner", "Title", "body", tt.filed...)
				tt.setup(f)
				if !tt.noSight {
					sight(t, mem, f)
				}
				if tt.hook != nil {
					tt.hook(f)
				}
				return f
			}
			githubtest.Replay(t, setup, checkPass(t, mem, nil))

			f := setup(t)
			before := len(f.State().Issues[0].Comments)
			var got checked
			if err := checkPass(t, mem, &got)(f); err != nil {
				t.Fatal(err)
			}
			if w := writesOf(f); !slices.Equal(w, tt.writes) {
				t.Errorf("writes = %q, want %q", w, tt.writes)
			}
			is := f.State().Issues[0]
			var added []string
			for _, c := range is.Comments[before:] {
				added = append(added, c.Body)
				// Only the App's own markers are read.
				for _, m := range github.ParseMarkers(c.Body) {
					if m.Kind != "revert" {
						t.Errorf("marker %+v in %q, want only revert markers", m, c.Body)
					}
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
			if !slices.Equal(got.held, tt.held) {
				t.Errorf("held = %q, want %q", got.held, tt.held)
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

// isIssue matches a read of issue 1.
func isIssue(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1" }

// isRemoval matches a removal of a label from issue 1.
func isRemoval(r githubtest.Request) bool {
	return r.Method == "DELETE" && strings.HasPrefix(r.Path, "/issues/1/labels/")
}

// writesOf returns f's writes as method and path.
func writesOf(f *githubtest.Fake) []string {
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path)
	}
	return writes
}

// renamed files issue 1 with mallory's foo, renamed to priority:p0, which
// nothing explains.
func renamed(t *testing.T) *githubtest.Fake {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Title", "body")
	f.AddLabel(1, "mallory", "foo")
	f.Advance(time.Hour)
	f.RenameLabel("foo", "priority:p0")
	return f
}

// passes runs a pass on f with mem after each wait, and returns what each
// one held back, and the pass that removed priority:p0, or -1. before runs
// before each pass.
func passes(t *testing.T, f *githubtest.Fake, mem memory, waits []time.Duration, before func()) ([][]string, int) {
	t.Helper()
	var held [][]string
	removedAt := -1
	for i, wait := range waits {
		if before != nil {
			before()
		}
		f.Advance(wait)
		var got checked
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		held = append(held, got.held)
		if removedAt < 0 && !slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
			removedAt = i
		}
	}
	return held, removedAt
}

// An unexplained label is removed once it has stayed so for
// UnexplainedWait across passes, not a second before.
func TestCheckLabelsUnexplainedWait(t *testing.T) {
	f, mem := renamed(t), memory{}
	held, at := passes(t, f, mem, []time.Duration{0, github.UnexplainedWait - time.Second, time.Second}, nil)
	if want := [][]string{{"priority:p0"}, nil, nil}; !reflect.DeepEqual(held, want) || at != 2 {
		t.Errorf("held = %q, removed by pass %d; want %q, and removed by pass 2", held, at, want)
	}
	if w := writesOf(f); !slices.Equal(w, []string{"POST /issues/1/comments", "DELETE /issues/1/labels/priority:p0"}) {
		t.Errorf("writes = %q, want one comment and the removal", w)
	}
}

// Comments every 50 seconds, untrusted or not, don't hold the removal off.
func TestCheckLabelsSpamDoesNotHoldOff(t *testing.T) {
	f, mem := renamed(t), memory{}
	waits := make([]time.Duration, 8)
	for i := range waits {
		waits[i] = 50 * time.Second
	}
	// The pass after first seeing it, and the 50-second waits, with a
	// comment each, a second each on the fake clock.
	_, at := passes(t, f, mem, waits, func() { f.AddComment(1, "mallory", "spam") })
	// First seen at pass 0; five minutes later is during pass 5 or 6.
	if at < 5 || at > 6 {
		t.Errorf("removed by pass %d, want pass 5 or 6, five minutes after the first", at)
	}
}

// A label's time is forgotten once it's explained or gone, so when it comes
// back unexplained it waits the whole time again.
func TestCheckLabelsUnexplainedForgotten(t *testing.T) {
	for name, explain := range map[string]func(f *githubtest.Fake){
		"explained": func(f *githubtest.Fake) {
			f.RemoveLabel(1, "owner", "priority:p0")
			f.AddLabel(1, "owner", "priority:p0")
		},
		"gone": func(f *githubtest.Fake) { f.RenameLabel("priority:p0", "baz") },
		"gone by the second read": func(f *githubtest.Fake) {
			f.BeforeRequest(isTimeline, func() { f.RenameLabel("priority:p0", "baz") })
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, mem := renamed(t), memory{}
			passes(t, f, mem, []time.Duration{0}, nil)
			f.Advance(4 * time.Minute)
			explain(f)
			if held, _ := passes(t, f, mem, []time.Duration{0}, nil); held[0] != nil {
				t.Fatalf("held = %q once %s, want none", held[0], name)
			}
			// Unexplained again: by a removal, then a rename onto it.
			f.RemoveLabel(1, "owner", "priority:p0")
			f.AddLabel(1, "mallory", "bar")
			f.RenameLabel("bar", "priority:p0")
			held, at := passes(t, f, mem, []time.Duration{0, 2 * time.Minute, github.UnexplainedWait - 2*time.Minute}, nil)
			if want := [][]string{{"priority:p0"}, nil, nil}; !reflect.DeepEqual(held, want) || at != 2 {
				t.Errorf("held = %q, removed by pass %d; want %q, and removed by pass 2", held, at, want)
			}
		})
	}
}

// A removal that found its label gone leaves no time behind, so when the
// owner adds that label by hand later, it's held back while its event is
// late, and not removed.
func TestCheckLabelsNoStaleTime(t *testing.T) {
	f, mem := githubtest.New(t, "o/r", "issuedog"), memory{}
	f.AddIssue("owner", "Title", "body")
	f.AddLabel(1, "mallory", "foo")
	f.RenameLabel("foo", "priority:p1")
	sight(t, mem, f)
	// The removal finds it gone: someone renamed it away just before.
	f.BeforeRequest(isRemoval, func() { f.RenameLabel("priority:p1", "bar") })
	f.AnswerOnce(isRemoval, http.StatusNotFound)
	if err := checkPass(t, mem, nil)(f); err != nil {
		t.Fatal(err)
	}
	f.Advance(time.Hour)
	f.RemoveLabel(1, "owner", "bar")
	f.Advance(time.Hour)
	f.LagEvents(2 * time.Second)
	f.AddLabel(1, "owner", "priority:p1")
	before := len(f.Writes())
	var got checked
	if err := checkPass(t, mem, &got)(f); err != nil {
		t.Fatal(err)
	}
	if w := writesOf(f)[before:]; len(w) != 0 || !slices.Equal(got.held, []string{"priority:p1"}) {
		t.Errorf("writes = %q, held = %q; want none, and priority:p1 held back", w, got.held)
	}
}

// GitHub writes label events a second or two after the change. Labels whose
// events are missing are held back, and removed only if the events are
// still missing after UnexplainedWait.
func TestCheckLabelsLateEvents(t *testing.T) {
	filed := []string{"stage:ready", "type:task", "priority:p3", "scope:github"}
	t.Run("labels an issue is filed with are held back until their events are written", func(t *testing.T) {
		f, mem := githubtest.New(t, "o/r", "issuedog"), memory{}
		f.LagEvents(2 * time.Second)
		f.AddIssue("owner", "Title", "body", filed...)
		var got checked
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		if w := writesOf(f); len(w) != 0 || len(got.held) != 4 {
			t.Errorf("writes before the events = %q, held %q; want none, and all four held", w, got.held)
		}
		f.Advance(2 * time.Second)
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		if w := writesOf(f); len(w) != 0 || got.held != nil {
			t.Errorf("writes once the events are written = %q, held %q; want none", w, got.held)
		}
		if want := []string{"priority:p3", "scope:github", "stage:ready", "type:task"}; !slices.Equal(f.State().Issues[0].Labels, want) {
			t.Errorf("labels = %q, want %q", f.State().Issues[0].Labels, want)
		}
		if len(got.events) != 1 || got.events[0].Kind != github.LabelOverride || got.events[0].Label != "stage:ready" {
			t.Errorf("events = %+v, want the owner's stage:ready, to adopt", got.events)
		}
	})
	t.Run("a label the App added is held back until its event is written", func(t *testing.T) {
		f, mem := githubtest.New(t, "o/r", "issuedog"), memory{}
		f.AddIssue("owner", "Title", "body")
		f.Advance(time.Hour)
		f.LagEvents(2 * time.Second)
		f.AddLabel(1, appBot, "priority:p1")
		for _, tt := range []struct {
			wait time.Duration
			held []string
		}{{0, []string{"priority:p1"}}, {2 * time.Second, nil}} {
			f.Advance(tt.wait)
			var got checked
			if err := checkPass(t, mem, &got)(f); err != nil {
				t.Fatal(err)
			}
			if w := writesOf(f); len(w) != 0 || !slices.Equal(got.held, tt.held) {
				t.Errorf("%v after the add: writes %q, held %q; want none, and %q held", tt.wait, w, got.held, tt.held)
			}
		}
	})
	t.Run("labels whose events are still missing after the wait are removed", func(t *testing.T) {
		mem := memory{}
		setup := func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", "issuedog")
			f.LagEvents(time.Hour)
			f.AddIssue("owner", "Title", "body", filed...)
			sight(t, mem, f)
			return f
		}
		githubtest.Replay(t, setup, checkPass(t, mem, nil))
		f := setup(t)
		if err := checkPass(t, mem, nil)(f); err != nil {
			t.Fatal(err)
		}
		for _, l := range f.State().Issues[0].Labels {
			t.Errorf("label %s is still on the issue", l)
		}
		want := []string{"POST /issues/1/comments"}
		for _, l := range []string{"priority:p3", "scope:github", "stage:ready", "type:task"} {
			want = append(want, "DELETE /issues/1/labels/"+l)
		}
		if w := writesOf(f); !slices.Equal(w, want) {
			t.Errorf("writes = %q, want %q", w, want)
		}
		var comments []string
		for _, c := range f.State().Issues[0].Comments {
			comments = append(comments, c.Body)
		}
		one := "I removed `priority:p3`, `scope:github`, `stage:ready`, `type:task`: no change by a trusted user or the App explains them, " +
			"so they may have come from renamed repo labels. Only trusted users can change pipeline labels.\n\n" +
			"<!-- issuedog:revert key=priority%3Ap3@0 -->\n<!-- issuedog:revert key=scope%3Agithub@0 -->\n" +
			"<!-- issuedog:revert key=stage%3Aready@0 -->\n<!-- issuedog:revert key=type%3Atask@0 -->"
		if !slices.Equal(comments, []string{one}) {
			t.Errorf("comments = %q, want %q", comments, []string{one})
		}
	})
}

// A label settle has just reverted isn't checked again: no second read of
// the issue, and nothing held back.
func TestCheckLabelsSkipsWhatSettleReverted(t *testing.T) {
	f, mem := githubtest.New(t, "o/r", "issuedog"), memory{}
	f.AddIssue("owner", "Title", "body")
	f.AddLabel(1, "mallory", "priority:p0") // event 9001
	var got checked
	if err := checkPass(t, mem, &got)(f); err != nil {
		t.Fatal(err)
	}
	if w, want := writesOf(f), []string{"POST /issues/1/comments", "DELETE /issues/1/labels/priority:p0"}; !slices.Equal(w, want) || got.held != nil {
		t.Errorf("writes = %q, held %q; want %q, and nothing held", w, got.held, want)
	}
	reads := 0
	for _, r := range f.Requests() {
		if isIssue(r) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("issue 1 read %d times, want once, by the snapshot", reads)
	}
}

// A failed read or removal is returned, wrapped; a removal that finds the
// label gone already is not a failure.
func TestCheckLabelsRenameErrors(t *testing.T) {
	tests := []struct {
		name string
		fail func(f *githubtest.Fake)
		want string // in the error, or "" for none
	}{
		{"the second read of the issue fails",
			func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() { f.AnswerOnce(isIssue, http.StatusInternalServerError) })
			}, "read the labels of o/r#1 again"},
		{"the second read of the timeline fails",
			func(f *githubtest.Fake) {
				f.BeforeRequest(isTimeline, func() { f.AnswerOnce(isTimeline, http.StatusInternalServerError) })
			}, "read the timeline of o/r#1 again"},
		{"the removal fails",
			func(f *githubtest.Fake) { f.AnswerOnce(isRemoval, http.StatusInternalServerError) }, "remove priority:p0 from o/r#1"},
		{"the removal finds the label gone",
			func(f *githubtest.Fake) { f.AnswerOnce(isRemoval, http.StatusNotFound) }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, mem := renamed(t), memory{}
			sight(t, mem, f)
			tt.fail(f)
			var got checked
			err := checkPass(t, mem, &got)(f)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(got.labels) != 0 {
					t.Errorf("returned labels = %q, want none", got.labels)
				}
				return
			}
			var gh *gogithub.ErrorResponse
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.As(err, &gh) {
				t.Errorf("error = %v, want one saying %q that wraps GitHub's", err, tt.want)
			}
		})
	}
}

// An issue is held back for HoldBound from when it was first found with an
// unexplained label, not a second longer; after that, the label is left
// out of the labels returned, and nothing is written.
func TestCheckLabelsHoldBound(t *testing.T) {
	f, mem := renamed(t), memory{}
	for i, tt := range []struct {
		wait time.Duration
		held bool
	}{{0, true}, {github.HoldBound - time.Second, true}, {time.Second, false}, {time.Minute, false}} {
		f.Advance(tt.wait)
		var got checked
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		if (got.held != nil) != tt.held || slices.Contains(got.labels, "priority:p0") != tt.held {
			t.Errorf("pass %d: held %q, labels %q; want held back %v, and priority:p0 returned only if so", i, got.held, got.labels, tt.held)
		}
	}
	if w := writesOf(f); len(w) != 0 {
		t.Errorf("writes = %q, want none", w)
	}
}

// The hold is timed per issue: an issue first found with an unexplained
// label is held back even while another, found so earlier, isn't any more.
func TestCheckLabelsHoldPerIssue(t *testing.T) {
	f, mem := renamed(t), memory{}
	f.AddIssue("owner", "Other", "body")
	if err := checkPass(t, mem, nil)(f); err != nil {
		t.Fatal(err)
	}
	f.Advance(time.Minute)
	f.LagEvents(2 * time.Second)
	f.AddLabel(2, appBot, "priority:p1")
	var one, two checked
	if err := checkPassOn(t, 1, mem, &one)(f); err != nil {
		t.Fatal(err)
	}
	if err := checkPassOn(t, 2, mem, &two)(f); err != nil {
		t.Fatal(err)
	}
	if one.held != nil || !slices.Equal(two.held, []string{"priority:p1"}) {
		t.Errorf("held %q on #1 and %q on #2; want #1 past its hold, and #2 held back for its late event", one.held, two.held)
	}
}

// A label renamed to a new name every four minutes, so that no name stays
// for UnexplainedWait, holds the issue back only at first. It's never
// removed, but it's always left out.
func TestCheckLabelsRenamedAgainAndAgain(t *testing.T) {
	f, mem := renamed(t), memory{}
	cur, names := "priority:p0", []string{"priority:p1", "priority:p0"}
	for i := range 30 {
		if i%4 == 3 {
			next := names[(i/4)%2]
			f.RenameLabel(cur, next)
			cur = next
		}
		var got checked
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		if i > 0 && (got.held != nil || slices.Contains(got.labels, cur)) {
			t.Errorf("minute %d: held %q, labels %q; want %s left out, and nothing held back", i, got.held, got.labels, cur)
		}
		f.Advance(time.Minute)
	}
	if w := writesOf(f); len(w) != 0 {
		t.Errorf("writes = %q, want none", w)
	}
}

// A label's removal starts again when its events change, so the owner's
// removing it and adding it back, with the add's event late, isn't removed.
func TestCheckLabelsOwnerAddsBackInLag(t *testing.T) {
	f, mem := renamed(t), memory{}
	if err := checkPass(t, mem, nil)(f); err != nil {
		t.Fatal(err)
	}
	f.Advance(github.UnexplainedWait)
	f.RemoveLabel(1, "owner", "priority:p0")
	f.LagEvents(5 * time.Second)
	f.AddLabel(1, "owner", "priority:p0")
	if err := checkPass(t, mem, nil)(f); err != nil {
		t.Fatal(err)
	}
	f.Advance(5 * time.Second)
	var got checked
	if err := checkPass(t, mem, &got)(f); err != nil {
		t.Fatal(err)
	}
	if w := writesOf(f); len(w) != 0 || !slices.Contains(got.labels, "priority:p0") {
		t.Errorf("writes %q, labels %q; want the owner's priority:p0 kept", w, got.labels)
	}
}

// A stage label that names the latest transition marker's stage is the App's
// own, explained by the marker: its late event holds nothing back, and it's
// never left out or removed.
func TestCheckLabelsMarkerStageExplained(t *testing.T) {
	f, mem := githubtest.New(t, "o/r", "issuedog"), memory{}
	f.AddIssue("owner", "Title", "body")
	f.AddComment(1, appBot, "<!-- issuedog:transition from=intake to=ready run=a -->")
	f.LagEvents(time.Hour)
	f.AddLabel(1, appBot, "stage:ready")
	for _, wait := range []time.Duration{0, time.Minute, github.UnexplainedWait} {
		f.Advance(wait)
		var got checked
		if err := checkPass(t, mem, &got)(f); err != nil {
			t.Fatal(err)
		}
		if got.held != nil || !slices.Equal(got.labels, []string{"stage:ready"}) {
			t.Errorf("%v on: held %q, labels %q; want stage:ready kept, nothing held back", wait, got.held, got.labels)
		}
	}
	if w := writesOf(f); len(w) != 0 {
		t.Errorf("writes = %q, want none", w)
	}
}

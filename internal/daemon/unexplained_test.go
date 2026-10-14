package daemon_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// watchWork makes d's Work note, for each pass, the labels it got issue 1
// with, or nil when it didn't get issue 1.
func watchWork(d *daemon.Daemon) *[][]string {
	var seen [][]string
	d.Work = func(_ context.Context, ready []daemon.Ready) error {
		var labels []string
		for _, r := range ready {
			if r.S.Number == 1 {
				labels = append([]string{}, r.S.Labels...)
			}
		}
		seen = append(seen, labels)
		return nil
	}
	return &seen
}

// A label a rename put on is removed once it has stayed unexplained for
// github.UnexplainedWait, however busy the issue is meanwhile. The issue
// is held back for its first pass only, as the passes are further apart
// than github.HoldBound, and after that Work gets it with the label left
// out: Work never gets it with the label.
func TestIterateRemovesUnexplainedLabel(t *testing.T) {
	every := func(n int, first, gap time.Duration) []time.Duration {
		waits := []time.Duration{first}
		for range n - 1 {
			waits = append(waits, gap)
		}
		return waits
	}
	tests := []struct {
		name   string
		setup  func(f *githubtest.Fake)
		before func(f *githubtest.Fake) // before each pass
		waits  []time.Duration          // before each pass
	}{
		{name: "after one untrusted comment",
			setup: func(f *githubtest.Fake) {
				f.AddComment(1, "mallory", "just a comment")
				f.RenameLabel("foo", "priority:p0")
			},
			waits: every(5, 10*time.Second, 2*time.Minute)},
		{name: "with untrusted comments every 50 seconds",
			setup:  func(f *githubtest.Fake) { f.RenameLabel("foo", "priority:p0") },
			before: func(f *githubtest.Fake) { f.AddComment(1, "mallory", "spam") },
			waits:  every(9, 50*time.Second, 50*time.Second)},
		{name: "on a quiet issue the owner then comments on",
			setup: func(f *githubtest.Fake) {
				f.RenameLabel("foo", "priority:p0")
				f.Advance(10 * time.Minute)
				f.AddComment(1, "owner", "an unrelated comment")
			},
			waits: every(5, 20*time.Second, 2*time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "ready", "do it", "priority:p2")
			f.Advance(time.Hour)
			f.AddLabel(1, "mallory", "foo") // unguarded, so it stands
			f.Advance(time.Hour)
			d := newDaemon(t, f, newOrigin(t), nil)
			seen := watchWork(d)
			iterate(t, d) // so issue 1 is known, and quiet
			tt.setup(f)
			held := -1 // the first pass that held issue 1 back
			var since time.Duration
			for i, wait := range tt.waits {
				if tt.before != nil {
					tt.before(f)
				}
				f.Advance(wait)
				iterate(t, d)
				got := (*seen)[i+1]
				on := slices.Contains(f.State().Issues[0].Labels, "priority:p0")
				if held < 0 && got == nil {
					held = i
				}
				if held >= 0 && i > held {
					since += wait
				}
				switch {
				case slices.Contains(got, "priority:p0"):
					t.Errorf("pass %d: Work got issue 1 with priority:p0: %q", i, got)
				case held >= 0 && i > held && got == nil:
					// Every gap is longer than the hold, so it's held back once.
					t.Errorf("pass %d: Work didn't get issue 1, held back past github.HoldBound", i)
				case held >= 0 && since < github.UnexplainedWait-10*time.Second && !on:
					t.Errorf("pass %d: priority:p0 removed %v after it was first held back, want %v", i, since, github.UnexplainedWait)
				}
			}
			if held < 0 || slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
				t.Fatalf("held back from pass %d; labels at the end %q, want priority:p0 held back, then removed", held, f.State().Issues[0].Labels)
			}
			if last := (*seen)[len(*seen)-1]; last == nil {
				t.Errorf("Work didn't get issue 1 after the removal")
			}
			reverts := 0
			for _, c := range f.State().Issues[0].Comments {
				if c.Author == app && strings.Contains(c.Body, "issuedog:revert key=priority%3Ap0@0") {
					reverts++
				}
			}
			if reverts != 1 {
				t.Errorf("%d revert comments, want 1", reverts)
			}
		})
	}
}

// An issue with a label left out is read again each pass, though nothing
// changes its updated_at, so the label is removed in time.
func TestIterateRereadsLeftOutLabels(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "do it", "priority:p2")
	f.AddLabel(1, "mallory", "foo")
	fileAt(f, "ready", "do it too")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameLabel("foo", "priority:p0")
	f.AddComment(1, "mallory", "poke")
	f.AddComment(2, "owner", "the newest, which every poll lists again")
	for range 7 {
		f.Advance(time.Minute)
		iterate(t, d)
	}
	if slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
		t.Errorf("labels %q after 7 minutes; want priority:p0 removed", f.State().Issues[0].Labels)
	}
}

// A new issue whose label events are a second or two late is held back for
// one pass, then is clean: nothing is removed, and nothing posted.
func TestIterateLateLabelEvents(t *testing.T) {
	for name, file := range map[string]func(f *githubtest.Fake){
		"filed by the App": func(f *githubtest.Fake) { fileAt(f, "ready", "do it", "priority:p2") },
		"filed by the owner, with labels": func(f *githubtest.Fake) {
			f.AddIssue("owner", "Issue", "do it", "type:task", "priority:p3", "scope:github")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			d := newDaemon(t, f, newOrigin(t), nil)
			seen := watchWork(d)
			iterate(t, d)
			f.LagEvents(2 * time.Second)
			file(f)
			labels := f.State().Issues[0].Labels
			comments := len(f.State().Issues[0].Comments)
			iterate(t, d)
			if got := (*seen)[1]; got != nil {
				t.Errorf("Work got issue 1 before its label events: %q", got)
			}
			f.Advance(7 * time.Second) // loop.idle_sleep
			iterate(t, d)
			if got := (*seen)[2]; !slices.Equal(got, labels) {
				t.Errorf("Work got issue 1 with %q once its events are written, want %q", got, labels)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes = %+v, want none", w)
			}
			if n := len(f.State().Issues[0].Comments); n != comments {
				t.Errorf("%d comments, want %d", n, comments)
			}
		})
	}
}

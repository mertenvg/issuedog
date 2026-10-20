package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #246: a repo whose config switches to types: labels while an
// issue's type waits, held back or left out, forgets it, so the issue isn't
// read on every pass once the type would have fallen due.

// The configs before and after the switch.
const (
	typeWaitIssueTypes = "types: issue-types\nnotify: [owner]\nloop:\n  idle_sleep: 7s\n"
	typeWaitLabels     = "types: labels\nnotify: [owner]\nloop:\n  idle_sleep: 7s\n"
)

// typeWaitWorked is the type of issue 1 that Work last got, and whether
// it had a type left out, or "none" if Work didn't get it.
type typeWaitWorked struct {
	typ     string
	leftOut bool
}

// typeWaitDaemon is issueTypeRevertDaemon's daemon on f, under the config
// cfg in o, with a Work that records issue 1 in worked.
func typeWaitDaemon(t *testing.T, f *githubtest.Fake, o *origin, cfg string, worked *typeWaitWorked) *issueTypeRevertLoop {
	t.Helper()
	o.commit(config.Path, cfg)
	d := &issueTypeRevertLoop{f: f, d: digestRunDaemon(t, f, o, nil, nil)}
	d.d.Work = func(_ context.Context, ready []daemon.Ready) error {
		*worked = typeWaitWorked{typ: "none"}
		for _, r := range ready {
			if r.S.Number == 1 {
				*worked = typeWaitWorked{typ: r.S.Type, leftOut: r.S.TypeLeftOut != nil}
			}
		}
		return nil
	}
	iterate(t, d.d)
	return d
}

// mallory's Bug, whose event never lands, is held back, then left out for
// the owner's Task, which stands. Then the config switches to types:
// labels, while the daemon runs or across a restart. Either way issue 1 is
// read at most once more, for its own type, Bug, which Work then gets, as
// a repo that uses labels reads it, and not again on any pass after, well
// past when its type would have fallen due, with nothing written. Bug fits
// ready, so it gets no type error.
func TestTypeWaitClear(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{"while it runs", false},
		{"across a restart", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := issueTypeRevertReady(nil)(t)
			o := newOrigin(t)
			var worked typeWaitWorked
			d := typeWaitDaemon(t, f, o, typeWaitIssueTypes, &worked)
			f.LagEvents(time.Hour)
			f.SetTypeBy(1, "mallory", "Bug")
			for _, wait := range []time.Duration{0, github.HoldBound + time.Second} {
				f.Advance(wait)
				if w := d.pass(t); len(w) != 0 {
					t.Fatalf("waiting, %v on: writes %q; want none", wait, w)
				}
			}
			if want := (typeWaitWorked{typ: "task", leftOut: true}); worked != want {
				t.Fatalf("left out: Work got %+v; want %+v", worked, want)
			}
			before := issueTypeWaitReads(f)
			if tc.restart {
				d = typeWaitDaemon(t, f, newOrigin(t), typeWaitLabels, &worked)
			} else {
				o.commit(config.Path, typeWaitLabels)
				if w := d.pass(t); len(w) != 0 {
					t.Errorf("the switch: writes %q; want none", w)
				}
			}
			// One read, of the issue and its timeline: the switch's, or the
			// restart's first.
			if n := issueTypeWaitReads(f) - before; n != 2 {
				t.Errorf("issue 1 read %d times at the switch; want its GET and its timeline's, once", n)
			}
			if want := (typeWaitWorked{typ: "bug"}); worked != want {
				t.Errorf("after the switch: Work got %+v; want %+v", worked, want)
			}
			before = issueTypeWaitReads(f)
			for range 6 {
				f.Advance(time.Minute)
				if w := d.pass(t); len(w) != 0 {
					t.Errorf("after the switch: writes %q; want none", w)
				}
			}
			if n := issueTypeWaitReads(f) - before; n != 0 {
				t.Errorf("issue 1 read %d times in the 6 minutes after the switch; want none", n)
			}
			if want := (typeWaitWorked{typ: "bug"}); worked != want {
				t.Errorf("6 minutes on: Work got %+v; want %+v", worked, want)
			}
			if got := f.State().Issues[0].Type; got != "Bug" {
				t.Errorf("type %q; want mallory's Bug, unguarded", got)
			}
		})
	}
}

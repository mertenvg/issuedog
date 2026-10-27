package daemon_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An untrusted close of an issue below an epic planned into stories never
// closes the epic before that close is judged (SPEC §7.3): neither for an
// issue the owner added below it by hand, which its record doesn't name,
// nor for one held below it by an untrusted removal of its link.

// epicCloseBelowExtra is the issue the owner adds below the epic by hand,
// in the pipeline, as epicCloseBelowStart files it.
const epicCloseBelowExtra = 4

// epicCloseBelowPasses runs n Iterates of d, each of which must succeed.
func epicCloseBelowPasses(tb testing.TB, d *daemon.Daemon, n int) {
	tb.Helper()
	for range n {
		if err := d.Iterate(context.Background()); err != nil {
			tb.Fatal(err)
		}
	}
}

// epicCloseBelowStart files the epic planned into twoStories and runs a
// daemon on it till its stories, #2 and #3, are filed, then has the owner
// add #4 below it by hand, at stage:intake, and runs passes till #4 is in
// the pipeline.
func epicCloseBelowStart(tb testing.TB) (*githubtest.Fake, *daemon.Daemon) {
	tb.Helper()
	f, o := newFake(tb), newOrigin(tb)
	doneParent(tb, f, "epic", twoStories)
	d := newDaemon(tb, f, o, nil)
	epicCloseBelowPasses(tb, d, 1)
	if n := f.AddIssue("owner", "One more story", "Added by hand.", "stage:intake"); n != epicCloseBelowExtra {
		tb.Fatalf("the issue added by hand is #%d; want #%d", n, epicCloseBelowExtra)
	}
	f.SetParent(epicCloseBelowExtra, 1)
	epicCloseBelowPasses(tb, d, 2)
	return f, d
}

// epicCloseBelowStep is a step of epicCloseBelowFake.
type epicCloseBelowStep func(testing.TB, *githubtest.Fake, *daemon.Daemon)

// epicCloseBelowFake is epicCloseBelowStart, then before, with the
// daemon, then mallory, a write collaborator, closes #4, its event late,
// then after, if set.
func epicCloseBelowFake(tb testing.TB, before, after epicCloseBelowStep) (*githubtest.Fake, *daemon.Daemon) {
	tb.Helper()
	f, d := epicCloseBelowStart(tb)
	before(tb, f, d)
	if f.State().Issues[0].Closed {
		tb.Fatal("before mallory's close, the epic is closed; want it open")
	}
	f.LagEvents(5 * time.Second)
	f.CloseBy(epicCloseBelowExtra, "mallory")
	f.LagEvents(0)
	if after != nil {
		after(tb, f, d)
	}
	return f, d
}

// epicCloseBelowCases are how #4 is below the epic when mallory closes it.
var epicCloseBelowCases = []struct {
	name          string
	before, after epicCloseBelowStep
}{
	// digestDoneStands, by the parent #4's snapshot named
	// (closingBelowSet).
	{"added below it", func(tb testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
		f.CloseBy(2, "owner")
		f.CloseBy(3, "owner")
		epicCloseBelowPasses(tb, d, 2)
	}, nil},
	// epicCloseHolds, once epicCloseOne found it.
	{"its link taken by mallory", func(tb testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
		f.CloseBy(2, "owner")
		f.CloseBy(3, "owner")
		epicCloseBelowPasses(tb, d, 1)
		f.UnlinkBy(epicCloseBelowExtra, "mallory")
		epicCloseBelowPasses(tb, d, 2)
	}, nil},
	// epicCloseUnlinked, on the read that would close the epic.
	{"its link taken before the stories closed", func(tb testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
		f.UnlinkBy(epicCloseBelowExtra, "mallory")
		epicCloseBelowPasses(tb, d, 1)
		f.CloseBy(2, "owner")
		epicCloseBelowPasses(tb, d, 1)
	}, func(_ testing.TB, f *githubtest.Fake, _ *daemon.Daemon) { f.CloseBy(3, "owner") }},
}

// epicCloseBelowPass is the pass after mallory's close, while its event is
// still late, then, once it's written, the passes till one writes nothing
// (closingDropQuiet), which judge her close and revert it.
func epicCloseBelowPass(f *githubtest.Fake, d *daemon.Daemon) error {
	if err := d.Iterate(context.Background()); err != nil {
		return err
	}
	f.Advance(10 * time.Second)
	return closingDropQuiet(f, d)
}

// Mallory's close of #4, its event late, doesn't close the epic, whatever
// write a crash comes at, while it waits to be judged and once it's
// reverted: the epic stays open with no write, #4 is open again, and a
// pass where nothing changed then costs only 304s. The owner's close of
// #4 then closes the epic, as completed, with one comment.
func TestEpicCloseBelowUntrusted(t *testing.T) {
	t.Parallel()
	for _, tc := range epicCloseBelowCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			daemons := map[*githubtest.Fake]*daemon.Daemon{}
			setup := func(tb testing.TB) *githubtest.Fake {
				f, d := epicCloseBelowFake(tb, tc.before, tc.after)
				daemons[f] = d
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, func(f *githubtest.Fake) error { return epicCloseBelowPass(f, daemons[f]) })
			f := setup(t)
			d := daemons[f]
			comments := len(f.State().Issues[0].Comments)
			if err := epicCloseBelowPass(f, d); err != nil {
				t.Fatal(err)
			}
			epicCloseOpen(t, f, 0, "once mallory's close is judged")
			if f.State().Issues[epicCloseBelowExtra-1].Closed {
				t.Fatalf("#%d is closed; want mallory's close reverted", epicCloseBelowExtra)
			}
			// #4 changed, reopened and held, so the epic held by its
			// link's removal was read again and judged, and that judging
			// left it to be read once more, on this pass.
			iterate(t, d)
			doneUnchanged(t, f, d)
			f.CloseBy(epicCloseBelowExtra, "owner")
			iterate(t, d)
			epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
		})
	}
}

// While mallory's close of #4 waits to be judged, its event late, the
// epic is read at most once, on the first pass that finds it held, and
// not on the passes after: what holds it is judged from what the poll
// listed and the snapshots held, with no request of the epic's.
func TestEpicCloseBelowLateNoRead(t *testing.T) {
	t.Parallel()
	for _, tc := range epicCloseBelowCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := epicCloseBelowFake(t, tc.before, tc.after)
			writes := len(f.Writes())
			iterate(t, d)
			iterate(t, d)
			from := len(f.Requests())
			for _, step := range []string{"a pass while the event is late", "the pass after"} {
				iterate(t, d)
				for _, r := range f.Requests()[from:] {
					if r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/") {
						t.Errorf("%s: requested %s %s; want no request of the epic's", step, r.Method, r.Path)
					}
				}
				from = len(f.Requests())
			}
			epicCloseOpen(t, f, writes, "while the event is late")
			if !f.State().Issues[epicCloseBelowExtra-1].Closed {
				t.Errorf("#%d is open; want mallory's close still to judge", epicCloseBelowExtra)
			}
		})
	}
}

// Mallory's close of #4 judged and reverted, but #4's read after its
// reopen failing, doesn't close the epic either: the pass fails, and #4,
// read again on the next, holds it open.
func TestEpicCloseBelowRereadFails(t *testing.T) {
	t.Parallel()
	for _, tc := range epicCloseBelowCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := epicCloseBelowFake(t, tc.before, func(tb testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
				if tc.after != nil {
					tc.after(tb, f, d)
				}
				f.Advance(10 * time.Second)
				f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPatch && r.Path == "/issues/4" }, func() {
					f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/4" }, http.StatusInternalServerError)
				})
			})
			from := len(f.Writes())
			epicCloseIterate(t, d, true, "the pass whose read of #4 fails")
			epicCloseOpen(t, f, from, "the pass whose read of #4 fails")
			for _, step := range []string{"the pass after", "the pass after that"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
		})
	}
}

// An issue below the epic that closes between the poll and its read is
// read as closed, though the poll hasn't listed it closed, so its close
// isn't judged yet: here mallory comments on #4, held below the epic once
// both stories closed, which has it read again, and closes it just before
// that read. The epic stays open, and once her close is judged it's
// reverted, and the epic stays open still.
func TestEpicCloseBelowClosedUnlisted(t *testing.T) {
	t.Parallel()
	f, d := epicCloseBelowStart(t)
	epicCloseBelowCases[0].before(t, f, d)
	from := len(f.Writes())
	f.AddComment(epicCloseBelowExtra, "mallory", "Done, I think.")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/4" }, func() {
		f.CloseBy(epicCloseBelowExtra, "mallory")
	})
	for _, step := range []string{"the pass that reads #4 closed", "the pass that judges her close", "the pass after"} {
		epicCloseIterate(t, d, false, step)
		epicCloseOpen(t, f, from, step)
	}
	if f.State().Issues[epicCloseBelowExtra-1].Closed {
		t.Errorf("#%d is closed; want mallory's close reverted", epicCloseBelowExtra)
	}
}

// The newest removal of an issue's link below the epic counts (SPEC
// §7.3): a trusted one lets the issue go, and the epic closes; an
// untrusted one holds it. Here #4, open, is taken from below the epic,
// linked below it again, and taken again, in either order, once both
// stories closed for the second removal. The owner's later removal lets
// it go though mallory's held the epic: #4 changes, so the epic is read
// again, and closes. Mallory's later removal holds it, though the owner's
// came first, and a change to #4 that's no removal, her comment, holds it
// still.
func TestEpicCloseUnlinkedNewest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		first, second string
		closes        bool
	}{
		{"mallory", "owner", true},
		{"owner", "mallory", false},
	} {
		t.Run(tc.first+" then "+tc.second, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), newOrigin(t)
			doneParent(t, f, "epic", twoStories)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			extra := f.AddIssue("owner", "One more thing", "Added by hand.")
			f.SetParent(extra, 1)
			iterate(t, d)
			f.UnlinkBy(extra, tc.first)
			iterate(t, d)
			f.CloseBy(2, "owner")
			f.CloseBy(3, "owner")
			if tc.first == "mallory" {
				from := len(f.Writes())
				epicCloseIterate(t, d, false, "the pass the stories closed in")
				epicCloseOpen(t, f, from, "the pass the stories closed in")
			}
			comments := len(f.State().Issues[0].Comments)
			f.SetParent(extra, 1)
			f.UnlinkBy(extra, tc.second)
			if tc.closes {
				iterate(t, d)
				epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
				return
			}
			from := len(f.Writes())
			for _, step := range []string{"the pass after mallory's removal", "the pass after"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
			f.AddComment(extra, "mallory", "Still mine.")
			for _, step := range []string{"the pass after mallory's comment", "the pass after that"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
		})
	}
}

// A removal of the link of an issue in another repo counts for nothing,
// even when an open issue here has its number: here mallory takes
// o/other#4 from below the epic, and #4 here is open, below nothing. Once
// both stories close, the epic closes.
func TestEpicCloseUnlinkedOtherRepo(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	doneParent(t, f, "epic", twoStories)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if n := f.AddIssue("owner", "Unrelated", "Below nothing."); n != 4 {
		t.Fatalf("the unrelated issue is #%d; want #4", n)
	}
	f.LinkSubIssue(1, "o/other", 4)
	iterate(t, d)
	f.UnlinkLinkedBy(1, "o/other", 4, "mallory")
	comments := len(f.State().Issues[0].Comments)
	f.CloseBy(2, "owner")
	f.CloseBy(3, "owner")
	iterate(t, d)
	epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
}

// A removal whose event is late of a link below another issue holds
// nothing on the epic: here #5 is below story #2, and once the owner has
// closed both stories and #4, and taken #5 from below #2, its event never
// written, the epic closes on that pass.
func TestEpicCloseBelowLateUnlinkElsewhere(t *testing.T) {
	t.Parallel()
	f, d := epicCloseBelowStart(t)
	if n := f.AddIssue("owner", "Below a story", "Added by hand."); n != 5 {
		t.Fatalf("the issue below #2 is #%d; want #5", n)
	}
	f.SetParent(5, 2)
	iterate(t, d)
	comments := len(f.State().Issues[0].Comments)
	f.CloseBy(2, "owner")
	f.CloseBy(3, "owner")
	f.CloseBy(epicCloseBelowExtra, "owner")
	f.LagEvents(time.Hour)
	f.UnlinkBy(5, "owner")
	f.LagEvents(0)
	iterate(t, d)
	epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
}

// A restart while mallory's close of #4, added below the epic by hand,
// waits to be judged, its event late, doesn't let the epic close: the
// fresh daemon holds no snapshot of #4, but the poll lists #4's parent,
// so it holds the epic as the daemon before it did, with no request of
// its own (SPEC §7.3), whatever write a crash comes at. Every pass of
// Replay's is a restart, as each is a fresh daemon on the fake. Once her
// close is judged, it's reverted, the epic stays open with no write, and
// a pass where nothing changed then costs only 304s. The owner's close of
// #4 then closes the epic, as completed, with one comment.
func TestEpicCloseBelowRestart(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := epicCloseBelowFake(tb, epicCloseBelowCases[0].before, nil)
		f.ForgetWrites()
		return f
	}
	restarted := func(f *githubtest.Fake) *daemon.Daemon { return newDaemon(t, f, newOrigin(t), nil) }
	githubtest.Replay(t, setup, func(f *githubtest.Fake) error { return epicCloseBelowPass(f, restarted(f)) })
	f := setup(t)
	d := restarted(f)
	comments := len(f.State().Issues[0].Comments)
	epicCloseIterate(t, d, false, "the restart's first pass, while the event is late")
	epicCloseOpen(t, f, 0, "the restart's first pass, while the event is late")
	if !f.State().Issues[epicCloseBelowExtra-1].Closed {
		t.Fatalf("#%d is open; want mallory's close still to judge", epicCloseBelowExtra)
	}
	f.Advance(10 * time.Second)
	if err := closingDropQuiet(f, d); err != nil {
		t.Fatal(err)
	}
	epicCloseOpen(t, f, 0, "once mallory's close is judged")
	if f.State().Issues[epicCloseBelowExtra-1].Closed {
		t.Fatalf("#%d is closed; want mallory's close reverted", epicCloseBelowExtra)
	}
	iterate(t, d)
	doneUnchanged(t, f, d)
	f.CloseBy(epicCloseBelowExtra, "owner")
	iterate(t, d)
	epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
}

// A removal of #4's link below the epic, once both stories closed, a
// minute after #4 last changed, so its snapshot is older than
// github.HoldBound, whose event GitHub writes late, holds the epic till
// it's written, then it's
// judged as any removal is (SPEC §7.3): mallory's holds the epic on,
// with no request of the epic's on the passes after, and the owner's lets
// it close. So it is when an older removal of #4's link, the owner's, is
// on the epic's timeline already, from before #4 was linked below it
// again: it isn't the removal the snapshots show. One whose event isn't
// written within github.HoldBound of the removal, past #4's updated_at,
// holds it no longer, so the epic closes; and one whose issue closed
// since, its close accepted, holds nothing, as that issue is settled.
func TestEpicCloseBelowLateUnlink(t *testing.T) {
	t.Parallel()
	relinked := func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
		f.UnlinkBy(epicCloseBelowExtra, "owner")
		iterate(t, d)
		f.SetParent(epicCloseBelowExtra, 1)
		f.RenameIssue(epicCloseBelowExtra, "owner", "One more story, again") // as a link moves its updated_at
		iterate(t, d)
	}
	for _, tc := range []struct {
		name, by string
		before   func(*testing.T, *githubtest.Fake, *daemon.Daemon)
		lag      time.Duration
		closed   bool // the owner closes #4 after the first pass
		closes   bool
	}{
		{"mallory's", "mallory", nil, 5 * time.Second, false, false},
		{"the owner's", "owner", nil, 5 * time.Second, false, true},
		{"mallory's, after an older one of the owner's", "mallory", relinked, 5 * time.Second, false, false},
		{"one never written", "owner", nil, time.Hour, false, true},
		{"one never written, its issue closed", "mallory", nil, time.Hour, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := epicCloseBelowStart(t)
			if tc.before != nil {
				tc.before(t, f, d)
			}
			f.CloseBy(2, "owner")
			f.CloseBy(3, "owner")
			iterate(t, d)
			f.Advance(time.Minute) // #4 last changed a minute before its removal
			from, comments := len(f.Writes()), len(f.State().Issues[0].Comments)
			f.LagEvents(tc.lag)
			f.UnlinkBy(epicCloseBelowExtra, tc.by)
			f.LagEvents(0)
			epicCloseIterate(t, d, false, "the pass that reads the removal, its event late")
			epicCloseOpen(t, f, from, "the pass that reads the removal, its event late")
			if tc.closed {
				f.CloseBy(epicCloseBelowExtra, "owner")
				iterate(t, d)
				epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
				return
			}
			f.Advance(10 * time.Second)
			if tc.lag > 10*time.Second {
				epicCloseIterate(t, d, false, "a pass still within the bound")
				epicCloseOpen(t, f, from, "a pass still within the bound")
				f.Advance(github.HoldBound)
			}
			if tc.closes {
				iterate(t, d)
				epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
				return
			}
			epicCloseIterate(t, d, false, "the pass that reads the event")
			epicCloseOpen(t, f, from, "the pass that reads the event")
			iterate(t, d)
			req := len(f.Requests())
			for _, step := range []string{"a pass where nothing changed", "the pass after"} {
				iterate(t, d)
				for _, r := range f.Requests()[req:] {
					if r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/") {
						t.Errorf("%s: requested %s %s; want no request of the epic's", step, r.Method, r.Path)
					}
				}
				req = len(f.Requests())
			}
			epicCloseOpen(t, f, from, "the passes after")
		})
	}
}

package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #182: an untrusted change of a GitHub issue type is
// reverted on a repo that uses issue types, as a type:* label's is, and
// gives no type error and no digest mention.

// issueTypeRevertWant is the App's comment on reverting the untrusted
// change keyed key: verb is "restored", with typ the type that stands, or
// "removed", with typ the type removed.
func issueTypeRevertWant(verb, typ string, key int64) string {
	return fmt.Sprintf("I %s the issue type `%s`: only trusted users can change the issue type, and mallory isn't one.\n\n"+
		"<!-- issuedog:revert key=%d -->", verb, typ, key)
}

// issueTypeRevertReady files issue 1, which the owner typed Task (event
// 9001), at ready, as the App moved it (event 9002, comment 1001), then
// runs more. A task fits ready; an epic doesn't.
func issueTypeRevertReady(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		f.AddIssue("owner", "Sync", "Fix the sync.")
		flagWant(t, f.SetTypeBy(1, "owner", "Task"), 9001)
		flagWant(t, f.AddLabel(1, app, "stage:ready"), 9002)
		flagWant(t, f.AddComment(1, app, "<!-- issuedog:transition from=intake to=ready run=rready -->"), 1001)
		if more != nil {
			more(t, f)
		}
		return f
	}
}

// issueTypeRevertCheck fails t unless issue 1 has type typ, no state
// error, and these comments after the transition.
func issueTypeRevertCheck(t *testing.T, f *githubtest.Fake, typ string, comments ...string) {
	t.Helper()
	want := append([]string{"<!-- issuedog:transition from=intake to=ready run=rready -->"}, comments...)
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want %q", got, want)
	}
	if got := f.State().Issues[0].Type; got != typ {
		t.Errorf("type %q; want %q", got, typ)
	}
	if errs := errorMarkers(f, 1); len(errs) != 0 {
		t.Errorf("state errors %q; want none", errs)
	}
}

// An untrusted change of the issue type, to one that doesn't fit the
// stage, is reverted under Replay: the comment, then the type set back.
// The pass's type check reads the type set back, so it reports no type
// error, and a pass after it writes nothing, taking the App's own write
// for the restore it is.
func TestIssueTypeRevertReplay(t *testing.T) {
	setup := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9003)
	})
	pass := typeErrPass(t, true)
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	errAckRun(t, f, pass)
	issueTypeRevertCheck(t, f, "Task", issueTypeRevertWant("restored", "Task", 9003))
	if w := f.Writes(); len(w) != 2 || w[1].Method != "PATCH" || w[1].Path != "/issues/1" {
		t.Errorf("writes %+v; want the comment and one PATCH of the issue", w)
	}
	typeErrQuiet(t, f, pass)
}

// An untrusted type on an issue with no type that stands is removed.
func TestIssueTypeRevertRemoves(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue("owner", "Sync", "Fix the sync.")
		f.AddLabel(1, app, "stage:ready")
		f.AddComment(1, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9002)
		return f
	}
	pass := typeErrPass(t, true)
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	errAckRun(t, f, pass)
	issueTypeRevertCheck(t, f, "", issueTypeRevertWant("removed", "Epic", 9002))
	typeErrQuiet(t, f, pass)
}

// After a trusted change, an untrusted one is reverted to the trusted
// type, not the one before it, and the trusted change keys the type.
func TestIssueTypeRevertTrustedStands(t *testing.T) {
	f := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, f.SetTypeBy(1, "owner", "Bug"), 9003)
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9004)
	})(t)
	pass := typeErrPass(t, true)
	errAckRun(t, f, pass)
	issueTypeRevertCheck(t, f, "Bug", issueTypeRevertWant("restored", "Bug", 9004))
	typeErrQuiet(t, f, pass)
	// The owner moves it to spec-review by hand, where a bug can't be: the
	// type error keys on the owner's change, 9003.
	f.RemoveLabel(1, "owner", "stage:ready")
	f.AddLabel(1, "owner", "stage:spec-review")
	errAckRun(t, f, pass)
	if errs := errorMarkers(f, 1); !slices.Equal(errs, []string{"<!-- issuedog:error kind=type key=9003 -->"}) {
		t.Errorf("state errors %q; want the type error keyed by the owner's change", errs)
	}
}

// A revert that fails is the pass's error, naming the change, and posts no
// type error. The next pass finds the change again, and only sets the type
// back, as the comment is posted already.
func TestIssueTypeRevertFails(t *testing.T) {
	f := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9003)
	})(t)
	f.FailWrite(2)
	pass := typeErrPass(t, true)
	err := pass(f)
	if err == nil || !strings.Contains(err.Error(), "revert mallory's change of the issue type on o/r#1") {
		t.Fatalf("pass = %v; want the failed revert", err)
	}
	issueTypeRevertCheck(t, f, "Epic", issueTypeRevertWant("restored", "Task", 9003))
	f.FailWrite(0)
	from := len(f.Writes())
	errAckRun(t, f, pass)
	if w := digestWrites(f, from); !slices.Equal(w, []string{"PATCH /issues/1"}) {
		t.Errorf("writes %q; want the type set back alone", w)
	}
	issueTypeRevertCheck(t, f, "Task", issueTypeRevertWant("restored", "Task", 9003))
	typeErrQuiet(t, f, pass)
}

// A revert GitHub drops, as for an App without push access, fails every
// pass with ErrIssueTypeRevertDropped, with one comment, and the untrusted
// type gives no type error.
func TestIssueTypeRevertDropped(t *testing.T) {
	f := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9003)
	})(t)
	f.DropTypeChanges()
	pass := typeErrPass(t, true)
	for range 2 {
		if err := pass(f); !errors.Is(err, github.ErrIssueTypeRevertDropped) {
			t.Fatalf("pass = %v; want ErrIssueTypeRevertDropped", err)
		}
	}
	issueTypeRevertCheck(t, f, "Epic", issueTypeRevertWant("restored", "Task", 9003))
}

// A repo that uses labels leaves issue types alone, as before #182.
func TestIssueTypeRevertLabelsRepo(t *testing.T) {
	f := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		f.SetTypeBy(1, "mallory", "Epic")
	})(t)
	typeErrQuiet(t, f, typeErrPass(t, false))
	if got := f.State().Issues[0].Type; got != "Epic" {
		t.Errorf("type %q; want mallory's Epic left alone", got)
	}
}

// On a repo that uses issue types, an issue with a type:* label and no
// issue type has no issue type to guard: its label's type isn't taken for
// an unexplained issue type, so nothing is held back or written.
func TestIssueTypeRevertLabelTypeOnly(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Sync", "Fix the sync.", "type:task")
	f.AddLabel(1, app, "stage:ready")
	f.AddComment(1, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	typeErrQuiet(t, f, typeErrPass(t, true))
}

// issueTypeRevertDaemon is a daemon of the whole loop, the digest
// included, on a repo that uses issue types, with owner as notify, after
// its first pass, which creates the digest as issue 2.
func issueTypeRevertDaemon(t *testing.T, f *githubtest.Fake) *issueTypeRevertLoop {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: issue-types\nnotify: [owner]\nloop:\n  idle_sleep: 7s\n")
	d := &issueTypeRevertLoop{f: f, d: digestRunDaemon(t, f, o, nil, nil)}
	iterate(t, d.d)
	if got := f.State().Issues[1].Title; got != "issuedog digest" {
		t.Fatalf("issue 2 is %q; want the digest", got)
	}
	return d
}

// issueTypeRevertLoop is issueTypeRevertDaemon's daemon and its fake.
type issueTypeRevertLoop struct {
	f *githubtest.Fake
	d *daemon.Daemon
}

// pass iterates d and returns the writes it made, as "METHOD /path".
func (d *issueTypeRevertLoop) pass(t *testing.T) []string {
	t.Helper()
	from := len(d.f.Writes())
	if err := d.d.Iterate(context.Background()); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	return digestWrites(d.f, from)
}

// #7's story verification: an untrusted user flips the issue type Task →
// Epic → Bug → Epic → Task, on a repo that uses issue types, with the loop
// running between flips. Each flip is reverted, with no type error and no
// mention in the digest, and a pass after the last writes nothing. Flips
// made all between two passes, back to the type that stands, write
// nothing at all. The owner's own flip to Epic, by contrast, is a type
// error, which the digest announces.
func TestIssueTypeRevertFlips(t *testing.T) {
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	var want []string
	for _, typ := range []string{"Epic", "Bug", "Epic", "Task"} {
		key := f.SetTypeBy(1, "mallory", typ)
		writes := d.pass(t)
		if typ != "Task" {
			want = append(want, issueTypeRevertWant("restored", "Task", key))
			if !slices.Equal(writes, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
				t.Errorf("flip to %s: writes %q; want the revert alone", typ, writes)
			}
		} else if len(writes) != 0 {
			t.Errorf("flip back to Task: writes %q; want none", writes)
		}
		issueTypeRevertCheck(t, f, "Task", want...)
		if got := digestRunNews(f, 2); got != news {
			t.Errorf("flip to %s: %d digest announcements; want %d", typ, got, news)
		}
	}
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("a pass after the flips wrote %q; want nothing", w)
	}
	for _, typ := range []string{"Epic", "Bug", "Epic", "Task"} {
		f.SetTypeBy(1, "mallory", typ)
	}
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("flips between two passes: writes %q; want none", w)
	}
	issueTypeRevertCheck(t, f, "Task", want...)
	key := f.SetTypeBy(1, "owner", "Epic")
	d.pass(t)
	if errs := errorMarkers(f, 1); !slices.Equal(errs, []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", key)}) {
		t.Errorf("after the owner's flip, state errors %q; want the type error", errs)
	}
	if got := digestRunNews(f, 2); got != news+1 {
		t.Errorf("after the owner's flip, %d digest announcements; want %d", got, news+1)
	}
}

// A revert GitHub drops fails Iterate, every pass, and the untrusted type
// still gives no type error and no mention.
func TestIssueTypeRevertDroppedLoop(t *testing.T) {
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	f.DropTypeChanges()
	key := f.SetTypeBy(1, "mallory", "Epic")
	for range 2 {
		if err := d.d.Iterate(context.Background()); !errors.Is(err, github.ErrIssueTypeRevertDropped) {
			t.Fatalf("Iterate = %v; want ErrIssueTypeRevertDropped", err)
		}
	}
	issueTypeRevertCheck(t, f, "Epic", issueTypeRevertWant("restored", "Task", key))
	if got := digestRunNews(f, 2); got != news {
		t.Errorf("%d digest announcements; want %d", got, news)
	}
}

// GitHub writes issue-type events a few seconds late (#182's verify). An
// untrusted change whose event isn't written yet holds the issue back,
// with no type error and no mention, and is reverted once its event lands,
// keyed by it. A pass after that writes nothing, though the App's own
// event is late too.
func TestIssueTypeRevertLateEvent(t *testing.T) {
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	f.LagEvents(2 * time.Second)
	key := f.SetTypeBy(1, "mallory", "Epic")
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("before the event lands: writes %q; want none", w)
	}
	issueTypeRevertCheck(t, f, "Epic")
	f.Advance(3 * time.Second)
	if w := d.pass(t); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Errorf("once the event lands: writes %q; want the revert alone", w)
	}
	issueTypeRevertCheck(t, f, "Task", issueTypeRevertWant("restored", "Task", key))
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("before the App's event lands: writes %q; want none", w)
	}
	f.Advance(3 * time.Second)
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("once the App's event lands: writes %q; want none", w)
	}
	if got := digestRunNews(f, 2); got != news {
		t.Errorf("%d digest announcements; want %d", got, news)
	}
}

// An untrusted change whose event never lands is held back for
// github.HoldBound, then reverted all the same, keyed by the type and the
// newest event, with no type error and no mention.
func TestIssueTypeRevertEventNeverLands(t *testing.T) {
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	f.LagEvents(time.Hour)
	f.SetTypeBy(1, "mallory", "Epic")
	for _, wait := range []time.Duration{0, github.HoldBound / 2} {
		f.Advance(wait)
		if w := d.pass(t); len(w) != 0 {
			t.Errorf("held back, after %v: writes %q; want none", wait, w)
		}
	}
	f.Advance(github.HoldBound/2 + time.Second) // past the bound, by GitHub's clock
	if w := d.pass(t); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Errorf("at the bound: writes %q; want the revert alone", w)
	}
	issueTypeRevertCheck(t, f, "Task", "I restored the issue type `Task`: no change by a trusted user or the App explains `epic`, "+
		"and only trusted users can change the issue type.\n\n<!-- issuedog:revert key=issue-type:epic@9001 -->")
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("after the revert: writes %q; want none", w)
	}
	if got := digestRunNews(f, 2); got != news {
		t.Errorf("%d digest announcements; want %d", got, news)
	}
}

// A trusted change whose event isn't written yet holds the issue back too,
// so its type error is posted once, keyed by its own event once that
// lands, and never under the change before it, with one mention.
func TestIssueTypeRevertLateTrusted(t *testing.T) {
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	f.LagEvents(2 * time.Second)
	key := f.SetTypeBy(1, "owner", "Epic")
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("before the event lands: writes %q; want none", w)
	}
	f.Advance(3 * time.Second)
	d.pass(t)
	d.pass(t)
	if errs := errorMarkers(f, 1); !slices.Equal(errs, []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", key)}) {
		t.Errorf("state errors %q; want the one type error, keyed %d", errs, key)
	}
	if got := f.State().Issues[0].Type; got != "Epic" {
		t.Errorf("type %q; want the owner's Epic", got)
	}
	if got := digestRunNews(f, 2); got != news+1 {
		t.Errorf("%d digest announcements; want %d", got, news+1)
	}
}

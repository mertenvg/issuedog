package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The poll after the digest is dropped lists every issue again, but it
// isn't a start's first, so an untrusted close or closed stage it lists is
// judged as on any later poll (SPEC §8.1, §9.4, #424). Only a real start
// counts an issue with one closed stage as judged.

// closingDropFake is digestPinned, the digest issue 1, with owner's task 2
// at pr-open, as the App set it.
func closingDropFake(t testing.TB) *githubtest.Fake {
	f := digestPinned(t)
	fileAt(f, "pr-open", "do it")
	f.SetType(2, "task")
	return f
}

// closingDropSettle is how many Iterates closingDropQuiet may take.
const closingDropSettle = 5

// closingDropQuiet iterates d till a pass writes nothing, within
// closingDropSettle Iterates. An Iterate that fails ends it, with its
// error.
func closingDropQuiet(f *githubtest.Fake, d *daemon.Daemon) error {
	for range closingDropSettle {
		from := len(f.Writes())
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		if len(f.Writes()) == from {
			return nil
		}
	}
	return fmt.Errorf("the daemon still wrote on its Iterate %d", closingDropSettle)
}

// closingDrop has d's next pass find the digest gone: a new error item on
// issue 3 makes it announce the item, which answers 404 once, so d drops
// the digest (digestDrop), and its next poll lists every issue again.
func closingDrop(t testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	digestError(f, "A", "5")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusNotFound)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the digest is gone") {
		t.Fatalf("Iterate = %v; want the digest gone", err)
	}
}

// closingDropCheck runs change on closingDropFake, after before, once a
// daemon's start and the pass that drops the digest (closingDrop) are
// done, then that daemon's passes till it writes nothing, under Replay.
// Each crash is a failed write that the same daemon's next pass tries
// again: a restart would count an issue with one closed stage as judged
// (SPEC §16's gap). Then, on a fresh fake, it checks issue 2 with want,
// and that an unchanged pass after sends only conditional GETs answered
// 304, and writes nothing.
func closingDropCheck(t *testing.T, before, change func(f *githubtest.Fake), want func(t *testing.T, f *githubtest.Fake)) {
	t.Helper()
	o := digestNotifyOrigin(t)
	daemons := map[*githubtest.Fake]*daemon.Daemon{}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := closingDropFake(tb)
		if before != nil {
			before(f)
		}
		d := digestDaemon(tb, f, o, nil)
		if err := closingDropQuiet(f, d); err != nil {
			tb.Fatal(err)
		}
		closingDrop(tb, f, d)
		change(f)
		daemons[f] = d
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, func(f *githubtest.Fake) error { return closingDropQuiet(f, daemons[f]) })
	f := setup(t)
	d := daemons[f]
	if err := closingDropQuiet(f, d); err != nil {
		t.Fatal(err)
	}
	want(t, f)
	writes, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("an unchanged pass wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[reqs:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// eve, untrusted, closes task 2 and swaps its stage label for
// stage:completed just after the pass that drops the digest: the poll
// that lists every issue again judges the close, which is reverted, and
// the label rules put stage:pr-open back. Taken for a start's first poll,
// it would count the close as judged, and it would stand.
func TestClosingDropUntrustedClose(t *testing.T) {
	t.Parallel()
	var event int64
	closingDropCheck(t, nil, func(f *githubtest.Fake) {
		event = f.CloseBy(2, "eve")
		f.RemoveLabel(2, "eve", "stage:pr-open")
		f.AddLabel(2, "eve", "stage:completed")
	}, func(t *testing.T, f *githubtest.Fake) {
		is := f.State().Issues[1]
		if is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) {
			t.Errorf("closed %t, labels %q; want eve's close reverted, and stage:pr-open back", is.Closed, is.Labels)
		}
		if got := closingComments(f, 2, 1); !slices.Contains(got, closingReverted("`eve`", event)) {
			t.Errorf("comments %q; want the close's revert, keyed %d", got, event)
		}
	})
}

// eve, untrusted, swaps the closed stage of task 2, whose close owner made
// and this daemon judged, for the other, just after the pass that drops
// the digest: the poll that lists every issue again judges the close again,
// and stage:completed is set back, with no comment. Taken for a start's
// first poll, it would count eve's stage:discarded as judged, and it would
// stand.
func TestClosingDropUntrustedSwap(t *testing.T) {
	t.Parallel()
	closingDropCheck(t, func(f *githubtest.Fake) { f.CloseBy(2, "owner") }, func(f *githubtest.Fake) {
		f.RemoveLabel(2, "eve", "stage:completed")
		f.AddLabel(2, "eve", "stage:discarded")
	}, func(t *testing.T, f *githubtest.Fake) {
		if is := f.State().Issues[1]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:completed"}) || len(is.Comments) != 1 {
			t.Errorf("closed %t, labels %q, comments %q; want it closed, stage:completed back, with no comment", is.Closed, is.Labels, is.Comments)
		}
	})
}

// A real start still counts a closed issue with one closed stage as
// judged, though eve swapped it while no daemon ran (SPEC §16's gap): it
// reads nothing of task 2, and writes nothing. A start whose first listing
// of the issues fails is still a start on its next poll. The poll after
// the digest is dropped lists task 2 again, unchanged since the start
// found it judged, so it reads nothing of it either.
func TestClosingDropRealStart(t *testing.T) {
	t.Parallel()
	f := closingDropFake(t)
	f.CloseBy(2, "owner")
	f.AddLabel(2, app, "stage:completed") // judged by an earlier run
	f.RemoveLabel(2, app, "stage:pr-open")
	f.RemoveLabel(2, "eve", "stage:completed")
	f.AddLabel(2, "eve", "stage:discarded")
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues" }, http.StatusInternalServerError)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "poll issues") {
		t.Fatalf("Iterate = %v; want the poll's listing failed", err)
	}
	reqs := len(f.Requests())
	if err := closingDropQuiet(f, d); err != nil {
		t.Fatal(err)
	}
	if r := readsOf(f, reqs, 2); len(r) != 0 || len(f.Writes()) != 0 {
		t.Errorf("the start read %+v and wrote %+v; want nothing, task 2 counted as judged", r, f.Writes())
	}
	closingDrop(t, f, d)
	reqs = len(f.Requests())
	if err := closingDropQuiet(f, d); err != nil {
		t.Fatal(err)
	}
	// Its writes are requests too, so this covers them.
	if r := readsOf(f, reqs, 2); len(r) != 0 {
		t.Errorf("after the drop, the requests of task 2 were %+v; want none", r)
	}
	if is := f.State().Issues[1]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:discarded"}) {
		t.Errorf("closed %t, labels %q; want eve's stage:discarded standing, as the start found it", is.Closed, is.Labels)
	}
}

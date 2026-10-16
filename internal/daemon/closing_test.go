package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Closes, reopens and the closed stages (plan #87 rev 2, T6, #190).

// closingFake is newFake, owner an admin and mallory a writer whom
// CODEOWNERS doesn't trust, with owner's task 1 at pr-open, as the App set
// it: its label is event 9001, and its move comment 1001.
func closingFake(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	fileAt(f, "pr-open", "do it")
	f.SetType(1, "task")
	return f
}

// closingWith is closingFake, then do.
func closingWith(do func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := closingFake(t)
		do(f)
		return f
	}
}

// closingJudged is do on closingFake's issue 1, then what the App leaves
// once it judged an accepted close as stage: its stage label swapped for
// stage's.
func closingJudged(stage string, do func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return closingWith(func(f *githubtest.Fake) {
		do(f)
		f.AddLabel(1, app, "stage:"+stage)
		f.RemoveLabel(1, app, "stage:pr-open")
	})
}

// closingPass is a new daemon's pass over f, with notify owner, as after a
// restart.
func closingPass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error { return newDaemon(t, f, o, nil).Iterate(context.Background()) }
}

// closingComments is the bodies of issue n's comments from the from'th on.
func closingComments(f *githubtest.Fake, n, from int) []string {
	var bodies []string
	for _, c := range f.State().Issues[n-1].Comments[from:] {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// closingWant is issue 1 after closingPass: its labels, whether it's
// closed, and its comments after closingFake's one.
type closingWant struct {
	labels   []string
	closed   bool
	comments []string
}

// closingCheck runs closingPass under Replay, then once on a fresh fake,
// and checks issue 1 against want, which it calls after setup, for the IDs
// setup makes. Then a second pass of the same daemon, and a restart's,
// write nothing, and read nothing of a closed issue.
func closingCheck(t *testing.T, setup func(testing.TB) *githubtest.Fake, want func(f *githubtest.Fake) closingWant) {
	t.Helper()
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, setup, closingPass(t, o))
	f := setup(t)
	w := want(f)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, w.labels) || is.Closed != w.closed || !slices.Equal(got, w.comments) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant %q, %t and\n%q", is.Labels, is.Closed, got, w.labels, w.closed, w.comments)
	}
	for name, d := range map[string]interface{ Iterate(context.Context) error }{"a second pass": d, "a restart": newDaemon(t, f, o, nil)} {
		writes, reqs := len(f.Writes()), len(f.Requests())
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.Writes()[writes:]; len(got) != 0 {
			t.Errorf("%s wrote %+v; want nothing", name, got)
		}
		if r := readsOf(f, reqs, 1); w.closed && len(r) != 0 {
			t.Errorf("%s read closed issue 1: %+v", name, r)
		}
	}
}

// closingReverted is the App's comment on reopening an issue that by
// closed, as event.
func closingReverted(by string, event int64) string {
	return fmt.Sprintf("I've reopened this issue: only a code owner, the App or a merged pull request can close it, and %s isn't one.\n\n"+
		"<!-- issuedog:revert key=%d -->", by, event)
}

// closingClosedAgain is the App's comment on closing an issue again that
// by reopened, as event.
func closingClosedAgain(by string, event int64) string {
	return fmt.Sprintf("I've closed this issue again: only a code owner can reopen it, and %s isn't one.\n\n<!-- issuedog:revert key=%d -->", by, event)
}

// closingHeld is the App's comment on holding an issue that by reopened, as
// event, at at, with seen.
func closingHeld(by string, event int64, at string, seen int64) string {
	return fmt.Sprintf("%s reopened this issue, so it's on hold, at `%s`, where it was before it closed. `/resume` here puts it back there.\n\n"+
		"<!-- issuedog:transition from=closed to=held at=%s reason=owner event=%d seen=%d -->", by, at, at, event, seen)
}

// A close by the owner, the App or a merged pull request stands, whoever
// merged it: the issue gets stage:completed, or stage:discarded for a close
// as not planned or as a duplicate, in place of its stage label, with no
// comment. Each was closed while no daemon ran, as the closes from before
// T6 are, so the first start judges each, and relabels it once.
func TestClosingAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(f *githubtest.Fake)
		label string
	}{
		{"the owner's", func(f *githubtest.Fake) { f.CloseBy(1, "owner") }, "stage:completed"},
		{"the owner's as not planned", func(f *githubtest.Fake) { f.CloseAs(1, "owner", "not_planned") }, "stage:discarded"},
		{"the owner's as a duplicate", func(f *githubtest.Fake) { f.CloseAs(1, "owner", "duplicate") }, "stage:discarded"},
		{"the App's", func(f *githubtest.Fake) { f.CloseBy(1, app) }, "stage:completed"},
		{"the App's as not planned", func(f *githubtest.Fake) { f.CloseAs(1, app, "not_planned") }, "stage:discarded"},
		{"a merged pull request's, merged by a writer", func(f *githubtest.Fake) { f.CloseByCommit(1, "mallory", "c0ffee", true) }, "stage:completed"},
		{"a merged pull request's, merged by eve", func(f *githubtest.Fake) { f.CloseByCommit(1, "eve", "c0ffee", true) }, "stage:completed"},
		{"the owner's push of a commit", func(f *githubtest.Fake) { f.CloseByCommit(1, "owner", "c0ffee", false) }, "stage:completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closingCheck(t, closingWith(tc.close), func(*githubtest.Fake) closingWant {
				return closingWant{labels: []string{tc.label}, closed: true}
			})
		})
	}
}

// A close by someone without write access, a ghost's included, is reverted
// with a comment carrying a revert marker keyed by its event, and holds
// nothing; a commit pushed with "Closes #1" is no merged pull request. The
// stage label is left alone.
func TestClosingUntrusted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(f *githubtest.Fake) int64
		by    string
	}{
		{"eve's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "eve") }, "`eve`"},
		{"eve's as not planned", func(f *githubtest.Fake) int64 { return f.CloseAs(1, "eve", "not_planned") }, "`eve`"},
		{"a ghost's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "") }, "someone GitHub doesn't name"},
		{"another bot's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "other[bot]") }, "`other[bot]`"},
		{"eve's push of a commit", func(f *githubtest.Fake) int64 { return f.CloseByCommit(1, "eve", "c0ffee", false) }, "`eve`"},
		{"a mention in the login", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "eve<!--@owner") }, "`eve&lt;!--@​owner`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event int64
			closingCheck(t, closingWith(func(f *githubtest.Fake) { event = tc.close(f) }), func(*githubtest.Fake) closingWant {
				return closingWant{labels: []string{"stage:pr-open"}, comments: []string{closingReverted(tc.by, event)}}
			})
		})
	}
}

// A write collaborator's close is reverted too, and it's a discussion
// trigger, keyed by its event: the reopened issue is held for it in the
// same pass, with the hold's comment as its notice. A commit they pushed
// is no merged pull request either.
func TestClosingWriter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(f *githubtest.Fake) int64
	}{
		{"mallory's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "mallory") }},
		{"mallory's push of a commit", func(f *githubtest.Fake) int64 { return f.CloseByCommit(1, "mallory", "c0ffee", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event int64
			closingCheck(t, closingWith(func(f *githubtest.Fake) { event = tc.close(f) }), func(*githubtest.Fake) closingWant {
				key := fmt.Sprint(event)
				return closingWant{labels: []string{"stage:held"}, comments: []string{discHeld("pr-open", "`mallory` closed this issue, which I reopened",
					"from=pr-open to=held at=pr-open origin=pr-open reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}
			})
		})
	}
}

// An untrusted reopen of an issue with a closed stage is reverted: it's
// closed again with the state_reason of the close it undid, with a revert
// comment, and its label stays.
func TestClosingUntrustedReopen(t *testing.T) {
	for _, tc := range []struct {
		name, by string
		login    string
		close    func(f *githubtest.Fake)
		stage    string
		reason   string
	}{
		{"mallory's, of a completed issue", "`mallory`", "mallory", func(f *githubtest.Fake) { f.CloseBy(1, "owner") }, "completed", "completed"},
		{"eve's, of a discarded issue", "`eve`", "eve", func(f *githubtest.Fake) { f.CloseAs(1, "owner", "not_planned") }, "discarded", "not_planned"},
		{"a ghost's, of a duplicate", "someone GitHub doesn't name", "", func(f *githubtest.Fake) { f.CloseAs(1, "owner", "duplicate") }, "discarded", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event int64
			setup := closingJudged(tc.stage, func(f *githubtest.Fake) { tc.close(f) })
			setup = then(setup, func(f *githubtest.Fake) { event = f.ReopenBy(1, tc.login) })
			closingCheck(t, setup, func(*githubtest.Fake) closingWant {
				return closingWant{labels: []string{"stage:" + tc.stage}, closed: true, comments: []string{closingClosedAgain(tc.by, event)}}
			})
			f := run(t, setup, closingPass(t, digestNotifyOrigin(t)))
			is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			if is.GetStateReason() != tc.reason {
				t.Errorf("state_reason %q; want %q, as before the reopen", is.GetStateReason(), tc.reason)
			}
		})
	}
}

// A trusted reopen moves the issue to held, closed → held, at the stage
// the latest transition marker shows, as an owner hold, and the closed
// stage goes. /resume here then puts it back there.
func TestClosingTrustedReopen(t *testing.T) {
	var event int64
	setup := then(closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") }),
		func(f *githubtest.Fake) { event = f.ReopenBy(1, "owner") })
	closingCheck(t, setup, func(*githubtest.Fake) closingWant {
		return closingWant{labels: []string{"stage:held"}, comments: []string{closingHeld("`owner`", event, "pr-open", 1001)}}
	})
	f, o := setup(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:pr-open"}) || is.Closed {
		t.Errorf("after /resume: labels %q, closed %t; want stage:pr-open, open", is.Labels, is.Closed)
	}
}

// A trusted reopen of an issue closed while held keeps the first hold's at
// and origin.
func TestClosingTrustedReopenHeld(t *testing.T) {
	var event int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		fileAt(f, "held", "do it")
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready origin=spec-review reason=discussion event=c7 seen=900 -->")
		f.CloseBy(1, "owner")
		f.AddLabel(1, app, "stage:completed")
		f.RemoveLabel(1, app, "stage:held")
		event = f.ReopenBy(1, "owner")
		return f
	}
	closingCheck(t, setup, func(*githubtest.Fake) closingWant {
		return closingWant{labels: []string{"stage:held"}, comments: []string{
			"<!-- issuedog:transition from=ready to=held at=ready origin=spec-review reason=discussion event=c7 seen=900 -->",
			fmt.Sprintf("`owner` reopened this issue, so it's on hold, at `ready`, where it was before it closed. `/resume` here puts it back there.\n\n"+
				"<!-- issuedog:transition from=closed to=held at=ready origin=spec-review reason=owner event=%d seen=900 -->", event)}}
	})
}

// A close and a reopen made while a daemon runs are judged on its next
// pass, and an unchanged pass after reads nothing of the closed issue, nor
// does a comment on it.
func TestClosingWhileRunning(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	step := func(name string, labels []string, closed bool) {
		t.Helper()
		iterate(t, d)
		if is := f.State().Issues[0]; !slices.Equal(is.Labels, labels) || is.Closed != closed {
			t.Fatalf("%s: labels %q, closed %t; want %q, %t", name, is.Labels, is.Closed, labels, closed)
		}
	}
	quietPass := func(name string) {
		t.Helper()
		writes, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		if w, r := f.Writes()[writes:], readsOf(f, reqs, 1); len(w) != 0 || len(r) != 0 {
			t.Fatalf("%s: writes %+v, reads %+v; want none", name, w, r)
		}
	}
	f.CloseBy(1, "eve")
	step("eve's close", []string{"stage:pr-open"}, false)
	f.CloseBy(1, "owner")
	step("the owner's close", []string{"stage:completed"}, true)
	quietPass("an unchanged pass")
	f.AddComment(1, "eve", "why?")
	quietPass("a comment on the closed issue")
	f.ReopenBy(1, "eve")
	step("eve's reopen", []string{"stage:completed"}, true)
	quietPass("the pass after it")
	f.ReopenBy(1, "owner")
	step("the owner's reopen", []string{"stage:held"}, false)
	f.CloseAs(1, "owner", "not_planned")
	step("the owner's close as not planned", []string{"stage:discarded"}, true)
	quietPass("an unchanged pass at the end")
	writes := len(f.Writes())
	iterate(t, newDaemon(t, f, o, nil))
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a restart wrote %+v; want nothing", w)
	}
}

// The closed issues from before T6 are each judged once, on the first
// start: one timeline read and the label writes each. A pass judges at
// most 20, the newest closes first; the rest wait for the next pass. Then
// nothing is read again, on a pass or a restart.
func TestClosingFirstStart(t *testing.T) {
	f, o := newFake(t), digestNotifyOrigin(t)
	for i := 1; i <= 23; i++ {
		fileAt(f, "pr-open", "do it")
		f.CloseBy(i, "owner")
	}
	d := newDaemon(t, f, o, nil)
	timelines := func(from int) []int {
		var got []int
		for _, r := range f.Requests()[from:] {
			var n int
			if _, err := fmt.Sscanf(r.Path, "/issues/%d/timeline", &n); err == nil && strings.HasSuffix(r.Path, "/timeline") {
				got = append(got, n)
			}
		}
		return got
	}
	completed := func() (got []int) {
		for _, is := range f.State().Issues {
			if slices.Equal(is.Labels, []string{"stage:completed"}) {
				got = append(got, is.Number)
			}
		}
		return got
	}
	from := len(f.Requests())
	iterate(t, d)
	var want []int
	for i := 23; i >= 4; i-- {
		want = append(want, i)
	}
	if got := timelines(from); !slices.Equal(got, want) {
		t.Errorf("the first pass read the timelines of %v; want the 20 newest closes, %v", got, want)
	}
	if got := completed(); len(got) != 20 || got[0] != 4 {
		t.Errorf("after the first pass, %v are completed; want 4 to 23", got)
	}
	if got := len(f.Writes()); got != 40 {
		t.Errorf("the first pass made %d writes; want 40, a label added and one removed on each of 20", got)
	}
	from = len(f.Requests())
	iterate(t, d)
	if got := timelines(from); !slices.Equal(got, []int{3, 2, 1}) {
		t.Errorf("the second pass read the timelines of %v; want the 3 left", got)
	}
	if got := completed(); len(got) != 23 {
		t.Errorf("after the second pass, %v are completed; want all 23", got)
	}
	for name, d := range map[string]interface{ Iterate(context.Context) error }{"a third pass": d, "a restart": newDaemon(t, f, o, nil)} {
		writes, reqs := len(f.Writes()), len(f.Requests())
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := timelines(reqs); len(got) != 0 || len(f.Writes()) != writes {
			t.Errorf("%s read the timelines of %v and wrote %+v; want nothing", name, got, f.Writes()[writes:])
		}
	}
}

// A crash part way through the first start's relabel resumes: Replay over
// three closed issues, whatever write a crash comes at.
func TestClosingFirstStartReplay(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		for i := 1; i <= 3; i++ {
			fileAt(f, "pr-open", "do it")
		}
		f.CloseBy(1, "owner")
		f.CloseAs(2, "owner", "not_planned")
		f.CloseBy(3, app)
		return f
	}
	githubtest.Replay(t, setup, closingPass(t, digestNotifyOrigin(t)))
}

// A close whose event GitHub hasn't written yet is looked at again on the
// next pass, with nothing written meanwhile.
func TestClosingLateEvent(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.LagEvents(5 * time.Second)
	f.CloseBy(1, "eve")
	writes := len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 || !f.State().Issues[0].Closed {
		t.Fatalf("writes %+v before the close's event shows; want none, and the issue closed", w)
	}
	f.Advance(10 * time.Second)
	iterate(t, d)
	if is := f.State().Issues[0]; is.Closed || len(is.Comments) != 2 {
		t.Errorf("closed %t, comments %q once the event shows; want eve's close reverted", is.Closed, is.Comments)
	}
}

// A reopen whose event GitHub hasn't written yet holds the issue back, with
// nothing written: the closed stage isn't put back meanwhile.
func TestClosingLateReopen(t *testing.T) {
	f, o := closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") })(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.LagEvents(5 * time.Second)
	event := f.ReopenBy(1, "owner")
	writes := len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Fatalf("writes %+v before the reopen's event shows; want none", w)
	}
	f.Advance(10 * time.Second)
	iterate(t, d)
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, []string{"stage:held"}) || !slices.Equal(got, []string{closingHeld("`owner`", event, "pr-open", 1001)}) {
		t.Errorf("labels %q, comments %q once the event shows; want the hold", is.Labels, got)
	}
}

// A reopen whose event doesn't show within 30 seconds of the issue's
// updated_at holds it back no longer: the label rules put the closed stage
// back to the latest marker's, with a disagreement error.
func TestClosingLateReopenBound(t *testing.T) {
	f, o := closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") })(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.LagEvents(time.Hour)
	f.ReopenBy(1, "owner")
	writes := len(f.Writes())
	iterate(t, d)
	f.Advance(29 * time.Second)
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Fatalf("writes %+v within the wait; want none", w)
	}
	f.Advance(time.Second)
	iterate(t, d)
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, []string{"stage:pr-open"}) || len(got) != 1 || !strings.Contains(got[0], "kind=disagreement") {
		t.Errorf("labels %q, comments %q after the wait; want stage:pr-open back, with a disagreement error", is.Labels, got)
	}
}

// The owner reopens a judged issue, and mallory closes it before the App's
// next pass: her close is reverted, the owner's reopen holds the issue, and
// her close, on a held issue, gets its notice alone.
func TestClosingWriterAfterTrustedReopen(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	reopen := f.ReopenBy(1, "owner")
	closed := f.CloseBy(1, "mallory")
	iterate(t, d)
	is := f.State().Issues[0]
	want := []string{closingHeld("`owner`", reopen, "pr-open", 1001), discNotice("`mallory` closed this issue, which I reopened", fmt.Sprint(closed))}
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, []string{"stage:held"}) || is.Closed || !slices.Equal(got, want) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant stage:held, open and\n%q", is.Labels, is.Closed, got, want)
	}
	writes := len(f.Writes())
	iterate(t, d)
	iterate(t, newDaemon(t, f, o, nil))
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a second pass and a restart wrote %+v; want nothing", w)
	}
}

// Gap, for SPEC §16 (T10, #194): a start takes a closed issue with one
// closed stage as judged, so a trusted reopen and an untrusted close that
// both come while no daemon runs, after an accepted close, aren't found.
func TestClosingGapWhileStopped(t *testing.T) {
	f := closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") })(t)
	f.ReopenBy(1, "owner")
	f.CloseBy(1, "mallory")
	reqs := len(f.Requests())
	iterate(t, newDaemon(t, f, digestNotifyOrigin(t), nil))
	if r := readsOf(f, reqs, 1); len(r) != 0 || len(f.Writes()) != 0 || !f.State().Issues[0].Closed {
		t.Errorf("reads %+v, writes %+v; want none, the close missed", r, f.Writes())
	}
}

// A closed stage on an open issue isn't valid: a trusted user's is put
// back, with a disagreement error, or removed from an issue with no
// transition marker; an untrusted user's is reverted as any label is.
func TestClosingStageOnOpenIssue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t testing.TB) *githubtest.Fake
		labels []string
		text   func(event int64) string
	}{
		{"the owner's, at ready", func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			file(f, 0, ready)
			f.AddLabel(1, "owner", "stage:completed")
			return f
		}, []string{"stage:ready"}, func(event int64) string {
			return fmt.Sprintf("The stage labels didn't match this issue's latest transition, to `ready`, so I've set them to match. "+
				"They were: `stage:ready`, `stage:completed`. `stage:completed` and `stage:discarded` are only for a closed issue.\n\n"+
				"<!-- issuedog:error kind=disagreement key=%d -->", event)
		}},
		{"the owner's, with no marker", func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			f.AddIssue("owner", "Issue", "body")
			f.AddLabel(1, "owner", "stage:discarded")
			return f
		}, nil, func(event int64) string {
			return fmt.Sprintf("`stage:discarded` is only for a closed issue, so I've removed it.\n\n<!-- issuedog:error kind=disagreement key=%d -->", event)
		}},
		{"eve's, at ready", func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			file(f, 0, ready)
			f.AddLabel(1, "eve", "stage:completed")
			return f
		}, []string{"stage:ready"}, func(event int64) string {
			return fmt.Sprintf("I removed `stage:completed`: only trusted users can change pipeline labels, and eve isn't one.\n\n"+
				"<!-- issuedog:revert key=%d -->", event)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			githubtest.Replay(t, tc.setup, commandsPass(t, 1))
			before := tc.setup(t)
			event := int64(9001 + len(before.State().Issues[0].Labels) - 1)
			f := run(t, tc.setup, commandsPass(t, 1))
			is := f.State().Issues[0]
			got := closingComments(f, 1, len(before.State().Issues[0].Comments))
			if !slices.Equal(is.Labels, tc.labels) || !slices.Equal(got, []string{tc.text(event)}) {
				t.Errorf("labels %q and comments\n%q\nwant %q and\n%q", is.Labels, got, tc.labels, []string{tc.text(event)})
			}
		})
	}
}

// A closed stage on a judged closed issue that someone untrusted swapped
// is set back, with no comment. The owner's swap stands, and is never read
// again.
func TestClosingStageSwapped(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.RemoveLabel(1, "eve", "stage:completed")
	f.AddLabel(1, "eve", "stage:discarded")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:completed"}) || len(is.Comments) != 1 {
		t.Errorf("after eve's swap: labels %q, comments %q; want stage:completed back, with no comment", is.Labels, is.Comments)
	}
	f.RemoveLabel(1, "owner", "stage:completed")
	f.AddLabel(1, "owner", "stage:discarded")
	writes := len(f.Writes())
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:discarded"}) || len(f.Writes()) != writes {
		t.Errorf("after the owner's swap: labels %q, writes %+v; want the owner's stage:discarded kept, nothing written", is.Labels, f.Writes()[writes:])
	}
	reqs := len(f.Requests())
	iterate(t, d)
	iterate(t, newDaemon(t, f, o, nil))
	if r := readsOf(f, reqs, 1); len(r) != 0 {
		t.Errorf("read issue 1 after its swap was judged: %+v", r)
	}
}

// eve takes the stage label off and closes the issue between passes: the
// close is still judged, from the issue this daemon held, and reverted,
// and the label rules put the label back.
func TestClosingStageTakenOff(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.RemoveLabel(1, "eve", "stage:pr-open")
	f.CloseBy(1, "eve")
	iterate(t, d)
	if is := f.State().Issues[0]; is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) {
		t.Errorf("closed %t, labels %q; want eve's close reverted, and stage:pr-open back", is.Closed, is.Labels)
	}
}

// A closed issue with no stage label is no pipeline issue, so its close is
// never judged, on a pass or a start.
func TestClosingOutsideThePipeline(t *testing.T) {
	f, o := newFake(t), digestNotifyOrigin(t)
	f.CloseBy(f.AddIssue("owner", "Issue", "body"), "eve")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddIssue("owner", "Another", "body")
	f.CloseBy(2, "eve")
	iterate(t, d)
	if r := append(readsOf(f, 0, 1), readsOf(f, 0, 2)...); len(r) != 0 || len(f.Writes()) != 0 {
		t.Errorf("reads %+v, writes %+v; want none", r, f.Writes())
	}
}

// A child's untrusted close writes nothing to its parent: only the child
// is reopened.
func TestClosingChildWritesNothingToParent(t *testing.T) {
	f, o := githubtest.New(t, "o/r", slug), digestNotifyOrigin(t)
	f.SetPermission("owner", "admin")
	file(f, 0, "from=plan-review to=planned cmd=r1")
	file(f, 1, ready)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	parent := f.State().Issues[0]
	writes := len(f.Writes())
	f.CloseBy(2, "eve")
	iterate(t, d)
	iterate(t, d)
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/1/") || w.Path == "/issues/1" {
			t.Errorf("wrote %+v to the parent; want nothing", w)
		}
	}
	if got := f.State().Issues[0]; !reflect.DeepEqual(got, parent) {
		t.Errorf("the parent is\n%+v\nwant it as before\n%+v", got, parent)
	}
	if child := f.State().Issues[1]; child.Closed {
		t.Error("the child is closed; want eve's close reverted")
	}
}

// A failed write of a close's judgement is the pass's error, and the close
// stays due, so the next pass judges it.
func TestClosingFailedWrite(t *testing.T) {
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	f.FailWrite(1)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the close of o/r#1") {
		t.Fatalf("Iterate = %v; want the failed label write", err)
	}
	f.FailWrite(0)
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:completed"}) {
		t.Errorf("labels %q; want stage:completed once the write works", is.Labels)
	}
}

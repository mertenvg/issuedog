package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Closes, reopens and the closed stages (SPEC §6.4, §7.1, §7.2, #190).

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
// the closed stages existed are, so the first start judges each, and
// relabels it once.
func TestClosingAccepted(t *testing.T) {
	t.Parallel()
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
		{"a merged pull request's, merged by a writer", func(f *githubtest.Fake) {
			f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
		}, "stage:completed"},
		{"a merged pull request's, merged by eve", func(f *githubtest.Fake) {
			f.CloseByPullRequest(1, "eve", githubtest.ClosingPR{Merged: true})
		}, "stage:completed"},
		{"the owner's push of a commit", func(f *githubtest.Fake) { f.CloseByCommit(1, "owner", "c0ffee", "") }, "stage:completed"},
		{"a writer's push of a commit to this repo", func(f *githubtest.Fake) { f.CloseByCommit(1, "mallory", "c0ffee", "") }, "stage:completed"},
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
	t.Parallel()
	for _, tc := range []struct {
		name  string
		close func(f *githubtest.Fake) int64
		by    string
	}{
		{"eve's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "eve") }, "`eve`"},
		{"eve's as not planned", func(f *githubtest.Fake) int64 { return f.CloseAs(1, "eve", "not_planned") }, "`eve`"},
		{"a ghost's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "") }, "someone GitHub doesn't name"},
		{"another bot's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "other[bot]") }, "`other[bot]`"},
		{"eve's commit in another repo", func(f *githubtest.Fake) int64 { return f.CloseByCommit(1, "eve", "c0ffee", "x/y") }, "`eve`"},
		{"eve's unmerged pull request", func(f *githubtest.Fake) int64 { return f.CloseByPullRequest(1, "eve", githubtest.ClosingPR{}) }, "`eve`"},
		{"eve's pull request merged into another branch", func(f *githubtest.Fake) int64 {
			return f.CloseByPullRequest(1, "eve", githubtest.ClosingPR{Merged: true, Base: "dev"})
		}, "`eve`"},
		{"eve's pull request merged in another repo", func(f *githubtest.Fake) int64 {
			return f.CloseByPullRequest(1, "eve", githubtest.ClosingPR{Merged: true, Repo: "x/y"})
		}, "`eve`"},
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
	t.Parallel()
	for _, tc := range []struct {
		name  string
		close func(f *githubtest.Fake) int64
	}{
		{"mallory's", func(f *githubtest.Fake) int64 { return f.CloseBy(1, "mallory") }},
		{"mallory's commit in another repo", func(f *githubtest.Fake) int64 { return f.CloseByCommit(1, "mallory", "c0ffee", "x/y") }},
		{"mallory's unmerged pull request", func(f *githubtest.Fake) int64 { return f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{}) }},
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
	t.Parallel()
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

// An untrusted reopen is closed again with the state_reason of the newest
// close before it, not an older one's.
func TestClosingUntrustedReopenNewestReason(t *testing.T) {
	t.Parallel()
	f := closingJudged("discarded", func(f *githubtest.Fake) {
		f.CloseAs(1, "owner", "not_planned")
		f.ReopenBy(1, "owner")
		f.CloseAs(1, "owner", "duplicate")
	})(t)
	f.ReopenBy(1, "eve")
	iterate(t, newDaemon(t, f, digestNotifyOrigin(t), nil))
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetState() != "closed" || is.GetStateReason() != "duplicate" {
		t.Errorf("%s as %s; want closed again as duplicate", is.GetState(), is.GetStateReason())
	}
}

// An earlier revert comment of another event, a label's, doesn't stand for
// a close's: the close gets its own.
func TestClosingRevertCommentKeyed(t *testing.T) {
	t.Parallel()
	var event int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := closingFake(t)
		f.AddComment(1, app, "I removed `priority:p0`: only trusted users can change pipeline labels, and eve isn't one.\n\n<!-- issuedog:revert key=77 -->")
		event = f.CloseBy(1, "eve")
		return f
	}
	f := run(t, setup, closingPass(t, digestNotifyOrigin(t)))
	if got := closingComments(f, 1, 2); f.State().Issues[0].Closed || !slices.Equal(got, []string{closingReverted("`eve`", event)}) {
		t.Errorf("comments %q; want the close reverted with its own revert comment", got)
	}
}

// The digest is never judged as a pipeline issue, even with a stage label.
func TestClosingNotTheDigest(t *testing.T) {
	t.Parallel()
	f, o := digestPinned(t), digestNotifyOrigin(t)
	f.AddLabel(1, app, "stage:held")
	f.CloseBy(1, "owner")
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:held"}) {
		t.Errorf("the digest's labels %q; want stage:held left alone", is.Labels)
	}
	for _, w := range f.Writes() {
		if strings.HasPrefix(w.Path, "/issues/1/labels") || w.Path == "/issues/1/comments" {
			t.Errorf("wrote %+v to the digest; want nothing judged", w)
		}
	}
}

// A trusted reopen moves the issue to held, closed → held, at the stage
// the latest transition marker shows, as an owner hold, and the closed
// stage goes. /resume here then puts it back there.
func TestClosingTrustedReopen(t *testing.T) {
	t.Parallel()
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

// A trusted reopen of a human task the App closed on /done (Transition, to
// closed) holds it at needs-human, the stage it was closed from, not at
// clarifying; /resume puts it back there.
func TestClosingTrustedReopenAppClose(t *testing.T) {
	t.Parallel()
	var event int64
	setup := humanTask("", func(t testing.TB, f *githubtest.Fake) {
		doneBy("owner", 1002)(t, f)
		f.AddReaction(1002, app, "+1")
		f.AddComment(1, app, doneClosed(`The "Done when" has no check for me to run.`, 1002))
		f.CloseAs(1, app, "completed")
		f.AddLabel(1, app, "stage:completed")
		f.RemoveLabel(1, app, "stage:needs-human")
		event = f.ReopenBy(1, "owner")
	})
	closingCheck(t, setup, func(*githubtest.Fake) closingWant {
		return closingWant{labels: []string{"stage:held"}, comments: []string{"/done", doneClosed(`The "Done when" has no check for me to run.`, 1002),
			closingHeld("`owner`", event, "needs-human", 1002)}}
	})
	f, o := setup(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:needs-human"}) || is.Closed {
		t.Errorf("after /resume: labels %q, closed %t; want stage:needs-human, open", is.Labels, is.Closed)
	}
}

// A trusted reopen of an issue closed while held keeps the first hold's at
// and origin.
func TestClosingTrustedReopenHeld(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// The closed issues from before the closed stages existed are each judged
// once, on the first start: one timeline read and the label writes each. A
// pass judges at most 20, the newest closes first; the rest wait for the
// next pass. Then nothing is read again, on a pass or a restart.
func TestClosingFirstStart(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// eve's close, whose event is late, after the owner's reopen, or after a
// reopen and a close whose events are late too, isn't taken for the newest
// event before it: it's judged, and reverted, once its event shows.
func TestClosingLateCloseAfterReopen(t *testing.T) {
	t.Parallel()
	for name, judged := range map[string]bool{"after a reopen judged": true, "after a late reopen": false} {
		t.Run(name, func(t *testing.T) {
			f, o := closingFake(t), digestNotifyOrigin(t)
			f.CloseBy(1, "owner")
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			if judged {
				f.ReopenBy(1, "owner")
				iterate(t, d)
				f.LagEvents(5 * time.Second)
			} else {
				f.LagEvents(5 * time.Second)
				f.ReopenBy(1, "owner")
			}
			f.CloseBy(1, "eve")
			writes := len(f.Writes())
			iterate(t, d)
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Fatalf("writes %+v before the events show; want none", w)
			}
			f.Advance(10 * time.Second)
			iterate(t, d)
			if is := f.State().Issues[0]; is.Closed {
				t.Errorf("closed, labels %q, once the events show; want eve's close reverted", is.Labels)
			}
		})
	}
}

// A reopen made between the poll and the timeline's read isn't judged as
// the close: nothing is written to the issue, which is open now.
func TestClosingReopenedBeforeTheRead(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.CloseBy(1, "owner")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Path == "/issues/1/timeline" }, func() { f.ReopenBy(1, "owner") })
	writes := len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("writes %+v; want none for the reopened issue", w)
	}
}

// After a restart, a comment on a judged closed issue reads nothing either.
func TestClosingRestartThenComment(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	iterate(t, newDaemon(t, f, o, nil))
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddComment(1, "eve", "why?")
	reqs := len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, reqs, 1); len(r) != 0 {
		t.Errorf("read closed issue 1 for a comment: %+v", r)
	}
}

// A closed issue deleted while its close waits to be judged is forgotten,
// with no error, once a judging on a later pass finds it gone again, as
// one answer may be wrong, and isn't read again.
func TestClosingGone(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.LagEvents(5 * time.Second)
	f.CloseBy(1, "owner")
	iterate(t, d)
	f.DeleteIssue()
	f.Advance(10 * time.Second)
	iterate(t, d)
	reqs := len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, reqs, 1); len(r) == 0 {
		t.Error("the pass after the first 404 didn't judge deleted issue 1 again; want a second judging")
	}
	reqs = len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, reqs, 1); len(r) != 0 {
		t.Errorf("read deleted issue 1 again: %+v", r)
	}
}

// A close whose timeline answers 404 once stays to judge, as one answer
// may be wrong, and a good judging after it ends that strike: here one
// that finds the close's event not written yet. So a second 404 after it
// is a first again, and the pass after that judges the close, which
// stands, at stage:completed. An issue filed after the close keeps it
// from the poll's cursor, which every poll lists again, so a close
// forgotten isn't found due again.
func TestClosingGoneOnce(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	timeline := func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1/timeline" }
	f.LagEvents(5 * time.Second)
	f.CloseBy(1, "owner")
	f.LagEvents(0)
	f.AddIssue("owner", "Newer", "")
	f.AnswerOnce(timeline, http.StatusNotFound)
	iterate(t, d) // the first 404
	iterate(t, d) // a good judging: the close's event isn't written yet
	f.Advance(10 * time.Second)
	f.AnswerOnce(timeline, http.StatusNotFound)
	iterate(t, d) // a first 404 again
	iterate(t, d)
	if got := f.State().Issues[0].Labels; !slices.Equal(got, []string{"stage:completed"}) {
		t.Errorf("labels %q; want the owner's close judged, at stage:completed", got)
	}
}

// A reopen whose event GitHub hasn't written yet holds the issue back, with
// nothing written: the closed stage isn't put back meanwhile.
func TestClosingLateReopen(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Gap, for SPEC §16 (#194): a start takes a closed issue with one
// closed stage as judged, so a trusted reopen and an untrusted close that
// both come while no daemon runs, after an accepted close, aren't found.
func TestClosingGapWhileStopped(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A closed stage whose standing change is the App's removal, which eve put
// back, explains nothing: the close as not planned gets stage:discarded.
func TestClosingStageRemovedByTheApp(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.ReopenBy(1, "owner")
	iterate(t, d) // held, and stage:completed removed
	f.CloseAs(1, "owner", "not_planned")
	f.RemoveLabel(1, "eve", "stage:held")
	f.AddLabel(1, "eve", "stage:completed")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:discarded"}) {
		t.Errorf("labels %q; want stage:discarded", is.Labels)
	}
}

// eve takes the stage label off and closes the issue between passes: the
// close is still judged, from the issue this daemon held, and reverted,
// and the label rules put the label back.
func TestClosingStageTakenOff(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A close is judged by the issue's own state_reason, as REST's closed event
// has none, and a close again after an untrusted reopen takes the closed
// stage's when GraphQL gives the close it undid no state reason either.
func TestClosingNoStateReason(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.ForgetCloser(1, f.CloseAs(1, "owner", "duplicate"))
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:discarded"}) {
		t.Fatalf("labels %q; want stage:discarded, from the issue's state_reason", is.Labels)
	}
	f.ReopenBy(1, "eve")
	iterate(t, d)
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetState() != "closed" || is.GetStateReason() != "not_planned" {
		t.Errorf("%s as %s after eve's reopen; want closed again as not_planned, from stage:discarded", is.GetState(), is.GetStateReason())
	}
}

// A close reopened between the poll and the read of the issue to revert it
// writes nothing, and the next pass forgets it.
func TestClosingReopenedMeanwhile(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.CloseBy(1, "eve")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1" }, func() { f.ReopenBy(1, "owner") })
	writes := len(f.Writes())
	iterate(t, d)
	iterate(t, d)
	reqs := len(f.Requests())
	iterate(t, d)
	if w, r := f.Writes()[writes:], readsOf(f, reqs, 1); len(w) != 0 || len(r) != 0 || f.State().Issues[0].Closed {
		t.Errorf("writes %+v, then reads %+v; want none, and the issue open", w, r)
	}
}

// A write collaborator's close of an issue that isn't in the pipeline, at
// stage:intake with no transition marker, holds nothing: it's reverted with
// a comment, as anyone's.
func TestClosingWriterOutsideThePipeline(t *testing.T) {
	t.Parallel()
	var event int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddLabel(f.AddIssue("eve", "Issue", "body"), "owner", "stage:intake") // an untrusted author's, so not admitted
		event = f.CloseBy(1, "mallory")
		return f
	}
	githubtest.Replay(t, setup, closingPass(t, digestNotifyOrigin(t)))
	f := run(t, setup, closingPass(t, digestNotifyOrigin(t)))
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 0); is.Closed || !slices.Equal(is.Labels, []string{"stage:intake"}) || !slices.Equal(got, []string{closingReverted("`mallory`", event)}) {
		t.Errorf("closed %t, labels %q, comments %q; want the close reverted with a comment, and no hold", is.Closed, is.Labels, got)
	}
}

// A closed stage the owner sets by hand on an issue held for its reopen is
// a disagreement, as on any open issue: the reopen is judged already.
func TestClosingStageAfterReopenHold(t *testing.T) {
	t.Parallel()
	f, o := closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") })(t), digestNotifyOrigin(t)
	f.ReopenBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	event := f.AddLabel(1, "owner", "stage:completed")
	iterate(t, d)
	is := f.State().Issues[0]
	want := fmt.Sprintf("The stage labels didn't match this issue's latest transition, to `held`, so I've set them to match. "+
		"They were: `stage:completed`, `stage:held`. `stage:completed` and `stage:discarded` are only for a closed issue.\n\n"+
		"<!-- issuedog:error kind=disagreement key=%d -->", event)
	if got := closingComments(f, 1, 2); !slices.Equal(is.Labels, []string{"stage:held"}) || !slices.Equal(got, []string{want}) {
		t.Errorf("labels %q, comments\n%q\nwant stage:held and\n%q", is.Labels, got, want)
	}
}

// A trusted reopen of an issue whose hold recorded no stage to go back to
// holds it at clarifying, the one stage /resume could restore.
func TestClosingTrustedReopenNoAt(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	fileAt(f, "held", "do it")
	f.CloseBy(1, "owner")
	f.AddLabel(1, app, "stage:completed")
	f.RemoveLabel(1, app, "stage:held")
	event := f.ReopenBy(1, "owner")
	iterate(t, newDaemon(t, f, o, nil))
	want := fmt.Sprintf("`owner` reopened this issue, so it's on hold, at `clarifying`, where it was before it closed. `/resume` here puts it back there.\n\n"+
		"<!-- issuedog:transition from=closed to=held at=clarifying reason=owner event=%d -->", event)
	if got := closingComments(f, 1, 1); !slices.Equal(got, []string{want}) {
		t.Errorf("comments\n%q\nwant\n%q", got, want)
	}
}

// A trusted reopen the table can't move, as of a type the pipeline doesn't
// know, holds nothing: its type error stands, and the label rules put the
// closed stage back to the latest marker's.
func TestClosingTrustedReopenUnknownType(t *testing.T) {
	t.Parallel()
	f, o := closingJudged("completed", func(f *githubtest.Fake) { f.CloseBy(1, "owner") })(t), digestNotifyOrigin(t)
	f.SetType(1, "feature")
	f.ReopenBy(1, "owner")
	iterate(t, newDaemon(t, f, o, nil))
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) || len(got) == 0 ||
		strings.Contains(strings.Join(got, "\n"), "to=held") {
		t.Errorf("closed %t, labels %q, comments %q; want no hold, and stage:pr-open back", is.Closed, is.Labels, got)
	}
}

// While a daemon runs: a close by a commit pushed to another repo that
// names this issue is reverted once, and the next pass asks nothing more;
// a write collaborator's merge of a pull request stands, asked about once.
func TestClosingMergesWhileRunning(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	quiet := func(name string) {
		t.Helper()
		writes, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		var asked []githubtest.Request
		for _, r := range f.Requests()[reqs:] {
			if r.Path == "/graphql" || strings.HasPrefix(r.Path, "/issues/1/") {
				asked = append(asked, r)
			}
		}
		if w := f.Writes()[writes:]; len(w) != 0 || len(asked) != 0 {
			t.Fatalf("%s: writes %+v, requests %+v; want none", name, w, asked)
		}
	}
	event := f.CloseByCommit(1, "eve", "c0ffee", "x/y")
	iterate(t, d)
	if is := f.State().Issues[0]; is.Closed || !slices.Equal(closingComments(f, 1, 1), []string{closingReverted("`eve`", event)}) {
		t.Fatalf("closed %t, comments %q; want eve's close reverted, with its comment", is.Closed, closingComments(f, 1, 1))
	}
	quiet("the pass after the revert")
	f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
	iterate(t, d)
	if is := f.State().Issues[0]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:completed"}) {
		t.Fatalf("closed %t, labels %q; want mallory's merge to stand, at stage:completed", is.Closed, is.Labels)
	}
	quiet("the pass after the merge")
}

// closingGraphQL counts the GraphQL requests from the from'th on.
func closingGraphQL(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Path == "/graphql" && r.Method == "POST" {
			n++
		}
	}
	return n
}

// A close whose closer GitHub won't show the App, FORBIDDEN, or a
// project's workflow's, is no merge: eve's is reverted once, with its
// comment, and nothing is asked again, on the next pass or the one after.
func TestClosingCloserForbidden(t *testing.T) {
	t.Parallel()
	for name, close := range map[string]func(f *githubtest.Fake) (int64, string){
		"forbidden": func(f *githubtest.Fake) (int64, string) {
			id := f.CloseByPullRequest(1, "eve", githubtest.ClosingPR{Merged: true})
			f.ForbidCloser(1, id)
			return id, "`eve`"
		},
		"a project's": func(f *githubtest.Fake) (int64, string) {
			return f.CloseByProject(1, "github-project-automation[bot]"), "`github-project-automation[bot]`"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, o := closingFake(t), digestNotifyOrigin(t)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			event, by := close(f)
			iterate(t, d)
			if is := f.State().Issues[0]; is.Closed || !slices.Equal(closingComments(f, 1, 1), []string{closingReverted(by, event)}) {
				t.Fatalf("closed %t, comments %q; want the close reverted, with its comment", is.Closed, closingComments(f, 1, 1))
			}
			reqs, writes := len(f.Requests()), len(f.Writes())
			iterate(t, d)
			iterate(t, d)
			if n := closingGraphQL(f, reqs); n != 0 || len(f.Writes()) != writes {
				t.Errorf("%d GraphQL requests and writes %+v after the revert; want none", n, f.Writes()[writes:])
			}
		})
	}
}

// Step 1 judges a write collaborator's close whose closer GitHub won't
// show the App as no merge too: the reopened issue is held for it, asked
// once, and a second pass asks nothing.
func TestClosingCloserForbiddenStep1(t *testing.T) {
	t.Parallel()
	var event int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := closingFake(t)
		event = f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
		f.ForbidCloser(1, event)
		f.ReopenBy(1, app) // reverted while no daemon ran its step 1
		return f
	}
	githubtest.Replay(t, setup, closingPass(t, digestNotifyOrigin(t)))
	f, o := setup(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	key := fmt.Sprint(event)
	want := []string{discHeld("pr-open", "`mallory` closed this issue, which I reopened",
		"from=pr-open to=held at=pr-open origin=pr-open reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:held"}) || !slices.Equal(closingComments(f, 1, 1), want) {
		t.Fatalf("labels %q, comments %q; want the hold", is.Labels, closingComments(f, 1, 1))
	}
	reqs := len(f.Requests())
	iterate(t, d)
	if n := closingGraphQL(f, reqs); n != 1 {
		t.Errorf("%d GraphQL requests on the next pass; want 1, the snapshot's, and no closer question", n)
	}
}

// closingFailure is the message of the error FailCloser puts under node.
const closingFailure = "Something went wrong while executing your query."

// An error under node that is neither FORBIDDEN nor NOT_FOUND, an untyped
// one or one of another type, says nothing of how the issue was closed
// (#204): a writer's merge is neither reverted nor held, the pass fails,
// and nothing is remembered, so each pass asks again, and the first one
// after the error has gone sees the merge, which then stands.
func TestClosingCloserNodeErrorRetried(t *testing.T) {
	t.Parallel()
	for name, typ := range map[string]string{"untyped": "", "INTERNAL": "INTERNAL"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, o := closingFake(t), digestNotifyOrigin(t)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			event := f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
			f.FailCloser(1, event, typ, closingFailure)
			for pass := range 2 {
				writes, reqs := len(f.Writes()), len(f.Requests())
				if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("closer of event CE_%d", event)) ||
					!strings.Contains(err.Error(), closingFailure) {
					t.Fatalf("pass %d: Iterate = %v; want the closer question's error", pass+1, err)
				}
				if w := f.Writes()[writes:]; len(w) != 0 {
					t.Fatalf("pass %d wrote %+v; want nothing while how it closed is unknown", pass+1, w)
				}
				if n := closingGraphQL(f, reqs); n == 0 {
					t.Fatalf("pass %d asked no GraphQL question; want the closer asked again", pass+1)
				}
			}
			f.MendCloser(1, event)
			iterate(t, d)
			if is := f.State().Issues[0]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:completed"}) || len(is.Comments) != 1 {
				t.Fatalf("closed %t, labels %q, comments %q; want mallory's merge to stand, at stage:completed, with no comment",
					is.Closed, is.Labels, closingComments(f, 1, 1))
			}
			writes := len(f.Writes())
			iterate(t, d)
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Errorf("the pass after the merge wrote %+v; want nothing", w)
			}
		})
	}
}

// closingReopenCloserFails is closingFake's issue 1 closed by the owner as
// a duplicate and judged discarded, with every question of how that close
// was made failing with an untyped error under node, then reopened by eve
// as event.
func closingReopenCloserFails(event *int64) func(testing.TB) *githubtest.Fake {
	setup := closingJudged("discarded", func(f *githubtest.Fake) {
		f.FailCloser(1, f.CloseAs(1, "owner", "duplicate"), "", closingFailure)
	})
	return then(setup, func(f *githubtest.Fake) { *event = f.ReopenBy(1, "eve") })
}

// When the question of how the close an untrusted reopen undid was made
// fails on every pass, the issue is closed again all the same, once, with
// the closed stage's reason, not_planned for stage:discarded, in place of
// the close's own, duplicate, rather than left open for as long as the
// question fails (#204). The first failure writes nothing and is asked
// again, so the close again comes on the second pass (#233). The close is
// safe to replay, and the next pass writes nothing.
func TestClosingUntrustedReopenCloserFails(t *testing.T) {
	t.Parallel()
	var event int64
	setup := closingReopenCloserFails(&event)
	closerErrCheck(t, setup, 2, func(*githubtest.Fake) closingWant {
		return closingWant{labels: []string{"stage:discarded"}, closed: true, comments: []string{closingClosedAgain("`eve`", event)}}
	})
	f := run(t, setup, closerErrRun(t, digestNotifyOrigin(t), 2, false))
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetStateReason() != "not_planned" {
		t.Errorf("state_reason %q; want not_planned, the closed stage's", is.GetStateReason())
	}
}

// The closed stage's reason stands in for the newest close's when that
// question fails, on its second pass (#233), not an older close's, which
// may be stale.
func TestClosingUntrustedReopenCloserFailsNotOlder(t *testing.T) {
	t.Parallel()
	f := closingJudged("discarded", func(f *githubtest.Fake) {
		f.CloseAs(1, "owner", "duplicate")
		f.ReopenBy(1, "owner")
		f.FailCloser(1, f.CloseAs(1, "owner", "not_planned"), "", closingFailure)
	})(t)
	f.ReopenBy(1, "eve")
	d := newDaemon(t, f, digestNotifyOrigin(t), nil)
	closerErrFailing(t, f, d, "the first pass")
	iterate(t, d)
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetState() != "closed" || is.GetStateReason() != "not_planned" {
		t.Errorf("%s as %s; want closed again as not_planned, the closed stage's, not as the older close's duplicate", is.GetState(), is.GetStateReason())
	}
}

// The close again on the closed stage's reason logs why, once, naming the
// failed question: on the second failing pass, as the first only fails the
// pass (#233), and not again on the pass after.
func TestClosingUntrustedReopenCloserFailsLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	var event int64
	f := closingReopenCloserFails(&event)(t)
	d := newDaemon(t, f, digestNotifyOrigin(t), nil)
	var warnings bytes.Buffer
	stop := logr.AddWriter(&warnings, logr.WithFilter(logr.W))
	closerErrFailing(t, f, d, "the first pass")
	iterate(t, d)
	iterate(t, d)
	logr.Wait()
	stop()
	want := fmt.Sprintf("o/r#1: closing it again as not_planned, the label's reason, as the reason of the close its reopen %d undid is unknown", event)
	if got := warnings.String(); strings.Count(got, "the label's reason") != 1 || !strings.Contains(got, want) || !strings.Contains(got, closingFailure) {
		t.Errorf("warnings = %q; want one, %q, with the question's error", got, want)
	}
}

// A close whose event is late, on an issue the poll's cursor has passed,
// stays due, and is judged once its event shows.
func TestClosingLateEventOlderIssue(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	fileAt(f, "pr-open", "one")
	fileAt(f, "pr-open", "two")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.LagEvents(5 * time.Second)
	f.CloseBy(1, "eve")
	f.AddComment(2, "owner", "a comment") // the newest, at the cursor
	iterate(t, d)
	f.LagEvents(0)
	f.Advance(10 * time.Second)
	f.AddComment(2, "owner", "another")
	iterate(t, d)
	if f.State().Issues[0].Closed {
		t.Error("issue 1 is closed; want eve's close reverted once its event shows")
	}
}

// eve takes the closed stage off a judged issue while the daemon runs: it's
// judged again, and put back.
func TestClosingStageRemoved(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	f.CloseBy(1, "owner")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.RemoveLabel(1, "eve", "stage:completed")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:completed"}) || len(is.Comments) != 1 {
		t.Errorf("labels %q, comments %q; want stage:completed back, with no comment", is.Labels, is.Comments)
	}
}

// A failed write of a close's judgement is the pass's error, and the close
// stays due, so the next pass judges it.
func TestClosingFailedWrite(t *testing.T) {
	t.Parallel()
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

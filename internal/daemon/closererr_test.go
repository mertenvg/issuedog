package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A closer question that keeps failing (#232), and the reason of a close
// again after an untrusted reopen when that question fails once (#233).

// closerErrCause is the error of a closer question of close event that
// FailCloser fails, as the closer state error quotes it.
func closerErrCause(event int64) string {
	return fmt.Sprintf("closer of event CE_%d in o/r: graphql errors: %s", event, closingFailure)
}

// closerErrMarker is the marker of the closer state error for close event.
func closerErrMarker(event int64) string {
	return fmt.Sprintf("<!-- issuedog:error kind=closer key=%d -->", event)
}

// closerErrOnClosed is the closer state error the App posts on a closed
// issue that by closed, as event, whose question failed on passes.
func closerErrOnClosed(by string, event int64, passes int) string {
	return fmt.Sprintf("I can't tell how this issue was closed: asking GitHub whether %s's close was a merge failed on %d passes. "+
		"So I've neither accepted nor reverted it, and its stage label stays as it is. "+
		"I ask again on every pass, and judge the close once GitHub answers.\n\nThe last question failed:\n\n```text\n%s\n```\n\n%s",
		by, passes, closerErrCause(event), closerErrMarker(event))
}

// closerErrOnOpen is the closer state error the App posts on an open issue
// that by closed, as event, whose question failed on passes.
func closerErrOnOpen(by string, event int64, passes int) string {
	return fmt.Sprintf("%s closed this issue, and it was reopened, but I can't tell whether that close was a merge: "+
		"asking GitHub how it was made failed on %d passes. Until GitHub answers, that close holds nothing here. "+
		"I ask again on every pass, and this error ends once it answers.\n\nThe last question failed:\n\n```text\n%s\n```\n\n%s",
		by, passes, closerErrCause(event), closerErrMarker(event))
}

// closerErrDigestLine is the digest's line for the closer state error
// keyed by close event.
func closerErrDigestLine(event int64) string {
	return fmt.Sprintf("State error `closer`, keyed `%d`.", event)
}

// closerErrRun is a new daemon's passes over f, as after a restart, with
// notify owner and the digest kept if digest is set: each pass but the
// last may fail on the closer question, and the last must not fail.
func closerErrRun(t testing.TB, o *origin, passes int, digest bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		d.Digest = digest
		for range passes - 1 {
			if err := d.Iterate(context.Background()); err != nil && !strings.Contains(err.Error(), closingFailure) {
				return err
			}
		}
		return d.Iterate(context.Background())
	}
}

// closerErrCheck is closingCheck for a judgement that takes passes passes
// of one daemon, each but the last failing on the closer question: it runs
// them under Replay (closerErrRun), then on a fresh fake, and checks issue
// 1 against want. Then a further pass of the same daemon, and a restart's,
// write nothing, and read nothing of a closed issue.
func closerErrCheck(t *testing.T, setup func(testing.TB) *githubtest.Fake, passes int, want func(f *githubtest.Fake) closingWant) {
	t.Helper()
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, setup, closerErrRun(t, o, passes, false))
	f := setup(t)
	w := want(f)
	d := newDaemon(t, f, o, nil)
	for pass := range passes - 1 {
		closerErrFailing(t, f, d, fmt.Sprintf("pass %d", pass+1))
	}
	iterate(t, d)
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, w.labels) || is.Closed != w.closed || !slices.Equal(got, w.comments) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant %q, %t and\n%q", is.Labels, is.Closed, got, w.labels, w.closed, w.comments)
	}
	for name, d := range map[string]interface{ Iterate(context.Context) error }{"a further pass": d, "a restart": newDaemon(t, f, o, nil)} {
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

// closerErrFailing runs a pass of d that must fail on the closer question,
// and checks that it wrote nothing to issue 1.
func closerErrFailing(t *testing.T, f *githubtest.Fake, d interface{ Iterate(context.Context) error }, name string) {
	t.Helper()
	writes := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), closingFailure) {
		t.Fatalf("%s: Iterate = %v; want the closer question's error", name, err)
	}
	for _, w := range f.Writes()[writes:] {
		if w.Path == "/issues/1" || strings.HasPrefix(w.Path, "/issues/1/") {
			t.Fatalf("%s wrote %s %s to issue 1; want nothing while how it closed is unknown", name, w.Method, w.Path)
		}
	}
}

// closerErrDigestBody is the digest's body: issue 2, as closingFake's
// issue 1 is the only other.
func closerErrDigestBody(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	issues := f.State().Issues
	if len(issues) < 2 {
		t.Fatal("no digest, issue 2")
	}
	return issues[1].Body
}

// closerErrMerge is closingFake's issue 1 closed by mallory's merge, as
// *event, with every question of how it was made failing with an untyped
// error under node, while no daemon ran.
func closerErrMerge(event *int64) func(testing.TB) *githubtest.Fake {
	return closingWith(func(f *githubtest.Fake) {
		*event = f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
		f.FailCloser(1, *event, "", closingFailure)
	})
}

// A writer's merge whose closer question keeps failing with an untyped
// error under node fails the pass, writing nothing, until it has failed on
// loop.max_attempts passes, 3. Then the closed issue gets one closer state
// error naming the error, the pass no longer fails, and the issue stays
// closed and unjudged at its stage, which the digest lists, as a closed
// issue's error a 👍 can't acknowledge. The post is safe to replay. An
// unchanged pass after it writes nothing, and asks only the failing
// question, as a pass before it did. Once GitHub answers, the merge stands
// and the error leaves the digest.
func TestCloserErrMergeReported(t *testing.T) {
	t.Parallel()
	var event int64
	setup := closerErrMerge(&event)
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, setup, closerErrRun(t, o, 3, true))
	f := setup(t)
	d := digestDaemon(t, f, o, nil)
	closerErrFailing(t, f, d, "pass 1")
	reqs := len(f.Requests())
	closerErrFailing(t, f, d, "pass 2")
	before := closingGraphQL(f, reqs)
	iterate(t, d)
	is := f.State().Issues[0]
	if want := []string{closerErrOnClosed("`mallory`", event, 3)}; !is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) ||
		!slices.Equal(closingComments(f, 1, 1), want) {
		t.Fatalf("closed %t, labels %q, comments\n%q\nwant closed at stage:pr-open, with\n%q", is.Closed, is.Labels, closingComments(f, 1, 1), want)
	}
	body := closerErrDigestBody(t, f)
	if !strings.Contains(body, closerErrDigestLine(event)) || !strings.Contains(body, "The issue is closed, so a 👍 won't acknowledge it") {
		t.Errorf("digest body %q; want the closer state error, which a 👍 can't acknowledge", body)
	}
	iterate(t, d) // the poll lists issue 1 again, for the comment
	for pass := range 2 {
		writes, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		if w := f.Writes()[writes:]; len(w) != 0 {
			t.Errorf("unchanged pass %d wrote %+v; want nothing", pass+1, w)
		}
		if n := closingGraphQL(f, reqs); n != before || n != 1 {
			t.Errorf("unchanged pass %d made %d GraphQL requests; want 1, the failing question, as pass 2 made %d", pass+1, n, before)
		}
		if r := readsOf(f, reqs, 1); len(r) != 1 || r[0].Path != "/issues/1/timeline" {
			t.Errorf("unchanged pass %d read issue 1 by %+v; want only the timeline, which judging reads", pass+1, r)
		}
	}
	f.MendCloser(1, event)
	iterate(t, d)
	if is := f.State().Issues[0]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:completed"}) || len(closingComments(f, 1, 1)) != 1 {
		t.Errorf("closed %t, labels %q, comments %q; want mallory's merge to stand, at stage:completed, with no new comment",
			is.Closed, is.Labels, closingComments(f, 1, 1))
	}
	if body := closerErrDigestBody(t, f); strings.Contains(body, "`closer`") {
		t.Errorf("digest body %q; want the closer state error gone once GitHub answers", body)
	}
}

// A restart forgets the count: the closed issue's question fails the pass
// again until it has failed on loop.max_attempts passes since the start,
// and then its state error is found, not posted again, and the digest
// lists it again.
func TestCloserErrMergeRestart(t *testing.T) {
	t.Parallel()
	var event int64
	f, o := closerErrMerge(&event)(t), digestNotifyOrigin(t)
	if err := closerErrRun(t, o, 3, true)(f); err != nil {
		t.Fatal(err)
	}
	d := digestDaemon(t, f, o, nil)
	closerErrFailing(t, f, d, "the restart's pass 1")
	closerErrFailing(t, f, d, "the restart's pass 2")
	writes := len(f.Writes())
	iterate(t, d)
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/1/") || w.Path == "/issues/1" {
			t.Errorf("the restart's pass 3 wrote %s %s to issue 1; want the state error found, not posted again", w.Method, w.Path)
		}
	}
	if got := closingComments(f, 1, 1); !slices.Equal(got, []string{closerErrOnClosed("`mallory`", event, 3)}) {
		t.Errorf("comments %q; want the one state error", got)
	}
	if body := closerErrDigestBody(t, f); !strings.Contains(body, closerErrDigestLine(event)) {
		t.Errorf("digest body %q; want the closer state error listed again", body)
	}
}

// A closed issue reopened and closed again between two polls has a newer
// close to judge: the older close's state error leaves the digest at once,
// even while the newer close's question fails too.
func TestCloserErrMergeNewerClose(t *testing.T) {
	t.Parallel()
	var event int64
	f, o := closerErrMerge(&event)(t), digestNotifyOrigin(t)
	d := digestDaemon(t, f, o, nil)
	closerErrFailing(t, f, d, "pass 1")
	closerErrFailing(t, f, d, "pass 2")
	iterate(t, d)
	if body := closerErrDigestBody(t, f); !strings.Contains(body, closerErrDigestLine(event)) {
		t.Fatalf("digest body %q; want the closer state error", body)
	}
	f.ReopenBy(1, "owner")
	newer := f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
	f.FailCloser(1, newer, "", closingFailure)
	closerErrFailing(t, f, d, "the pass after the newer close")
	if body := closerErrDigestBody(t, f); strings.Contains(body, "`closer`") {
		t.Errorf("digest body %q; want the older close's error gone, and none yet for the newer", body)
	}
}

// closerErrStep1 is closingFake's issue 1 closed by mallory, a writer, by
// her merge, as *event, with every question of how it was made failing
// with an untyped error under node, and the close reverted by the App,
// all while no daemon ran, so step 1 asks how it was made, as a trigger.
func closerErrStep1(event *int64) func(testing.TB) *githubtest.Fake {
	return closingWith(func(f *githubtest.Fake) {
		*event = f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
		f.FailCloser(1, *event, "", closingFailure)
		f.ReopenBy(1, app)
	})
}

// In step 1, a writer's close whose closer question keeps failing with an
// untyped error under node fails the pass, writing nothing, until it has
// failed on loop.max_attempts passes. Then the open issue gets one closer
// state error, and the close holds nothing, so the pass carries on, and
// the digest lists the error, open. The post is safe to replay. An
// unchanged pass after it writes nothing, and asks only the failing
// question; a restart finds the error, which stands at once. Once GitHub
// answers, the error ends: the merge holds nothing, and the digest drops
// it.
func TestCloserErrStep1Reported(t *testing.T) {
	t.Parallel()
	var event int64
	setup := closerErrStep1(&event)
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, setup, closerErrRun(t, o, 3, false))
	f := setup(t)
	d := digestDaemon(t, f, o, nil)
	closerErrFailing(t, f, d, "pass 1")
	closerErrFailing(t, f, d, "pass 2")
	iterate(t, d)
	is := f.State().Issues[0]
	if want := []string{closerErrOnOpen("`mallory`", event, 3)}; is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) ||
		!slices.Equal(closingComments(f, 1, 1), want) {
		t.Fatalf("closed %t, labels %q, comments\n%q\nwant open at stage:pr-open, not held, with\n%q", is.Closed, is.Labels, closingComments(f, 1, 1), want)
	}
	if body := closerErrDigestBody(t, f); !strings.Contains(body, closerErrDigestLine(event)) || !strings.Contains(body, "React 👍 to the error comment") {
		t.Errorf("digest body %q; want the open closer state error", body)
	}
	iterate(t, d) // the poll lists issue 1 again, for the comment
	for _, name := range []string{"an unchanged pass", "the next"} {
		writes, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		if w := f.Writes()[writes:]; len(w) != 0 {
			t.Errorf("%s wrote %+v; want nothing", name, w)
		}
		if n := closingGraphQL(f, reqs); n != 1 {
			t.Errorf("%s made %d GraphQL requests; want 1, the failing question", name, n)
		}
	}
	restart := digestDaemon(t, f, o, nil)
	writes := len(f.Writes())
	iterate(t, restart)
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/1/") || w.Path == "/issues/1" {
			t.Errorf("a restart wrote %s %s to issue 1; want the state error standing at once", w.Method, w.Path)
		}
	}
	f.MendCloser(1, event)
	iterate(t, restart)
	if is := f.State().Issues[0]; is.Closed || !slices.Equal(is.Labels, []string{"stage:pr-open"}) || len(closingComments(f, 1, 1)) != 1 {
		t.Errorf("closed %t, labels %q, comments %q; want mallory's merge to hold nothing", is.Closed, is.Labels, closingComments(f, 1, 1))
	}
	if body := closerErrDigestBody(t, f); strings.Contains(body, "`closer`") {
		t.Errorf("digest body %q; want the closer state error ended once GitHub answers", body)
	}
}

// GraphQL failing as a whole, as in an outage, says nothing of the close
// itself: each pass fails, without bound, and no state error is posted.
func TestCloserErrOutageUnbounded(t *testing.T) {
	t.Parallel()
	f, o := closingFake(t), digestNotifyOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true})
	for pass := range 5 {
		f.FailGraphQL("boom") // the next GraphQL answer, the closer question's
		writes := len(f.Writes())
		if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("pass %d: Iterate = %v; want GraphQL's error", pass+1, err)
		}
		if w := f.Writes()[writes:]; len(w) != 0 {
			t.Fatalf("pass %d wrote %+v; want nothing", pass+1, w)
		}
	}
}

// closerErrReClose is closingFake's issue 1 closed by the owner as a
// duplicate, as *closed, and judged discarded, with every question of how
// that close was made failing with an untyped error under node, then
// reopened by eve, as *reopen.
func closerErrReClose(closed, reopen *int64) func(testing.TB) *githubtest.Fake {
	setup := closingJudged("discarded", func(f *githubtest.Fake) {
		*closed = f.CloseAs(1, "owner", "duplicate")
		f.FailCloser(1, *closed, "", closingFailure)
	})
	return then(setup, func(f *githubtest.Fake) { *reopen = f.ReopenBy(1, "eve") })
}

// closerErrStateReason is issue 1's state and state_reason on f.
func closerErrStateReason(t *testing.T, f *githubtest.Fake) (string, string) {
	t.Helper()
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	return is.GetState(), is.GetStateReason()
}

// One transient failure of the question of the close an untrusted reopen
// undid writes nothing, and the issue is closed again on the next pass,
// once the question answers, with the close's own reason, duplicate, not
// the closed stage's (#233). That is safe to replay.
func TestCloserErrReCloseTransient(t *testing.T) {
	t.Parallel()
	var closed, reopen int64
	setup := closerErrReClose(&closed, &reopen)
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, setup, func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil && !strings.Contains(err.Error(), closingFailure) {
			return err
		}
		f.MendCloser(1, closed)
		return d.Iterate(context.Background())
	})
	f := setup(t)
	d := newDaemon(t, f, o, nil)
	writes := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "is asked again next pass") ||
		!strings.Contains(err.Error(), closingFailure) {
		t.Fatalf("pass 1: Iterate = %v; want the question asked again next pass", err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Fatalf("pass 1 wrote %+v; want nothing on one failure", w)
	}
	f.MendCloser(1, closed)
	iterate(t, d)
	if st, reason := closerErrStateReason(t, f); st != "closed" || reason != "duplicate" {
		t.Errorf("%s as %s; want closed again as duplicate, the close's own reason", st, reason)
	}
	if got := closingComments(f, 1, 1); !slices.Equal(got, []string{closingClosedAgain("`eve`", reopen)}) {
		t.Errorf("comments %q; want the one close-again comment", got)
	}
}

// A restart forgets a failure of that question: the restarted daemon's
// first failure writes nothing either, and the issue is closed again, as
// not_planned, the closed stage's reason, only on its second.
func TestCloserErrReCloseRestart(t *testing.T) {
	t.Parallel()
	var closed, reopen int64
	f, o := closerErrReClose(&closed, &reopen)(t), digestNotifyOrigin(t)
	closerErrFailing(t, f, newDaemon(t, f, o, nil), "the first start's pass")
	d := newDaemon(t, f, o, nil)
	closerErrFailing(t, f, d, "the restart's pass 1")
	iterate(t, d)
	if st, reason := closerErrStateReason(t, f); st != "closed" || reason != "not_planned" {
		t.Errorf("%s as %s; want closed again as not_planned, the closed stage's", st, reason)
	}
}

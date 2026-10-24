package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// The digest keeps updating while its pin check or pin fails (#226).

// digestPinFailCheckNote is the pin note while the isPinned check fails
// with an HTTP 502.
const digestPinFailCheckNote = "I couldn't pin this issue yet: checking whether it's pinned failed (HTTP 502). " +
	"I'll try again when the list changes, or pin it by hand.\n\n"

// digestPinFailListed is the digest's list, under any pin note, with one
// item: issue n's error from digestError keyed 5, the fake's second
// comment.
func digestPinFailListed(n int) string {
	return fmt.Sprintf("1 item waits on you.\n\n### 1. #%d: A\n\n**State error**, priority p2, unblocks 1 open issue (itself included)\n\n"+
		"- State error `disagreement`, keyed `5`.\n- The stage label disagreed with the marker, so I put it back.\n"+
		"- React 👍 to the error comment to acknowledge it.\n- Answer here: https://github.com/o/r/issues/%d#issuecomment-1002\n\n"+
		"<!-- issuedog:digest -->", n, n)
}

// digestPinFailChecks counts the requests from the from'th on that
// FailIsPinned(502) answered: the isPinned checks, as nothing else
// answers a 502.
func digestPinFailChecks(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Path == "/graphql" && r.Status == http.StatusBadGateway {
			n++
		}
	}
	return n
}

// With isPinned failing on every pass, the failure is logged and named in
// the note, and a new item still updates the body and is announced once.
// An unchanged pass makes no request about the digest, and a changed one
// checks the pin once more, with no write of its own. The next start
// checks again, and the same failure leaves the body as it is; once the
// check works, the digest is pinned and the note goes.
func TestDigestPinFailCheckEveryPass(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	defer stop()
	f, o := newFake(t), digestNotifyOrigin(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	f.FailIsPinned(http.StatusBadGateway)
	d := digestDaemon(t, f, o, nil)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != digestPinFailCheckNote+digestList {
		t.Fatalf("writes %q, body %q; want the edit that names the failure", got, f.State().Issues[0].Body)
	}
	logr.Wait()
	if want := "pinning o/r#1, the digest, failed, so its body says so, and a pass that changes it tries again: checking whether it's pinned failed: "; !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q; want %q", logs.String(), want)
	}
	reqs := len(f.Requests())
	iterate(t, d)
	if got := digestRequests(f, reqs); len(got) != 0 {
		t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
	}
	n := digestError(f, "A", "5")
	reqs = len(f.Requests())
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Fatalf("writes %q with a new item; want its announcement and the edit", got)
	}
	if got := digestPinFailChecks(f, reqs); got != 1 {
		t.Errorf("the changed pass checked the pin %d times; want once", got)
	}
	key := fmt.Sprintf("err-%d-disagreement-5", n)
	if c := digestComments(f, 1); len(c) != 1 || !strings.HasSuffix(c[0], "<!-- issuedog:digest-new items="+key+" -->") {
		t.Errorf("comments = %q; want one announcement of %s", c, key)
	}
	body := f.State().Issues[0].Body
	if !strings.HasPrefix(body, digestPinFailCheckNote) || !strings.Contains(body, fmt.Sprintf("### 1. #%d: A\n", n)) || f.State().Issues[0].Pinned {
		t.Errorf("body = %q; want the note, then the item listed, and no pin", body)
	}
	reqs = len(f.Requests())
	for range 2 {
		if got := digestPass(t, f, d); len(got) != 0 {
			t.Errorf("a later pass wrote %q; want nothing, and no second announcement", got)
		}
	}
	if got := digestPinFailChecks(f, reqs); got != 0 {
		t.Errorf("unchanged passes checked the pin %d times; want none", got)
	}
	reqs = len(f.Requests())
	if got := digestPass(t, f, digestDaemon(t, f, o, nil)); len(got) != 0 || digestPinFailChecks(f, reqs) != 1 {
		t.Errorf("the next start wrote %q, with %d checks; want nothing written, after one check", got, digestPinFailChecks(f, reqs))
	}
	f.FailIsPinned(0)
	got := digestPass(t, f, digestDaemon(t, f, o, nil))
	if is := f.State().Issues[0]; !reflect.DeepEqual(got, []string{"POST /graphql", "PATCH /issues/1"}) || !is.Pinned || strings.HasPrefix(is.Body, "I couldn't pin") {
		t.Errorf("writes %q, issue %+v once the check works; want it pinned, and the note gone", got, is)
	}
}

// Replay passes for the rewrite while the isPinned check fails: the
// announcement and the edit that lists the item under the failure's note.
func TestDigestPinFailCheckReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "issuedog digest", digestPending)
		digestError(f, "A", "5")
		f.FailIsPinned(http.StatusBadGateway)
		return f
	}
	pass := func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) ||
		!strings.HasPrefix(f.State().Issues[0].Body, digestPinFailCheckNote) {
		t.Fatalf("writes %q, body %q; want the announcement, then the edit under the note", got, f.State().Issues[0].Body)
	}
	githubtest.Replay(t, setup, pass)
}

// A 401 or 403 from pinIssue is a refusal, as a GraphQL error is: the note
// names it, and the same start doesn't try again. A 500 or a 404 isn't:
// the note says pinning failed, and a pass that changes the body tries
// again.
func TestDigestPinFailHTTPStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		note   string
		again  []string // the writes of the next pass that changes the body
	}{
		{http.StatusUnauthorized, "I couldn't pin this issue: `HTTP 401: Unauthorized`. Pin it by hand.", []string{"PATCH /issues/1"}},
		{http.StatusForbidden, "I couldn't pin this issue: `HTTP 403: Forbidden`. Pin it by hand.", []string{"PATCH /issues/1"}},
		{http.StatusInternalServerError, "I couldn't pin this issue yet: pinning it failed (HTTP 500). I'll try again when the list changes, or pin it by hand.",
			[]string{"POST /graphql", "PATCH /issues/1"}},
		// A 404 from /graphql itself isn't the digest gone either (#428).
		{http.StatusNotFound, "I couldn't pin this issue yet: pinning it failed (HTTP 404). I'll try again when the list changes, or pin it by hand.",
			[]string{"POST /graphql", "PATCH /issues/1"}},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.AddIssue(app, "issuedog digest", digestPending)
			f.FailPinStatus(tc.status)
			d := digestDaemon(t, f, newOrigin(t), nil)
			if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /graphql", "PATCH /issues/1"}) || f.State().Issues[0].Body != tc.note+"\n\n"+digestList {
				t.Fatalf("writes %q, body %q; want the pin, then the edit with %q", got, f.State().Issues[0].Body, tc.note)
			}
			reqs := len(f.Requests())
			iterate(t, d)
			if got := digestRequests(f, reqs); len(got) != 0 {
				t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
			}
			digestError(f, "A", "5")
			if got := digestPass(t, f, d); !reflect.DeepEqual(got, tc.again) || !strings.HasPrefix(f.State().Issues[0].Body, tc.note+"\n\n") || f.State().Issues[0].Pinned {
				t.Errorf("writes %q, body %q once the list changed; want %q, the note kept, and no pin", got, f.State().Issues[0].Body, tc.again)
			}
		})
	}
}

// Replay passes for the rewrite when the pin is refused with a 403: the
// pin, the edit with its note, then the announcement.
func TestDigestPinFail403Replay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "issuedog digest", digestPending)
		digestError(f, "A", "5")
		f.FailPinStatus(http.StatusForbidden)
		return f
	}
	pass := func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /graphql", "PATCH /issues/1", "POST /issues/1/comments"}) ||
		!strings.HasPrefix(f.State().Issues[0].Body, "I couldn't pin this issue: `HTTP 403: Forbidden`. Pin it by hand.\n\n") {
		t.Fatalf("writes %q, body %q; want the pin, the edit with its note, then the announcement", got, f.State().Issues[0].Body)
	}
	githubtest.Replay(t, setup, pass)
}

// After a failed check, an edit that takes the note out is put right on
// the next pass, which checks again, as it writes the body anyway: so the
// digest is pinned once the check works, and the note goes for good.
func TestDigestPinFailNoteEditedOut(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	f.FailIsPinned(http.StatusBadGateway)
	d := digestDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.EditBody(1, "owner", digestList)
	reqs := len(f.Requests())
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != digestPinFailCheckNote+digestList ||
		digestPinFailChecks(f, reqs) != 1 {
		t.Fatalf("writes %q, body %q, %d checks after the edit; want one check, and the note put back", got, f.State().Issues[0].Body, digestPinFailChecks(f, reqs))
	}
	// Edited out again once the check works: the pin is all that's
	// written, as the body is already the one without the note.
	f.FailIsPinned(0)
	f.EditBody(1, "owner", digestList)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /graphql"}) || !f.State().Issues[0].Pinned ||
		f.State().Issues[0].Body != digestList {
		t.Errorf("writes %q, issue %+v once the check works; want only the pin, and the body without the note", got, f.State().Issues[0])
	}
	if got := digestPass(t, f, d); len(got) != 0 {
		t.Errorf("the next pass wrote %q; want nothing", got)
	}
}

// Replay passes for the rewrite when pinIssue fails with a 5xx, which
// isn't a refusal: the pin, the announcement, then the edit that lists the
// item under the failure's note.
func TestDigestPinFail5xxReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "issuedog digest", digestPending)
		digestError(f, "A", "5")
		f.FailPinStatus(http.StatusInternalServerError)
		return f
	}
	pass := func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	note := "I couldn't pin this issue yet: pinning it failed (HTTP 500). I'll try again when the list changes, or pin it by hand.\n\n"
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /graphql", "POST /issues/1/comments", "PATCH /issues/1"}) ||
		f.State().Issues[0].Body != note+digestPinFailListed(2) || f.State().Issues[0].Pinned {
		t.Fatalf("writes %q, body %q; want the pin, the announcement, then the edit under the note, and no pin", got, f.State().Issues[0].Body)
	}
	githubtest.Replay(t, setup, pass)
}

// Replay passes for a digest created while the isPinned check fails: the
// create, the announcement, then the edit that swaps the create's note for
// the failure's, with no pin.
func TestDigestPinFailCheckCreateReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		digestError(f, "A", "5")
		f.FailIsPinned(http.StatusBadGateway)
		return f
	}
	pass := func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /issues", "POST /issues/2/comments", "PATCH /issues/2"}) ||
		f.State().Issues[1].Body != digestPinFailCheckNote+digestPinFailListed(1) || f.State().Issues[1].Pinned {
		t.Fatalf("writes %q, body %q; want the create, the announcement, then the edit under the note, and no pin", got, f.State().Issues[1].Body)
	}
	githubtest.Replay(t, setup, pass)
}

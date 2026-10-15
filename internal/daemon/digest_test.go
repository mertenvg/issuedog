package daemon_test

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// The digest's body as T7 writes it: a placeholder list and the identity
// marker, under the pin note while there is one.
const (
	digestList    = "This issue will list what waits on you: open questions, pending approvals and state errors.\n\n<!-- issuedog:digest -->"
	digestPending = "I couldn't pin this issue yet. If this line stays, pin it by hand.\n\n" + digestList
)

// digestDaemon is newDaemon with the digest kept.
func digestDaemon(t testing.TB, f *githubtest.Fake, o *origin, visits *[]int) *daemon.Daemon {
	t.Helper()
	d := newDaemon(t, f, o, visits)
	d.Digest = true
	return d
}

// digestWrites is f's writes from the from'th on, as "METHOD /path".
func digestWrites(f *githubtest.Fake, from int) []string {
	var got []string
	for _, w := range f.Writes()[from:] {
		got = append(got, w.Method+" "+w.Path)
	}
	return got
}

// digestRequests is the requests from the from'th on that name issue 1 or
// are GraphQL: those about the digest, which is issue 1.
func digestRequests(f *githubtest.Fake, from int) []githubtest.Request {
	var got []githubtest.Request
	for _, r := range f.Requests()[from:] {
		if r.Path == "/graphql" || r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/") {
			got = append(got, r)
		}
	}
	return got
}

// The first Iterate creates one digest and pins it, and once it's pinned,
// its body loses the pin note. Later passes, and a new daemon on the same
// fake, find it and write nothing, and an unchanged pass makes no request
// about it.
func TestDigestCreatedAndPinnedOnce(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.W))
	defer stop()
	f, o := newFake(t), newOrigin(t)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	if got, want := digestWrites(f, 0), []string{"POST /issues", "POST /graphql", "PATCH /issues/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	is := f.State().Issues
	if len(is) != 1 || is[0].Title != "issuedog digest" || is[0].Author != app || is[0].Body != digestList || !is[0].Pinned || is[0].Closed {
		t.Fatalf("issues = %+v; want one open, pinned digest of the App's, with no pin note", is)
	}
	if body := f.Writes()[0].Body; !strings.Contains(body, "I couldn't pin this issue yet.") {
		t.Errorf("the create's body = %s; want the pin note, so a crash before the pin leaves it", body)
	}
	for i, next := range []*daemon.Daemon{d, d, digestDaemon(t, f, o, nil)} {
		from, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, next)
		if w := digestWrites(f, from); len(w) != 0 || len(f.State().Issues) != 1 {
			t.Errorf("pass %d wrote %q, issues %+v; want nothing written or created", i+2, w, f.State().Issues)
		}
		if got := digestRequests(f, reqs); i == 1 && len(got) != 0 {
			t.Errorf("the unchanged pass sent %+v about the digest; want none", got)
		}
	}
	logr.Wait()
	if strings.Contains(logs.String(), "more than one digest") {
		t.Errorf("logs = %q; want no word of other digests", logs.String())
	}
}

// A failed create is the pass's error, after which Work still runs, and the
// next pass creates the digest.
func TestDigestCreateFailure(t *testing.T) {
	f := newFake(t)
	d := digestDaemon(t, f, newOrigin(t), nil)
	worked := 0
	d.Work = func(context.Context, []daemon.Ready) error { worked++; return nil }
	f.FailWrite(1)
	if err := d.Iterate(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "create the digest: ") || worked != 1 {
		t.Fatalf("Iterate = %v, Work ran %d times; want the create's error, and Work run", err, worked)
	}
	iterate(t, d)
	if is := f.State().Issues; len(is) != 1 || !is[0].Pinned {
		t.Errorf("issues = %+v; want one pinned digest", is)
	}
}

// Replay passes for the create, the pin and the edit that drops the note,
// including a crash after the create was applied, which the next pass finds.
func TestDigestCreateReplay(t *testing.T) {
	o := newOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake { return newFake(t) },
		func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
}

// Triage, registered in the rig, runs on the issue a user filed with the
// digest's marker, which isn't a digest, and never on the digest the
// daemon creates beside it.
func TestDigestNeverTriaged(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Not a digest", "Please list things. <!-- issuedog:digest -->")
	g := triageRig(t, f, triageOrigin(t), t.TempDir())
	g.d.Digest = true
	g.claude(triageOut("leave", true, triageReason, triageSummary), 0)
	for range 3 {
		g.pass()
	}
	is := f.State().Issues
	if len(is) != 2 || is[1].Title != "issuedog digest" || !is[1].Pinned || len(is[1].Comments) != 0 || len(is[1].Labels) != 0 {
		t.Fatalf("issues = %+v; want the user's, then a pinned digest with no comments or labels", is)
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{1}) {
		t.Errorf("runs on %v; want one triage run, on the user's issue 1", got)
	}
}

// With two digests, the lower number is the digest and the other is
// logged, and neither is in the pipeline: step 1 never visits them, nor
// creates another. Their marker is read even once an edit made the body
// dirty. The user's issue with the marker is visited as any other.
func TestDigestLowestWinsAndStaysOut(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.W))
	defer stop()
	f := newFake(t)
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
	f.AddIssue(app, "issuedog digest", digestList)
	f.AddIssue("owner", "Mine", digestList)
	f.EditBody(1, "mallory", "Edited. <!-- issuedog:digest -->")
	var visits []int
	d := digestDaemon(t, f, newOrigin(t), &visits)
	iterate(t, d)
	iterate(t, d)
	logr.Wait()
	if w := f.Writes(); len(w) != 0 || len(f.State().Issues) != 3 {
		t.Errorf("writes %+v, issues %+v; want none, and no new digest", w, f.State().Issues)
	}
	if !slices.Equal(visits, []int{3}) {
		t.Errorf("step 1 visited %v; want only the user's issue 3", visits)
	}
	if want := "o/r has more than one digest: #1 is the digest, and the App's issues [2], which carry its marker too, are left alone"; strings.Count(logs.String(), want) != 1 {
		t.Errorf("logs = %q; want %q once", logs.String(), want)
	}
}

// A pin GitHub refuses, as with three pinned issues already, is logged, the
// note names GitHub's message, escaped, and the loop carries on to Work.
// The same daemon doesn't try again; the next start does, and once the pin
// works, the note goes. An unpin by the owner after that stands.
func TestDigestPinFailure(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	defer stop()
	f, o := newFake(t), newOrigin(t)
	f.FailPin("Repository already has 3 pinned issues <!-- issuedog:x --> @eve `x`")
	d := digestDaemon(t, f, o, nil)
	worked := 0
	d.Work = func(context.Context, []daemon.Ready) error { worked++; return nil }
	iterate(t, d)
	logr.Wait()
	note := "I couldn't pin this issue: `Repository already has 3 pinned issues &lt;!-- issuedog:x --> @​eve 'x'`. Pin it by hand.\n\n"
	if is := f.State().Issues[0]; is.Pinned || is.Body != note+digestList || worked != 1 {
		t.Fatalf("issue %+v, Work ran %d times; want unpinned, with the note, and Work run", is, worked)
	}
	if want := "pinning o/r#1, the digest, failed, so its body says so, and the next start tries again: "; !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q; want %q", logs.String(), want)
	}
	reqs := len(f.Requests())
	iterate(t, d)
	if got := digestRequests(f, reqs); len(got) != 0 {
		t.Errorf("the same daemon sent %+v about the digest; want no second try", got)
	}
	// The next start tries again; the same refusal leaves the body as it is.
	from := len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if got := digestWrites(f, from); !reflect.DeepEqual(got, []string{"POST /graphql"}) || f.State().Issues[0].Body != note+digestList {
		t.Errorf("writes %q on the next start; want only the pin, and the note kept", got)
	}
	f.FailPin("")
	from = len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if is := f.State().Issues[0]; !is.Pinned || is.Body != digestList || !reflect.DeepEqual(digestWrites(f, from), []string{"POST /graphql", "PATCH /issues/1"}) {
		t.Errorf("issue %+v after %q; want it pinned on the next start, and its note gone", is, digestWrites(f, from))
	}
	f.SetPinned(1, false)
	from = len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if w := digestWrites(f, from); len(w) != 0 || f.State().Issues[0].Pinned {
		t.Errorf("wrote %q after the owner's unpin; want it to stand", w)
	}
}

// A digest pinned by hand while its note is there loses the note on the next
// start, with no pin, as IsPinned is asked first.
func TestDigestPinnedByHand(t *testing.T) {
	f := newFake(t)
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestPending), true)
	iterate(t, digestDaemon(t, f, newOrigin(t), nil))
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != digestList {
		t.Errorf("writes %q, body %q; want only the edit that drops the note", got, f.State().Issues[0].Body)
	}
}

// Replay passes for a refused pin and its note.
func TestDigestPinFailureReplay(t *testing.T) {
	o := newOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "issuedog digest", digestPending)
		f.FailPin("Forbidden")
		return f
	}, func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
}

// A trusted close turns the digest off: nothing is written to it, and no
// other is created, across a restart too. An untrusted reopen of it is
// reverted, as is an untrusted close of an open digest, by a user or another
// bot. A trusted reopen turns it on. A pass after a revert's reads makes no
// request about the digest.
func TestDigestClose(t *testing.T) {
	f, o := newFake(t), newOrigin(t)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	step := func(name string, change func(), want []string, closed bool) {
		t.Helper()
		change()
		from := len(f.Writes())
		iterate(t, d)
		iterate(t, d)
		is := f.State().Issues
		if got := digestWrites(f, from); !reflect.DeepEqual(got, want) || len(is) != 1 || is[0].Closed != closed {
			t.Errorf("%s: writes %q, issues %+v; want %q, and the digest closed %t", name, got, is, want, closed)
		}
		reqs := len(f.Requests())
		iterate(t, d)
		if got := digestRequests(f, reqs); len(got) != 0 {
			t.Errorf("%s: an unchanged pass sent %+v about the digest; want none", name, got)
		}
	}
	step("owner closes", func() { f.CloseBy(1, "owner") }, nil, true)
	step("mallory reopens", func() { f.ReopenBy(1, "mallory") }, []string{"PATCH /issues/1"}, true)
	step("owner reopens", func() { f.ReopenBy(1, "owner") }, nil, false)
	step("mallory closes", func() { f.CloseBy(1, "mallory") }, []string{"PATCH /issues/1"}, false)
	step("a bot closes", func() { f.CloseBy(1, "other[bot]") }, []string{"PATCH /issues/1"}, false)
	step("a ghost closes", func() { f.Close(1) }, []string{"PATCH /issues/1"}, false)
	step("owner closes again", func() { f.CloseBy(1, "owner") }, nil, true)

	// What stands is read from the timeline, so a new daemon finds it too.
	f.ReopenBy(1, "mallory")
	from := len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if got := digestWrites(f, from); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || !f.State().Issues[0].Closed {
		t.Errorf("a new daemon wrote %q on mallory's reopen; want it closed again", got)
	}
	from = len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if got := digestWrites(f, from); len(got) != 0 || len(f.State().Issues) != 1 {
		t.Errorf("a new daemon wrote %q on the closed digest; want nothing, and no new digest", got)
	}
}

// A close whose event GitHub hasn't written yet is looked at again each
// pass, with nothing reverted, until the event shows: then an untrusted
// close is reverted, and a trusted one stands.
func TestDigestCloseEventLate(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.I))
	defer stop()
	f := newFake(t)
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
	d := digestDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.LagEvents(time.Minute)
	f.CloseBy(1, "mallory")
	iterate(t, d)
	iterate(t, d)
	logr.Wait()
	if w := f.Writes(); len(w) != 0 || !f.State().Issues[0].Closed {
		t.Fatalf("wrote %+v before the closed event was written; want nothing", w)
	}
	if want := "o/r#1, the digest, has no closed event yet, so it's looked at again next pass"; strings.Count(logs.String(), want) != 2 {
		t.Errorf("logs = %q; want %q on each pass", logs.String(), want)
	}
	f.Advance(time.Minute)
	iterate(t, d)
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Closed {
		t.Errorf("writes %q once the event showed; want the reopen", got)
	}
	f.CloseBy(1, "owner") // its event is late too
	iterate(t, d)
	f.Advance(time.Minute)
	iterate(t, d)
	iterate(t, d)
	if got := digestWrites(f, 1); len(got) != 0 || !f.State().Issues[0].Closed {
		t.Errorf("writes %q on the owner's late close; want none", got)
	}
	// While the owner's reopen has no event, the owner's close is newest:
	// the digest isn't closed again.
	f.ReopenBy(1, "owner")
	iterate(t, d)
	f.Advance(time.Minute)
	iterate(t, d)
	if got := digestWrites(f, 1); len(got) != 0 || f.State().Issues[0].Closed {
		t.Errorf("writes %q on the owner's late reopen; want none", got)
	}
}

// Replay passes for both reverts.
func TestDigestCloseReplay(t *testing.T) {
	o := newOrigin(t)
	for name, change := range map[string]func(*githubtest.Fake){
		"untrusted close":  func(f *githubtest.Fake) { f.CloseBy(1, "mallory") },
		"untrusted reopen": func(f *githubtest.Fake) { f.CloseBy(1, "owner"); f.ReopenBy(1, "mallory") },
	} {
		t.Run(name, func(t *testing.T) {
			githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
				f := newFake(t)
				f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
				change(f)
				return f
			}, func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
		})
	}
}

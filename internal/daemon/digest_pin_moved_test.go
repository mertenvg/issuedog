package daemon_test

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A digest found gone by its pin check or its pin is dropped, and a digest
// transferred just before its pin is never pinned in its new repo (#413).

// pinMovedFake is a fake whose issue 1 is a new digest, not yet pinned,
// that is transferred to elsewhere/r just before PinIssue reads it, the
// first GET of the issue in a pass: after the isPinned check found it
// unpinned. The fake then pins it in its new repo if asked by its node
// ID, as GitHub most likely would.
func pinMovedFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" },
		func() { f.Transfer(1, "elsewhere/r") })
	return f
}

// pinMovedRequests is the requests from the from'th on that name issue n
// or are GraphQL.
func pinMovedRequests(f *githubtest.Fake, from, n int) []githubtest.Request {
	var got []githubtest.Request
	for _, r := range f.Requests()[from:] {
		if p := "/issues/" + strconv.Itoa(n); r.Path == "/graphql" || r.Path == p || strings.HasPrefix(r.Path, p+"/") {
			got = append(got, r)
		}
	}
	return got
}

// A digest transferred between its isPinned check and its pin isn't
// pinned: PinIssue's read finds it in the other repo, so no mutation is
// sent, and the pass's error marks it gone. Nothing is written to it, and
// it's dropped, so the next pass lists every issue again, finds no digest,
// and creates and pins one. After that, an unchanged pass makes no
// request about the digest.
func TestDigestPinMovedDropped(t *testing.T) {
	t.Parallel()
	f := pinMovedFake(t)
	d := digestDaemon(t, f, newOrigin(t), nil)
	err := d.Iterate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the digest is gone") ||
		!strings.Contains(err.Error(), "pin the digest: pin o/r#1: transferred: answered by GET /repositories/2/issues/1") {
		t.Errorf("Iterate = %v; want the digest gone, as its pin found it transferred", err)
	}
	if got := digestWrites(f, 0); len(got) != 0 {
		t.Errorf("writes = %q; want none: no pin and no edit of the moved digest", got)
	}
	if is := f.State().Issues[0]; is.Pinned || is.Body != digestPending {
		t.Errorf("the moved digest = %+v; want it unpinned in its new repo, its body as it was", is)
	}
	from := len(f.Requests())
	got := digestPass(t, f, d)
	if want := []string{"POST /issues", "POST /graphql", "PATCH /issues/2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes of the pass after = %q; want %q: a new digest, created and pinned", got, want)
	}
	if !digestRunRelisted(f, from) {
		t.Error("the pass after didn't list every issue again; want it to, as the digest was dropped")
	}
	if is := f.State().Issues; len(is) != 2 || is[0].Pinned || !is[1].Pinned || is[1].Body != digestList {
		t.Errorf("issues = %+v; want the moved digest unpinned, and a new one, pinned, listing nothing", is)
	}
	from = len(f.Requests())
	iterate(t, d)
	if got := pinMovedRequests(f, from, 2); len(got) != 0 {
		t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
	}
}

// A pin check GitHub answers with an HTTP 404 from /graphql itself
// doesn't find the digest gone, as that says nothing of the digest (#428):
// it's an ordinary failure, so the pass has no error, the note names it,
// and the digest is kept. While the 404 lasts, an unchanged pass makes no
// request about the digest and lists no issue again. Once it stops, the
// next start pins the same digest, with no second one.
func TestDigestPinMovedCheck404Kept(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	f.FailIsPinned(http.StatusNotFound)
	d := digestDaemon(t, f, o, nil)
	note := "I couldn't pin this issue yet: checking whether it's pinned failed (HTTP 404). I'll try again when the list changes, or pin it by hand.\n\n"
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != note+digestList {
		t.Fatalf("writes %q, body %q; want the edit that names the failure", got, f.State().Issues[0].Body)
	}
	for range 2 {
		from := len(f.Requests())
		iterate(t, d)
		if got := digestRequests(f, from); len(got) != 0 {
			t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
		}
		if digestRunRelisted(f, from) {
			t.Error("an unchanged pass listed every issue again; want the digest kept")
		}
	}
	f.FailIsPinned(0)
	if got := digestPass(t, f, digestDaemon(t, f, o, nil)); !reflect.DeepEqual(got, []string{"POST /graphql", "PATCH /issues/1"}) {
		t.Errorf("writes of the next start = %q; want the same digest pinned, and its note gone", got)
	}
	if is := f.State().Issues; len(is) != 1 || !is[0].Pinned || is[0].Body != digestList {
		t.Errorf("issues = %+v; want the one digest, pinned, listing nothing", is)
	}
}

// Replay passes for a pass that finds the digest transferred at its pin,
// and the next, which creates a new digest and pins it.
func TestDigestPinMovedReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	pass := func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil && !strings.Contains(err.Error(), "the digest is gone") {
			return err
		}
		return d.Iterate(context.Background())
	}
	f := pinMovedFake(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /issues", "POST /graphql", "PATCH /issues/2"}) || f.State().Issues[0].Pinned {
		t.Fatalf("writes %q, moved digest %+v; want a new digest, created and pinned, and the moved one unpinned", got, f.State().Issues[0])
	}
	githubtest.Replay(t, pinMovedFake, pass)
}

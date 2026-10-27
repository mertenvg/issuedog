package daemon_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A digest deleted between PinIssue's GET and the pinIssue mutation is
// found gone by the mutation's NOT_FOUND, not taken as a refusal (#495).

// pinDeletedFake is a fake whose issue 1 is a new digest, not yet pinned,
// that is deleted just before the pinIssue mutation: after PinIssue's GET,
// the first of the issue in a pass, read it. From then on, 1's paths
// answer 404, and, as on GitHub, the next create, the new digest's, gets
// a new number, 2.
func pinDeletedFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	pin := func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/graphql" }
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" },
		func() { f.BeforeRequest(pin, f.DeleteIssue) })
	return f
}

// A digest deleted just before its pin is dropped as gone in that pass:
// the pass's error names the mutation's NOT_FOUND, and the pin is the only
// write, with no edit naming it as a refusal. The next pass lists every
// issue again, finds no digest, and creates and pins one. After that, an
// unchanged pass makes no request about the digest.
func TestDigestPinDeletedDropped(t *testing.T) {
	t.Parallel()
	f := pinDeletedFake(t)
	d := digestDaemon(t, f, newOrigin(t), nil)
	err := d.Iterate(context.Background())
	if !errors.Is(err, github.ErrIssueNotFound) || !strings.Contains(err.Error(), "the digest is gone") ||
		!strings.Contains(err.Error(), "pin the digest: pin o/r#1: GitHub found no such issue: ") {
		t.Errorf("Iterate = %v; want the digest gone, as its pin found no such issue", err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /graphql"}) {
		t.Errorf("writes = %q; want the pin alone, and no edit of the deleted digest", got)
	}
	from := len(f.Requests())
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues", "POST /graphql", "PATCH /issues/2"}) {
		t.Errorf("writes of the pass after = %q; want a new digest, created and pinned", got)
	}
	if !digestRunRelisted(f, from) {
		t.Error("the pass after didn't list every issue again; want it to, as the digest was dropped")
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Number != 2 || !is[0].Pinned || is[0].Body != digestList {
		t.Errorf("issues = %+v; want only a new digest, #2, pinned, listing nothing", is)
	}
	from = len(f.Requests())
	iterate(t, d)
	if got := pinMovedRequests(f, from, 2); len(got) != 0 {
		t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
	}
}

// Replay passes for a pass whose pin finds the digest deleted, and the
// next, which creates a new digest and pins it.
func TestDigestPinDeletedReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	pass := func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil && !strings.Contains(err.Error(), "the digest is gone") {
			return err
		}
		return d.Iterate(context.Background())
	}
	f := pinDeletedFake(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /graphql", "POST /issues", "POST /graphql", "PATCH /issues/2"}) {
		t.Fatalf("writes %q; want the pin, then a new digest, created and pinned", got)
	}
	githubtest.Replay(t, pinDeletedFake, pass)
}

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// The digest's isPinned check finds it gone by GraphQL's NOT_FOUND for
// it, and an HTTP 404 from /graphql itself is an ordinary failure (#428).

// pinNotFoundFake is a fake whose issue 1 is a new digest, not yet
// pinned, that is transferred to elsewhere/r just before the isPinned
// check, the first GraphQL request of a pass, so GitHub answers it with
// NOT_FOUND for the issue.
func pinNotFoundFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/graphql" },
		func() { f.Transfer(1, "elsewhere/r") })
	return f
}

// A digest transferred before its isPinned check is dropped as gone in
// that pass: the pass's error says so, nothing is written, and no
// "pinning failed" line is logged. The next pass lists every issue again,
// finds no digest, and creates and pins one. After that, an unchanged pass
// makes no request about the digest.
func TestDigestPinNotFoundDropped(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	defer stop()
	f := pinNotFoundFake(t)
	d := digestDaemon(t, f, newOrigin(t), nil)
	err := d.Iterate(context.Background())
	if !errors.Is(err, github.ErrIssueNotFound) || !strings.Contains(err.Error(), "the digest is gone") ||
		!strings.Contains(err.Error(), "check the pin of the digest: is o/r#1 pinned: GitHub found no such issue: ") {
		t.Errorf("Iterate = %v; want the digest gone, as its pin check found no such issue", err)
	}
	if got := digestWrites(f, 0); len(got) != 0 {
		t.Errorf("writes = %q; want none: no pin and no edit of the moved digest", got)
	}
	logr.Wait()
	if strings.Contains(logs.String(), "pinning o/r#1, the digest, failed") {
		t.Errorf("logs = %q; want no pinning failure, as the digest is gone", logs.String())
	}
	from := len(f.Requests())
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues", "POST /graphql", "PATCH /issues/2"}) {
		t.Errorf("writes of the pass after = %q; want a new digest, created and pinned", got)
	}
	if !digestRunRelisted(f, from) {
		t.Error("the pass after didn't list every issue again; want it to, as the digest was dropped")
	}
	if is := f.State().Issues; len(is) != 2 || is[0].Pinned || is[0].Body != digestPending || !is[1].Pinned || is[1].Body != digestList {
		t.Errorf("issues = %+v; want the moved digest as it was, and a new one, pinned, listing nothing", is)
	}
	from = len(f.Requests())
	iterate(t, d)
	if got := pinMovedRequests(f, from, 2); len(got) != 0 {
		t.Errorf("an unchanged pass sent %+v about the digest; want none", got)
	}
}

// Replay passes for a pass whose isPinned check finds the digest
// transferred, and the next, which creates a new digest and pins it.
func TestDigestPinNotFoundReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	pass := func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil && !strings.Contains(err.Error(), "the digest is gone") {
			return err
		}
		return d.Iterate(context.Background())
	}
	f := pinNotFoundFake(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !reflect.DeepEqual(got, []string{"POST /issues", "POST /graphql", "PATCH /issues/2"}) || f.State().Issues[0].Pinned {
		t.Fatalf("writes %q, moved digest %+v; want a new digest, created and pinned, and the moved one unpinned", got, f.State().Issues[0])
	}
	githubtest.Replay(t, pinNotFoundFake, pass)
}

// Replay passes for the rewrite while /graphql answers the isPinned check,
// or the pin, with an HTTP 404: the digest is kept, and its writes are
// the ordinary failure's, under its note.
func TestDigestPinGraphQL404Replay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		fail   func(*githubtest.Fake)
		writes []string
		note   string
	}{
		{"check", func(f *githubtest.Fake) { f.FailIsPinned(http.StatusNotFound) },
			[]string{"POST /issues/1/comments", "PATCH /issues/1"},
			"I couldn't pin this issue yet: checking whether it's pinned failed (HTTP 404). "},
		{"pin", func(f *githubtest.Fake) { f.FailPinStatus(http.StatusNotFound) },
			[]string{"POST /graphql", "POST /issues/1/comments", "PATCH /issues/1"},
			"I couldn't pin this issue yet: pinning it failed (HTTP 404). "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := digestNotifyOrigin(t)
			setup := func(t testing.TB) *githubtest.Fake {
				f := newFake(t)
				f.AddIssue(app, "issuedog digest", digestPending)
				digestError(f, "A", "5")
				tc.fail(f)
				return f
			}
			pass := func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) }
			f := setup(t)
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			if got := digestWrites(f, 0); !reflect.DeepEqual(got, tc.writes) || !strings.HasPrefix(f.State().Issues[0].Body, tc.note) || f.State().Issues[0].Pinned {
				t.Fatalf("writes %q, body %q; want %q, under the note, and no pin", got, f.State().Issues[0].Body, tc.writes)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}

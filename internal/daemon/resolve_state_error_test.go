package daemon_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A discussion resolved on an issue whose type the pipeline doesn't know
// leaves nothing: the move to clarifying is a state error, reported once,
// and the issue stays held.
func TestResolveLeaveStateError(t *testing.T) {
	t.Parallel()
	setup := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {
		f.SetType(1, "chore")
		f.AddReaction(1002, "owner", "+1")
	})
	f := run(t, setup, resolvePass(t, 1))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("labels %q; want the issue still held", l)
	}
	for _, c := range f.State().Issues[0].Comments {
		if strings.Contains(c.Body, "to=clarifying") {
			t.Errorf("comment %q; want no move to clarifying", c.Body)
		}
	}
}

// When leaving the hold is a state error, as for a type the pipeline
// doesn't know, the ancestors aren't read (#188): the error is reported
// once, and an unchanged pass costs no rate limit.
func TestResolveNoAncestorReadsOnStateError(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	for n := 1; n <= 3; n++ {
		fileAt(f, "ready", "do it")
		f.SetType(n, "task")
		if n > 1 {
			f.SetParent(n, n-1)
		}
	}
	d := resolveDaemon(t, f, newOrigin(t))
	c := f.AddComment(3, "mallory", "Wait.")
	iterate(t, d)
	f.SetTypeBy(3, "owner", "Feature")
	for range 2 {
		iterate(t, d)
	}
	f.AddReaction(c, "owner", "+1")
	for range 3 {
		iterate(t, d)
	}
	if l := f.State().Issues[2].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("#3: labels %q; want it still held", l)
	}
	writes, requests := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("an unchanged pass wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[requests:] {
		if r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only requests answered 304", r)
		}
	}
}

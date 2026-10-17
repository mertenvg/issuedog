package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// When leaving the hold is a state error, as for a type the pipeline
// doesn't know, the ancestors aren't read (#188's verify, attempt 3): the
// error is reported once, and an unchanged pass costs no rate limit.
func TestResolveNoAncestorReadsOnStateError(t *testing.T) {
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

// A 👍 given after the /resume, on a trigger made before it, doesn't count
// as a discussion that ended while the owner's hold stayed: the /resume
// accepts the trigger itself, and restores at.
func TestResolveThumbAfterResume(t *testing.T) {
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	w := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	f.AddReaction(w, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, body)
	}
}

// Replay over the record of a /resume that would have gone to clarifying,
// kept held by a later trigger: after=1.
func TestResolveAfterRecordReplay(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		w := f.AddComment(1, "mallory", "Wait.")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		f.AddComment(1, "owner", "/resume")
		f.AddComment(1, "mallory", "One more thing.")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, " after=1 ") {
		t.Errorf("labels %q, newest comment %q; want the issue held, with the record carrying after=1", l, body)
	}
}

// Replay over the record that names an ancestor's /hold with no act to
// resolve the last trigger, as mallory deleting her own comment.
func TestResolveHeldRecordReplay(t *testing.T) {
	var hold int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		for n := 1; n <= 3; n++ {
			fileAt(f, "ready", "do it")
			f.SetType(n, "task")
			if n > 1 {
				f.SetParent(n, n-1)
			}
		}
		step := func(n int) {
			if err := resolvePass(t, n)(f); err != nil {
				t.Fatal(err)
			}
		}
		f.AddComment(2, "owner", "/hold")
		step(2)
		c := f.AddComment(3, "mallory", "Wait.")
		step(3)
		f.AddComment(2, "owner", "/resume")
		step(2)
		step(3)
		hold = f.AddComment(1, "owner", "/hold")
		step(1)
		f.ForgetWrites()
		f.DeleteCommentBy("mallory", c)
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=discussion held=%d -->", hold)) {
		t.Errorf("#3: labels %q, newest comment %q; want it held, with the record naming #1's /hold", l, body)
	}
}

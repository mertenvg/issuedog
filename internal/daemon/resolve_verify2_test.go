package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveResumeTwice posts /resume on issue 1 of f, then mallory's comment
// before the pass, which keeps it held with the /resume's record, then a
// second /resume, by passes, and returns that second /resume's ID.
func resolveResumeTwice(t *testing.T, f *githubtest.Fake) int64 {
	t.Helper()
	f.AddComment(1, "owner", "/resume")
	f.AddComment(1, "mallory", "One more thing.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "cmd=") {
		t.Fatalf("labels %q, newest comment %q; want the issue held, with the /resume's record", l, body)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	return resume
}

// A second /resume, after the first kept the issue held for a later
// trigger, restores at (#188's verify, attempt 2): the first one's record
// says no discussion ended, as that /resume itself went back to at. So it
// is from a discussion hold, and from an owner /hold.
func TestResolveSecondResume(t *testing.T) {
	for name, setup := range map[string]func(t testing.TB) *githubtest.Fake{
		"a discussion hold": resolveHeldWith(func(_ testing.TB, _ *githubtest.Fake) {}),
		"an owner /hold": func(t testing.TB) *githubtest.Fake {
			f := discFake(t)
			f.AddComment(1, "owner", "/hold")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			resume := resolveResumeTwice(t, f)
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
				!strings.Contains(body, fmt.Sprintf("Resumed by `/resume`, back to `ready`.\n\n<!-- issuedog:transition from=held to=ready cmd=%d ", resume)) {
				t.Errorf("labels %q, newest comment %q; want the second /resume back to ready", l, body)
			}
		})
	}
}

// A first /resume that would itself have gone to clarifying, a discussion
// resolved while the owner's hold stayed, carries that in its record
// (after=1), so the second /resume goes to clarifying.
func TestResolveSecondResumeAfterDiscussion(t *testing.T) {
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	w := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(w, "owner", "+1")
	resume := resolveResumeTwice(t, f)
	st := f.State().Issues[0]
	var record string
	for _, c := range st.Comments {
		if strings.Contains(c.Body, "issuedog:ended reason=discussion") {
			record = c.Body
		}
	}
	if !strings.Contains(record, "after=1") || !slices.Equal(st.Labels, []string{"stage:clarifying"}) ||
		!strings.Contains(newestComment(f, 1), fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("record %q, labels %q, newest comment %q; want the first /resume's record with after=1, and the second to clarifying",
			record, st.Labels, newestComment(f, 1))
	}
}

// When the last trigger goes with no act to resolve it, as mallory
// deleting her own comment, an ancestor's /hold found keeps the issue held,
// and the record naming it is written all the same (#188's verify, attempt
// 2), with no resolved time, so no later pass reads the ancestors again:
// an unchanged pass costs no rate limit.
func TestResolveAncestorHoldWithNoAct(t *testing.T) {
	f := newFake(t)
	for n := 1; n <= 3; n++ {
		fileAt(f, "ready", "do it")
		f.SetType(n, "task")
		if n > 1 {
			f.SetParent(n, n-1)
		}
	}
	d := resolveDaemon(t, f, newOrigin(t))
	f.AddComment(2, "owner", "/hold")
	iterate(t, d)
	c := f.AddComment(3, "mallory", "Wait.")
	iterate(t, d)
	f.AddComment(2, "owner", "/resume")
	for range 2 {
		iterate(t, d)
	}
	if body := newestComment(f, 3); !strings.Contains(body, "issuedog:ended reason=owner") {
		t.Fatalf("newest comment on #3 %q; want the record that #2's /resume ended its owner hold", body)
	}
	hold := f.AddComment(1, "owner", "/hold")
	iterate(t, d)
	f.DeleteCommentBy("mallory", c)
	for range 3 {
		iterate(t, d)
	}
	want := fmt.Sprintf("No discussion trigger is open on this issue, but it stays on hold: a `/hold` on an issue above it, in comment %d, "+
		"hasn't ended. `/resume` there, or here, ends that.\n\n<!-- issuedog:ended reason=discussion held=%d -->", hold, hold)
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:held"}) || body != want {
		t.Fatalf("#3: labels %q, newest comment %q; want it held, with %q", l, body, want)
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
	f.AddComment(1, "owner", "/resume")
	for range 2 {
		iterate(t, d)
	}
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("#3: labels %q, newest comment %q; want it back at ready once #1's /resume ends the /hold, no discussion having ended", l, body)
	}
}

// The walk up for ancestors' /holds stops at a parent in another repo, as
// madeAbove's does: its /hold keeps nothing held here.
func TestResolveAboveStopsAtOtherRepo(t *testing.T) {
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	f.Transfer(1, "o/other")
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want clarifying, the parent in another repo", l, newestComment(f, 2))
	}
}

package daemon_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveAtClarifying is resolveReview's story, its /approve, 1004, having
// countered mallory's comment, at the clarifying after the hold, origin
// spec-review, by a pass.
func resolveAtClarifying(t testing.TB) *githubtest.Fake {
	f := resolveReview(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "owner", "/approve") })(t)
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newestComment(f, 1), "to=clarifying at=spec-review origin=spec-review") {
		t.Fatalf("newest comment %q; want the move to clarifying after the hold", newestComment(f, 1))
	}
	return f
}

// resolveStillWaiting checks that issue 1 is at clarifying, its newest
// move carrying origin=spec-review, and the /approve in 1004 still waits,
// with no reaction.
func resolveStillWaiting(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	is := f.State().Issues[0]
	var moves []string
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:transition") {
			moves = append(moves, c.Body)
		}
	}
	if !slices.Equal(is.Labels, []string{"stage:clarifying"}) || !strings.Contains(moves[len(moves)-1], "origin=spec-review") {
		t.Errorf("labels %q, newest move %q; want clarifying, carrying origin=spec-review", is.Labels, moves[len(moves)-1])
	}
	if r := is.Comments[3].Reactions; is.Comments[3].Body != "/approve" || len(r) != 0 {
		t.Errorf("the /approve %q has reactions %q; want it still waiting", is.Comments[3].Body, r)
	}
}

// At the clarifying after a hold, a later hold of either kind carries the
// origin forward, and so does the /resume back there, so the waiting
// /approve keeps waiting (SPEC §6.4): a discussion hold, an owner /hold,
// and a hold set by hand.
func TestResolveOriginCarried(t *testing.T) {
	t.Parallel()
	t.Run("a discussion hold", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.AddComment(1, "mallory", "And another thing.")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
	t.Run("an owner /hold", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if body := newestComment(f, 1); !strings.Contains(body, "from=clarifying to=held at=clarifying origin=spec-review reason=owner") {
			t.Errorf("newest comment %q; want the /hold carrying origin=spec-review", body)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
	t.Run("a hold by hand", func(t *testing.T) {
		f := resolveAtClarifying(t)
		f.RemoveLabel(1, "owner", "stage:clarifying")
		f.AddLabel(1, "owner", "stage:held")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if body := newestComment(f, 1); !strings.Contains(body, "from=clarifying to=held at=clarifying origin=spec-review reason=owner") {
			t.Errorf("newest comment %q; want the hold by hand carrying origin=spec-review", body)
		}
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		resolveStillWaiting(t, f)
	})
}

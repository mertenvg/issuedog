package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The poll records the digests and the App's digest-new comments on any
// issue. An edited one counts for nothing, nor does one by anyone else. A
// comment or body whose marker an edit took out is forgotten.
func TestDigestPollRecords(t *testing.T) {
	const app = "issuedog[bot]"
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue(app, "issuedog digest", "<!-- issuedog:digest -->")
	f.AddIssue("owner", "Mine", "<!-- issuedog:digest -->")
	first := f.AddComment(1, app, "New. <!-- issuedog:digest-new items=q-1-1,rev-1-2 -->")
	f.EditComment(f.AddComment(1, app, "<!-- issuedog:digest-new items=edited -->"), "owner", "<!-- issuedog:digest-new items=edited -->")
	f.AddComment(1, "owner", "<!-- issuedog:digest-new items=owners -->")
	f.AddComment(2, app, "<!-- issuedog:digest-new items=elsewhere -->")
	f.SetPullRequest(f.AddIssue(app, "A pull request", "<!-- issuedog:digest -->"))
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	poll := func() {
		t.Helper()
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	keys := func(n int) []string { return slices.Sorted(maps.Keys(d.digest.announced(n))) }
	if !d.digest.has(1) || d.digest.has(2) || d.digest.has(3) || !slices.Equal(keys(1), []string{"q-1-1", "rev-1-2"}) || !slices.Equal(keys(2), []string{"elsewhere"}) {
		t.Errorf("digests %v, announced on 1 %q and on 2 %q; want issue 1 alone, with q-1-1 and rev-1-2, and elsewhere on 2",
			d.digest.issues, keys(1), keys(2))
	}
	if !d.stale[2] || d.stale[1] {
		t.Errorf("stale = %v; want the user's issue 2, not the digest", d.stale)
	}
	f.EditComment(first, app, "No marker now.")
	f.EditBody(1, "owner", "No marker now.")
	poll()
	if d.digest.has(1) || len(keys(1)) != 0 {
		t.Errorf("digests %v, announced on 1 %q; want both forgotten", d.digest.issues, keys(1))
	}
}

// A digest marked to be read again, as a run's lease or a command that
// touched it would, is never read or visited: it stays out of the pipeline.
func TestDigestNeverReadAgain(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->")
	f.AddIssue("owner", "Mine", "")
	var visits []int
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{},
		Reactions: func(_ context.Context, _ *Applier, s *github.Snapshot, _ Dirt) error {
			visits = append(visits, s.Number)
			return nil
		}}
	// The clone can't sync, so the trust and config given stay.
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults()}
	for i := range 2 {
		if i > 0 {
			d.reread(1, false)
		}
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d.snaps[1] != nil || d.stale[1] || !slices.Equal(visits, []int{2}) {
		t.Errorf("snapshot %v, stale %v, visits %v; want no snapshot of the digest, and only issue 2 visited", d.snaps[1], d.stale, visits)
	}
}

// digestEdit writes a body once: the body it wrote is the one it compares
// with until the poll lists the digest again, as T8 relies on.
func TestDigestEditOnce(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.digestEdit(context.Background(), 1, "new <!-- issuedog:digest -->"); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes(); len(w) != 1 || f.State().Issues[0].Body != "new <!-- issuedog:digest -->" {
		t.Errorf("writes = %+v; want one edit, to the new body", w)
	}
}

// The pin note names GitHub's message alone when the error wraps
// github.ErrGraphQL, and the whole error otherwise, on one line.
func TestDigestPinNote(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("pin o/r#1: %w: Resource not accessible", github.ErrGraphQL),
			"I couldn't pin this issue: `Resource not accessible`. Pin it by hand."},
		{errors.New("two\nlines"), "I couldn't pin this issue: `two lines`. Pin it by hand."},
	} {
		if got := digestPinNote(tc.err); got != tc.want {
			t.Errorf("digestPinNote(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The owner's /resume on K, under J, under A, waits for the first /hold
// posted before it on an ancestor that isn't processed, nearest first
// (holdOrderWaits): K is touched, so it's read again next pass, and only
// the ancestor it waits for is read again too, not every ancestor, so the
// extra reads are the awaited one's alone. A /hold on an ancestor that's
// the digest, which no pass visits, is waited for by nothing, and nothing
// is read again for it.
func TestHoldOrderRereadsAwaited(t *testing.T) {
	t.Parallel()
	const a, j, k = 1, 2, 3
	for _, tt := range []struct {
		name    string
		holds   []int // the issues the owner's /holds are posted on, before K's /resume
		digest  int   // the issue that's the digest, if any
		wait    bool
		touched []int
		reread  []int
	}{
		{"J's /hold, the nearest, is the one waited for", []int{a, j}, 0, true, []int{k}, []int{j}},
		{"A's /hold, the farthest, is read again alone", []int{a}, 0, true, []int{k}, []int{a}},
		{"a /hold on the digest is waited for by nothing", []int{a}, a, false, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := githubtest.New(t, "o/r", "issuedog")
			f.SetPermission("owner", "admin")
			for range 3 {
				f.AddIssue("owner", "Part", "A part.")
			}
			f.SetParent(j, a)
			f.SetParent(k, j)
			for _, n := range tt.holds {
				f.AddComment(n, "owner", "/hold")
			}
			resume := f.AddComment(k, "owner", "/resume")
			c := f.Client(t)
			s, err := c.Snapshot(ctx, "o", "r", k)
			if err != nil {
				t.Fatal(err)
			}
			var touched, reread []int
			st := &Step{Applier: &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog", Touched: func(n int) { touched = append(touched, n) }},
				Trust: ownerOnly{}, holdOrderReread: func(n int) { reread = append(reread, n) }, holdOrderDigest: func(n int) bool { return n == tt.digest }}
			d, err := st.Dirty(ctx, s, st.Trust)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(s.Comments, func(cm github.Comment) bool { return cm.ID == resume })
			if i < 0 {
				t.Fatalf("#%d: no comment %d", k, resume)
			}
			cmd, ok := state.ParseCommand(s.Comments[i].Body)
			if !ok {
				t.Fatalf("comment %d: %q is no command", resume, s.Comments[i].Body)
			}
			wait, err := st.holdOrderWaits(ctx, s, d, s.Comments[i], cmd)
			if err != nil {
				t.Fatal(err)
			}
			if wait != tt.wait || !slices.Equal(touched, tt.touched) || !slices.Equal(reread, tt.reread) {
				t.Errorf("waits %t, touched %v, read again %v; want %t, %v and %v", wait, touched, reread, tt.wait, tt.touched, tt.reread)
			}
		})
	}
}

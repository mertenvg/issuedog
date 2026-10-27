package daemon

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The walk up from C's /resume, under the digest D, which someone made a
// sub-issue of G, passes over D, as the daemon never takes a snapshot of
// the digest (SPEC §5.1): it reads D's parent alone, and goes on to G, so
// G's /hold still counts as made above C. With D a parent of nobody's, the
// 404 for its parent ends the walk at D. Neither reads D as an issue, nor
// counts its comments.
func TestMadeAboveDigestPassedOver(t *testing.T) {
	t.Parallel()
	const g, digest, c = 1, 2, 3
	for _, tt := range []struct {
		name   string
		under  bool // whether D is a sub-issue of G
		issues []int
		nodes  []int
	}{
		{"under G", true, []int{g, digest, c}, []int{c, g}},
		{"under nothing", false, []int{digest, c}, []int{c}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := githubtest.New(t, "o/r", "issuedog")
			f.SetPermission("owner", "admin")
			f.AddIssue("owner", "G", "The parent.")
			f.AddIssue("issuedog[bot]", "issuedog digest", "Nothing is waiting on you.\n\n<!-- issuedog:digest -->")
			f.AddIssue("owner", "C", "The child.")
			if tt.under {
				f.SetParent(digest, g)
			}
			f.SetParent(c, digest)
			hold := f.AddComment(g, "owner", "/hold")
			onDigest := f.AddComment(digest, "owner", "/hold")
			resume := f.AddComment(c, "owner", "/resume")
			cl := f.Client(t)
			s, err := cl.Snapshot(ctx, "o", "r", c)
			if err != nil {
				t.Fatal(err)
			}
			a := &Applier{Client: cl, Owner: "o", Repo: "r", AppSlug: "issuedog", madeAboveDigest: func(n int) bool { return n == digest }}
			d, err := a.Dirty(ctx, s, ownerOnly{})
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(s.Comments, func(cm github.Comment) bool { return cm.ID == resume })
			if i < 0 {
				t.Fatalf("#%d: no comment %d", c, resume)
			}
			cmd, ok := state.ParseCommand(s.Comments[i].Body)
			if !ok {
				t.Fatalf("comment %d: %q is no command", resume, s.Comments[i].Body)
			}
			from := len(f.Requests())
			above, err := a.madeAbove(ctx, Cmd{Command: cmd, Comment: s.Comments[i], S: s, Dirt: d, Trust: ownerOnly{}})
			if err != nil {
				t.Fatal(err)
			}
			var nodes []int
			for _, n := range above.nodes {
				nodes = append(nodes, n.s.Number)
			}
			if got := slices.Sorted(maps.Keys(above.issues)); !slices.Equal(got, tt.issues) || !slices.Equal(nodes, tt.nodes) {
				t.Errorf("issues %v, nodes %v; want %v and %v", got, nodes, tt.issues, tt.nodes)
			}
			if above.ids[hold] != tt.under || above.ids[onDigest] {
				t.Errorf("ids %v; want G's /hold, %d, there %t, and not the digest's comment %d", above.ids, hold, tt.under, onDigest)
			}
			for _, r := range f.Requests()[from:] {
				if r.Method == http.MethodGet && r.Path == "/issues/2" {
					t.Errorf("the walk up sent %+v; want no read of the digest", r)
				}
			}
		})
	}
}

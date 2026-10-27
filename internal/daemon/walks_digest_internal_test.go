package daemon

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// walksDigestFake is o/r with G, #1, the owner's, the digest D, #2, which
// someone made a sub-issue of G when under is set, and C, #3, the owner's
// task under D, held for mallory's comment. G and D each have an owner's
// /hold, posted during C's hold, that the App 👍'd, as a /hold it applied
// with no marker. It returns the two /holds' comment IDs.
func walksDigestFake(t *testing.T, under bool) (f *githubtest.Fake, onG, onDigest int64) {
	t.Helper()
	const g, digest, c = 1, 2, 3
	f = githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin")
	f.AddIssue("owner", "G", "The parent.")
	f.AddIssue("issuedog[bot]", "issuedog digest", "Nothing is waiting on you.\n\n<!-- issuedog:digest -->")
	f.AddIssue("owner", "C", "The child.")
	if under {
		f.SetParent(digest, g)
	}
	f.SetParent(c, digest)
	f.AddComment(c, "issuedog[bot]", "<!-- issuedog:transition from=intake to=ready run=a -->")
	wait := f.AddComment(c, "mallory", "Wait.")
	f.AddComment(c, "issuedog[bot]", "<!-- issuedog:transition from=ready to=held at=ready origin=ready reason=discussion event=c"+
		strconv.FormatInt(wait, 10)+" -->")
	onG = f.AddComment(g, "owner", "/hold")
	f.AddReaction(onG, "issuedog[bot]", "+1")
	onDigest = f.AddComment(digest, "owner", "/hold")
	f.AddReaction(onDigest, "issuedog[bot]", "+1")
	return f, onG, onDigest
}

// walksDigestNoRead fails t for any request from the from'th on that reads
// the digest, #2, as an issue, or its comments.
func walksDigestNoRead(t *testing.T, f *githubtest.Fake, from int, walk string) {
	t.Helper()
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && (r.Path == "/issues/2" || r.Path == "/issues/2/comments" || r.Path == "/issues/2/timeline") {
			t.Errorf("%s sent %+v; want no read of the digest", walk, r)
		}
	}
}

// The walk up from C at the end of its discussion (resolveAbove) passes
// over the digest D, as madeAbove does (SPEC §5.1, #527): it reads D's
// parent alone, and goes on to G, so G's /hold, which the App applied,
// keeps C held, while D's /hold, which no pass handles, keeps nothing
// held. With D a parent of nobody's, the walk ends at D, and no /hold
// keeps C held.
func TestResolveAboveDigestPassedOver(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		under bool
	}{
		{"under G", true},
		{"under nothing", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, onG, onDigest := walksDigestFake(t, tt.under)
			cl := f.Client(t)
			s, err := cl.Snapshot(ctx, "o", "r", 3)
			if err != nil {
				t.Fatal(err)
			}
			a := &Applier{Client: cl, Owner: "o", Repo: "r", AppSlug: "issuedog", madeAboveDigest: func(n int) bool { return n == 2 }}
			d, err := a.Dirty(ctx, s, ownerOnly{})
			if err != nil {
				t.Fatal(err)
			}
			is := state.Derive(s, a.AppSlug, d.Has)
			from := len(f.Requests())
			above, err := a.resolveAbove(ctx, s, d, ownerOnly{}, is, resolvePeriodOf(s, a.AppSlug, d.Has))
			if err != nil {
				t.Fatal(err)
			}
			var cmds []string
			for _, m := range above {
				cmds = append(cmds, m.Fields["cmd"])
			}
			var want []string
			if tt.under {
				want = []string{strconv.FormatInt(onG, 10)}
			}
			if !slices.Equal(cmds, want) {
				t.Errorf("holds above %v, by /hold %v; want %v, never the digest's %d", above, cmds, want, onDigest)
			}
			walksDigestNoRead(t, f, from, "the walk up")
		})
	}
}

// The walk down from G's /hold (family) passes over the digest D, which
// someone made G's sub-issue, as the walk up does (SPEC §5.1, #527): D
// isn't among the nodes, so nothing is written to it, and it isn't read,
// but the walk goes on to C, below D, so G's /hold holds C too.
func TestFamilyDigestPassedOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, onG, _ := walksDigestFake(t, true)
	cl := f.Client(t)
	s, err := cl.Snapshot(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := &Applier{Client: cl, Owner: "o", Repo: "r", AppSlug: "issuedog", madeAboveDigest: func(n int) bool { return n == 2 }}
	d, err := a.Dirty(ctx, s, ownerOnly{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(s.Comments, func(cm github.Comment) bool { return cm.ID == onG })
	if i < 0 {
		t.Fatalf("#1: no comment %d", onG)
	}
	cmd, ok := state.ParseCommand(s.Comments[i].Body)
	if !ok {
		t.Fatalf("comment %d: %q is no command", onG, s.Comments[i].Body)
	}
	from := len(f.Requests())
	family, err := a.family(ctx, Cmd{Command: cmd, Comment: s.Comments[i], S: s, Dirt: d, Trust: ownerOnly{}})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []int
	for _, n := range family {
		nodes = append(nodes, n.s.Number)
	}
	if want := []int{3, 1}; !slices.Equal(nodes, want) {
		t.Errorf("family %v; want %v: C, then G, and never the digest", nodes, want)
	}
	walksDigestNoRead(t, f, from, "the walk down")
}

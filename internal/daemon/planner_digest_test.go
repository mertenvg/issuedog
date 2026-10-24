package daemon_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
)

// TestPlannerContextDigest: a story whose parent is the digest, which
// someone made its parent, gets a parent.md saying so, and a story with
// the digest as a sub-issue lists it as not in the pipeline, with no type.
// Neither reads anything of the digest, as the daemon never takes a
// snapshot of it (SPEC §5.1, #527).
func TestPlannerContextDigest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		parent bool // whether the digest is the story's parent, or else its sub-issue
		file   string
		want   string
	}{
		{"the story's parent", true, "parent.md",
			"# Parent\n\nThis story's parent is issuedog's digest, which isn't in the pipeline, so issuedog doesn't read it.\n"},
		{"the story's sub-issue", false, "children.md", "\n\n- #1: issuedog digest (open, type none, not in the pipeline)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			digest := f.AddIssue(app, "issuedog digest", digestList)
			f.SetPinned(digest, true)
			n := f.AddIssue("owner", "The story", "")
			f.AddLabel(n, "owner", "type:story")
			f.AddComment(n, app, "Approved.\n\n<!-- issuedog:transition from=spec-review to=planning cmd=r7 rev=1 body=abcd1234 -->")
			if tc.parent {
				f.SetParent(n, digest)
			} else {
				f.SetParent(digest, n)
			}
			d := ivDaemon(t, f, newOrigin(t))
			d.Digest = true
			iterate(t, d) // finds the digest
			in := ivInput(t, d, n)
			from := len(f.Requests())
			got, err := (&daemon.Planner{Daemon: d}).Context(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if text := string(got[tc.file]); !strings.HasSuffix(text, tc.want) {
				t.Errorf("%s doesn't end with %q:\n%s", tc.file, tc.want, text)
			}
			for _, r := range f.Requests()[from:] {
				if r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/") {
					t.Errorf("the planner sent %+v; want nothing of the digest read", r)
				}
			}
		})
	}
}

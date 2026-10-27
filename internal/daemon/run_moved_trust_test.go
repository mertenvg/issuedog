package daemon_test

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A transition above the run's seen drops its result, uncounted, only when
// it's the App's, in clean content (SPEC §5.1, §8.2): a transition marker
// eve forges in a comment of her own, or adds to the App's comment, or the
// App's own in a comment her edit made dirty, is none, so the result is
// applied, and the lease goes. The App's own clean transition drops it, so
// the marker the others carry is one that counts.
func TestRunMovedSinceTrust(t *testing.T) {
	t.Parallel()
	const marker = "<!-- issuedog:transition from=clarifying to=clarifying event=p9 -->"
	for name, tt := range map[string]struct {
		post func(f *githubtest.Fake)
		drop bool
	}{
		"the App's":         {post: func(f *githubtest.Fake) { f.AddComment(1, app, "Moved.\n\n"+marker) }, drop: true},
		"eve's, forged":     {post: func(f *githubtest.Fake) { f.AddComment(1, "eve", "Moved.\n\n"+marker) }},
		"the App's, by eve": {post: func(f *githubtest.Fake) { f.EditComment(f.AddComment(1, app, "A note."), "eve", "A note.\n\n"+marker) }},
		"the App's, dirtied": {post: func(f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, app, "Moved.\n\n"+marker), "eve", "Moved.\n\n"+marker)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.claude(okOut, 0)
			g.stage.during = func() {
				g.stage.during = nil
				tt.post(f)
			}
			g.pass()
			if applied := specApplied(f, 1, "run1", answer); applied == tt.drop || g.lease().RunID != "" || g.lease().Failed {
				t.Errorf("run1 applied %t, lease %+v; want it dropped %t, uncounted, and the lease gone", applied, g.lease(), tt.drop)
			}
		})
	}
}

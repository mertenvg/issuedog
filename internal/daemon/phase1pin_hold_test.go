package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// TestPhase1PinHoldWhileGateShut pins what epic #1's verifier found
// untested (#228): /hold while a usage limit shuts the gate. Stage work
// waits, but step 1 doesn't (SPEC §3.3), so the /hold applies in the pass
// it's posted in, with its 👍, and claude never runs. Once the gate opens,
// the held issue still gets no run, though its stage has work pending, and
// /resume lets it run again. A run's saved result left by a restart is
// dropped, not applied, once the /hold in the same pass has held the issue.
func TestPhase1PinHoldWhileGateShut(t *testing.T) {
	t.Parallel()
	for name, saved := range map[string]bool{"no lease": false, "a saved result": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			if saved {
				saveRun(t, root, "saved", okOut, answer)
			}
			g := newRig(t, f, newOrigin(t), root)
			g.claude(okOut, 0)
			g.stage.pending = always
			reset := githubtest.Start.Add(time.Hour)
			if !g.r.Gate.Observe(g.now, true, fmt.Sprintf("usage limit reached|%d", reset.Unix())) {
				t.Fatal("the gate didn't shut")
			}
			hold := f.AddComment(1, "owner", "/hold")
			g.pass()
			is := f.State().Issues[0]
			if !slices.Equal(is.Labels, []string{"stage:held"}) || g.runs() != 0 {
				t.Fatalf("labels %v after %d runs; want the /hold applied while the gate is shut, and no run", is.Labels, g.runs())
			}
			if i := slices.IndexFunc(is.Comments, func(c githubtest.CommentState) bool { return c.Body == "/hold" }); i < 0 {
				t.Errorf("comments %+v; want the /hold", is.Comments)
			} else if got := is.Comments[i].Reactions; !slices.Equal(got, []string{app + " +1"}) {
				t.Errorf("reactions to the /hold = %v; want the App's 👍", got)
			}
			if last := newestComment(f, 1); !strings.Contains(last, fmt.Sprintf(" to=held at=clarifying reason=owner cmd=%d ", hold)) {
				t.Errorf("newest comment %q; want the hold's move", last)
			}
			if slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return c.Body == specWritten("saved", answer) }) ||
				len(g.leftovers()) != 0 {
				t.Errorf("comments %+v, left %v; want no saved result applied to the held issue, and nothing left", is.Comments, g.leftovers())
			}

			g.now = reset.Add(time.Second)
			writes := len(f.Writes())
			for range 2 {
				g.pass()
			}
			if g.runs() != 0 || len(f.Writes()) != writes {
				t.Fatalf("with the gate open: %d runs, writes %+v; want no run on the held issue, and no write", g.runs(), f.Writes()[writes:])
			}

			f.AddComment(1, "owner", "/resume")
			for range 2 {
				g.pass()
			}
			if labels := f.State().Issues[0].Labels; g.runs() != 1 || !slices.Equal(labels, []string{"stage:spec-review"}) {
				t.Errorf("after /resume: %d runs, labels %v; want one run, applied", g.runs(), labels)
			}
		})
	}
}

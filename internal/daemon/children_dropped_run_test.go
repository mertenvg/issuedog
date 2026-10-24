package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A dropped task's run (SPEC §6.4, §8.2, #369): an approved revision that
// drops a task whose run's result is still waiting to be applied closes
// the task as not planned, like any other, and the result is dropped on
// the closed issue, uncounted, with nothing of it written there.

// droppedRunRevision is the golden story revision with T2 dropped rather
// than changed, so the task picked up at in-progress, T2's child, is
// dropped, with T3's.
func droppedRunRevision(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("plan %v has no item 2", plan)
	}
	items[1] = map[string]any{"id": "T2", "title": "Refuse an expired quote at booking", "status": "dropped"}
	return out
}

// droppedRunStage stands in for a task's stage run, the implementer's,
// at in-progress: its Apply moves the task to verifying, then posts
// followUp, once. Its first Apply fails, before any write if before is
// set, else after its move, as a crash or a failed write part way
// through would leave it, so the lease waits for a later pass.
type droppedRunStage struct {
	*fakeStage
	before, failed bool
}

func (s *droppedRunStage) Apply(ctx context.Context, a *daemon.Applier, r daemon.Result) error {
	if s.before && !s.failed {
		s.failed = true
		return errors.New("applying failed before any write")
	}
	m := r.Move(state.Verifying, state.TriggerImplemented)
	m.Text = "Implemented."
	if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
		return fmt.Errorf("move to verifying: %w", err)
	}
	if !s.failed {
		s.failed = true
		return errors.New("applying failed after the move")
	}
	if slices.ContainsFunc(r.S.Comments, func(c github.Comment) bool { return c.Body == followUp }) {
		return nil
	}
	if _, _, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, r.S.Number, gogithub.IssueCommentRequest{Body: followUp}); err != nil {
		return fmt.Errorf("post the follow-up: %w", err)
	}
	return nil
}

// Through the loop: a pass with no approval handler runs T2's child, at
// in-progress, and its result fails to apply, its move written or not,
// so the lease waits. Then the owner's 👍 on the revision that drops T2
// is handled, by the same daemon, or by a new one on the same state
// directory, as after a restart: the next pass closes T2's child as not
// planned, and its waiting result is dropped, uncounted, with no write of
// it on the closed issue: not its move, if it wasn't written, nor its
// follow-up. The lease and the run's files go, and no run starts again.
func TestChildrenDroppedRunResult(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ before, restart bool }{
		"unmoved":                  {before: true},
		"moved":                    {},
		"unmoved, after a restart": {before: true, restart: true},
		"moved, after a restart":   {restart: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			r := changedStoryWith(t, f, false, nil, changedPlanData(t, droppedRunRevision(t)))
			o, root := newOrigin(t), t.TempDir()
			o.commit(config.Path, "types: labels\nmodels: {allowed: [opus]}\n") // the model revision 1 gave T2
			newDroppedRig := func(st *droppedRunStage) *rig {
				g := newRig(t, f, o, root)
				g.claude(okOut, 0)
				st.fakeStage = &fakeStage{agent: "implementer", pending: func(in daemon.Input) bool { return in.S.Number == r.t2 }}
				g.r.Stages = map[state.Stage]daemon.Stage{state.InProgress: st}
				return g
			}
			g := newDroppedRig(&droppedRunStage{before: tt.before})

			if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "applying failed") {
				t.Fatalf("the run's pass returned %v, want its failed apply", err)
			}
			if l := g.lease(); g.runs() != 1 || l.RunID != "run1" || l.Failed || l.ApplyFailures != 1 {
				t.Fatalf("after the run's pass: %d runs, lease %+v; want run1's lease waiting, with one failed apply", g.runs(), l)
			}
			moved := marked(f, r.t2, "run1")
			if moved == tt.before {
				t.Fatalf("run1's move on T2's child written %t, want %t", moved, !tt.before)
			}
			comments := len(f.State().Issues[r.t2-1].Comments)

			if tt.restart {
				g = newDroppedRig(&droppedRunStage{before: tt.before, failed: true})
			}
			g.d.Step.Reactions = daemon.ApproveByReaction(g.d.Step.CurrentTrust())
			g.d.Step.Models = g.d.CurrentModels
			g.pass()
			t2 := f.State().Issues[r.t2-1]
			if !t2.Closed || droppedReason(t, f, r.t2) != "not_planned" {
				t.Fatalf("T2's child is closed %t, as %q; want it closed as not planned", t2.Closed, droppedReason(t, f, r.t2))
			}
			if got := len(t2.Comments); got != comments+1 || !strings.HasPrefix(t2.Comments[got-1].Body, "Closed as not planned, since #1's plan, rev 2, drops T2 ") {
				t.Errorf("T2's child's comments since the run's pass are %+v; want only the close", t2.Comments[comments:])
			}
			if got := marked(f, r.t2, "run1"); got != moved || slices.ContainsFunc(t2.Comments, func(c githubtest.CommentState) bool { return c.Body == followUp }) {
				t.Errorf("T2's child has run1's move %t, and its follow-up or not; want the move %t, as before the drop, and no follow-up", got, moved)
			}
			if l := g.lease(); l.RunID != "" || l.Failed || g.runs() != 1 {
				t.Errorf("lease %+v, %d runs; want the result dropped, uncounted, and no run again", l, g.runs())
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v, want nothing of the dropped run", left)
			}
		})
	}
}

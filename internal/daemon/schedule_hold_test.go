package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// pickHoldPicked reports whether task n has the pick's comment.
func pickHoldPicked(t *testing.T, f *githubtest.Fake, n int) bool {
	t.Helper()
	return slices.ContainsFunc(implementerComments(t, f, n), func(c github.Comment) bool {
		return c.Author.Login == app && strings.Contains(c.Body, "<!-- issuedog:transition from=ready to=in-progress ")
	})
}

// pickHoldNotPicked fails the test unless the pass that made searches and
// blockedBy picked nothing: no run, task n at want, with no pick's
// comment, and no search or blocked_by read, as no task is at ready.
func pickHoldNotPicked(g *rig, n int, want string, searches, blockedBy int, pass string) {
	g.t.Helper()
	picked := pickHoldPicked(g.t, g.f, n)
	if g.runs() != 0 || pickStage(g.f, n) != want || picked || searches != 0 || blockedBy != 0 {
		g.t.Errorf("%s: claude ran %d times, #%d is at %s, picked %v, and the pass made %d searches and %d blocked_by reads; "+
			"want no run, the task at %s, not picked, and neither request", pass, g.runs(), n, pickStage(g.f, n), picked, searches, blockedBy, want)
	}
}

// A ready task held, by the owner's /hold or by a discussion trigger, here
// mallory's comment, isn't picked: on the pass that holds it or after,
// with no search, as no task is at ready. The owner's /resume sends it
// back to ready, and the pass that handles it picks it, keyed by the
// resume's move to ready, not by the move to ready before the hold, and
// with seen= that move, and its run starts (SPEC §6.4, §8.1).
func TestPickHeldReadyTask(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, login, body string
	}{
		{"the owner's /hold", "owner", "/hold"},
		{"a discussion trigger", "mallory", "What about refunds?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := pickReady(f, "p1")
			f.AddComment(n, tt.login, tt.body)
			g := pickRig(t, f)
			searches, blockedBy := pickPass(g)
			pickHoldNotPicked(g, n, "stage:held", searches, blockedBy, "the pass that holds it")
			searches, blockedBy = pickPass(g)
			pickHoldNotPicked(g, n, "stage:held", searches, blockedBy, "the pass after")
			f.AddComment(n, "owner", "/resume")
			g.pass()
			if got := g.leaseIssues(); !slices.Equal(got, []int{n}) || pickStage(f, n) != "stage:verifying" {
				t.Fatalf("after the /resume, the runs leased %v, and #%d is at %s; want it picked, and at verifying", got, n, pickStage(f, n))
			}
			cs := implementerComments(t, f, n)
			i := slices.IndexFunc(cs, func(c github.Comment) bool {
				return c.Author.Login == app && strings.Contains(c.Body, "<!-- issuedog:transition from=held to=ready ")
			})
			if i < 0 || i+1 >= len(cs) {
				t.Fatalf("#%d's comments are %+v; want the resume's move to ready, then the pick's", n, cs)
			}
			if want := pickMoved(cs[i].ID, cs[i].ID); cs[i+1].Body != want {
				t.Errorf("the comment after the resume's move =\n%s\nwant the pick's, keyed by it,\n%s", cs[i+1].Body, want)
			}
		})
	}
}

// A ready task's stage label that disagrees with its markers is brought in
// line by step 1 before the pick, which goes by the markers (SPEC §5.1,
// §8.1). A task held at ready, which mallory, who isn't trusted, relabels
// stage:ready, gets stage:held back and isn't picked, with no search; and
// with the write putting it back failing, step 1 fails on it, so that pass
// picks nothing. A task at ready that mallory relabels stage:clarifying,
// or whose stage:ready mallory takes off, gets stage:ready back, and is
// picked in the same pass, keyed by its move to ready.
func TestPickLabelDisagreesAtReady(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		held      bool   // the owner's /hold holds the task first
		take, put string // the stage label mallory takes off, and the one mallory puts on
		fail      bool   // the first write to the task fails
	}{
		{name: "held, labeled ready", held: true, take: "stage:held", put: "stage:ready"},
		{name: "held, labeled ready, put back failing", held: true, take: "stage:held", put: "stage:ready", fail: true},
		{name: "ready, labeled clarifying", take: "stage:ready", put: "stage:clarifying"},
		{name: "ready, unlabeled", take: "stage:ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := pickReady(f, "p1")
			if tt.held {
				f.AddComment(n, "owner", "/hold")
			}
			g := pickRig(t, f)
			if tt.held {
				g.pass()
			}
			f.RemoveLabel(n, "mallory", tt.take)
			if tt.put != "" {
				f.AddLabel(n, "mallory", tt.put)
			}
			from := len(f.Requests())
			if tt.fail {
				f.AnswerOnce(func(r githubtest.Request) bool {
					return r.Method != http.MethodGet && strings.HasPrefix(r.Path, fmt.Sprintf("/issues/%d/", n))
				}, http.StatusInternalServerError)
				if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
					t.Fatalf("the pass = %v; want step 1's 500", err)
				}
				searches, blockedBy := pickReads(f, from)
				if g.runs() != 0 || pickHoldPicked(t, f, n) || searches != 0 || blockedBy != 0 {
					t.Errorf("the pass whose step 1 failed: claude ran %d times, picked %v, and the pass made %d searches and %d blocked_by reads; "+
						"want no run, not picked, and neither request", g.runs(), pickHoldPicked(t, f, n), searches, blockedBy)
				}
				from = len(f.Requests())
			}
			g.pass()
			searches, blockedBy := pickReads(f, from)
			if tt.held {
				pickHoldNotPicked(g, n, "stage:held", searches, blockedBy, "the pass that puts stage:held back")
				return
			}
			if got := g.leaseIssues(); !slices.Equal(got, []int{n}) || pickStage(f, n) != "stage:verifying" {
				t.Fatalf("the runs leased %v, and #%d is at %s; want it picked, and at verifying", got, n, pickStage(f, n))
			}
			cs := implementerComments(t, f, n)
			i := slices.IndexFunc(cs, func(c github.Comment) bool { return strings.HasPrefix(c.Body, "I've picked this task") })
			if want := pickMoved(cs[0].ID, cs[0].ID); i < 0 || cs[i].Body != want {
				t.Errorf("#%d's comments are %+v; want the pick's,\n%s", n, cs, want)
			}
		})
	}
}

// A whole run in which a run's lease holds the worker: a restart left the
// interview's saved result, whose apply fails once, so its lease stays,
// and no task is picked that pass, with no search and no blocked_by read,
// though one waits at ready. The next pass applies the result, ending the
// lease, and picks the task, whose run starts (SPEC §8.1, §8.2).
func TestPickLeaseHoldsWorker(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	root := t.TempDir()
	saveRun(t, root, "saved", okOut, interview(f))
	waiting := pickReady(f, "p1")
	g := newImplementerRig(t, f, newOrigin(t), root, nil, implementerOutput(t, "commits"))
	g.r.Stages[state.Clarifying] = g.stage
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	from := len(f.Requests())
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("the pass = %v; want the apply's 500", err)
	}
	searches, blockedBy := pickReads(f, from)
	if l := g.lease(); l.RunID != "saved" || l.ApplyFailures != 1 {
		t.Fatalf("the lease = %+v; want run saved's, kept after one failure to apply", l)
	}
	pickHoldNotPicked(g, waiting, "stage:ready", searches, blockedBy, "the pass whose apply failed")
	g.pass()
	if l := g.lease(); l.RunID != "" || pickStage(f, 1) != "stage:spec-review" {
		t.Fatalf("the lease = %+v, and #1 is at %s; want the result applied, and the lease gone", l, pickStage(f, 1))
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{waiting}) || pickStage(f, waiting) != "stage:verifying" {
		t.Errorf("once the lease ended, the runs leased %v, and #%d is at %s; want #%d picked, and at verifying", got, waiting, pickStage(f, waiting), waiting)
	}
}

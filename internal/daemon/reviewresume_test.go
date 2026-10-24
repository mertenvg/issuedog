package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is #318: an owner's comment posted while an interviewer or
// planner run is in progress, when the run is left as a lease to resume.
// Step 1 👍s the comment as an answer before Work resumes the lease, so the
// comment must reach a run as input, or be a revision request at the review
// stage the run's move lands at: never neither, and never both.

// reviewResumeStage is a fakeStage whose Apply posts what the stage's agent
// posts on its move to review: at clarifying, a spec, with the next spec
// revision (PostRevision), and at planning, plan revision 1 (planText). Its
// Context gives the inputs' bodies as the prompt.
type reviewResumeStage struct {
	*fakeStage
	at state.Stage
}

func (rs reviewResumeStage) Apply(ctx context.Context, a *daemon.Applier, r daemon.Result) error {
	if rs.at == state.Planning {
		m := r.Move(state.PlanReview, state.TriggerPlanValid)
		m.PlanRev, m.Text = 1, planText(r.S.Number, 1)
		return a.Transition(ctx, r.S, r.Dirt.Has, m)
	}
	m := r.Move(state.SpecReview, state.TriggerSpecWritten)
	m.Text = "The spec."
	return a.PostRevision(ctx, r.S, r.Dirt.Has, m, nil)
}

// reviewResumeSave leaves the files of run id on issue 1 at stage under
// root, as a daemon that stopped once the run ended, before applying it,
// would: the lease, having read up to comment seen, okOut as its result,
// and a clone.
func reviewResumeSave(t testing.TB, root string, stage state.Stage, id string, seen int64) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: stage, RunID: id, Clone: filepath.Join(root, "runs", id), Seen: seen,
		AttemptKey: fmt.Sprintf("%s-%d", stage, seen), Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"state/lease.json": string(l), "runs/" + id + "/f.txt": "x", "state/runs/" + id + ".json": okOut}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// reviewResumePlanning files issue 1, the owner's epic, at planning, as the
// App moved it there: revision 1 of its spec, then its approval. It returns
// the approval's ID, the newest comment when a planner run starts.
func reviewResumePlanning(f *githubtest.Fake) int64 {
	n := f.AddIssue("owner", "Idea", "the idea")
	f.SetType(n, "Epic")
	f.AddLabel(n, app, "stage:planning")
	rev := f.AddComment(n, app, revision(1, "the idea"))
	return f.AddComment(n, app, fmt.Sprintf("Revision 1 of the spec is approved, by owner's 👍.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=r9 seen=%d -->", state.BodyHash("the idea"), rev))
}

// The issue's case, through the loop with step 1 wired as daemon.NewStep
// wires it, for each review stage: the owner's comment posted during the
// run is 👍'd as an answer by the first pass after a restart, which drops
// the resumed result rather than lose it (SPEC §8.2). The next pass's run
// has it as input, and its move's seen covers it, so at the review stage
// it's no revision request. Two more passes write nothing and start no run,
// and the second, unchanged, asks only conditional GETs answered 304.
func TestReviewResumeLease(t *testing.T) {
	t.Parallel()
	for _, stage := range []state.Stage{state.Clarifying, state.Planning} {
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			review, seen := state.SpecReview, int64(0)
			if stage == state.Planning {
				review, seen = state.PlanReview, reviewResumePlanning(f)
			} else {
				seen = interview(f)
			}
			comment := f.AddComment(1, "owner", "also X") // during the run
			root := t.TempDir()
			reviewResumeSave(t, root, stage, "saved", seen)
			g := newRig(t, f, newOrigin(t), root)
			g.claude(okOut, 0)
			g.r.Stages = map[state.Stage]daemon.Stage{
				state.Clarifying: reviewResumeStage{g.stage, state.Clarifying},
				state.Planning:   reviewResumeStage{&fakeStage{agent: "planner"}, state.Planning},
			}
			g.d.Step = daemon.NewStep(g.d.Step.Applier, nil, g.d.CurrentModels) // Iterate loads the trust from main

			g.pass()
			is := f.State().Issues[0]
			last := is.Comments[len(is.Comments)-1]
			if marked(f, 1, "saved") || last.Body != "also X" || !slices.Equal(last.Reactions, []string{app + " +1"}) || g.lease().RunID != "" || g.runs() != 0 {
				t.Fatalf("after the restart's pass: comments %+v, lease %+v, %d runs; want the comment 👍'd as an answer, the saved result dropped, and no run yet",
					is.Comments, g.lease(), g.runs())
			}

			g.pass()
			is = f.State().Issues[0]
			move := fmt.Sprintf("from=%s to=%s ", stage, review)
			moved := slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool {
				return strings.Contains(c.Body, move) && strings.Contains(c.Body, "run=run1 seen="+strconv.FormatInt(comment, 10)+" ")
			})
			if g.runs() != 1 || !strings.Contains(g.file("prompt.1"), "also X") || !moved || !slices.Equal(is.Labels, []string{"stage:" + string(review)}) {
				t.Fatalf("run 1 of %d saw %q; comments %+v, labels %v; want the comment as its input, and its move to %s with seen=%d",
					g.runs(), g.file("prompt.1"), is.Comments, is.Labels, review, comment)
			}

			writes := len(f.Writes())
			g.pass()
			from := len(f.Requests())
			g.pass()
			if w := f.Writes(); len(w) != writes || g.runs() != 1 {
				t.Errorf("at %s: writes %+v, %d runs; want no write, so no revision request, and no run", review, w[writes:], g.runs())
			}
			for _, r := range f.Requests()[from:] {
				if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
					t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
				}
			}
			for _, c := range f.State().Issues[0].Comments {
				if c.Body == "also X" && !slices.Equal(c.Reactions, []string{app + " +1"}) {
					t.Errorf("the comment's reactions %q; want only the App's 👍 as an answer", c.Reactions)
				}
			}
		})
	}
}

// reviewResumeSpecReview files issue 1, the owner's story with body "body",
// at clarifying, as the App moved it there (comment 1001), with the owner's
// answer 1a (1002), which the App 👍'd (1003). Then it runs during, as
// while the interviewer ran, which returns the ID of the comment it posted,
// or 0. The run's move to spec-review follows, with revision 1 of the body,
// the run having read up to that comment if read is set, else up to 1002.
func reviewResumeSpecReview(read bool, during func(t testing.TB, f *githubtest.Fake) int64) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Story")
		f.AddLabel(1, app, "stage:clarifying")
		post(t, f, 1001, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
		post(t, f, 1002, "owner", "1a")
		if id := f.AddReaction(1002, app, "+1"); id != 1003 {
			t.Fatalf("reaction ID = %d, want 1003", id)
		}
		seen := int64(1002)
		if id := during(t, f); read && id != 0 {
			seen = id
		}
		h := state.BodyHash("body")
		f.AddComment(1, app, fmt.Sprintf("The spec.\n\n<!-- issuedog:transition from=clarifying to=spec-review rev=1 body=%s run=r1 seen=%d -->\n"+
			"<!-- issuedog:revision rev=1 body=%s -->", h, seen, h))
		f.AddLabel(1, app, "stage:spec-review")
		f.RemoveLabel(1, app, "stage:clarifying")
	})
}

// reviewResumeAnswered is the owner's comment "also X" posted during the
// run, then 👍'd by the App as an answer at clarifying or planning, as the
// first pass after a restart does before Work resumes the lease. It returns
// the comment's ID.
func reviewResumeAnswered(_ testing.TB, f *githubtest.Fake) int64 {
	id := f.AddComment(1, "owner", "also X")
	f.AddReaction(id, app, "+1")
	return id
}

// reviewResumePlanReview is planReviewRun with reviewResumeAnswered during
// the planner's run, so the comment is 1004, and the plan comment's move
// having read up to it if read is set, else up to 1001.
func reviewResumePlanReview(read bool) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Epic")
		labelEvent(t, f.AddLabel(1, app, "stage:spec-review"), 9001)
		post(t, f, 1001, app, revision(1, "body"))
		if id := f.AddReaction(1001, "owner", "+1"); id != 1002 {
			t.Fatalf("reaction ID = %d, want 1002", id)
		}
		post(t, f, 1003, app, approvedRev(1, "owner's 👍", "r1002", "1001"))
		f.AddLabel(1, app, "stage:planning")
		f.RemoveLabel(1, app, "stage:spec-review")
		if id := reviewResumeAnswered(t, f); id != 1004 {
			t.Fatalf("comment ID = %d, want 1004", id)
		}
		seen := "1001"
		if read {
			seen = "1004"
		}
		f.AddComment(1, app, revisePlanComment(1, seen))
		f.AddLabel(1, app, "stage:plan-review")
		f.RemoveLabel(1, app, "stage:planning")
	})
}

// At a review stage, a comment the App 👍'd as an answer, posted after the
// seen of the run's move there, which no marker names, is a revision
// request: the 👍 acknowledged it, and no run read it (#318). The resumed
// apply drops a result that would leave such a comment (TestReviewResumeLease),
// but a read that lists the comment without the run's move, such as one
// GitHub answers before the move shows, leaves it so. A comment the run read,
// a request at a stage since moved on, and an answer at clarifying are
// processed by their 👍: a pass writes nothing for them, but the override
// it records for the stage set by hand. Each pass is under Replay, a second
// pass writes nothing, and the Inputs are those after it.
func TestReviewResumeAckedRequest(t *testing.T) {
	t.Parallel()
	none := func(testing.TB, *githubtest.Fake) int64 { return 0 }
	specReview, clarifying := []string{"stage:spec-review"}, []string{"stage:clarifying"}
	for _, tt := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		want   outcome
		inputs []string
		quiet  bool // the pass writes nothing
	}{
		{"at spec-review, a comment 👍'd as an answer during the run sends the spec back, as the notes",
			reviewResumeSpecReview(false, reviewResumeAnswered),
			outcome{comments: []string{sentBack("1004")}, labels: clarifying, acks: []string{"1a", "also X"}},
			[]string{"also X"}, false},
		{"at plan-review, one 👍'd during the planner run sends the plan back, as the notes",
			reviewResumePlanReview(false),
			outcome{comments: []string{planSentBack("1004")}, labels: []string{"stage:planning"}, acks: []string{"also X"}},
			[]string{"also X"}, false},
		{"one the interviewer run read isn't a request",
			reviewResumeSpecReview(true, reviewResumeAnswered),
			outcome{labels: specReview, acks: []string{"1a", "also X"}}, nil, true},
		{"one the planner run read isn't a request",
			reviewResumePlanReview(true),
			outcome{labels: []string{"stage:plan-review"}, acks: []string{"also X"}}, nil, true},
		{"one edited after its 👍 gets one reply, as an applied command does",
			reviewResumeSpecReview(false, func(t testing.TB, f *githubtest.Fake) int64 {
				id := reviewResumeAnswered(t, f)
				f.EditComment(id, "owner", "also Y")
				return id
			}),
			outcome{comments: []string{edited("also Y", "1004")}, labels: specReview, acks: []string{"1a", "also Y"}}, nil, false},
		{"one at a stage since moved on, as the owner set planning by hand, isn't handled again",
			func(t testing.TB) *githubtest.Fake {
				f := reviewResumeSpecReview(false, reviewResumeAnswered)(t)
				handSet(t, f, state.SpecReview, state.Planning, 9005)
				return f
			},
			outcome{comments: []string{overridden(state.Planning, "from=spec-review to=planning by=owner event=9005")},
				labels: []string{"stage:planning"}, acks: []string{"1a", "also X"}}, nil, false},
		{"an answer at clarifying, 👍'd after the round's move, isn't 👍'd again",
			filed(nil, func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Story")
				f.AddLabel(1, app, "stage:clarifying")
				post(t, f, 1001, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
				reviewResumeAnswered(t, f)
			}),
			outcome{labels: clarifying, acks: []string{"also X"}}, []string{"also X"}, true},
		{"nor is the run's input at spec-review",
			reviewResumeSpecReview(false, none),
			outcome{labels: specReview, acks: []string{"1a"}}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
			if !reflect.DeepEqual(in, tt.inputs) {
				t.Errorf("inputs = %q, want %q", in, tt.inputs)
			}
			if !tt.quiet {
				return
			}
			f := tt.setup(t)
			if err := visit(t, owners, &in)(f); err != nil {
				t.Fatal(err)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("the pass wrote %+v; want nothing", w)
			}
		})
	}
}

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// stepper runs step 1 on issue 1, with "owner" trusted, and records what its
// handlers see. /hold, /resume and /priority record the command and ack it,
// /resume is rejected unless the issue is held, /approve is rejected, and
// Reactions records "reactions".
type stepper struct {
	calls  []string
	inputs [][]github.Comment // each pass's
}

func (sp *stepper) pass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		apply := daemon.Handler{Apply: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
			sp.calls = append(sp.calls, strings.TrimSpace("/"+c.Name+" "+c.Arg))
			return a.Ack(ctx, c.Comment.ID)
		}}
		resume, priority := apply, apply
		resume.Check = func(_ context.Context, a *daemon.Applier, c daemon.Cmd) error {
			if state.Derive(c.S, a.AppSlug, c.Dirt.Has).Stage != state.Held {
				return daemon.Rejection{Reason: "nothing is held"}
			}
			return nil
		}
		priority.Check = func(_ context.Context, _ *daemon.Applier, c daemon.Cmd) error {
			if c.Arg == "p0" {
				return errors.New("can't read the labels")
			}
			return nil
		}
		// The App is trusted here, to show Step skips it whatever Trust says.
		st := &daemon.Step{Applier: a, Trust: trusted{"owner": true, app: true},
			Commands: map[string]daemon.Handler{"hold": apply, "resume": resume, "priority": priority,
				"approve": {Check: func(context.Context, *daemon.Applier, daemon.Cmd) error {
					return daemon.Rejection{Reason: "the spec changed"}
				}}},
			Reactions: func(context.Context, *daemon.Applier, *github.Snapshot, daemon.Dirt) error {
				sp.calls = append(sp.calls, "reactions")
				return nil
			}}
		in, err := st.Run(context.Background(), s)
		sp.inputs = append(sp.inputs, in)
		return err
	}
}

// at files issue 1 at stage, set by the App: label event 9001 and its marker
// in comment 1001. Then it runs more.
func at(stage state.Stage, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		labelEvent(t, f.AddLabel(1, app, "stage:"+string(stage)), 9001)
		post(t, f, 1001, app, "<!-- issuedog:transition from=intake to="+string(stage)+" run=a -->")
		more(t, f)
	})
}

// post comments on issue 1 as login, failing t unless the comment's ID is want.
func post(t testing.TB, f *githubtest.Fake, want int64, login, body string) {
	t.Helper()
	if id := f.AddComment(1, login, body); id != want {
		t.Fatalf("comment ID = %d, want %d", id, want)
	}
}

func rejected(quote, reason, id string) string {
	return "> " + quote + "\n\nI didn't apply this command: " + reason +
		". Post a new comment to try again.\n\n<!-- issuedog:reply reason=rejected cmd=" + id + " -->"
}

const unknownFrob = "unknown command `/frob`: the commands are " +
	"`/approve`, `/revise <text>`, `/hold`, `/resume`, `/priority pN`, " +
	"`/model [verifier] <name>`, `/effort [verifier] <level>`, `/done`, `/track` and `/ignore`"

const lost = "my reply saying why was lost"

// ivWordingLostEdited is the reply posted again to a 👎'd comment, quoted,
// edited after the 👎, whose reply was lost (#229).
func ivWordingLostEdited(quote, id string) string {
	return "> " + quote + "\n\nI didn't act on this comment, and my reply saying why was lost. " +
		"It was edited after I rejected it, so I've ignored the edit. Post a new comment to try again." +
		"\n\n<!-- issuedog:reply reason=rejected cmd=" + id + " -->"
}

// forged is an App comment's text as mallory rewrites it: under a copy of
// the note MarkDirty would write, which it then leaves alone, so the marker
// stays readable.
func forged(marker string) string {
	return "<!-- issuedog:dirty by=mallory -->\n> ⚠️ issuedog ignores this comment: **mallory** edited it without approval.\n\n" + marker
}

func edited(quote, id string) string {
	return "> " + quote + "\n\nThis was edited after I acted on it, so I've ignored the edit. " +
		"Post a new comment instead.\n\n<!-- issuedog:reply reason=edited cmd=" + id + " -->"
}

func TestStep(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	tests := []struct {
		name     string
		setup    func(testing.TB) *githubtest.Fake
		writes   []string // the clean run's, as method and path, if set
		comments []string // the comments it adds
		calls    []string // the handlers' calls, in order
		acks     []string // the comments the App has 👍'd after it
		downs    []string // the comments the App has 👎'd after it
		inputs   []string
	}{
		{name: "each rejection gets one reply and a 👎, the quote escaped",
			setup: at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob <!-- issuedog:transition to=planning -->")
				post(t, f, 1003, "owner", "/priority p9\nit's urgent") // only the first line is quoted
				post(t, f, 1004, "owner", "/approve.")
				post(t, f, 1005, "owner", "/approve")
			}),
			comments: []string{
				rejected("/frob &lt;!-- issuedog:transition to=planning -->", unknownFrob, "1002"),
				rejected("/priority p9", "bad argument: `/priority` takes p0, p1, p2 or p3", "1003"),
				rejected("/approve.", "bad argument: put a space between `/approve` and what follows it", "1004"),
				rejected("/approve", "the spec changed", "1005")},
			downs: []string{"/frob <!-- issuedog:transition to=planning -->", "/priority p9\nit's urgent", "/approve.", "/approve"}},
		{name: "label events come before commands, and /done with no handler waits",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:needs-human"), 9002)
				post(t, f, 1002, "owner", "/done")
				post(t, f, 1003, "owner", "/approve")
			}),
			comments: []string{"owner set `stage:needs-human` by hand, so I've recorded it as an owner override.\n\n" +
				"<!-- issuedog:transition from=ready to=needs-human by=owner event=9002 -->",
				rejected("/approve", "wrong stage: `/approve` applies only at `spec-review` or `plan-review`, "+
					"and this issue is at `needs-human`", "1003")},
			downs: []string{"/approve"}},
		{name: "a rejection is a 👎, then one reply, and a later /hold doesn't apply it",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/resume")
				post(t, f, 1003, "owner", "/hold")
			}),
			writes:   []string{"POST /issues/comments/1002/reactions", "POST /issues/1/comments", "POST /issues/comments/1003/reactions"},
			comments: []string{rejected("/resume", "nothing is held", "1002")},
			calls:    []string{"/hold"}, acks: []string{"/hold"}, downs: []string{"/resume"}},
		{name: "a rejected command whose reply is made dirty or deleted gets it again, and still never runs",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				for _, id := range []int64{1002, 1005} {
					post(t, f, id, "owner", "/resume")
					f.AddReaction(id, app, "-1")
					post(t, f, id+2, app, rejected("/resume", "nothing is held", strconv.FormatInt(id, 10)))
				}
				post(t, f, 1008, "owner", "/hold") // now /resume would apply
				post(t, f, 1009, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1008 -->")
				f.AddReaction(1008, app, "+1")
				f.EditComment(1004, "mallory", "nothing to see")
				f.DeleteComment(1007)
			}),
			comments: []string{rejected("/resume", lost, "1002"), rejected("/resume", lost, "1005")},
			acks:     []string{"/hold"}, downs: []string{"/resume", "/resume"}},
		{name: "a rejected command edited into plain text isn't an answer; a reply with no 👎 gets only the 👎",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob")
				post(t, f, 1003, app, "<!-- issuedog:reply reason=rejected cmd=1002 -->") // but no 👎 yet
				f.EditComment(1002, "owner", "1a")
				post(t, f, 1004, "owner", "/frob") // its reply was lost
				f.AddReaction(1004, app, "-1")
				f.EditComment(1004, "owner", "2b")
			}),
			// Edited after its 👎, so the reply isn't worded from "2b" (#229).
			comments: []string{ivWordingLostEdited("2b", "1004")}, downs: []string{"1a", "2b"}},
		{name: "a valid command nothing handles waits",
			setup: at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/revise shorter") })},
		{name: "commands are applied oldest first, with their latest text, and once",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/resume")
				f.EditComment(1002, "owner", "/resume x") // before the 👍, so no reply
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, "owner", "/hold")
				post(t, f, 1005, "owner", "/priority p9")
				f.EditComment(1005, "owner", "/priority p1")
			}),
			calls: []string{"/hold", "/priority p1"}, acks: []string{"/resume x", "/hold", "/priority p1"}},
		{name: "only the App's reactions make a command processed",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				f.AddReaction(1002, "mallory", "+1")
				f.AddReaction(1002, "owner", "+1")
				f.AddReaction(1002, "mallory", "-1")
			}),
			calls: []string{"/hold"}, acks: []string{"/hold"}},
		{name: "a 👍'd command edited after the 👍 gets one reply, even edited into plain text",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/priority p1")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "/priority p2 <!-- x -->")
				post(t, f, 1004, app, "<!-- issuedog:transition from=ready to=clarifying run=x -->")
				f.EditComment(1004, "mallory", forged("<!-- issuedog:transition from=ready to=clarifying run=x -->"))
				post(t, f, 1005, "owner", "/priority p3") // posted at ready: the dirty marker doesn't count
				f.AddReaction(1005, app, "+1")
				f.EditComment(1005, "owner", "never mind")
				post(t, f, 1007, "owner", "a note") // not an answer at ready
			}),
			comments: []string{edited("/priority p2 &lt;!-- x -->", "1002"), edited("never mind", "1005")},
			acks:     []string{"/priority p2 <!-- x -->", "never mind"}},
		{name: "an applied command missing its 👍 gets it, and one reply only if edited since its marker",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1002 -->")
				post(t, f, 1004, "owner", "/resume")
				post(t, f, 1005, app, "<!-- issuedog:transition from=held to=ready cmd=1004 -->")
				f.EditComment(1002, "owner", "/hold x")
				f.EditComment(1004, "owner", "/resume x")
				post(t, f, 1006, app, edited("/resume x", "1004")) // replied, but never 👍'd
				post(t, f, 1007, "owner", "/hold")
				f.EditComment(1007, "owner", "/hold y") // before its marker
				post(t, f, 1008, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1007 -->")
			}),
			comments: []string{edited("/hold x", "1002")}, acks: []string{"/hold x", "/resume x", "/hold y"}},
		{name: "an applied command at clarifying isn't an answer",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/priority p1")
				f.AddReaction(1002, app, "+1")
			}),
			acks: []string{"/priority p1"}},
		{name: "a lost edit reply is posted once more, however often the command is edited",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/priority p1")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "/priority p2")
				post(t, f, 1004, app, edited("/priority p2", "1002"))
				f.EditComment(1004, "mallory", "nothing to see")
				f.EditComment(1002, "owner", "/priority p3")
			}),
			comments: []string{edited("/priority p3", "1002")}, acks: []string{"/priority p3"}},
		{name: "an answer is 👍'd once, and its latest text counts until its round ends",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1004, "owner", "2: free text")
			}),
			acks: []string{"1b", "2: free text"}, inputs: []string{"1b", "2: free text"}},
		{name: "seen uses up a round answered in two comments, but not one posted during the run",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "2b")
				post(t, f, 1004, "owner", "3c")
				post(t, f, 1005, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1003 -->")
			}),
			acks: []string{"3c"}, inputs: []string{"3c"}},
		{name: "an old remark from ready isn't an answer, nor replied to when edited after a seen covered it",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "a remark")
				post(t, f, 1003, app, "<!-- issuedog:transition from=ready to=clarifying run=b -->")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1003 -->")
				f.EditComment(1002, "owner", "a remark, edited")
				post(t, f, 1005, "owner", "1a")
			}),
			acks: []string{"1a"}, inputs: []string{"1a"}},
		{name: "a reply doesn't end the round",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "/frob")
			}),
			comments: []string{rejected("/frob", unknownFrob, "1003")},
			acks:     []string{"1a"}, downs: []string{"/frob"}, inputs: []string{"1a"}},
		{name: "an answer edited after a seen covered it gets one reply, and one edited before gets none",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "2a")
				f.AddReaction(1002, app, "+1")
				f.AddReaction(1003, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1006, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1003 -->")
				f.EditComment(1003, "owner", "2b")
			}),
			comments: []string{edited("2b", "1003")}, acks: []string{"1b", "2b"}},
		{name: "an answer edited after a hold, which has no seen, gets no reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner run=h -->")
				f.EditComment(1002, "owner", "1b")
			}),
			acks: []string{"1b"}},
		{name: "an answer edited after a dirty marker's seen covered it gets no reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1002 -->")
				f.EditComment(1004, "mallory", forged("<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1002 -->"))
				f.EditComment(1002, "owner", "1b")
			}),
			acks: []string{"1b"}, inputs: []string{"1b"}},
		// At planning, a plain comment in the planner's round is an answer,
		// as at clarifying (#253). The planner isn't built yet, so its
		// round is a hand-built marker.
		{name: "at planning, an answer to the planner's round is 👍'd once, and its latest text counts until its round ends",
			setup: at(state.Planning, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "<!-- issuedog:transition from=planning to=planning run=p seen=1001 round=3 questions=2 -->")
				post(t, f, 1003, "owner", "1a")
				f.AddReaction(1003, app, "+1")
				f.EditComment(1003, "owner", "1b")
				post(t, f, 1005, "owner", "2: free text")
			}),
			acks: []string{"1b", "2: free text"}, inputs: []string{"1b", "2: free text"}},
		{name: "at planning, an answer edited after a planner move's seen covered it gets one reply, and one edited before gets none",
			setup: at(state.Planning, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "2a")
				f.AddReaction(1002, app, "+1")
				f.AddReaction(1003, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1006, app, "<!-- issuedog:transition from=planning to=planning run=p seen=1003 round=1 questions=1 -->")
				f.EditComment(1003, "owner", "2b")
			}),
			comments: []string{edited("2b", "1003")}, acks: []string{"1b", "2b"}},
		{name: "at planning, seen uses up a comment before the planner's round, and only one after it is the round's answer",
			setup: at(state.Planning, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "a note")
				post(t, f, 1003, app, "<!-- issuedog:transition from=planning to=planning run=p seen=1002 round=1 questions=1 -->")
				post(t, f, 1004, "owner", "1a")
			}),
			acks: []string{"1a"}, inputs: []string{"1a"}},
		{name: "at planning, untrusted, App and dirty comments get no 👍 and no reply",
			setup: at(state.Planning, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "<!-- issuedog:transition from=planning to=planning run=p seen=1001 round=1 questions=1 -->")
				post(t, f, 1003, "mallory", "1a")
				post(t, f, 1004, app, "1a")
				post(t, f, 1005, "owner", "1b")
				f.EditComment(1005, "mallory", "1c")
				f.EditComment(1005, "owner", "1d")
			})},
		{name: "untrusted and App comments don't count",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "mallory", "/frob")
				post(t, f, 1003, app, "/frob")
				post(t, f, 1004, "mallory", "1a")
			})},
		{name: "a dirty command or answer doesn't count, even after a trusted edit",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				for i, body := range []string{"/frob", "1a"} {
					id := 1002 + int64(i)
					post(t, f, id, "owner", body)
					f.EditComment(id, "mallory", body)
					f.EditComment(id, "owner", body)
				}
			})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			githubtest.Replay(t, tt.setup, (&stepper{}).pass(t))

			before := len(tt.setup(t).State().Issues[0].Comments)
			var errs bytes.Buffer
			stop := logtest.CaptureTo(t, &errs, logr.E)
			sp := &stepper{}
			f := run(t, tt.setup, sp.pass(t))
			logr.Wait()
			stop()
			// The second pass calls no handler.
			if want := slices.Concat(tt.calls, []string{"reactions", "reactions"}); !reflect.DeepEqual(sp.calls, want) {
				t.Errorf("calls = %q, want %q", sp.calls, want)
			}
			var writes []string
			for _, w := range f.Writes() {
				writes = append(writes, w.Method+" "+w.Path)
			}
			if tt.writes != nil && !reflect.DeepEqual(writes, tt.writes) {
				t.Errorf("writes = %q, want %q", writes, tt.writes)
			}
			got := f.State().Issues[0]
			var added, acks, downs []string
			for _, c := range got.Comments[before:] {
				added = append(added, c.Body)
			}
			for _, c := range got.Comments {
				if slices.Contains(c.Reactions, app+" +1") {
					acks = append(acks, c.Body)
				}
				if slices.Contains(c.Reactions, app+" -1") {
					downs = append(downs, c.Body)
				}
			}
			if !reflect.DeepEqual(added, tt.comments) {
				t.Errorf("new comments = %q, want %q", added, tt.comments)
			}
			if !reflect.DeepEqual(acks, tt.acks) {
				t.Errorf("👍'd comments = %q, want %q", acks, tt.acks)
			}
			if !reflect.DeepEqual(downs, tt.downs) {
				t.Errorf("👎'd comments = %q, want %q", downs, tt.downs)
			}
			for i, in := range sp.inputs {
				var bodies []string
				for _, c := range in {
					bodies = append(bodies, c.Body)
				}
				if !reflect.DeepEqual(bodies, tt.inputs) {
					t.Errorf("pass %d: inputs = %q, want %q", i+1, bodies, tt.inputs)
				}
			}
			// No rejection is a state error.
			if errs.Len() > 0 {
				t.Errorf("error logs = %q, want none", errs.String())
			}
		})
	}
}

// A Check that fails, rather than rejecting, fails the pass, which writes and
// applies nothing.
func TestStepCheckError(t *testing.T) {
	t.Parallel()
	f := at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/priority p0") })(t)
	sp := &stepper{}
	if err := sp.pass(t)(f); err == nil {
		t.Errorf("pass = nil, want the Check's error")
	}
	if w := f.Writes(); len(w) != 0 || len(sp.calls) != 0 {
		t.Errorf("writes %+v and calls %q, want none", w, sp.calls)
	}
}

// resumeChild is a /resume on issue 1 that resumes its held child, issue 2,
// then acks, so its last write is the 👍. Its Check rejects it unless issue 2
// is held or, if it keeps the Handler contract, issue 2 has its cmd marker.
func resumeChild(keep bool) daemon.Handler {
	child := func(ctx context.Context, a *daemon.Applier) (state.Issue, error) {
		s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, 2)
		if err != nil {
			return state.Issue{}, fmt.Errorf("child: %w", err)
		}
		return state.Derive(s, a.AppSlug, clean), nil
	}
	return daemon.Handler{
		Check: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
			is, err := child(ctx, a)
			if err == nil && is.Stage != state.Held && !(keep && is.Keys[state.CmdKey(c.Comment.ID)]) {
				return daemon.Rejection{Reason: "nothing is held"}
			}
			return err
		},
		Apply: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
			is, err := child(ctx, a)
			if err != nil {
				return err
			}
			if !is.Keys[state.CmdKey(c.Comment.ID)] {
				body := fmt.Sprintf("<!-- issuedog:transition from=held to=ready cmd=%d -->", c.Comment.ID)
				if _, _, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, 2, gogithub.IssueCommentRequest{Body: body}); err != nil {
					return fmt.Errorf("resume the child: %w", err)
				}
			}
			return a.Ack(ctx, c.Comment.ID)
		},
	}
}

// heldChild files issue 1 at ready, with a /resume (comment 1003), and its
// child, issue 2, held by the App (comment 1002).
func heldChild(t testing.TB) *githubtest.Fake {
	return at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
		f.AddIssue("owner", "Child", "body", "stage:held")
		f.AddComment(2, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner run=h -->")
		post(t, f, 1003, "owner", "/resume")
	})(t)
}

func resumePass(t *testing.T, keep bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: a, Trust: trusted{"owner": true}, Commands: map[string]daemon.Handler{"resume": resumeChild(keep)}}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// A handler that keeps the Handler contract survives a crash part way through
// Apply. One whose Check rejects what Apply began has the applied command 👎'd.
func TestHandlerContract(t *testing.T) {
	t.Parallel()
	githubtest.Replay(t, heldChild, resumePass(t, true))

	f := heldChild(t)
	f.FailWrite(2) // the 👍, after issue 2's marker
	if err := resumePass(t, false)(f); err == nil {
		t.Fatal("the first pass didn't crash")
	}
	f.FailWrite(0)
	if err := resumePass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if c := f.State().Issues[0].Comments[1]; c.Body != "/resume" || !slices.Contains(c.Reactions, app+" -1") {
		t.Errorf("comment %+v, want the applied /resume 👎'd by a Check that breaks the contract", c)
	}
}

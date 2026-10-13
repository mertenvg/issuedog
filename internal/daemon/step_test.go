package daemon_test

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// stepper runs step 1 on issue 1, with "owner" trusted, and records what its
// handlers see. /hold, /resume and /priority record the command and ack it,
// /approve is rejected, and Reactions records "reactions".
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
		apply := func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
			sp.calls = append(sp.calls, strings.TrimSpace("/"+c.Name+" "+c.Arg))
			return a.Ack(ctx, c.Comment.ID)
		}
		// The App is trusted here, to show Step skips it whatever Trust says.
		st := &daemon.Step{Applier: a, Trust: trusted{"owner": true, app: true},
			Commands: map[string]daemon.Handler{"hold": apply, "resume": apply, "priority": apply,
				"approve": func(context.Context, *daemon.Applier, daemon.Cmd) error {
					return daemon.Rejection{Reason: "the spec changed"}
				}},
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
	"`/approve`, `/revise <text>`, `/hold`, `/resume`, `/priority pN` and `/done`"

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
	tests := []struct {
		name     string
		setup    func(testing.TB) *githubtest.Fake
		comments []string // the comments it adds
		calls    []string // the handlers' calls, in order
		acks     []string // the comments the App has 👍'd after it
		downs    []string // the comments the App has 👎'd after it
		inputs   []string
	}{
		{name: "each rejection gets one reply and a 👎, the quote escaped",
			setup: at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob <!-- issuedog:transition to=planning -->")
				post(t, f, 1003, "owner", "/priority p9")
				post(t, f, 1004, "owner", "/approve.")
				post(t, f, 1005, "owner", "/approve")
			}),
			comments: []string{
				rejected("/frob &lt;!-- issuedog:transition to=planning -->", unknownFrob, "1002"),
				rejected("/priority p9", "bad argument: `/priority` takes p0, p1, p2 or p3", "1003"),
				rejected("/approve.", "bad argument: put a space between `/approve` and what follows it", "1004"),
				rejected("/approve", "the spec changed", "1005")},
			downs: []string{"/frob <!-- issuedog:transition to=planning -->", "/priority p9", "/approve.", "/approve"}},
		{name: "label events come before commands",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:needs-human"), 9002)
				post(t, f, 1002, "owner", "/done")
				post(t, f, 1003, "owner", "/approve")
			}),
			comments: []string{"owner set `stage:needs-human` by hand, so I've recorded it as an owner override.\n\n" +
				"<!-- issuedog:transition from=ready to=needs-human by=owner event=9002 -->",
				rejected("/done", "`/done` isn't handled yet", "1002"),
				rejected("/approve", "wrong stage: `/approve` applies only at `spec-review` or `plan-review`, "+
					"and this issue is at `needs-human`", "1003")},
			downs: []string{"/done", "/approve"}},
		{name: "a rejected command stays rejected when its reply is made dirty or deleted",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				for _, id := range []int64{1002, 1005} {
					post(t, f, id, "owner", "/resume")
					post(t, f, id+1, app, rejected("/resume", "nothing is held", strconv.FormatInt(id, 10)))
					f.AddReaction(id, app, "-1")
				}
				post(t, f, 1008, "owner", "/hold")
				f.EditComment(1003, "mallory", "nothing to see")
				f.DeleteComment(1006)
			}),
			calls: []string{"/hold"}, acks: []string{"/hold"}, downs: []string{"/resume", "/resume"}},
		{name: "a rejected command edited into plain text isn't an answer, and gets only its missing 👎",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob")
				post(t, f, 1003, app, "<!-- issuedog:reply reason=rejected cmd=1002 -->") // but no 👎 yet
				f.EditComment(1002, "owner", "1a")
				post(t, f, 1004, "owner", "/frob") // its reply was lost
				f.AddReaction(1004, app, "-1")
				f.EditComment(1004, "owner", "2b")
			}),
			downs: []string{"1a", "2b"}},
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
		{name: "an applied command missing its 👍 gets it, and one reply if edited since",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1002 -->")
				post(t, f, 1004, "owner", "/resume")
				post(t, f, 1005, app, "<!-- issuedog:transition from=held to=ready cmd=1004 -->")
				f.EditComment(1002, "owner", "/hold x")
				f.EditComment(1004, "owner", "/resume x")
				post(t, f, 1006, app, edited("/resume x", "1004")) // replied, but never 👍'd
			}),
			comments: []string{edited("/hold x", "1002")}, acks: []string{"/hold x", "/resume x"}},
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
			stop := logr.AddWriter(&errs, logr.WithFilter(logr.E))
			sp := &stepper{}
			f := run(t, tt.setup, sp.pass(t))
			logr.Wait()
			stop()
			// The second pass calls no handler.
			if want := slices.Concat(tt.calls, []string{"reactions", "reactions"}); !reflect.DeepEqual(sp.calls, want) {
				t.Errorf("calls = %q, want %q", sp.calls, want)
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

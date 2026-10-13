package daemon_test

import (
	"bytes"
	"context"
	"reflect"
	"slices"
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
	inputs []github.Comment
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
		sp.inputs, err = st.Run(context.Background(), s)
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

func edited(quote, id string) string {
	return "> " + quote + "\n\nThis was edited after I acted on it, so I've ignored the edit. " +
		"Post a new comment instead.\n\n<!-- issuedog:reply reason=edited cmd=" + id + " -->"
}

// lostReply sets up a /resume rejected with the App's reply and 👎, then a
// /hold, then loses the reply: mallory edits it, so it's dirty, or it's
// deleted.
func lostReply(deleted bool) func(testing.TB) *githubtest.Fake {
	return at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
		post(t, f, 1002, "owner", "/resume")
		post(t, f, 1003, app, rejected("/resume", "nothing is held", "1002"))
		f.AddReaction(1002, app, "-1")
		post(t, f, 1005, "owner", "/hold")
		if deleted {
			f.DeleteComment(1003)
		} else {
			f.EditComment(1003, "mallory", "nothing to see")
		}
	})
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
		{name: "an unknown command is rejected, its quote escaped",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob <!-- issuedog:transition to=planning -->")
			}),
			comments: []string{rejected("/frob &lt;!-- issuedog:transition to=planning -->", unknownFrob, "1002")},
			downs:    []string{"/frob <!-- issuedog:transition to=planning -->"}},
		{name: "a bad argument is rejected",
			setup:    at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/priority p9") }),
			comments: []string{rejected("/priority p9", "bad argument: `/priority` takes p0, p1, p2 or p3", "1002")},
			downs:    []string{"/priority p9"}},
		{name: "a command at the wrong stage is rejected",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			comments: []string{rejected("/approve",
				"wrong stage: `/approve` applies only at `spec-review` or `plan-review`, and this issue is at `ready`", "1002")},
			downs: []string{"/approve"}},
		{name: "/done is rejected as not handled yet",
			setup:    at(state.NeedsHuman, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/done") }),
			comments: []string{rejected("/done", "`/done` isn't handled yet", "1002")}, downs: []string{"/done"}},
		{name: "a handler's rejection gets a reply",
			setup:    at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			comments: []string{rejected("/approve", "the spec changed", "1002")}, downs: []string{"/approve"}},
		{name: "a known command glued to punctuation has a bad argument",
			setup:    at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve.") }),
			comments: []string{rejected("/approve.", "bad argument: put a space between `/approve` and what follows it", "1002")},
			downs:    []string{"/approve."}},
		{name: "a rejected command whose 👎 is missing only gets its 👎, even after an edit",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob")
				post(t, f, 1003, app, "<!-- issuedog:reply reason=rejected cmd=1002 -->")
				f.EditComment(1002, "owner", "/hold")
			}),
			downs: []string{"/hold"}},
		{name: "a rejected command stays rejected when its reply is made dirty",
			setup: lostReply(false), calls: []string{"/hold"}, acks: []string{"/hold"}, downs: []string{"/resume"}},
		{name: "a rejected command stays rejected when its reply is deleted",
			setup: lostReply(true), calls: []string{"/hold"}, acks: []string{"/hold"}, downs: []string{"/resume"}},
		{name: "a valid command nothing handles waits",
			setup: at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/revise shorter") })},
		{name: "commands are applied oldest first, with their latest text",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, "owner", "/priority p9")
				f.EditComment(1003, "owner", "/priority p1")
			}),
			calls: []string{"/hold", "/priority p1"}, acks: []string{"/hold", "/priority p1"}},
		{name: "a command the App 👍'd after an edit isn't applied again",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				f.EditComment(1002, "owner", "/resume")
				f.AddReaction(1002, app, "+1")
			}),
			acks: []string{"/resume"}},
		{name: "a command edited after the App's 👍, with no marker, gets one reply",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/priority p1")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "/priority p2 <!-- x -->")
			}),
			comments: []string{edited("/priority p2 &lt;!-- x -->", "1002")}, acks: []string{"/priority p2 <!-- x -->"}},
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
		{name: "only the App's 👍 makes a command processed",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				f.AddReaction(1002, "mallory", "+1")
				f.AddReaction(1002, "owner", "+1")
			}),
			calls: []string{"/hold"}, acks: []string{"/hold"}},
		{name: "only the App's 👎 makes a command processed",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				f.AddReaction(1002, "mallory", "-1")
			}),
			calls: []string{"/hold"}, acks: []string{"/hold"}},
		{name: "a command a marker names only gets its 👍",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1002 -->")
			}),
			acks: []string{"/hold"}},
		{name: "an answer is 👍'd once, and its latest text counts until its round ends",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1004, "owner", "2: free text")
			}),
			acks: []string{"1b", "2: free text"}, inputs: []string{"1b", "2: free text"}},
		{name: "a round answered in two comments is used up by the next marker's seen",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "2b")
				f.AddReaction(1002, app, "+1")
				f.AddReaction(1003, app, "+1")
				post(t, f, 1006, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1003 -->")
			}),
			acks: []string{"1a", "2b"}},
		{name: "a comment posted during a run stays in Inputs",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, "owner", "3c")
				post(t, f, 1005, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1002 -->")
			}),
			acks: []string{"1a", "3c"}, inputs: []string{"3c"}},
		{name: "an old remark from ready isn't in a later round's Inputs",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=ready run=b seen=1002 -->")
				post(t, f, 1005, "owner", "a remark")
				post(t, f, 1006, app, "<!-- issuedog:transition from=ready to=in-progress run=c -->")
				post(t, f, 1007, app, "<!-- issuedog:transition from=in-progress to=clarifying run=c -->")
				post(t, f, 1008, "owner", "2b")
			}),
			acks: []string{"1a", "2b"}, inputs: []string{"2b"}},
		{name: "a reply doesn't end the round",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "/frob")
			}),
			comments: []string{rejected("/frob", unknownFrob, "1003")},
			acks:     []string{"1a"}, downs: []string{"/frob"}, inputs: []string{"1a"}},
		{name: "comments from before the transition into clarifying aren't answers",
			setup: at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "thoughts")
				post(t, f, 1003, app, "<!-- issuedog:transition from=spec-review to=clarifying run=b -->")
			})},
		{name: "an answer edited after its 👍 but before a marker's seen covered it gets no reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1002 -->")
			}),
			acks: []string{"1b"}},
		{name: "an answer edited after a marker's seen covered it gets one reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b seen=1002 -->")
				f.EditComment(1002, "owner", "1b")
			}),
			comments: []string{edited("1b", "1002")}, acks: []string{"1b"}},
		{name: "a plain comment outside clarifying isn't an answer",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "a note") })},
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
		{name: "label events come before commands",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:needs-human"), 9002)
				post(t, f, 1002, "owner", "/done")
			}),
			comments: []string{"owner set `stage:needs-human` by hand, so I've recorded it as an owner override.\n\n" +
				"<!-- issuedog:transition from=ready to=needs-human by=owner event=9002 -->",
				rejected("/done", "`/done` isn't handled yet", "1002")},
			downs: []string{"/done"}},
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
			var added, acks, downs, inputs []string
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
			for _, c := range sp.inputs {
				inputs = append(inputs, c.Body)
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
			if !reflect.DeepEqual(inputs, tt.inputs) {
				t.Errorf("inputs = %q, want %q", inputs, tt.inputs)
			}
			// No rejection is a state error.
			if errs.Len() > 0 {
				t.Errorf("error logs = %q, want none", errs.String())
			}
		})
	}
}

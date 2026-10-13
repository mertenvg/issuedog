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
		inputs   []string
	}{
		{name: "an unknown command is rejected, its quote escaped",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob <!-- issuedog:transition to=planning -->")
			}),
			comments: []string{rejected("/frob &lt;!-- issuedog:transition to=planning -->", "unknown command `/frob`: the commands are "+
				"`/approve`, `/revise <text>`, `/hold`, `/resume`, `/priority pN` and `/done`", "1002")}},
		{name: "a bad argument is rejected",
			setup:    at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/priority p9") }),
			comments: []string{rejected("/priority p9", "bad argument: `/priority` takes p0, p1, p2 or p3", "1002")}},
		{name: "a command at the wrong stage is rejected",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			comments: []string{rejected("/approve",
				"wrong stage: `/approve` applies only at `spec-review` or `plan-review`, and this issue is at `ready`", "1002")}},
		{name: "/done is rejected as not handled yet",
			setup:    at(state.NeedsHuman, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/done") }),
			comments: []string{rejected("/done", "`/done` isn't handled yet", "1002")}},
		{name: "a handler's rejection gets a reply",
			setup:    at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			comments: []string{rejected("/approve", "the spec changed", "1002")}},
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
		{name: "only the App's 👍 makes a command processed",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				f.AddReaction(1002, "mallory", "+1")
				f.AddReaction(1002, "owner", "+1")
			}),
			calls: []string{"/hold"}, acks: []string{"/hold"}},
		{name: "a known command glued to punctuation has a bad argument",
			setup:    at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve.") }),
			comments: []string{rejected("/approve.", "bad argument: put a space between `/approve` and what follows it", "1002")}},
		{name: "a command a marker names only gets its 👍",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=1002 -->")
			}),
			acks: []string{"/hold"}},
		{name: "a command with a reply isn't processed again, even after an edit",
			setup: at(state.Ready, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/frob")
				post(t, f, 1003, app, "<!-- issuedog:reply reason=rejected cmd=1002 -->")
				f.EditComment(1002, "owner", "/hold")
			})},
		{name: "answers are 👍'd and listed until a marker uses them",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "used")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b cmd=1002 -->")
				post(t, f, 1005, "owner", "1a")
				f.AddReaction(1005, app, "+1")
				f.EditComment(1005, "owner", "1b") // not used yet, so its latest text counts
				post(t, f, 1007, "owner", "2: free text")
			}),
			acks: []string{"used", "1b", "2: free text"}, inputs: []string{"1b", "2: free text"}},
		{name: "an answer edited after its 👍 but before a marker used it gets no reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "1b")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b cmd=1002 -->")
			}),
			acks: []string{"1b"}},
		{name: "an answer edited after a marker used it gets one reply",
			setup: at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "1a")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=clarifying run=b cmd=1002 -->")
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
				rejected("/done", "`/done` isn't handled yet", "1002")}},
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
			var added, acks, inputs []string
			for _, c := range got.Comments[before:] {
				added = append(added, c.Body)
			}
			for _, c := range got.Comments {
				if slices.Contains(c.Reactions, app+" +1") {
					acks = append(acks, c.Body)
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

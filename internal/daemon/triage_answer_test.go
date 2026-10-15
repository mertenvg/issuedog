package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// flagTracked is the App's move of issue n out of intake, keyed cmd=key,
// as a /track or a 👍 on a flag makes it: stage:clarifying, then the
// marker, whose comment's ID it returns.
func flagTracked(f *githubtest.Fake, n int, key string) int64 {
	f.AddLabel(n, app, "stage:clarifying")
	return f.AddComment(n, app, "Tracked.\n\n<!-- issuedog:transition from=intake to=clarifying cmd="+key+" -->")
}

// flagPosted is a triage flag on issue n, recommending handle, and returns
// its comment's ID.
func flagPosted(f *githubtest.Fake, n int) int64 {
	return f.AddComment(n, app, flagText("handle"))
}

// flagText is a flag recommending rec.
func flagText(rec string) string {
	return "Should issuedog handle it?\n\n<!-- issuedog:triage verdict=ask run=t1 recommend=" + rec + " -->"
}

// flagLeftText is a "left" comment by run.
const flagLeftText = "Left.\n\n<!-- issuedog:triage verdict=leave run=t0 -->"

// flagIssue files issue 1 by author, then runs more.
func flagIssue(author string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue(author, "Title", "body")
		if more != nil {
			more(t, f)
		}
		return f
	}
}

// flagWith posts the flag, recommending rec, as comment 1001, with the
// App's triage:ask, as triage leaves it, then runs more.
func flagWith(rec string, more func(t testing.TB, f *githubtest.Fake)) func(t testing.TB, f *githubtest.Fake) {
	return func(t testing.TB, f *githubtest.Fake) {
		t.Helper()
		if id := f.AddComment(1, app, flagText(rec)); id != 1001 {
			t.Fatalf("the flag's ID = %d, want 1001", id)
		}
		f.AddLabel(1, app, "triage:ask")
		if more != nil {
			more(t, f)
		}
	}
}

// flagWant fails t unless the next ID the fake gives, a comment's or a
// reaction's, is id.
func flagWant(t testing.TB, id, want int64) {
	t.Helper()
	if id != want {
		t.Fatalf("ID = %d, want %d", id, want)
	}
}

// flagPass is a visit to issue 1 as the loop makes it, with the flag
// answers and tr: step 1, then IntakeVisit, then FlagVisit.
func flagPass(t testing.TB, tr github.Trust) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr,
			Commands: daemon.FlagCommands(), Reactions: daemon.FlagByReaction(tr)}
		if _, err := st.Run(context.Background(), s); err != nil {
			return err
		}
		if err := st.IntakeVisit(context.Background(), s); err != nil {
			return err
		}
		return st.FlagVisit(context.Background(), s)
	}
}

// flagAdmitted is the move into the pipeline that by made, keyed cmd=key.
func flagAdmitted(by, key string) string {
	return by + ", so this issue is in the pipeline (SPEC §6.2). I've moved it to `clarifying`.\n\n" +
		"<!-- issuedog:transition from=intake to=clarifying cmd=" + key + " -->"
}

// flagLeft is the "left" comment that why records, keyed cmd=key.
func flagLeft(why, key string) string {
	return why + " A code owner can post `/track` to bring it into the pipeline.\n\n<!-- issuedog:triage verdict=leave cmd=" + key + " -->"
}

const (
	flagTakes  = "owner's 👍 on the flag takes its recommendation"
	flagIgnore = "owner posted `/ignore`, so issuedog leaves this issue to people."
	flagFrob   = "its type `frob` isn't one the pipeline knows (SPEC §4)"
)

var flagOwners = trusted{"owner": true, "owner2": true}

// registerReverted is the App's revert of mallory's approved, whose
// labeled event is key.
func registerReverted(key string) string {
	return "I removed `approved`: only trusted users can change pipeline labels, and mallory isn't one.\n\n<!-- issuedog:revert key=" + key + " -->"
}

// A code owner's 👍 on the latest flag takes its recommendation, and every
// other 👍 is ignored. Each runs under Replay.
func TestFlagByReaction(t *testing.T) {
	ask, clarifying := []string{"triage:ask"}, []string{"stage:clarifying"}
	up := func(by string) func(testing.TB, *githubtest.Fake) {
		return func(t testing.TB, f *githubtest.Fake) { flagWant(t, f.AddReaction(1001, by, "+1"), 1002) }
	}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		trust trusted // nil is flagOwners
		want  outcome
	}{
		{name: "a 👍 on a flag recommending handle admits the issue",
			setup: flagIssue("owner", flagWith("handle", up("owner"))),
			want:  outcome{comments: []string{flagAdmitted(flagTakes, "r1002")}, labels: clarifying}},
		{name: "a 👍 on a flag recommending handle admits eve's issue",
			setup: flagIssue("eve", flagWith("handle", up("owner"))),
			want:  outcome{comments: []string{flagAdmitted(flagTakes, "r1002")}, labels: clarifying}},
		{name: "a 👍 on a flag recommending leave records left",
			setup: flagIssue("eve", flagWith("leave", up("owner"))),
			want:  outcome{comments: []string{flagLeft(flagTakes+": issuedog leaves this issue to people.", "r1002")}, labels: []string{"triage:left"}}},
		{name: "the earliest trusted 👍 counts",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "mallory", "+1")
				flagWant(t, f.AddReaction(1001, "owner2", "+1"), 1003)
				f.AddReaction(1001, "owner", "+1")
			})),
			want: outcome{comments: []string{flagAdmitted("owner2's 👍 on the flag takes its recommendation", "r1003")}, labels: clarifying}},
		{name: "a 👍 on a task's flag says it waits",
			setup: flagIssue("owner", func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "type:task")
				flagWith("handle", up("owner"))(t, f)
			}),
			want: outcome{comments: []string{strings.Replace(flagAdmitted(flagTakes, "r1002"), "`clarifying`.", "`clarifying`."+
				" It's a task: bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3).", 1)},
				labels: []string{"stage:clarifying", "type:task"}}},
		{name: "a 👍 on an older flag is ignored",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, flagText("handle"))
				f.AddReaction(1001, "owner", "+1")
			})),
			want: outcome{labels: ask}},
		{name: "a 👍 on a flag that's been answered by left is ignored",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, flagLeftText)
				f.RemoveLabel(1, app, "triage:ask")
				f.AddLabel(1, app, "triage:left")
				f.AddReaction(1001, "owner", "+1")
			})),
			want: outcome{labels: []string{"triage:left"}}},
		{name: "an untrusted 👍 is ignored",
			setup: flagIssue("owner", flagWith("handle", up("mallory"))),
			want:  outcome{labels: ask}},
		{name: "the App's 👍 is ignored, whatever Trust says",
			setup: flagIssue("owner", flagWith("handle", up(app))),
			trust: trusted{"owner": true, app: true},
			want:  outcome{labels: ask, acks: []string{flagText("handle")}}},
		{name: "a reaction that isn't 👍 is ignored",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "heart") })),
			want:  outcome{labels: ask}},
		{name: "a flag with no recommendation is ignored",
			setup: flagIssue("owner", flagWith("maybe", up("owner"))),
			want:  outcome{labels: ask}},
		{name: "a 👍 recommending leave on an issue with a stage label is ignored",
			setup: flagIssue("eve", func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				flagWith("leave", up("owner"))(t, f)
			}),
			want: outcome{labels: []string{"stage:intake"}}},
		// approved added in the same pass as the 👍 (#141's verify): no
		// "left" comment beside IntakeVisit's admission, whose move then
		// answers the flag.
		{name: "a 👍 recommending leave on an issue with approved is ignored, and IntakeVisit admits it",
			setup: flagIssue("eve", flagWith("leave", func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9002)
				up("owner")(t, f)
			})),
			want: outcome{comments: []string{admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "a 👍 recommending leave applies once mallory's approved is reverted",
			setup: flagIssue("eve", flagWith("leave", func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "mallory", "approved"), 9002)
				up("owner")(t, f)
			})),
			want: outcome{comments: []string{registerReverted("9002"), flagLeft(flagTakes+": issuedog leaves this issue to people.", "r1002")},
				labels: []string{"triage:left"}}},
		{name: "a 👍 recommending handle admits an issue with stage:intake",
			setup: flagIssue("eve", func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				flagWith("handle", up("owner"))(t, f)
			}),
			want: outcome{comments: []string{flagAdmitted(flagTakes, "r1002")}, labels: clarifying}},
		{name: "a 👍 after a move answered the flag is ignored",
			setup: flagIssue("eve", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				flagTracked(f, 1, "5")
				f.RemoveLabel(1, "owner", "stage:clarifying")
				ev := f.AddLabel(1, "owner", "stage:intake")
				f.AddComment(1, app, fmt.Sprintf("<!-- issuedog:transition from=clarifying to=intake by=owner event=%d -->", ev))
				f.AddReaction(1001, "owner", "+1")
			})),
			want: outcome{labels: []string{"stage:intake"}}},
		{name: "a 👍 for a type the pipeline doesn't know is reported once, as a type error keyed by its event",
			setup: flagIssue("owner", func(t testing.TB, f *githubtest.Fake) {
				flagWant(t, f.AddLabel(1, "owner", "type:frob"), 9001)
				flagWith("handle", up("owner"))(t, f)
			}),
			want: outcome{comments: []string{typeErrWant("frob", "intake", 9001, true)}, labels: []string{"triage:ask", "type:frob"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := tt.trust
			if tr == nil {
				tr = flagOwners
			}
			check(t, tt.setup, flagPass(t, tr), tt.want)
		})
	}
}

// /track admits a flagged or left issue, and /ignore records left, at
// intake only. Each runs under Replay.
func TestFlagCommands(t *testing.T) {
	clarifying := []string{"stage:clarifying"}
	post := func(body string, want int64) func(testing.TB, *githubtest.Fake) {
		return func(t testing.TB, f *githubtest.Fake) { flagWant(t, f.AddComment(1, "owner", body), want) }
	}
	at := func(stage string, more func(testing.TB, *githubtest.Fake)) func(testing.TB, *githubtest.Fake) {
		return func(t testing.TB, f *githubtest.Fake) {
			f.AddLabel(1, app, "stage:"+stage)
			f.AddComment(1, app, "<!-- issuedog:transition from=intake to="+stage+" run=a -->")
			more(t, f)
		}
	}
	left := func(more func(testing.TB, *githubtest.Fake)) func(testing.TB, *githubtest.Fake) {
		return func(t testing.TB, f *githubtest.Fake) {
			f.AddComment(1, app, flagLeftText)
			f.AddLabel(1, app, "triage:left")
			more(t, f)
		}
	}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{name: "/track admits a flagged issue",
			setup: flagIssue("owner", flagWith("leave", post("/track", 1002))),
			want:  outcome{comments: []string{flagAdmitted("owner posted `/track`", "1002")}, labels: clarifying, acks: []string{"/track"}}},
		{name: "/track admits eve's issue that was left",
			setup: flagIssue("eve", left(post("/track", 1002))),
			want:  outcome{comments: []string{flagAdmitted("owner posted `/track`", "1002")}, labels: clarifying, acks: []string{"/track"}}},
		{name: "/track admits an issue triage hasn't seen",
			setup: flagIssue("eve", post("/track", 1001)),
			want:  outcome{comments: []string{flagAdmitted("owner posted `/track`", "1001")}, labels: clarifying, acks: []string{"/track"}}},
		{name: "/track on an issue of a type the pipeline doesn't know is rejected",
			setup: flagIssue("owner", func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "type:frob")
				post("/track", 1001)(t, f)
			}),
			want: outcome{comments: []string{rejected("/track", "this issue can't move to `clarifying`: "+flagFrob, "1001")},
				labels: []string{"type:frob"}, downs: []string{"/track"}}},
		{name: "/track at clarifying is rejected",
			setup: flagIssue("owner", at("clarifying", post("/track", 1002))),
			want: outcome{comments: []string{rejected("/track", "wrong stage: `/track` applies only at `intake`, and this issue is at `clarifying`", "1002")},
				labels: clarifying, downs: []string{"/track"}}},
		{name: "an untrusted /track is ignored",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) { f.AddComment(1, "mallory", "/track") })),
			want:  outcome{labels: []string{"triage:ask"}}},
		{name: "/ignore records left on a flagged issue",
			setup: flagIssue("owner", flagWith("handle", post("/ignore", 1002))),
			want:  outcome{comments: []string{flagLeft(flagIgnore, "1002")}, labels: []string{"triage:left"}, acks: []string{"/ignore"}}},
		// approved added in the same pass as /ignore (#141's verify): no
		// "left" comment beside IntakeVisit's admission.
		{name: "/ignore on an issue with approved is rejected, and IntakeVisit admits it",
			setup: flagIssue("eve", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9002)
				post("/ignore", 1002)(t, f)
			})),
			want: outcome{comments: []string{rejected("/ignore", "this issue has `approved`, which brings it into the pipeline instead", "1002"),
				admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}, downs: []string{"/ignore"}}},
		{name: "/ignore applies once mallory's approved is reverted",
			setup: flagIssue("eve", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "mallory", "approved"), 9002)
				post("/ignore", 1002)(t, f)
			})),
			want: outcome{comments: []string{registerReverted("9002"), flagLeft(flagIgnore, "1002")},
				labels: []string{"triage:left"}, acks: []string{"/ignore"}}},
		{name: "/ignore records left on an issue triage hasn't seen",
			setup: flagIssue("eve", post("/IGNORE", 1001)),
			want:  outcome{comments: []string{flagLeft(flagIgnore, "1001")}, labels: []string{"triage:left"}, acks: []string{"/IGNORE"}}},
		{name: "/ignore at clarifying is rejected",
			setup: flagIssue("owner", at("clarifying", post("/ignore", 1002))),
			want: outcome{comments: []string{rejected("/ignore", "wrong stage: `/ignore` applies only at `intake`, and this issue is at `clarifying`", "1002")},
				labels: clarifying, downs: []string{"/ignore"}}},
		{name: "/ignore on an issue with stage:intake is rejected",
			setup: flagIssue("eve", func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				post("/ignore", 1001)(t, f)
			}),
			want: outcome{comments: []string{rejected("/ignore", "only an issue with no stage label can be left to people, and this one has `stage:intake`", "1001")},
				labels: []string{"stage:intake"}, downs: []string{"/ignore"}}},
		{name: "/ignore on an issue left already is rejected for good",
			setup: flagIssue("owner", left(post("/ignore", 1002))),
			want: outcome{comments: []string{final("/ignore", "I didn't apply this command", "issuedog has left this issue to people already", "1002")},
				labels: []string{"triage:left"}, downs: []string{"/ignore"}}},
		{name: "/ignore, then /track",
			setup: flagIssue("owner", flagWith("handle", func(t testing.TB, f *githubtest.Fake) {
				post("/ignore", 1002)(t, f)
				post("/track", 1003)(t, f)
			})),
			want: outcome{comments: []string{flagLeft(flagIgnore, "1002"), flagAdmitted("owner posted `/track`", "1003")},
				labels: clarifying, acks: []string{"/ignore", "/track"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, flagPass(t, flagOwners), tt.want) })
	}
}

// FlagVisit brings the triage:* labels in line with the latest triage
// marker, and writes no comment. Each runs under Replay.
func TestFlagVisitLabels(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  []string
		moves int // the comments IntakeVisit posts: no other comment is new
	}{
		{"a triage:ask someone removed is put back",
			flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.RemoveLabel(1, "mallory", "triage:ask") })),
			[]string{"triage:ask"}, 0},
		{"a triage:ask lost to a crash is put back",
			flagIssue("eve", func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, app, flagText("handle")) }),
			[]string{"triage:ask"}, 0},
		{"a triage:left someone removed is put back",
			flagIssue("owner", func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, app, flagLeftText) }),
			[]string{"triage:left"}, 0},
		{"a triage:ask added with no flag is removed",
			flagIssue("owner", func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "mallory", "Triage:Ask") }),
			nil, 0},
		{"a flag, then left, swaps the labels",
			flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, app, flagLeftText) })),
			[]string{"triage:left"}, 0},
		{"a dirty flag counts for nothing",
			flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.EditComment(1001, "owner", "edited") })),
			nil, 0},
		{"both come off once the issue is in the pipeline",
			flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, app, "triage:left")
				flagTracked(f, 1, "5")
			})),
			[]string{"stage:clarifying"}, 0},
		{"both come off an issue admitted by approved",
			flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "approved") })),
			[]string{"approved", "stage:clarifying"}, 1},
		{"both come off an issue with stage:intake",
			flagIssue("eve", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "stage:intake") })),
			[]string{"stage:intake"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			githubtest.Replay(t, tt.setup, flagPass(t, flagOwners))
			f := run(t, tt.setup, flagPass(t, flagOwners))
			is := f.State().Issues[0]
			if !slices.Equal(is.Labels, tt.want) {
				t.Errorf("labels %v, want %v", is.Labels, tt.want)
			}
			before := len(tt.setup(t).State().Issues[0].Comments)
			if got := is.Comments[before:]; len(got) != tt.moves {
				t.Errorf("new comments %+v, want %d, IntakeVisit's", got, tt.moves)
			}
		})
	}
}

// FlagVisit leaves a closed issue's labels alone.
func TestFlagVisitClosed(t *testing.T) {
	f := flagIssue("owner", func(_ testing.TB, f *githubtest.Fake) {
		f.AddLabel(1, "mallory", "triage:ask")
		f.Close(1)
	})(t)
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&daemon.Step{Applier: a, Trust: flagOwners}).FlagVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("FlagVisit wrote %+v to a closed issue, want nothing", w)
	}
}

// A 👍 on a flag recommending leave applies once. Its "left" comment is
// then the latest triage marker, so a second visit writes nothing, even
// with the issue still at intake and no stage label, and a visit after a
// crash between that comment and triage:left only adds the label, by
// FlagVisit: no second "left" comment.
func TestFlagLeaveByReactionAppliesOnce(t *testing.T) {
	setup := flagIssue("owner", flagWith("leave", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }))
	want := []string{flagText("leave"), flagLeft(flagTakes+": issuedog leaves this issue to people.", "r1002")}
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "a second visit", true: "after a crash before the label"}[crash], func(t *testing.T) {
			f := setup(t)
			if crash {
				f.FailWrite(2) // the add of triage:left, after the comment
			}
			pass := flagPass(t, flagOwners)
			if err := pass(f); (err != nil) != crash {
				t.Fatalf("first visit: %v", err)
			}
			if got := f.State().Issues[0].Labels; crash && !slices.Equal(got, []string{"triage:ask"}) {
				t.Fatalf("labels after the crash = %v, want triage:ask still", got)
			}
			n := len(f.Writes())
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if got := commentBodies(f, 1); !slices.Equal(got, want) || !slices.Equal(is.Labels, []string{"triage:left"}) {
				t.Errorf("comments %q, labels %v; want one left comment and triage:left", got, is.Labels)
			}
			for _, w := range f.Writes()[n:] {
				if !strings.Contains(w.Path, "/labels") {
					t.Errorf("the second visit wrote %+v, want only label writes", w)
				}
			}
			if !crash && len(f.Writes()) != n {
				t.Errorf("the second visit wrote %+v, want nothing", f.Writes()[n:])
			}
		})
	}
}

// FlagByReaction reads no reactions on a closed issue, or on one past
// intake, whatever its flag says.
func TestFlagByReactionReadsNothingOffIntake(t *testing.T) {
	for name, more := range map[string]func(f *githubtest.Fake){
		"closed":        func(f *githubtest.Fake) { f.Close(1) },
		"at clarifying": func(f *githubtest.Fake) { flagTracked(f, 1, "r1002") },
		"held from intake": func(f *githubtest.Fake) {
			f.AddComment(1, app, "<!-- issuedog:transition from=intake to=held at=intake reason=owner cmd=5 -->")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }))(t)
			more(f)
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			before := len(f.Requests())
			if err := daemon.FlagByReaction(flagOwners)(context.Background(), a, s, daemon.Dirt{}); err != nil {
				t.Fatal(err)
			}
			if got := f.Requests()[before:]; len(got) != 0 {
				t.Errorf("FlagByReaction made %+v, want no request", got)
			}
		})
	}
}

// An error reading trust or the reactions, or making a write, fails the
// pass, wrapped.
func TestFlagByReactionErrors(t *testing.T) {
	status := func(err error, code int) bool {
		var e *gogithub.ErrorResponse
		return errors.As(err, &e) && e.Response.StatusCode == code
	}
	for _, tt := range []struct {
		name  string
		rec   string
		trust github.Trust
		gone  bool // delete the flag after the snapshot
		fail  int  // FailWrite's n
		want  func(error) bool
	}{
		{"trust", "handle", failTrust{}, false, 0, func(err error) bool {
			return errors.Is(err, errTrust) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: trust owner: ")
		}},
		{"reactions", "handle", flagOwners, true, 0, func(err error) bool {
			return status(err, http.StatusNotFound) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: ")
		}},
		{"the admission's marker", "handle", flagOwners, false, 1, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: move o/r#1 to clarifying")
		}},
		{"the admission's labels", "handle", flagOwners, false, 3, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: triage label: ")
		}},
		{"the left comment", "leave", flagOwners, false, 1, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: left: ")
		}},
		{"the left label", "leave", flagOwners, false, 2, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "answers to the flag on o/r#1: left: triage label: ")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := flagIssue("owner", flagWith(tt.rec, func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }))(t)
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if tt.gone {
				f.DeleteComment(1001)
			}
			f.FailWrite(tt.fail)
			if err := daemon.FlagByReaction(tt.trust)(context.Background(), a, s, daemon.Dirt{}); !tt.want(err) {
				t.Errorf("err = %v, want it wrapped", err)
			}
		})
	}
}

// The commands' writes fail the pass, wrapped, and the next pass finishes.
func TestFlagCommandErrors(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		fail       int
		want       string
	}{
		{"/track's move", "/track", 1, "/track: track: move o/r#1 to clarifying"},
		{"/track's labels", "/track", 3, "/track: track: triage label: "},
		{"/track's 👍", "/track", 4, "/track: track: react +1"},
		{"/ignore's comment", "/ignore", 1, "/ignore: ignore: left: "},
		{"/ignore's label", "/ignore", 2, "/ignore: ignore: left: triage label: "},
		{"/ignore's 👍", "/ignore", 4, "/ignore: ignore: react +1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := flagIssue("owner", flagWith("handle", func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", tt.body) }))(t)
			f.FailWrite(tt.fail)
			pass := flagPass(t, flagOwners)
			if err := pass(f); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			acked := slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
				return c.Body == tt.body && slices.Contains(c.Reactions, app+" +1")
			})
			if !acked {
				t.Errorf("%s isn't 👍'd after the second pass", tt.body)
			}
		})
	}
}

// An /ignore applied during a triage run leaves the run's result with
// nothing to write: a code owner decided, so there's no flag, no "left"
// of the run's, and no admission. The run isn't counted.
func TestTriageAfterIgnoreDuringRun(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "An idea.")
	g := triageRig(t, f, triageOrigin(t), t.TempDir())
	g.claude(triageOut("handle", true, triageReason, triageSummary), 0)
	ready := g.ready(1)
	left := flagLeft(flagIgnore, "5") // as /ignore wrote it, after step 1
	f.AddComment(1, app, left)
	if err := g.r.Work(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if got := commentBodies(f, 1); g.runs() != 1 || !slices.Equal(got, []string{left}) || g.lease().RunID != "" {
		t.Errorf("%d runs, comments %q, lease %+v; want one run, applied with no writes and not counted", g.runs(), got, g.lease())
	}
}

// Through the loop's own Iterate, with the flag answers set as newStep
// sets them: triage flags the issue, a triage:ask someone removes comes
// back with no comment, and the owner's 👍, which changes no updated_at,
// is found on the next pass and admits the issue.
func TestFlagAnswerThroughTheLoop(t *testing.T) {
	f := newFake(t)
	f.AddIssue("eve", "Dry run?", "Maybe a --dry-run.")
	g := triageRig(t, f, triageOrigin(t), t.TempDir())
	g.d.Step.Reactions = daemon.FlagByReaction(g.d.Step.CurrentTrust())
	for name, h := range daemon.FlagCommands() {
		g.d.Step.Commands[name] = h
	}
	g.claude(triageOut("handle", true, triageReason, triageSummary), 0)
	g.pass()
	flag := triageFlag(1, "run1", "Handle it", "Leave it", triageReason, triageSummary, "handle")
	if got, is := commentBodies(f, 1), f.State().Issues[0]; !slices.Equal(got, []string{flag}) || !slices.Equal(is.Labels, []string{"triage:ask"}) {
		t.Fatalf("comments %q, labels %v; want the flag and triage:ask", got, is.Labels)
	}
	f.RemoveLabel(1, "mallory", "triage:ask")
	g.pass()
	if got, is := commentBodies(f, 1), f.State().Issues[0]; len(got) != 1 || !slices.Equal(is.Labels, []string{"triage:ask"}) {
		t.Fatalf("comments %q, labels %v; want triage:ask back, and no comment", got, is.Labels)
	}
	g.pass() // nothing changed: the flagged issue is visited all the same
	r := f.AddReaction(1001, "owner", "+1")
	g.pass()
	want := flagAdmitted(flagTakes, fmt.Sprintf("r%d", r))
	if got, is := commentBodies(f, 1), f.State().Issues[0]; !slices.Equal(got, []string{flag, want}) || !slices.Equal(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("comments %q, labels %v; want the flag, then the 👍's admission, at clarifying", got, is.Labels)
	}
	if s := state.Derive(g.snapshot(1), slug, func(int64) bool { return false }); s.Stage != state.Clarifying {
		t.Errorf("stage = %s, want clarifying", s.Stage)
	}
}

// The admission of eve's issue by a 👍 or a /track is dated by the 👍 or
// the comment, through the loop's own writes: an edit between it and the
// App's move is untrusted.
func TestFlagAdmissionFlow(t *testing.T) {
	for name, answer := range map[string]func(f *githubtest.Fake){
		"👍":      func(f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") },
		"/track": func(f *githubtest.Fake) { f.AddComment(1, "owner", "/track") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			n := ivEve(f, "epic")
			flagWith("handle", nil)(t, f)
			d := ivDaemon(t, f, newOrigin(t))
			d.Step.Reactions = daemon.FlagByReaction(d.Step.CurrentTrust())
			d.Step.Commands = daemon.FlagCommands()
			answer(f)
			f.EditBody(n, "eve", "EVE-EDITED") // before the pass that moves it
			ivPass(t, d, n)
			if st := ivStage(t, d, n); st != state.Clarifying {
				t.Fatalf("stage = %s, want clarifying", st)
			}
			ivOnlyIn(t, ivContext(t, d, n), "EVE-EDITED", "untrusted.md")
		})
	}
}

// Context fails, with no files, when the 👍s on the flag a move names, or
// the trust of a command's author, can't be read.
func TestFlagAdmissionErrors(t *testing.T) {
	t.Run("the reactions", func(t *testing.T) {
		f := newFake(t)
		n := ivEve(f, "epic")
		flagTracked(f, n, fmt.Sprintf("r%d", f.AddReaction(flagPosted(f, n), "owner", "+1")))
		d := ivDaemon(t, f, newOrigin(t))
		in := ivInput(t, d, n)
		f.AnswerOnce(func(r githubtest.Request) bool { return strings.HasSuffix(r.Path, "/reactions") }, http.StatusInternalServerError)
		files, err := (&daemon.Interviewer{Daemon: d}).Context(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "the command that admitted it: the 👍s on the flag in comment") || files != nil {
			t.Errorf("Context = %d files, %v; want none and the reactions' error, wrapped", len(files), err)
		}
	})
	for _, who := range []string{"mallory", "zed"} {
		t.Run("the trust of the "+map[string]string{"mallory": "command's author", "zed": "👍's author"}[who], func(t *testing.T) {
			f := newFake(t)
			n := ivEve(f, "epic")
			if who == "mallory" {
				flagTracked(f, n, fmt.Sprint(f.AddComment(n, who, "/track")))
			} else {
				flagTracked(f, n, fmt.Sprintf("r%d", f.AddReaction(flagPosted(f, n), who, "+1")))
			}
			d := ivDaemon(t, f, newOrigin(t))
			in := ivInput(t, d, n)
			d.Step.Trust = ivRenamerFails{}
			files, err := (&daemon.Interviewer{Daemon: d}).Context(context.Background(), in)
			if !errors.Is(err, errTrust) || !strings.Contains(err.Error(), "the command that admitted it: ") || files != nil {
				t.Errorf("Context = %d files, %v; want none and the trust error, wrapped", len(files), err)
			}
		})
	}
}

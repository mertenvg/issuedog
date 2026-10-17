package state

import (
	"errors"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// The commands that would move the issue wait while it's held, or at the
// clarifying after a hold, whose latest transition carries an origin
// (#87's assumption 8). /resume, and the commands that move nothing,
// never wait, nor does anything at any other stage, or at a clarifying
// reached otherwise.
func TestCommandWaits(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	at := func(markers ...string) Issue {
		s := &github.Snapshot{}
		for i, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return Derive(s, "issuedog", func(int64) bool { return false })
	}
	const review = "from=clarifying to=spec-review run=a"
	all := []string{"approve", "revise", "done", "track", "ignore"}
	for _, tc := range []struct {
		name  string
		is    Issue
		waits []string
	}{
		{"held", at(review, "from=spec-review to=held at=spec-review origin=spec-review reason=discussion event=c9"), all},
		{"at the clarifying after a hold", at(review, "from=spec-review to=held at=spec-review reason=owner cmd=5",
			"from=held to=clarifying at=spec-review origin=spec-review cmd=6"), all},
		{"after a question round there, which carries the origin", at(review, "from=held to=clarifying at=spec-review origin=spec-review event=h11",
			"from=clarifying to=clarifying round=1 run=b origin=spec-review questions=2"), all},
		{"at spec-review", at(review), nil},
		{"at a clarifying /revise reached", at(review, "from=spec-review to=clarifying cmd=7 seen=7"), nil},
		{"at a clarifying whose origin is no stage to go back to", at(review, "from=held to=clarifying at=spec-review origin=held event=h11"), nil},
		{"after a question round there that lost the origin", at(review, "from=held to=clarifying at=spec-review origin=spec-review event=h11",
			"from=clarifying to=clarifying round=1 run=b questions=2"), nil},
	} {
		var got []string
		for _, name := range []string{"approve", "revise", "done", "track", "ignore", "resume", "hold", "priority", "model", "effort"} {
			if (Command{Name: name}).Waits(tc.is) {
				got = append(got, name)
			}
		}
		if !slices.Equal(got, tc.waits) {
			t.Errorf("%s: %q wait; want %q", tc.name, got, tc.waits)
		}
		if want := tc.is.Stage == Clarifying && len(tc.waits) > 0; tc.is.AfterHold() != want {
			t.Errorf("%s: AfterHold = %t; want %t", tc.name, !want, want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Command
		ok   bool
	}{
		{"/approve", Command{Name: "approve"}, true},
		{"  /HOLD \r\nwaiting on a vendor", Command{Name: "hold", Text: "waiting on a vendor"}, true},
		{"/revise make it shorter\nand clearer", Command{Name: "revise", Arg: "make it shorter", Text: "make it shorter\nand clearer"}, true},
		{"/priority p1", Command{Name: "priority", Arg: "p1", Text: "p1"}, true},
		{"/frob it", Command{Name: "frob", Arg: "it", Text: "it"}, true},
		{"/approve.", Command{Name: "approve", Arg: ".", Text: ".", glued: true}, true},
		{"/frob.", Command{}, false},
		{"approve", Command{}, false},
		{"/usr/bin is missing", Command{}, false},
		{"1a 2c\n/hold", Command{}, false},
		{"/1", Command{}, false},
		{"/", Command{}, false},
	} {
		if got, ok := ParseCommand(tc.body); got != tc.want || ok != tc.ok {
			t.Errorf("ParseCommand(%q) = %+v, %v; want %+v, %v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCommandCheck(t *testing.T) {
	for _, tc := range []struct {
		body  string
		stage Stage
		want  error
	}{
		{"/approve", SpecReview, nil},
		{"/approve", Ready, ErrWrongStage},
		{"/revise\nshorter", PlanReview, nil},
		{"/revise", SpecReview, ErrBadArgument},
		{"/revise.notes", SpecReview, ErrBadArgument},
		{"/priority p3", Held, nil},
		{"/priority p9", Ready, ErrBadArgument},
		{"/hold now", Ready, ErrBadArgument},
		{"/done", NeedsHuman, nil},
		{"/done", Ready, ErrWrongStage},
		{"/frob", Ready, ErrUnknownCommand},
		{"/track", Intake, nil},
		{"/TRACK", Intake, nil},
		{"/track", Clarifying, ErrWrongStage},
		{"/track", Held, ErrWrongStage},
		{"/track it", Intake, ErrBadArgument},
		{"/ignore", Intake, nil},
		{"/ignore", Clarifying, ErrWrongStage},
		{"/ignore.", Intake, ErrBadArgument},
	} {
		c, ok := ParseCommand(tc.body)
		if !ok {
			t.Fatalf("%q isn't a command", tc.body)
		}
		if err := c.Check(tc.stage); !errors.Is(err, tc.want) {
			t.Errorf("%q at %s: Check = %v, want %v", tc.body, tc.stage, err, tc.want)
		}
	}
}

// /model and /effort take a value, or "verifier" and a value, at any stage.
// Check leaves the value itself to the daemon's handlers.
func TestCommandSetting(t *testing.T) {
	for _, tc := range []struct {
		body     string
		value    string
		verifier bool
		check    error
	}{
		{"/model opus", "opus", false, nil},
		{"/MODEL Verifier claude-opus-5-5", "claude-opus-5-5", true, nil},
		{"/model VERIFIER opus", "opus", true, nil},
		{"/effort VERIFIER", "", false, ErrBadArgument},
		{"/effort verifier max\nnotes below", "max", true, nil},
		{"/effort \thigh ", "high", false, nil},
		{"/model gpt", "gpt", false, nil},
		{"/effort extreme", "extreme", false, nil},
		{"/model", "", false, ErrBadArgument},
		{"/model verifier", "", false, ErrBadArgument},
		{"/model opus sonnet", "", false, ErrBadArgument},
		{"/effort verifier max now", "", false, ErrBadArgument},
		{"/effort", "", false, ErrBadArgument},
		{"/model.opus", "", false, ErrBadArgument},
	} {
		c, ok := ParseCommand(tc.body)
		if !ok {
			t.Fatalf("%q isn't a command", tc.body)
		}
		if v, verifier := c.Setting(); v != tc.value || verifier != tc.verifier {
			t.Errorf("%q: Setting = %q, %t; want %q, %t", tc.body, v, verifier, tc.value, tc.verifier)
		}
		for _, stage := range []Stage{Ready, Held, SpecReview} {
			if err := c.Check(stage); !errors.Is(err, tc.check) {
				t.Errorf("%q at %s: Check = %v, want %v", tc.body, stage, err, tc.check)
			}
		}
	}
}

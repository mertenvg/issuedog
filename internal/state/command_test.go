package state

import (
	"errors"
	"testing"
)

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

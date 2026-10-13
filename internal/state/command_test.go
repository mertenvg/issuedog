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
		{"/priority p3", Held, nil},
		{"/priority p9", Ready, ErrBadArgument},
		{"/hold now", Ready, ErrBadArgument},
		{"/done", NeedsHuman, nil},
		{"/done", Ready, ErrWrongStage},
		{"/frob", Ready, ErrUnknownCommand},
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

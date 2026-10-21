package state

import (
	"errors"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// The commands that would move the issue wait while it's held, or at the
// clarifying after a hold, whose latest transition carries an origin
// (SPEC §6.4). /resume, and the commands that move nothing,
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

// At planning, the comment whose revision request sent the plan back, from
// plan-review, is the notes (SPEC §7.4), as a spec's request is at
// clarifying. A hold at planning keeps them once the issue is back at
// planning, as at clarifying.
func TestNotesAtPlanning(t *testing.T) {
	t.Parallel()
	const (
		back   = "from=plan-review to=planning cmd=5 seen=5"
		hold   = "from=planning to=held at=planning reason=owner cmd=6 seen=5"
		resume = "from=held to=planning cmd=7 seen=5"
	)
	for _, tt := range []struct {
		name    string
		markers []string
		reply   bool // a reply marker names the request
		want    int64
	}{
		{"sent back", []string{back}, false, 5},
		{"through a hold", []string{back, hold, resume}, false, 5},
		{"through a hold by hand on it too", []string{back, hold, "from=held to=held at=planning reason=owner by=owner event=8 seen=5", resume}, false, 5},
		{"through a hand-set planning, which keeps the round too", []string{back, hold, "from=held to=planning by=owner event=8 seen=5"}, false, 5},
		{"held, no notes yet", []string{back, hold}, false, 0},
		{"a resume to another stage than the hold's", []string{back, hold, "from=held to=plan-review by=owner event=8 seen=5"}, false, 0},
		{"a resume to clarifying", []string{back, hold, "from=held to=clarifying by=owner event=8 seen=5"}, false, 0},
		{"after the planner's round, they're gone", []string{back, "from=planning to=planning round=1 questions=2 run=b seen=5"}, false, 0},
		{"an edit after they were used drops them", []string{back}, true, 0},
		{"a spec approval sends no notes", []string{"from=spec-review to=planning rev=1 body=aaaaaaaa cmd=5 seen=5"}, false, 0},
		{"nor does a move from plan-review anywhere else", []string{"from=plan-review to=clarifying cmd=5 seen=5"}, false, 0},
		{"nor a move to planning set by hand, keyed by no comment", []string{"from=plan-review to=planning by=owner event=8"}, false, 0},
	} {
		s := heldStackSnap(tt.markers...)
		if tt.reply {
			s.Comments = append(s.Comments, github.Comment{ID: int64(len(s.Comments) + 1), Author: appBot, Body: "<!-- issuedog:reply reason=edited cmd=5 -->"})
		}
		if got := Derive(s, slug, clean).Notes(); got != tt.want {
			t.Errorf("%s: notes = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// At planning after the plan was sent back, the request is the first of
// Inputs, a /revise though it may be, then what came after it, and so after
// a hold at planning too, with what came while held (SPEC §9.2, §6.4).
func TestInputsAtPlanning(t *testing.T) {
	t.Parallel()
	const (
		plan   = "from=planning to=plan-review rev=1 run=p1 seen=1"
		back   = "from=plan-review to=planning cmd=2 seen=2"
		hold   = "from=planning to=held at=planning reason=owner cmd=4 seen=2"
		resume = "from=held to=planning cmd=7 seen=2"
	)
	inputs := func(s *github.Snapshot) []int64 {
		var ids []int64
		is := Derive(s, slug, clean)
		for _, c := range is.Inputs(s, func(c github.Comment) bool { return c.Author == owner }) {
			ids = append(ids, c.ID)
		}
		return ids
	}
	// Unheld, 1: the plan comment's move; 2: the request; 3: its move; 4: a
	// comment after it. Held, 4 is /hold; 5: the hold; 6: a comment while
	// held; 7: /resume. Then markers.
	snap := func(request string, held bool, markers ...string) *github.Snapshot {
		s := heldStackSnap(plan, "", back, "")
		s.Comments[1].Body, s.Comments[3].Body = request, "and add X"
		if held {
			s = heldStackSnap(plan, "", back, "", hold, "", "")
			s.Comments[1].Body, s.Comments[3].Body, s.Comments[6].Body = request, "/hold", "/resume"
		}
		for _, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(len(s.Comments) + 1), Author: appBot, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return s
	}
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		want []int64
	}{
		{"the request, then what came after it", snap("too long", false), []int64{2, 4}},
		{"a /revise is the notes too", snap("/revise shorter", false), []int64{2, 4}},
		{"while held, there's none", snap("too long", true), nil},
		{"after /resume, the notes and what came while held", snap("too long", true, resume), []int64{2, 6}},
		{"a /revise is the notes after a hold too", snap("/revise shorter", true, resume), []int64{2, 6}},
		{"after a move out of held to plan-review, there's none", snap("too long", true, "from=held to=plan-review by=owner event=9 seen=2"), nil},
		{"after the planner's round, the notes are gone", snap("too long", false, "from=planning to=planning round=1 questions=2 run=b seen=4"), nil},
	} {
		if got := inputs(tt.s); !slices.Equal(got, tt.want) {
			t.Errorf("%s: inputs = %v, want %v", tt.name, got, tt.want)
		}
	}
}

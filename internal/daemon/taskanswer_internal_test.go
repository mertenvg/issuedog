package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// taskAnswerComments are a task's comments from its implementer round to
// its move back to ready on owner's answer 11, with seen 15, then after:
//   - 9: owner's, before the round;
//   - 10: the App's round, in-progress → clarifying;
//   - 11: owner's answer, the move's key;
//   - 12: eve's, who isn't trusted;
//   - 13: owner's command;
//   - 14: alice's answer, dirty;
//   - 15: alice's answer, the move's seen;
//   - 16: the App's move to ready;
//   - 17: owner's, after the move.
func taskAnswerComments() []github.Comment {
	owner, alice := github.Author{Login: "owner", Type: "User"}, github.Author{Login: "alice", Type: "User"}
	return []github.Comment{
		{ID: 9, Author: owner, Body: "Before the round."},
		{ID: 10, Author: contextTestApp, Body: "Round 1.\n\n<!-- issuedog:transition from=in-progress to=clarifying round=1 run=run1 questions=1 -->"},
		{ID: 11, Author: owner, Body: "1a"},
		{ID: 12, Author: github.Author{Login: "eve", Type: "User"}, Body: "1b"},
		{ID: 13, Author: owner, Body: "/priority p1"},
		{ID: 14, Author: alice, Body: "1b, edited"},
		{ID: 15, Author: alice, Body: "And keep it short."},
		{ID: 16, Author: contextTestApp, Body: "Answered.\n\n<!-- issuedog:transition from=clarifying to=ready cmd=11 seen=15 -->"},
		{ID: 17, Author: owner, Body: "Later."},
	}
}

// The answers the implementer's next run gets (SPEC §12) are the comments
// from the key of the latest move back to ready on an answer to its seen
// that are clean, trusted and not commands, while no implementer run has
// moved the task since. A move to ready that isn't an answer's, keyed by a
// run, gives none, and so does one an implementer run's move came after,
// such as a move to verifying the owner sent back to in-progress by hand.
func TestImplementerAnswers(t *testing.T) {
	t.Parallel()
	app := contextTestApp
	for _, tt := range []struct {
		name string
		more []github.Comment
		want []int64
	}{
		{"the answers", nil, []int64{11, 15}},
		{"after an override to in-progress", []github.Comment{
			{ID: 18, Author: app, Body: "<!-- issuedog:transition from=ready to=in-progress by=owner event=900 -->"},
		}, []int64{11, 15}},
		{"after the next run's move", []github.Comment{
			{ID: 18, Author: app, Body: "<!-- issuedog:transition from=ready to=in-progress by=owner event=900 -->"},
			{ID: 19, Author: app, Body: "<!-- issuedog:transition from=in-progress to=verifying run=run2 head=abc -->"},
			{ID: 20, Author: app, Body: "<!-- issuedog:transition from=verifying to=in-progress by=owner event=901 -->"},
		}, nil},
		{"after a move to ready by a run", []github.Comment{
			{ID: 18, Author: app, Body: "<!-- issuedog:transition from=ready to=clarifying by=owner event=900 -->"},
			{ID: 19, Author: app, Body: "<!-- issuedog:transition from=clarifying to=ready run=run3 -->"},
		}, nil},
	} {
		s := &github.Snapshot{Number: 1, Comments: append(taskAnswerComments(), tt.more...)}
		d := Dirt{14: "alice"}
		got, err := implementerAnswers(context.Background(), s, d, contextTestTC(), state.Derive(s, "issuedog", d.Has))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		var ids []int64
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		if !slices.Equal(ids, tt.want) {
			t.Errorf("%s: the answers are %v, want %v", tt.name, ids, tt.want)
		}
	}
}

// A trust check that fails fails the answers, which the run's context
// then reports as a setup failure.
func TestImplementerAnswersTrustFails(t *testing.T) {
	t.Parallel()
	cs := taskAnswerComments()
	cs[2].Author.Login = "broken"
	s := &github.Snapshot{Number: 1, Comments: cs}
	_, err := implementerAnswers(context.Background(), s, Dirt{}, contextTestTC(), state.Derive(s, "issuedog", Dirt{}.Has))
	if !errors.Is(err, errContextTestTrust) {
		t.Errorf("implementerAnswers = %v, want the trust check's error", err)
	}
}

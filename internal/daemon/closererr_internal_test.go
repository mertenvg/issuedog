package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// closerErrSnap is open issue 7 whose comment 1 carries the closer state
// error for close 900, and with ack, comment 2 an ack reply to it.
func closerErrSnap(ack bool) *github.Snapshot {
	comments := []string{"I can't tell whether that close was a merge.\n\n" + openErrMarker(closerErrKind, "900", "")}
	if ack {
		comments = append(comments, openErrMarker(closerErrKind, "900", "r5"))
	}
	return openErrSnap("", comments...)
}

// A closer state error is open until the question of its close is
// answered while the daemon runs, or a trusted 👍 acknowledges it. With no
// answers known, as in a test that gives none, it's open; an answer for
// another close changes nothing. On a closed issue it's never open there.
func TestCloserErrOpenErrors(t *testing.T) {
	t.Parallel()
	none := func(int64) bool { return false }
	inputs := func() ([]github.Comment, error) { return nil, errors.New("not asked") }
	for _, tc := range []struct {
		name     string
		s        *github.Snapshot
		answered func(string) bool
		want     bool
	}{
		{"no answers known", closerErrSnap(false), nil, true},
		{"not answered", closerErrSnap(false), func(string) bool { return false }, true},
		{"another close answered", closerErrSnap(false), func(key string) bool { return key == "901" }, true},
		{"answered", closerErrSnap(false), func(key string) bool { return key == "900" }, false},
		{"acknowledged", closerErrSnap(true), nil, false},
		{"closed", func() *github.Snapshot { s := closerErrSnap(false); s.Closed = true; return s }(), nil, false},
	} {
		open, err := openErrors(tc.s, state.Derive(tc.s, openErrSlug, none), inputs, tc.answered)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		want := []openErr(nil)
		if tc.want {
			want = []openErr{{ErrorKey: state.ErrorKey{Kind: closerErrKind, Key: "900"}, Comment: 1}}
		}
		if !slices.Equal(open, want) {
			t.Errorf("%s: open %+v; want %+v", tc.name, open, want)
		}
	}
}

// closerErrNodeErr is an error ClosingCloser gives for an untyped error
// under node.
func closerErrNodeErr(t *testing.T) error {
	t.Helper()
	f := githubtest.New(t, "o/r", openErrSlug)
	n := f.AddIssue("owner", "A", "")
	event := f.CloseByPullRequest(n, "mallory", githubtest.ClosingPR{Merged: true})
	f.FailCloser(n, event, "", "Something went wrong while executing your query.")
	_, err := f.Client(t).ClosingCloser(t.Context(), "o", "r", "CE_"+strconv.FormatInt(event, 10))
	if !github.CloserErrUnderNode(err) {
		t.Fatalf("ClosingCloser = %v; want an error under node", err)
	}
	return err
}

// The Applier counts a close's failed questions once a pass, however often
// it's asked, and those with an error under node apart; an answer forgets
// them and records the answer, by the close's event ID.
func TestCloserErrFail(t *testing.T) {
	t.Parallel()
	nodeErr, other := closerErrNodeErr(t), fmt.Errorf("closer: %w", errors.New("a failed request"))
	cl := github.Closing{ID: 900, NodeID: "CE_900"}
	a := &Applier{}
	check := func(name string, passes, node int) {
		t.Helper()
		if p, n := a.closerErrPasses(cl), a.closerErrNodePasses(cl); p != passes || n != node {
			t.Errorf("%s: %d passes, %d under node; want %d and %d", name, p, n, passes, node)
		}
	}
	a.closerErr.pass = 1
	a.closerErrFail(cl, nodeErr)
	a.closerErrFail(cl, nodeErr)
	check("pass 1, asked twice", 1, 1)
	a.closerErr.pass = 2
	a.closerErrFail(cl, other)
	check("pass 2, a failed request", 2, 1)
	a.closerErr.pass = 3
	a.closerErrFail(cl, other)
	a.closerErrFail(cl, nodeErr)
	check("pass 3, both", 3, 2)
	if a.closerErrAnswered("900") {
		t.Error("answered before any answer")
	}
	a.closerErrAnswer(cl)
	check("answered", 0, 0)
	if !a.closerErrAnswered("900") || a.closerErrAnswered("901") {
		t.Error("want close 900 answered, and only it")
	}
}

// A closed issue's closer state error, on the digest, says it ends once
// GitHub answers, not by a 👍, which isn't read on a closed issue.
func TestCloserErrLines(t *testing.T) {
	t.Parallel()
	s := closerErrSnap(false)
	got := closerErrLines(s, openErr{ErrorKey: state.ErrorKey{Kind: closerErrKind, Key: "900"}, Comment: 1})
	want := []waitLine{{Text: "State error `closer`, keyed `900`."}, {Text: "I can't tell whether that close was a merge.", Copied: true},
		{Text: "The issue is closed, so a 👍 won't acknowledge it: it ends once GitHub says how the issue was closed."}}
	if !slices.Equal(got, want) {
		t.Errorf("lines %+v; want %+v", got, want)
	}
}

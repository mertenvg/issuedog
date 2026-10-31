package daemon

import (
	"fmt"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// roundIssue is issue 1 at whatever its markers say, with one App comment
// a marker, the nth with ID 10+n, as Derive reads it.
func roundIssue(markers ...string) (*github.Snapshot, state.Issue) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Number: 1}
	for i, m := range markers {
		s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"})
	}
	return s, state.Derive(s, "issuedog", func(int64) bool { return false })
}

// A task's round at in-progress starts at its latest move other than the
// verify loop's own (SPEC §8.3), the implementer's move to verifying by a
// run and the move back on findings keyed event=v: so its key stays the
// pick's through the loop, and its count is the attempt= of the loop's
// latest move. An owner's move between those stages, keyed by a timeline
// event, starts a round, as does a move out of held, at its comment. At
// any other stage, the round is the latest move's, as before.
func TestTaskRound(t *testing.T) {
	t.Parallel()
	const (
		pick     = "from=ready to=in-progress event=s3 seen=5"
		verify1  = "from=in-progress to=verifying attempt=1 run=a head=h seen=10"
		findings = "from=verifying to=in-progress attempt=1 event=v11 head=h seen=11"
		verify2  = "from=in-progress to=verifying attempt=2 run=b head=h seen=12"
	)
	for _, tt := range []struct {
		name     string
		markers  []string
		key      string
		attempts int // -1: not judged, as RunStage counts them at in-progress only
	}{
		{"the pick", []string{pick}, "in-progress-5", 0},
		{"findings", []string{pick, verify1, findings}, "in-progress-5", 1},
		{"at verifying, the move's own round", []string{pick, verify1}, "verifying-10", -1},
		{"after a second verify's findings", []string{pick, verify1, findings, verify2,
			"from=verifying to=in-progress attempt=2 event=v13 head=h seen=13"}, "in-progress-5", 2},
		{"findings that name no attempt", []string{pick, verify1, "from=verifying to=in-progress event=v11 head=h seen=11"}, "in-progress-5", 0},
		{"findings that name no attempt above 0", []string{pick, verify1, "from=verifying to=in-progress attempt=-2 event=v11 head=h seen=11"}, "in-progress-5", 0},
		{"an owner's move back", []string{pick, verify1, "from=verifying to=in-progress by=owner event=99"}, "in-progress-12", 0},
		{"an owner's move there and back", []string{pick, verify1, findings,
			"from=in-progress to=verifying by=owner event=98", "from=verifying to=in-progress by=owner event=99"}, "in-progress-14", 0},
		{"out of held", []string{pick, verify1, findings, "from=in-progress to=held at=in-progress reason=owner cmd=1 seen=11",
			"from=held to=in-progress cmd=2 seen=11"}, "in-progress-14", 0},
	} {
		_, is := roundIssue(tt.markers...)
		if got := attemptKey(is, nil); got != tt.key {
			t.Errorf("%s: attempt key = %s, want %s", tt.name, got, tt.key)
		}
		if got := roundAttempts(is, attemptBound(is, nil)); tt.attempts >= 0 && got != tt.attempts {
			t.Errorf("%s: the round's attempts = %d, want %d", tt.name, got, tt.attempts)
		}
	}
}

// An Issue built with no comment IDs for its markers keys its round as
// the latest move's, at in-progress too, and counts no attempts.
func TestTaskRoundWithoutIDs(t *testing.T) {
	t.Parallel()
	_, is := roundIssue("from=ready to=in-progress event=s3 seen=5", "from=in-progress to=verifying attempt=1 run=a seen=10",
		"from=verifying to=in-progress attempt=1 event=v11 seen=11")
	is.MarkerComments = nil
	if got, want := attemptKey(is, nil), "in-progress-11"; got != want {
		t.Errorf("attempt key = %s, want %s", got, want)
	}
	if got := roundAttempts(is, 0); got != 0 {
		t.Errorf("the round's attempts = %d, want 0", got)
	}
}

// openErrors keys a task's attempts error at in-progress as RunStage
// does, by the round the verify loop's moves don't end: one posted for the
// round stays open through the loop's moves, and one for an earlier round
// ends.
func TestOpenErrorsTaskRound(t *testing.T) {
	t.Parallel()
	s, _ := roundIssue("from=ready to=in-progress event=s3 seen=5", "from=in-progress to=verifying attempt=1 run=a seen=10",
		"from=verifying to=in-progress attempt=1 event=v11 seen=11")
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	for _, key := range []string{"in-progress-5", "in-progress-11"} {
		s.Comments = append(s.Comments, github.Comment{ID: int64(20 + len(s.Comments)), Author: app,
			Body: fmt.Sprintf("Stopped.\n\n<!-- issuedog:error kind=attempts key=%s -->", key)})
	}
	is := state.Derive(s, "issuedog", func(int64) bool { return false })
	open, err := openErrors(s, is, func() ([]github.Comment, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Key != "in-progress-5" {
		t.Errorf("open errors = %+v; want the round's, keyed in-progress-5, alone", open)
	}
}

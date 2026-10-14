package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

type ownerOnly struct{}

func (ownerOnly) Trusted(_ context.Context, login string) (bool, error) { return login == "owner", nil }

// leftOutApplier files issue 1 with mallory's foo renamed to label, and
// returns an Applier on it that has held the label back, then left it out,
// and the snapshot of that last check.
func leftOutApplier(t *testing.T, label string) (*githubtest.Fake, *Applier, *github.Snapshot) {
	t.Helper()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Issue", "body")
	f.AddLabel(1, "mallory", "foo")
	f.Advance(time.Hour)
	f.RenameLabel("foo", label)
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	if err := a.CheckLabels(context.Background(), a.snapshot(t, 1), ownerOnly{}, func(int64) bool { return false }); !errors.Is(err, ErrLabelsHeld) {
		t.Fatalf("first check: %v, want the label held back", err)
	}
	f.Advance(github.HoldBound)
	s := a.snapshot(t, 1)
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil || !s.LeftOut[label] {
		t.Fatalf("second check: %v, left out %v; want %s left out", err, s.LeftOut, label)
	}
	return f, a, s
}

func (a *Applier) snapshot(t *testing.T, n int) *github.Snapshot {
	t.Helper()
	s, err := a.Client.Snapshot(context.Background(), a.Owner, a.Repo, n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The App's write of a label left out forgets the time it was first seen,
// so a check soon after, before the App's own events show, doesn't remove
// it, though the old time would have it due.
func TestAddLabelForgetsUnexplained(t *testing.T) {
	f, a, _ := leftOutApplier(t, "priority:p1")
	f.Advance(github.UnexplainedWait - github.HoldBound - time.Second)
	s := a.snapshot(t, 1)
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil {
		t.Fatal(err)
	}
	f.LagEvents(10 * time.Second) // past the writes' own ticks of the clock
	if err := a.addLabel(context.Background(), s, "priority:p1"); err != nil {
		t.Fatal(err)
	}
	f.Advance(time.Second) // the old time is due now
	writes := len(f.Writes())
	err := a.CheckLabels(context.Background(), a.snapshot(t, 1), ownerOnly{}, func(int64) bool { return false })
	if err != nil && !errors.Is(err, ErrLabelsHeld) {
		t.Fatal(err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 || !slices.Contains(f.State().Issues[0].Labels, "priority:p1") {
		t.Errorf("writes %+v, labels %q; want the App's priority:p1 kept", w, f.State().Issues[0].Labels)
	}
}

// A second check of the same snapshot checks its left-out labels again, so
// they stay left out and remembered.
func TestCheckLabelsAgainKeepsLeftOut(t *testing.T) {
	_, a, s := leftOutApplier(t, "priority:p1")
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, due := a.seen().Due("o", "r", 1); !s.LeftOut["priority:p1"] || !due {
		t.Errorf("left out %v, remembered %v; want priority:p1 left out and remembered", s.LeftOut, due)
	}
}

// removeLabel keeps the type up to date with the labels.
func TestRemoveLabelUpdatesType(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Issue", "body")
	f.AddLabel(1, "issuedog[bot]", "type:task")
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s := a.snapshot(t, 1)
	if err := a.removeLabel(context.Background(), s, "type:task"); err != nil {
		t.Fatal(err)
	}
	if s.Type != "" || len(s.Labels) != 0 {
		t.Errorf("type %q, labels %q after the removal; want none", s.Type, s.Labels)
	}
}

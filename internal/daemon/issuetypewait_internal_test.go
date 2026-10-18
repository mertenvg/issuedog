package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// issueTypeWaitLeftOut files issue 1, typed Task by the owner, then Epic
// by login, whose event lags by lag, and returns an Applier on a repo that
// uses issue types that has held Epic back, then left it out, and the
// snapshot of that last check (#183).
func issueTypeWaitLeftOut(t *testing.T, login string, lag time.Duration) (*githubtest.Fake, *Applier, *github.Snapshot) {
	t.Helper()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Issue", "body")
	f.SetTypeBy(1, "owner", "Task")
	f.LagEvents(lag)
	f.SetTypeBy(1, login, "Epic")
	f.LagEvents(0)
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog", IssueTypes: true, Unexplained: &github.Unexplained{}}
	none := func(int64) bool { return false }
	if err := a.CheckLabels(context.Background(), a.snapshot(t, 1), ownerOnly{}, none); !errors.Is(err, ErrLabelsHeld) {
		t.Fatalf("first check = %v; want Epic held back", err)
	}
	f.Advance(github.HoldBound + time.Second)
	s := a.snapshot(t, 1)
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, none); err != nil {
		t.Fatal(err)
	}
	return f, a, s
}

// A type left out gives the snapshot the type that stands, Task, and keeps
// Epic as the issue's own: a check of the same snapshot again judges Epic,
// not Task, so it stays left out. A path that reads the issue again
// without CheckLabels leaves it out too (hide, hidden), while the issue
// has it, and only on a repo that uses issue types.
func TestIssueTypeWaitSnapshot(t *testing.T) {
	t.Parallel()
	f, a, s := issueTypeWaitLeftOut(t, "mallory", time.Hour)
	task, epic := f.IssueTypeID("Task"), f.IssueTypeID("Epic")
	want := github.IssueTypeLeftOut{Name: "Epic", ID: epic, To: "Task", ToID: task}
	check := func(what string, s *github.Snapshot) {
		t.Helper()
		if s.Type != "task" || s.TypeName != "Task" || s.TypeID != task || s.TypeFromLabels || s.TypeLeftOut == nil || *s.TypeLeftOut != want {
			t.Errorf("%s: type %q, %q, %d, from labels %t, left out %+v; want Task, with %+v left out",
				what, s.Type, s.TypeName, s.TypeID, s.TypeFromLabels, s.TypeLeftOut, want)
		}
	}
	check("left out", s)
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil {
		t.Fatal(err)
	}
	check("checked again", s)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none", w)
	}
	fresh := a.snapshot(t, 1)
	a.hide(fresh)
	check("hide", fresh)
	fresh = a.snapshot(t, 1)
	check("hidden", a.hidden(fresh))
	if fresh.Type != "epic" || fresh.TypeLeftOut != nil {
		t.Errorf("hidden changed the snapshot it copied: type %q, left out %+v", fresh.Type, fresh.TypeLeftOut)
	}
	labels := &Applier{Client: a.Client, Owner: "o", Repo: "r", AppSlug: "issuedog", Unexplained: a.Unexplained}
	fresh = a.snapshot(t, 1)
	labels.hide(fresh)
	if fresh.Type != "epic" || fresh.TypeLeftOut != nil {
		t.Errorf("hide on a repo that uses labels: type %q, left out %+v; want Epic, as read", fresh.Type, fresh.TypeLeftOut)
	}
	f.SetType(1, "Bug") // no event
	fresh = a.snapshot(t, 1)
	a.hide(fresh)
	if fresh.Type != "bug" || fresh.TypeLeftOut != nil {
		t.Errorf("hide once the issue has another type: type %q, left out %+v; want Bug, as read", fresh.Type, fresh.TypeLeftOut)
	}
}

// A trusted change left out stands once its event lands: a check of the
// same snapshot gives it back its own type, Epic, keyed by that event,
// and forgets the type left out.
func TestIssueTypeWaitSnapshotLands(t *testing.T) {
	t.Parallel()
	f, a, s := issueTypeWaitLeftOut(t, "owner", time.Minute)
	if s.Type != "task" {
		t.Fatalf("left out: type %q; want task", s.Type)
	}
	f.Advance(time.Minute) // its event lands
	if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if s.Type != "epic" || s.TypeName != "Epic" || s.TypeID != f.IssueTypeID("Epic") || s.TypeLeftOut != nil || s.TypeEvent != 9002 {
		t.Errorf("type %q, %q, %d, left out %+v, keyed %d; want Epic, keyed by its event, 9002", s.Type, s.TypeName, s.TypeID, s.TypeLeftOut, s.TypeEvent)
	}
	if _, ok := a.seen().IssueTypeLeftOut("o", "r", 1); ok {
		t.Error("the type is still left out")
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none", w)
	}
}

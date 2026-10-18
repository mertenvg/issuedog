package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

func TestTimeline(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body", "stage:ready")
	gone := f.AddComment(n, "owner", "deleted later")
	c := f.Client(t)
	// stage:ready is there already, so only priority:p1 gets an event.
	if _, _, err := c.Issues.AddLabelsToIssue(context.Background(), "o", "r", n, []string{"stage:ready", "priority:p1"}); err != nil {
		t.Fatal(err)
	}
	said := f.AddComment(n, "issuedog[bot]", "hi")
	if _, err := c.Issues.RemoveLabelForIssue(context.Background(), "o", "r", n, "priority:p1"); err != nil {
		t.Fatal(err)
	}
	f.RemoveLabel(n, "mallory", "STAGE:READY")
	f.DeleteComment(gone)

	events, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.GetActor().GetLogin()+" "+e.GetActor().GetType()+" "+e.GetLabel().GetName()+e.GetBody())
	}
	want := []string{"labeled owner User stage:ready", "labeled issuedog[bot] Bot priority:p1", "commented issuedog[bot] Bot hi",
		"unlabeled issuedog[bot] Bot priority:p1", "unlabeled mallory User stage:ready"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline = %q, want %q", got, want)
	}
	if len(events) == len(want) && events[2].GetID() != said {
		t.Errorf("commented event ID = %d, want the comment's, %d", events[2].GetID(), said)
	}
}

// RenameLabel renames a label in the repo and on its issues, ignoring case,
// and past events keep the old name. RenameLabelInEvents renames them too.
func TestRenameLabel(t *testing.T) {
	for name, tt := range map[string]struct {
		rename func(f *Fake, from, to string)
		event  string // the first event's label after it
	}{
		"RenameLabel":         {(*Fake).RenameLabel, "model:old"},
		"RenameLabelInEvents": {(*Fake).RenameLabelInEvents, "Model:Old"},
	} {
		t.Run(name, func(t *testing.T) {
			f := New(t, "o/r", "issuedog")
			f.AddRepoLabel("model:old")
			n := f.AddIssue("owner", "Title", "body")
			f.AddLabel(n, "owner", "model:old")
			f.AddLabel(n, "owner", "aa")
			tt.rename(f, "MODEL:OLD", "Model:Old")
			if got := f.RepoLabels(); !reflect.DeepEqual(got, []string{"Model:Old"}) {
				t.Errorf("repo labels = %q, want Model:Old", got)
			}
			if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"Model:Old", "aa"}) {
				t.Errorf("issue labels = %q, want Model:Old and aa", got)
			}
			events, err := f.Client(t).TimelineSince(context.Background(), "o", "r", n, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[0].GetLabel().GetName() != tt.event || events[1].GetLabel().GetName() != "aa" {
				t.Errorf("events = %+v, want the first named %s and the second aa", events, tt.event)
			}
		})
	}
}

// LagEvents hides label events until the clock, which Advance moves and
// every answer is dated by, reaches them.
func TestLagEvents(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.LagEvents(2 * time.Second)
	n := f.AddIssue("owner", "Title", "body", "stage:ready")
	c := f.Client(t)
	for _, tt := range []struct {
		wait   time.Duration
		events int
	}{{0, 0}, {time.Second, 0}, {time.Second, 1}} {
		f.Advance(tt.wait)
		events, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != tt.events {
			t.Errorf("events after %v more = %+v, want %d", tt.wait, events, tt.events)
		}
	}
	_, resp, err := c.Issues.Get(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if date, want := resp.Header.Get("Date"), Start.Add(3*time.Second).Format(http.TimeFormat); date != want {
		t.Errorf("Date = %q, want the clock's, %q", date, want)
	}
}

// RenameIssue changes the title and writes a renamed event with its own ID,
// by its actor, saying what the title was and is.
func TestRenameIssue(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Old", "body")
	label := f.AddLabel(n, "owner", "priority:p1")
	first := f.RenameIssue(n, "eve", "Middle")
	second := f.RenameIssue(n, "owner", "New")
	if first == label || second == first || first == 0 {
		t.Errorf("event IDs: label %d, renames %d and %d; want them distinct", label, first, second)
	}
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "New" {
		t.Errorf("title = %q, want New", s.Title)
	}
	events, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		if e.GetEvent() == "renamed" {
			got = append(got, fmt.Sprintf("%d %s %s to %s", e.GetID(), e.GetActor().GetLogin(), e.GetRename().GetFrom(), e.GetRename().GetTo()))
		}
	}
	if want := []string{fmt.Sprintf("%d eve Old to Middle", first), fmt.Sprintf("%d owner Middle to New", second)}; !reflect.DeepEqual(got, want) {
		t.Errorf("renamed events = %q, want %q", got, want)
	}
}

// A PATCH with a new title, as the App's rewrite of a changed child sends
// with its body (#195's T7), renames the issue as the App, with its
// renamed event; one with the same title writes no event.
func TestEditIssueTitle(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "Old", "body")
	ctx, c := context.Background(), f.Client(t)
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Title: new("New"), Body: new("new body")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Title: new("New")}); err != nil {
		t.Fatal(err)
	}
	if is := f.State().Issues[0]; is.Title != "New" || is.Body != "new body" || is.BodyEditedBy != "issuedog[bot]" {
		t.Errorf("issue = %+v, want the App's new title and body", is)
	}
	events, err := c.TimelineSince(ctx, "o", "r", n, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		if e.GetEvent() == "renamed" {
			got = append(got, fmt.Sprintf("%s %s to %s", e.GetActor().GetLogin(), e.GetRename().GetFrom(), e.GetRename().GetTo()))
		}
	}
	if want := []string{"issuedog[bot] Old to New"}; !reflect.DeepEqual(got, want) {
		t.Errorf("renamed events = %q, want %q", got, want)
	}
}

// SetTypeBy sets the issue type with the issue-type event GitHub writes,
// added, changed or removed, which CheckLabels takes as IssueType when a
// trusted user made it.
func TestSetTypeBy(t *testing.T) {
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	c := f.Client(t)
	filed, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	updated := filed.UpdatedAt
	for _, tc := range []struct{ name, event, typ string }{
		{"Epic", "issue_type_added", "epic"},
		{"Task", "issue_type_changed", "task"},
		{"", "issue_type_removed", ""},
	} {
		id := f.SetTypeBy(n, "owner", tc.name)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		timeline, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		last := timeline[len(timeline)-1]
		if !s.UpdatedAt.Equal(last.GetCreatedAt().Time) || !s.UpdatedAt.After(updated) {
			t.Errorf("%q: updated at %v, want the event's time %v, after %v", tc.name, s.UpdatedAt, last.GetCreatedAt(), updated)
		}
		updated = s.UpdatedAt
		if s.Type != tc.typ || last.GetID() != id || last.GetEvent() != tc.event || last.GetActor().GetLogin() != "owner" {
			t.Errorf("%q: type %q, last event %d %s by %s; want %q, %d %s by owner", tc.name, s.Type, last.GetID(), last.GetEvent(),
				last.GetActor().GetLogin(), tc.typ, id, tc.event)
		}
		found, err := c.CheckLabels(context.Background(), "o", "r", n, s.Labels, "app", typeByTrust{}, func(int64) bool { return false }, nil, &github.IssueTypeGuard{Type: tc.name})
		if err != nil || found.IssueType != id || found.IssueTypeRevert != nil {
			t.Errorf("%q: CheckLabels' IssueType %d and revert %+v, %v; want %d and none", tc.name, found.IssueType, found.IssueTypeRevert, err, id)
		}
	}
}

// typeByTrust trusts owner alone.
type typeByTrust struct{}

func (typeByTrust) Trusted(_ context.Context, login string) (bool, error) {
	return login == "owner", nil
}

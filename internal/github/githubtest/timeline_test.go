package githubtest

import (
	"context"
	"reflect"
	"testing"
	"time"
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

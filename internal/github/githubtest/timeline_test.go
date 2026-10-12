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
	if _, _, err := c.Issues.AddLabelsToIssue(context.Background(), "o", "r", n, []string{"stage:ready", "priority:p1"}); err != nil {
		t.Fatal(err)
	}
	said := f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition to=held -->")
	if _, err := c.Issues.RemoveLabelForIssue(context.Background(), "o", "r", n, "priority:p1"); err != nil {
		t.Fatal(err)
	}
	held := f.AddLabel(n, "owner", "stage:held")
	if id := f.AddLabel(n, "owner", "STAGE:held"); id != 0 {
		t.Errorf("AddLabel of a label the issue has = %d, want 0", id)
	}
	if id := f.RemoveLabel(n, "mallory", "STAGE:READY"); id == 0 {
		t.Errorf("RemoveLabel of stage:ready = 0, want an event ID")
	}
	f.DeleteComment(gone)

	events, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.GetActor().GetLogin()+" "+e.GetActor().GetType()+" "+e.GetLabel().GetName()+e.GetBody())
	}
	want := []string{
		"labeled owner User stage:ready",
		"labeled issuedog[bot] Bot priority:p1", // the App's add of stage:ready changed nothing
		"commented issuedog[bot] Bot <!-- issuedog:transition to=held -->",
		"unlabeled issuedog[bot] Bot priority:p1",
		"labeled owner User stage:held",
		"unlabeled mallory User stage:ready",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline = %q, want %q", got, want)
	}
	if len(events) == len(want) && (events[2].GetID() != said || events[2].GetUser().GetLogin() != "issuedog[bot]" || events[4].GetID() != held) {
		t.Errorf("IDs = %d and %d, users %v, want the comment's ID %d and user, and held's %d",
			events[2].GetID(), events[4].GetID(), events[2].GetUser(), said, held)
	}
}

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

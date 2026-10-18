package github_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

func TestSnapshot(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin") // a collaborator, as authorAssociation says
	at := func(s int) time.Time { return githubtest.Start.Add(time.Duration(s) * time.Second) }
	epic := f.AddIssue("owner", "Epic", "an idea")
	f.SetType(epic, "Epic")
	task := f.AddIssue("owner", "Task", "do it", "stage:ready", "type:task")
	f.SetParent(task, epic)
	var want []github.Comment
	for i := range 130 { // two pages
		author := map[bool]string{true: "issuedog[bot]", false: "owner"}[i%2 == 0]
		id := f.AddComment(task, author, fmt.Sprintf("comment %d", i))
		typ := map[bool]string{true: "Bot", false: "User"}[i%2 == 0]
		association := map[bool]string{true: "NONE", false: "COLLABORATOR"}[i%2 == 0]
		want = append(want, github.Comment{ID: id, Node: fmt.Sprintf("IC_%d", id), Author: github.Author{Login: author, Type: typ},
			Body: fmt.Sprintf("comment %d", i), CreatedAt: at(3 + i), UpdatedAt: at(3 + i), Association: association})
	}
	f.AddReaction(want[120].ID, "owner", "+1")
	f.AddReaction(want[120].ID, "issuedog[bot]", "eyes")
	c := f.Client(t)
	ctx := context.Background()

	got, err := c.Snapshot(ctx, "o", "r", task)
	if err != nil {
		t.Fatal(err)
	}
	owner := github.Author{Login: "owner", Type: "User"}
	wantTask := &github.Snapshot{Number: task, Title: "Task", Body: "do it", BodyNode: "I_2", Author: owner, AuthorAssociation: "COLLABORATOR",
		CreatedAt: at(2), UpdatedAt: at(132),
		Labels: []string{"stage:ready", "type:task"}, Type: "task", TypeFromLabels: true, Parent: epic, Comments: want}
	if !reflect.DeepEqual(got, wantTask) {
		t.Errorf("snapshot with a parent =\n%+v\nwant\n%+v", got, wantTask)
	}

	got, err = c.Snapshot(ctx, "o", "r", epic)
	if err != nil {
		t.Fatal(err)
	}
	wantEpic := &github.Snapshot{Number: epic, Title: "Epic", Body: "an idea", BodyNode: "I_1", Author: owner, AuthorAssociation: "COLLABORATOR",
		CreatedAt: at(1), UpdatedAt: at(1), Type: "epic", TypeName: "Epic", TypeID: f.IssueTypeID("Epic")}
	if !reflect.DeepEqual(got, wantEpic) {
		t.Errorf("snapshot without a parent =\n%+v\nwant\n%+v", got, wantEpic)
	}

	reactions, err := c.Reactions(ctx, "o", "r", want[120].ID)
	if err != nil {
		t.Fatal(err)
	}
	wantReactions := []github.Reaction{
		{ID: 1131, User: owner, Content: "+1", CreatedAt: at(133)},
		{ID: 1132, User: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Content: "eyes", CreatedAt: at(134)},
	}
	if !reflect.DeepEqual(reactions, wantReactions) {
		t.Errorf("reactions = %+v, want %+v", reactions, wantReactions)
	}
}

// A type label gives the type in lower case, whatever its case, unless the
// issue has an issue type, and SetLabels keeps the type up to date with the
// labels it leaves, without those left out.
func TestSnapshotTypeFromLabels(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	plain := f.AddIssue("owner", "Plain", "body", "Type:Epic", "stage:ready")
	typed := f.AddIssue("owner", "Typed", "body", "Type:Epic")
	f.SetType(typed, "Task")
	c := f.Client(t)
	for _, tt := range []struct {
		n                int
		read, set, after string
	}{{plain, "epic", "", ""}, {typed, "task", "task", "task"}} {
		s, err := c.Snapshot(context.Background(), "o", "r", tt.n)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{s.Type}
		s.SetLabels([]string{"stage:ready"})
		got = append(got, s.Type)
		s.LeftOut = map[string]bool{"type:epic": true}
		s.SetLabels([]string{"Type:Epic", "stage:ready"})
		got = append(got, s.Type)
		if want := []string{tt.read, tt.set, tt.after}; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(s.Labels, []string{"stage:ready"}) {
			t.Errorf("#%d: types %q, labels %q; want %q, and Type:Epic left out", tt.n, got, s.Labels, want)
		}
	}
}

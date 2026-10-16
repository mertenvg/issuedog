package github_test

import (
	"context"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// discNobody trusts nobody.
type discNobody struct{}

func (discNobody) Trusted(context.Context, string) (bool, error) { return false, nil }

// An owner, an org member or a collaborator may have write access, so only
// the other associations are outsiders, and an unknown one isn't.
func TestDiscOutsider(t *testing.T) {
	for a, want := range map[string]bool{
		"OWNER": false, "MEMBER": false, "COLLABORATOR": false, "": false, "SOMETHING_NEW": false,
		"CONTRIBUTOR": true, "FIRST_TIMER": true, "FIRST_TIME_CONTRIBUTOR": true, "MANNEQUIN": true, "NONE": true,
	} {
		if got := github.DiscOutsider(a); got != want {
			t.Errorf("DiscOutsider(%q) = %v, want %v", a, got, want)
		}
	}
}

// The snapshot reads each comment's authorAssociation and the issue
// author's, and CheckLabels returns the renamed events, oldest first,
// acting on none.
func TestDiscSnapshotAndRenames(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("mallory", "write")
	n := f.AddIssue("eve", "Title", "body")
	f.AddComment(n, "mallory", "a")
	f.AddComment(n, "eve", "b")
	first := f.RenameIssue(n, "eve", "Title 2")
	second := f.RenameIssue(n, "mallory", "Title 3")
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if s.AuthorAssociation != "NONE" || s.Comments[0].Association != "COLLABORATOR" || s.Comments[1].Association != "NONE" {
		t.Errorf("associations %q, %q, %q; want NONE, COLLABORATOR, NONE", s.AuthorAssociation, s.Comments[0].Association, s.Comments[1].Association)
	}
	found, err := c.CheckLabels(context.Background(), "o", "r", n, s.Labels, "issuedog", discNobody{}, func(int64) bool { return false }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.DiscRenames) != 2 || found.DiscRenames[0].ID != first || found.DiscRenames[0].By.Login != "eve" || found.DiscRenames[0].Title != "Title 2" ||
		found.DiscRenames[1].ID != second || found.DiscRenames[1].By.Login != "mallory" || found.DiscRenames[1].Title != "Title 3" ||
		!found.DiscRenames[0].At.Before(found.DiscRenames[1].At) {
		t.Errorf("renames %+v; want eve's, then mallory's", found.DiscRenames)
	}
	if len(found.Events) != 0 || len(f.Writes()) != 0 {
		t.Errorf("events %+v, writes %+v; want none for renames", found.Events, f.Writes())
	}
}

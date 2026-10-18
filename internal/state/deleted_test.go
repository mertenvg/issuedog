package state

import (
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// An App comment deleted after the latest revision's comment was posted
// hides it, as a dirty one does, whoever deleted it (#87's assumption 11),
// and so does a deleted comment whose author GitHub doesn't name. One
// deleted at or before that time, or anyone else's, doesn't.
func TestDeletedHidesRevision(t *testing.T) {
	const rev = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
	posted := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	other := github.Author{Login: "other[bot]", Type: "Bot"}
	appUser := github.Author{Login: slug + "[bot]", Type: "User"}
	del := func(by, author github.Author, at time.Time) github.DeletedComment {
		return github.DeletedComment{ID: "CDE_1", By: by, Author: author, At: at}
	}
	for _, tc := range []struct {
		name    string
		deleted []github.DeletedComment
		hidden  bool
	}{
		{"none", nil, false},
		{"the App's, by mallory, after it", []github.DeletedComment{del(mallory, appBot, posted.Add(time.Second))}, true},
		{"the App's, by the owner, after it", []github.DeletedComment{del(owner, appBot, posted.Add(time.Hour))}, true},
		{"the App's, by nobody GitHub names, after it", []github.DeletedComment{del(github.Author{}, appBot, posted.Add(time.Second))}, true},
		{"one whose author GitHub doesn't name, after it", []github.DeletedComment{del(owner, github.Author{}, posted.Add(time.Second))}, true},
		{"the App's, in the same second", []github.DeletedComment{del(owner, appBot, posted)}, false},
		{"the App's, before it", []github.DeletedComment{del(owner, appBot, posted.Add(-time.Second))}, false},
		{"the owner's, after it", []github.DeletedComment{del(owner, owner, posted.Add(time.Second))}, false},
		{"another App's, after it", []github.DeletedComment{del(owner, other, posted.Add(time.Second))}, false},
		{"a User with the App's login, after it", []github.DeletedComment{del(owner, appUser, posted.Add(time.Second))}, false},
		{"any of several", []github.DeletedComment{del(owner, owner, posted.Add(time.Second)), del(owner, appBot, posted.Add(-time.Second)),
			del(mallory, appBot, posted.Add(2*time.Second))}, true},
	} {
		s := snap("", []github.Author{appBot, owner}, rev, "hi")
		s.Comments[0].CreatedAt, s.Comments[1].CreatedAt = posted, posted.Add(time.Hour)
		s.Deleted = tc.deleted
		got, ok := LatestRevision(s, slug, SpecRevision, clean)
		if want := (Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1, Hidden: tc.hidden}); got != want || !ok {
			t.Errorf("%s: LatestRevision = %+v, %v, want %+v, true", tc.name, got, ok, want)
		}
	}
	// The latest revision is the one whose time counts: an App comment
	// deleted between two revisions hides only the older one.
	s := snap("", []github.Author{appBot, appBot}, rev, "<!-- issuedog:revision rev=2 body=bbbbbbbb -->")
	s.Comments[0].CreatedAt, s.Comments[1].CreatedAt = posted, posted.Add(time.Hour)
	s.Deleted = []github.DeletedComment{del(owner, appBot, posted.Add(time.Minute))}
	if got, _ := LatestRevision(s, slug, SpecRevision, clean); got.Rev != 2 || got.Hidden {
		t.Errorf("LatestRevision = %+v, want revision 2, not hidden", got)
	}
	if got, _ := LatestRevision(s, slug, SpecRevision, dirtyIDs(2)); got.Rev != 1 || !got.Hidden {
		t.Errorf("LatestRevision with revision 2 dirty = %+v, want revision 1, hidden", got)
	}
}

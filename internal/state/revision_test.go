package state

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

func TestBodyHash(t *testing.T) {
	// printf 'a\nb\n' | sha256sum
	if got := BodyHash("a\nb\n"); got != "911169dd" {
		t.Errorf("BodyHash = %s, want 911169dd", got)
	}
	for _, body := range []string{"a\r\nb\r\n", "a\rb\r"} {
		if got := BodyHash(body); got != "911169dd" {
			t.Errorf("BodyHash(%q) = %s, want its line endings normalised", body, got)
		}
	}
	for _, body := range []string{"a\nb", "a\nb\n<!-- issuedog:origin plan=#1 rev=1 item=T1 -->", ""} {
		if BodyHash(body) == "911169dd" {
			t.Errorf("BodyHash(%q) matches a\\nb\\n", body)
		}
	}
}

func TestLatestRevision(t *testing.T) {
	const (
		rev1 = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
		rev2 = "<!-- issuedog:transition from=clarifying to=spec-review rev=2 body=bbbbbbbb run=x -->\n<!-- issuedog:revision rev=2 body=bbbbbbbb -->"
	)
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  Revision
		ok    bool
	}{
		{"none", snap("", []github.Author{appBot}, "<!-- issuedog:transition from=intake to=clarifying run=x -->"), clean, Revision{}, false},
		{"the last comment's", snap("", []github.Author{appBot, appBot, owner}, rev1, rev2, "hi"), clean,
			Revision{Rev: 2, Hash: "bbbbbbbb", Comment: 2}, true},
		{"a dirty one isn't a revision, and hides the one before it", snap("", []github.Author{appBot, appBot}, rev1, rev2), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1, Hidden: true}, true},
		{"any dirty App comment after it hides it, whatever it says", snap("", []github.Author{appBot, appBot}, rev1, "a reply"), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1, Hidden: true}, true},
		{"a dirty comment by anyone else doesn't hide it", snap("", []github.Author{appBot, owner}, rev1, rev2), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"a dirty App comment before it doesn't hide it", snap("", []github.Author{appBot, appBot}, rev2, rev1), dirtyIDs(1),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 2}, true},
		{"only the App's count", snap("", []github.Author{appBot, owner}, rev1, rev2), clean, Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"the body's don't count", snap(rev1, nil), clean, Revision{}, false},
		{"only revision markers count, not others with rev and body",
			snap("", []github.Author{appBot, appBot}, rev1, "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r9 -->"),
			clean, Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"a malformed one is skipped", snap("", []github.Author{appBot, appBot}, rev1, "<!-- issuedog:revision rev=x body=cccccccc -->"), clean,
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
	} {
		got, ok := LatestRevision(tt.s, slug, tt.dirty)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: LatestRevision = %+v, %v, want %+v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

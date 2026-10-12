package state

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

func TestDirty(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	edits := func(by ...github.Author) []github.Edit {
		var h []github.Edit
		for _, a := range by {
			h = append(h, github.Edit{By: a})
		}
		return h
	}
	for _, tc := range []struct {
		name    string
		author  github.Author
		history []github.Edit
		want    github.Author
		dirty   bool
	}{
		{"never edited", app, nil, github.Author{}, false},
		{"the App's comment, edited only by the App", app, edits(app, app, app), github.Author{}, false},
		{"a trusted user's comment, edited only by them", owner, edits(owner, owner), github.Author{}, false},
		{"the App's comment, edited by a trusted user", app, edits(app, owner), owner, true},
		{"untrusted, then trusted", app, edits(app, mallory, owner), mallory, true},
		{"untrusted, then the App", app, edits(app, mallory, app), mallory, true},
		{"the oldest of several", owner, edits(owner, mallory, app, owner), mallory, true},
		{"a trusted user's comment, rewritten by the App", owner, edits(owner, app, app), github.Author{}, false},
		{"the App's edit is skipped for the oldest", owner, edits(owner, app, mallory, owner), mallory, true},
		{"untrusted, then the App, on a trusted user's body", owner, edits(owner, mallory, app), mallory, true},
		{"another App's bot", owner, edits(owner, github.Author{Login: "other[bot]", Type: "Bot"}),
			github.Author{Login: "other[bot]", Type: "Bot"}, true},
		{"an editor GitHub doesn't name", owner, edits(owner, github.Author{}), github.Author{}, true},
		{"by a deleted account, edited by GitHub's unnamed editor", github.Author{}, edits(github.Author{}), github.Author{}, true},
		{"a User with the App's login but no [bot]", app, edits(app, github.Author{Login: "issuedog", Type: "User"}),
			github.Author{Login: "issuedog", Type: "User"}, true},
	} {
		by, dirty := Dirty(tc.author, tc.history, "issuedog")
		if by != tc.want || dirty != tc.dirty {
			t.Errorf("%s: Dirty = %+v, %v, want %+v, %v", tc.name, by, dirty, tc.want, tc.dirty)
		}
	}
}

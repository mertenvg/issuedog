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
		{"a User with the App's full login", owner, edits(owner, github.Author{Login: "issuedog[bot]", Type: "User"}),
			github.Author{Login: "issuedog[bot]", Type: "User"}, true},
		{"another App's bot", owner, edits(owner, github.Author{Login: "other[bot]", Type: "Bot"}),
			github.Author{Login: "other[bot]", Type: "Bot"}, true},
		{"an editor GitHub doesn't name", owner, edits(owner, github.Author{}), github.Author{}, true},
		{"by a deleted account, edited by GitHub's unnamed editor", github.Author{}, edits(github.Author{}), github.Author{}, true},
		{"a User with the App's login but no [bot]", app, edits(app, github.Author{Login: "issuedog", Type: "User"}),
			github.Author{Login: "issuedog", Type: "User"}, true},
	} {
		by, dirty := Dirty(7, tc.author, tc.history, "issuedog", nil)
		if by != tc.want || dirty != tc.dirty {
			t.Errorf("%s: Dirty = %+v, %v, want %+v, %v", tc.name, by, dirty, tc.want, tc.dirty)
		}
	}
}

// A trusted user's edit is sanctioned, as trusted holds now, except on a
// comment the App wrote; the body counts as people's whoever wrote it.
// Neither a bot nor an unnamed editor is, whatever trusted says.
func TestSanctionedTrusted(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	alice := github.Author{Login: "alice", Type: "User"}
	bob := github.Author{Login: "Bob", Type: "User"}
	trusted := func(l string) bool { return l == "Bob" || l == "" || l == "other[bot]" }
	for _, tc := range []struct {
		name           string
		id             int64
		author, editor github.Author
		trusted        func(string) bool
		want           bool
	}{
		{"a trusted user, on a person's comment", 7, alice, bob, trusted, true},
		{"a trusted user, on a person's body", BodyID, alice, bob, trusted, true},
		{"a trusted user, on the App's body", BodyID, app, bob, trusted, true},
		{"a trusted user, on the App's comment", 7, app, bob, trusted, false},
		{"the user once trusted", 7, alice, bob, func(string) bool { return false }, false},
		{"no trust", 7, alice, bob, nil, false},
		{"a bot trusted says it trusts", 7, alice, github.Author{Login: "other[bot]", Type: "Bot"}, trusted, false},
		{"an unnamed editor", 7, alice, github.Author{}, trusted, false},
		{"the author", 7, alice, alice, nil, true},
		{"the App, on the App's comment", 7, app, app, nil, true},
	} {
		if got := Sanctioned(tc.id, tc.author, tc.editor, "issuedog", tc.trusted); got != tc.want {
			t.Errorf("%s: Sanctioned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Only the history after the App's last full rewrite of the body counts.
// A comment's never does: the App doesn't rewrite comments whole, and a
// rewrite marker in a comment's text means nothing.
func TestDirtyAfterRewrite(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	bob := github.Author{Login: "bob", Type: "User"}
	rewrite := "<!-- issuedog:rewrite run=r1 replaced=mallory -->\n## Problem"
	e := func(by github.Author, text string) github.Edit { return github.Edit{By: by, Text: text} }
	trusted := func(l string) bool { return l == "bob" }
	for _, tc := range []struct {
		name    string
		id      int64
		history []github.Edit
		want    github.Author
		dirty   bool
	}{
		{"untrusted, then the App's rewrite", BodyID, []github.Edit{e(owner, "idea"), e(mallory, "evil"), e(app, rewrite)}, github.Author{}, false},
		{"untrusted after the rewrite", BodyID, []github.Edit{e(owner, "idea"), e(app, rewrite), e(mallory, "evil"), e(owner, "x")}, mallory, true},
		{"the last rewrite counts", BodyID, []github.Edit{e(owner, "idea"), e(app, rewrite), e(mallory, "evil"), e(app, rewrite), e(bob, "fix")}, github.Author{}, false},
		{"the App's note isn't a rewrite", BodyID, []github.Edit{e(owner, "idea"), e(mallory, "evil"),
			e(app, "<!-- issuedog:dirty by=mallory -->\n> ⚠️ issuedog ignores this issue description")}, mallory, true},
		{"a deleted rewrite fails closed", BodyID, []github.Edit{e(owner, "idea"), e(mallory, "evil"), e(app, "")}, mallory, true},
		{"someone else's rewrite text", BodyID, []github.Edit{e(owner, "idea"), e(mallory, rewrite), e(owner, "x")}, mallory, true},
		{"an escaped marker", BodyID, []github.Edit{e(owner, "idea"), e(mallory, "evil"), e(app, github.Escape(rewrite))}, mallory, true},
		{"the marker not on the first line", BodyID, []github.Edit{e(owner, "idea"), e(mallory, "evil"), e(app, "hi\n"+rewrite)}, mallory, true},
		{"a comment", 7, []github.Edit{e(owner, "idea"), e(mallory, "evil"), e(app, rewrite)}, mallory, true},
	} {
		by, dirty := Dirty(tc.id, owner, tc.history, "issuedog", trusted)
		if by != tc.want || dirty != tc.dirty {
			t.Errorf("%s: Dirty = %+v, %v, want %+v, %v", tc.name, by, dirty, tc.want, tc.dirty)
		}
	}
}

// SinceRewrite keeps what Dirty counts: a body's history after its last
// full rewrite, none if that is the last entry, and all of a comment's
// (#174).
func TestSinceRewrite(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	rewrite := "<!-- issuedog:rewrite run=r1 -->\n## Problem"
	idea, evil, rw, edit := github.Edit{By: owner, Text: "idea"}, github.Edit{By: mallory, Text: "evil"},
		github.Edit{By: app, Text: rewrite}, github.Edit{By: owner, Text: "x"}
	for _, tc := range []struct {
		name    string
		id      int64
		history []github.Edit
		want    []github.Edit
	}{
		{"no rewrite", BodyID, []github.Edit{idea, evil, edit}, []github.Edit{idea, evil, edit}},
		{"after the rewrite", BodyID, []github.Edit{idea, evil, rw, edit}, []github.Edit{edit}},
		{"after the last rewrite", BodyID, []github.Edit{idea, rw, evil, rw, edit}, []github.Edit{edit}},
		{"the rewrite last", BodyID, []github.Edit{idea, evil, rw}, []github.Edit{}},
		{"someone else's rewrite text", BodyID, []github.Edit{idea, {By: mallory, Text: rewrite}, edit},
			[]github.Edit{idea, {By: mallory, Text: rewrite}, edit}},
		{"a comment", 7, []github.Edit{idea, evil, rw, edit}, []github.Edit{idea, evil, rw, edit}},
		{"no history", BodyID, nil, nil},
	} {
		got := SinceRewrite(tc.id, tc.history, "issuedog")
		if len(got) != len(tc.want) {
			t.Errorf("%s: SinceRewrite = %+v, want %+v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: SinceRewrite = %+v, want %+v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// ParseRewrite takes only a first line exactly as FormatMarker writes a
// rewrite marker, with run and replaced at most, and returns the text
// below it, whatever ends the line.
func TestParseRewrite(t *testing.T) {
	for _, tc := range []struct {
		text, replaced, rest string
		ok                   bool
	}{
		{"<!-- issuedog:rewrite run=r1 -->\nspec\nmore", "", "spec\nmore", true},
		{"<!-- issuedog:rewrite run=r1 replaced=mallory -->\r\nspec", "mallory", "spec", true},
		{"<!-- issuedog:rewrite run=r1 -->\rspec\r", "", "spec\r", true},
		{"<!-- issuedog:rewrite run=r1 -->\n\nspec", "", "\nspec", true},
		{"<!-- issuedog:rewrite -->", "", "", true},
		{"<!-- issuedog:rewrite replaced=mallory run=r1 -->\nspec", "", "", false}, // not FormatMarker's order
		{"<!--  issuedog:rewrite run=r1 -->\nspec", "", "", false},
		{"<!-- issuedog:rewrite run=r1 --> spec", "", "", false},
		{"<!-- issuedog:rewrite by=x run=r1 -->\nspec", "", "", false}, // in FormatMarker's order, but by isn't a field it has
		{"<!-- issuedog:dirty by=x -->\nspec", "", "", false},
		{"&lt;!-- issuedog:rewrite run=r1 -->\nspec", "", "", false},
		{"spec\n<!-- issuedog:rewrite run=r1 -->", "", "", false},
		{"", "", "", false},
	} {
		m, rest, ok := ParseRewrite(tc.text)
		if ok != tc.ok || m.Fields["replaced"] != tc.replaced || rest != tc.rest {
			t.Errorf("ParseRewrite(%q) = %+v, %q, %v, want replaced=%q, %q, %v", tc.text, m, rest, ok, tc.replaced, tc.rest, tc.ok)
		}
	}
}

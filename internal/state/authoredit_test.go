package state

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// After the App's full rewrite of the body, the App is its author, so the
// issue's author's own edit after it is judged like anyone else's: by
// trust (SPEC §5.1). Their edits before it still are theirs, and so are a
// comment's, which the App never rewrites whole. An App entry whose text
// someone deleted counts as a rewrite.
func TestAuthorEditWriter(t *testing.T) {
	filer := github.Author{Login: "filer", Type: "User"}
	rewrite := "<!-- issuedog:rewrite run=r1 -->\n## Problem"
	e := func(by github.Author, text string) github.Edit { return github.Edit{By: by, Text: text} }
	trusted := func(l string) bool { return l == "owner" }
	for _, tc := range []struct {
		name    string
		id      int64
		author  github.Author
		history []github.Edit
		writer  github.Author // after the whole history
		want    github.Author
		dirty   bool
	}{
		{"the author's edit, no rewrite", BodyID, filer, []github.Edit{e(filer, "idea"), e(filer, "idea v2")}, filer, github.Author{}, false},
		{"the author's edit after the rewrite", BodyID, filer, []github.Edit{e(filer, "idea"), e(appBot, rewrite), e(filer, "evil")},
			appBot, filer, true},
		{"the author's edit before the rewrite", BodyID, filer, []github.Edit{e(filer, "idea"), e(filer, "idea v2"), e(appBot, rewrite)},
			appBot, github.Author{}, false},
		{"a trusted author's edit after the rewrite", BodyID, owner, []github.Edit{e(owner, "idea"), e(appBot, rewrite), e(owner, "v2")},
			appBot, github.Author{}, false},
		{"after a rewrite whose text was deleted", BodyID, filer, []github.Edit{e(filer, "idea"), e(appBot, ""), e(filer, "evil")},
			appBot, filer, true},
		{"before a rewrite whose text was deleted", BodyID, filer,
			[]github.Edit{e(filer, "idea"), e(filer, "idea v2"), e(appBot, ""), e(owner, "v3")}, appBot, github.Author{}, false},
		{"the App's text that isn't a rewrite", BodyID, filer, []github.Edit{e(filer, "idea"), e(appBot, "## Problem"), e(filer, "v2")},
			filer, github.Author{}, false},
		{"someone else's empty text", BodyID, filer, []github.Edit{e(filer, "idea"), e(owner, ""), e(filer, "v2")},
			filer, github.Author{}, false},
		{"another App's rewrite", BodyID, filer, []github.Edit{e(filer, "idea"),
			e(github.Author{Login: "other[bot]", Type: "Bot"}, rewrite), e(filer, "v2")}, filer, github.Author{Login: "other[bot]", Type: "Bot"}, true},
		{"a comment", 7, filer, []github.Edit{e(filer, "idea"), e(appBot, rewrite), e(filer, "v2")}, filer, github.Author{}, false},
	} {
		if got := AuthorEditWriter(tc.id, tc.author, tc.history, slug); got != tc.writer {
			t.Errorf("%s: AuthorEditWriter = %+v, want %+v", tc.name, got, tc.writer)
		}
		by, dirty := Dirty(tc.id, tc.author, tc.history, slug, trusted)
		if by != tc.want || dirty != tc.dirty {
			t.Errorf("%s: Dirty = %+v, %v, want %+v, %v", tc.name, by, dirty, tc.want, tc.dirty)
		}
	}
	if got := AuthorEditWriter(BodyID, filer, nil, slug); got != filer {
		t.Errorf("AuthorEditWriter with no history = %+v, want the author", got)
	}
}

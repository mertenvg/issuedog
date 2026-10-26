package daemon

import (
	"strconv"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// #360: the items read back from a digest's body render as the body showed
// them. digestRenderKinds' four kinds, with titles that hold a mention, a
// tag and a marker, the copied lines of a round and a flag, and every
// title escaped, plus one whose title ends in "#" and tries links, and a
// pin note, read back and rendered again with the same scores, come out
// the same bytes. Each keeps its issue, kind, priority, link and the
// comment its link names.
func TestDigestKeptParseRoundTrip(t *testing.T) {
	t.Parallel()
	items := digestRenderKinds(t)
	items = append(items, waitItem{Issue: 9, Kind: waitDone, Key: "done-9", Title: digestRenderHostileLinks + " #", Priority: 3, Unblocks: 1,
		Lines: []waitLine{{Text: "The story waits for story verification."}}, Link: "https://github.com/o/r/issues/9"})
	body := digestRenderBody(digestRenderPinNote, items)
	kept := digestKeptParse(body)
	var again []waitItem
	for _, it := range items {
		got := kept[it.Issue]
		if len(got) != 1 {
			t.Fatalf("read back %+v for #%d; want one item", got, it.Issue)
		}
		k := got[0]
		if k.Kept == nil || k.Kind != it.Kind || k.Priority != it.Priority || k.Link != it.Link || k.Comment != it.Comment || k.Key != "" {
			t.Errorf("read back %+v for #%d; want kind %s, priority %d, link %s, comment %d, kept, with no key", k, it.Issue, it.Kind, it.Priority, it.Link, it.Comment)
		}
		k.Unblocks = it.Unblocks // waitRank's, from the snapshots
		again = append(again, k)
	}
	if len(kept) != len(items) {
		t.Errorf("read back items for %d issues; want %d", len(kept), len(items))
	}
	if got := digestRenderBody(digestRenderPinNote, again); got != body {
		t.Errorf("rendered again:\n%s\nwant the body it was read from:\n%s", got, body)
	}
	if got := digestKeptParse(strings.ReplaceAll(body, "\n", "\r\n")); len(got) != len(items) {
		t.Errorf("read back %d issues' items from the body with CRLF line endings; want %d", len(got), len(items))
	}
	for _, k := range again {
		if news := k.news(); news != nil {
			t.Errorf("#%d's kept item announces %q; want nothing, as the body listed it", k.Issue, news)
		}
	}
}

// Only an item in the form digestRenderItem writes is read back: one whose
// kind isn't named, or whose priority isn't one, or whose lines have one
// that's no list item, or no link last, is passed over, and so are the
// lines around the items, the one on items cut to fit among them.
func TestDigestKeptParsePassesOver(t *testing.T) {
	t.Parallel()
	item := func(n int, about, lines string) string {
		return "### 1. #" + strconv.Itoa(n) + ": T\n\n" + about + "\n\n" + lines
	}
	const link = "- Answer here: https://github.com/o/r/issues/"
	body := strings.Join([]string{
		"3 items wait on you, the ones that unblock the most first.",
		item(1, "**State error**, priority p2, unblocks 1 open issue (itself included)", "- An error.\n"+link+"1"),
		item(2, "**Something else**, priority p2", link+"2"),
		item(3, "**State error**, priority p7", link+"3"),
		item(4, "**State error**, priority p1", "Not a list item.\n"+link+"4"),
		item(5, "**State error**, priority p1", "- No link."),
		item(6, "**State error**, priority p0, unblocks 3 open issues (itself included)", link+"6#issuecomment-77"),
		"…and 2 more, cut to fit GitHub's limit on an issue body.",
		digestRenderIdentity,
	}, "\n\n")
	kept := digestKeptParse(body)
	if len(kept) != 2 || len(kept[1]) != 1 || len(kept[6]) != 1 {
		t.Fatalf("read back %+v; want #1's and #6's items alone", kept)
	}
	if k := kept[1][0]; k.Kind != waitError || k.Priority != 2 || k.Kept.Title != "T" || len(k.Kept.Lines) != 1 || k.Kept.Lines[0] != "- An error." {
		t.Errorf("#1's item %+v; want a p2 state error with its title and line", k)
	}
	if k := kept[6][0]; k.Priority != 0 || k.Comment != 77 || len(k.Kept.Lines) != 0 {
		t.Errorf("#6's item %+v; want a p0 item, comment 77, with no lines but its link", k)
	}
}

// The body is read back only if it's a version the App wrote: never
// edited, or the text of its newest edit, which must be the App's, or the
// original post alone.
func TestDigestKeptApp(t *testing.T) {
	t.Parallel()
	app, eve := waitApp, github.Author{Login: "eve", Type: "User"}
	for _, c := range []struct {
		name string
		h    []github.Edit
		want bool
	}{
		{"never edited", nil, true},
		{"the App's edit, newest", []github.Edit{{By: app, Text: "old"}, {By: eve, Text: "eve's"}, {By: app, Text: "body\r\n"}}, true},
		{"the original post alone", []github.Edit{{By: eve, Text: "body"}}, true},
		{"someone else's edit, newest", []github.Edit{{By: app, Text: "old"}, {By: eve, Text: "body"}}, false},
		{"the App's edit, newest, of other text", []github.Edit{{By: app, Text: "old"}, {By: app, Text: "other"}}, false},
		{"the App's edit, newest, deleted", []github.Edit{{By: app, Text: "old"}, {By: app}}, false},
	} {
		if got := digestKeptApp(c.h, "body", waitSlug); got != c.want {
			t.Errorf("%s: digestKeptApp = %v; want %v", c.name, got, c.want)
		}
	}
}

// #614: while one issue the identity marker or the title rule holds gets
// no item read back, another with none cached still gets its own.
func TestDigestKeptReadHeld(t *testing.T) {
	t.Parallel()
	item := func(n int) string {
		return "### 1. #" + strconv.Itoa(n) + ": T\n\n**State error**, priority p2\n\n- An error.\n- Answer here: https://github.com/o/r/issues/" + strconv.Itoa(n)
	}
	d := &Daemon{digestRestartUnknown: map[int]bool{1: true, 3: true}}
	d.digest.issues = map[int]digestSeen{2: {number: 2}}
	d.digest.titled = map[int]*gogithub.Issue{1: {}}
	d.digestKeptRead(2, item(1)+"\n\n"+item(3)+"\n\n"+digestRenderIdentity)
	if _, ok := d.digestKept[1]; ok || len(d.digestKept[3]) != 1 {
		t.Errorf("read back %+v; want #3's item alone, as #1 is held", d.digestKept)
	}
}

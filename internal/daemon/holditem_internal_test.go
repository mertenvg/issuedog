package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Hold items (#192, plan #87's T8): what waitFind lists for a held issue
// with open discussion triggers, how each trigger reads, the key, which
// triggers are mentioned already (#7's Q2a), and the goldens.

var holdItemMallory = github.Author{Login: "mallory", Type: "User"}

// holdItemAt is m minutes after waitT0.
func holdItemAt(m int) time.Time { return waitT0.Add(time.Duration(m) * time.Minute) }

// holdItemComment is comment id on issue 5, by author, posted id-500
// minutes after waitT0.
func holdItemComment(id int64, author github.Author, body string) github.Comment {
	return github.Comment{ID: id, Author: author, Body: body, CreatedAt: holdItemAt(int(id - 500))}
}

// holdItemSnap is open issue 5, an epic, with the App's move to ready in
// comment 501, then comments.
func holdItemSnap(comments ...github.Comment) *github.Snapshot {
	s := &github.Snapshot{Number: 5, Title: "Ship it", Type: "epic", Author: waitOwner, Body: "The spec.", Labels: []string{"stage:held"},
		CreatedAt: waitT0}
	s.Comments = append([]github.Comment{holdItemComment(501, waitApp, "<!-- issuedog:transition from=intake to=ready run=rready -->")}, comments...)
	return s
}

// holdItemHeld is the App's hold of issue 5 at ready for the trigger
// keyed key, in comment id, with its cc line if cc is set and its notice
// marker if notice is.
func holdItemHeld(id int64, key string, cc, notice bool) github.Comment {
	text := "On hold for discussion, from `ready`: it. Only a code owner can move this issue, so it stays on hold until they resolve this."
	if cc {
		text += "\n\ncc @owner"
	}
	text += "\n\n<!-- issuedog:transition from=ready to=held at=ready origin=ready reason=discussion event=" + key + " seen=501 -->"
	if notice {
		text += "\n<!-- issuedog:notice key=" + key + " -->"
	}
	return holdItemComment(id, waitApp, text)
}

// holdItemNotice is the App's notice of the trigger keyed key, in comment
// id, with its cc line.
func holdItemNotice(id int64, key string) github.Comment {
	return holdItemComment(id, waitApp, "Also on hold for discussion: it. This issue stays on hold until a code owner accepts it.\n\ncc @owner\n\n"+
		"<!-- issuedog:notice key="+key+" -->")
}

// holdItemOwnerHeld is comments 502 and 503: the owner's /hold, and the
// App's move to held for it.
func holdItemOwnerHeld() []github.Comment {
	return []github.Comment{holdItemComment(502, waitOwner, "/hold"),
		holdItemComment(503, waitApp, "On hold.\n\n<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=502 seen=501 -->")}
}

// holdItemFindOne is the one item waitFind finds on s, with its dirty
// content d and open triggers holds.
func holdItemFindOne(t *testing.T, s *github.Snapshot, d Dirt, holds ...discTrigger) waitItem {
	t.Helper()
	in := waitIn(s, d)
	in.Holds = holds
	return waitFindOne(t, in)
}

// holdItemTriggers is the trigger lines of it.
func holdItemTriggers(it waitItem) []holdItemTrigger {
	var hs []holdItemTrigger
	for _, l := range it.Lines {
		if l.Hold != nil {
			hs = append(hs, *l.Hold)
		}
	}
	return hs
}

// holdItemKinds are a held issue 5 for each kind of trigger, each held by
// it with the App's comment 503 (and its notice, but for a comment), and
// the trigger itself. The last is a comment trigger known from its record
// alone: comment 502 is gone.
func holdItemKinds() []struct {
	name string
	s    *github.Snapshot
	d    Dirt
	tr   discTrigger
} {
	edit := discEditKey(502, holdItemAt(10))
	return []struct {
		name string
		s    *github.Snapshot
		d    Dirt
		tr   discTrigger
	}{
		{"comment", holdItemSnap(holdItemComment(502, holdItemMallory, "Wait, is this right?"), holdItemHeld(503, "c502", true, false)), nil,
			discTrigger{Kind: discComment, Key: "c502", By: "mallory", At: holdItemAt(2), ID: 502}},
		{"edit", holdItemSnap(holdItemComment(502, waitOwner, "The plan, edited by mallory."), holdItemHeld(503, edit, true, true)), Dirt{502: "mallory"},
			discTrigger{Kind: discEdit, Key: edit, By: "mallory", At: holdItemAt(10), ID: 502, Of: "owner"}},
		{"own-edit", holdItemSnap(holdItemComment(502, holdItemMallory, "Approved, then edited."), holdItemHeld(503, edit, true, true)), nil,
			discTrigger{Kind: discOwnEdit, Key: edit, By: "mallory", At: holdItemAt(10), ID: 502}},
		{"deletion", holdItemSnap(holdItemHeld(503, "dCDE_x1", true, true)), nil,
			discTrigger{Kind: discDeletion, Key: "dCDE_x1", By: "mallory", At: holdItemAt(2), Of: waitSlug + "[bot]"}},
		{"rename", holdItemSnap(holdItemHeld(503, "9001", true, true)), nil,
			discTrigger{Kind: discRename, Key: "9001", By: "mallory", At: holdItemAt(2), Title: "Wait [x](https://evil.example) @eve <b>"}},
		{"recorded", holdItemSnap(holdItemHeld(503, "c502", true, false)), nil,
			discTrigger{Kind: discComment, Key: "c502", At: holdItemAt(3), ID: 502, FromRecord: true}},
	}
}

// Each kind of trigger on an issue a discussion hold holds is one hold
// item, keyed by the hold marker's event, linked to the issue, with a
// line for the trigger that links it and says how it's resolved. The
// hold's comment ends in a cc line, so each is mentioned already.
func TestHoldItemKinds(t *testing.T) {
	issue := "https://github.com/o/r/issues/5"
	want := map[string]holdItemTrigger{
		"comment": {Link: issue + "#issuecomment-502", How: "A code owner's 👍 on the comment approves it, and any later comment of theirs here counters it."},
		"edit":    {Link: issue + "#issuecomment-503", How: "A code owner's 👍 on its notice accepts it."},
		"own-edit": {Link: issue + "#issuecomment-503",
			How: "A code owner's 👍 on its notice accepts it."},
		"deletion": {Link: issue + "#issuecomment-503", How: "A code owner's 👍 on its notice accepts it."},
		"rename":   {Link: issue + "#issuecomment-503", How: "A code owner's 👍 on its notice accepts it."},
		"recorded": {Link: issue + "#issuecomment-503", How: "A later comment by a code owner here counters it."},
	}
	for _, c := range holdItemKinds() {
		t.Run(c.name, func(t *testing.T) {
			it := holdItemFindOne(t, c.s, c.d, c.tr)
			if it.Kind != waitHold || it.Key != "hold-5-"+c.tr.Key || it.Link != issue || it.Comment != state.BodyID || it.Title != "Ship it" {
				t.Errorf("got %s %s %s comment %d %q; want a hold keyed hold-5-%s, linked to the issue", it.Kind, it.Key, it.Link, it.Comment, it.Title, c.tr.Key)
			}
			w := want[c.name]
			w.Trigger, w.Mentioned = c.tr, true
			if hs := holdItemTriggers(it); len(hs) != 1 || hs[0] != w {
				t.Errorf("triggers %+v; want %+v", hs, w)
			}
			if texts := waitTexts(it.Lines); !slices.Equal(texts, []string{"On hold for discussion, from `ready`, with 1 open trigger that only a code owner can resolve.",
				"", "Post `/resume` here to accept every open trigger at once."}) {
				t.Errorf("lines %q; want the hold, its trigger and /resume", texts)
			}
			if news := it.news(); len(news) != 0 {
				t.Errorf("news %q; want none, the hold's comment mentioned it", news)
			}
		})
	}
}

// holdItemStacked is issue 5 under the owner's /hold, with open
// discussion triggers that write no hold marker (Q2b): mallory's comment
// 504, which has no App comment; a rename with its notice 505; a deletion
// whose notice discLost held back; and an edit of the body known from its
// record alone, in notice 506.
func holdItemStacked() (*github.Snapshot, []discTrigger) {
	edit := discEditKey(state.BodyID, holdItemAt(1))
	s := holdItemSnap(append(holdItemOwnerHeld(), holdItemComment(504, holdItemMallory, "Wait."), holdItemNotice(505, "9001"),
		holdItemNotice(506, edit))...)
	return s, []discTrigger{
		{Kind: discEdit, Key: edit, At: holdItemAt(1), ID: state.BodyID, FromRecord: true},
		{Kind: discComment, Key: "c504", By: "mallory", At: holdItemAt(4), ID: 504},
		{Kind: discRename, Key: "9001", By: "mallory", At: holdItemAt(5), Title: "Wait"},
		{Kind: discDeletion, Key: "dCDE_y2", By: "mallory", At: holdItemAt(7), Of: "owner"},
	}
}

// Under an owner hold, the key names the /hold's marker, cmd=502, which no
// trigger changes. The comment and the deletion with no notice aren't
// mentioned, so they announce the hold; the rename's and the recorded
// edit's notices mentioned them. The deletion is resolved only by
// /resume, and the owner hold is named.
func TestHoldItemStacked(t *testing.T) {
	s, holds := holdItemStacked()
	it := holdItemFindOne(t, s, nil, holds...)
	issue := "https://github.com/o/r/issues/5"
	if it.Key != "hold-5-cmd502" {
		t.Errorf("key %s; want the /hold marker's, hold-5-cmd502", it.Key)
	}
	want := []holdItemTrigger{
		{Trigger: holds[0], Link: issue + "#issuecomment-506", How: "A code owner's 👍 on its notice accepts it.", Mentioned: true},
		{Trigger: holds[1], Link: issue + "#issuecomment-504", How: "A code owner's 👍 on the comment approves it, and any later comment of theirs here counters it."},
		{Trigger: holds[2], Link: issue + "#issuecomment-505", How: "A code owner's 👍 on its notice accepts it.", Mentioned: true},
		{Trigger: holds[3], Link: issue, How: "It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it."},
	}
	if got := holdItemTriggers(it); !slices.Equal(got, want) {
		t.Errorf("triggers\n%+v\nwant\n%+v", got, want)
	}
	if texts := waitTexts(it.Lines); !slices.Equal(texts, []string{"On hold for discussion, from `ready`, with 4 open triggers that only a code owner can resolve.",
		"", "", "", "", "It's on an owner hold too, so it stays held after these until `/resume` ends that.",
		"Post `/resume` here to accept every open trigger at once."}) {
		t.Errorf("lines %q", texts)
	}
	if news := it.news(); !slices.Equal(news, []string{"hold-5-c504", "hold-5-dCDE_y2"}) {
		t.Errorf("news %q; want the comment's and the deletion's", news)
	}
}

// The key is the event of the hold marker that moved the issue into held,
// though an older trigger is open: one found after the hold, as a
// deletion GitHub's event lag showed late.
func TestHoldItemKeyFromTheMarker(t *testing.T) {
	s := holdItemSnap(holdItemComment(502, holdItemMallory, "Wait."), holdItemHeld(503, "c502", true, false))
	it := holdItemFindOne(t, s, nil, discTrigger{Kind: discDeletion, Key: "dCDE_z", By: "mallory", At: holdItemAt(1), Of: "owner"},
		discTrigger{Kind: discComment, Key: "c502", By: "mallory", At: holdItemAt(2), ID: 502})
	if it.Key != "hold-5-c502" {
		t.Errorf("key %s; want the hold marker's, hold-5-c502", it.Key)
	}
	if news := it.news(); !slices.Equal(news, []string{"hold-5-dCDE_z"}) {
		t.Errorf("news %q; want the deletion's, which no App comment named", news)
	}
}

// Under a hold set by hand, the key names its marker by the label's
// event, not by the trigger, and the hand-set hold is named as an owner
// hold.
func TestHoldItemKeyUnderHandSetHold(t *testing.T) {
	s := holdItemSnap(holdItemComment(502, waitApp, "<!-- issuedog:transition from=ready to=held at=ready reason=owner by=owner event=77 seen=501 -->"),
		holdItemComment(503, holdItemMallory, "Wait."))
	it := holdItemFindOne(t, s, nil, discTrigger{Kind: discComment, Key: "c503", By: "mallory", At: holdItemAt(3), ID: 503})
	if it.Key != "hold-5-77" {
		t.Errorf("key %s; want the hand-set hold's event, hold-5-77", it.Key)
	}
	if texts := waitTexts(it.Lines); !slices.Contains(texts, "It's on an owner hold too, so it stays held after these until `/resume` ends that.") {
		t.Errorf("lines %q; want the hand-set hold named as an owner hold", texts)
	}
}

// A hold set by hand on a held issue writes a held → held marker, but the
// first hold still keys the item: the key doesn't move while the issue
// stays held.
func TestHoldItemKeyFirstHoldWins(t *testing.T) {
	s := holdItemSnap(append(holdItemOwnerHeld(),
		holdItemComment(504, waitApp, "<!-- issuedog:transition from=held to=held at=ready reason=owner by=owner event=77 seen=501 -->"),
		holdItemComment(505, holdItemMallory, "Wait."))...)
	it := holdItemFindOne(t, s, nil, discTrigger{Kind: discComment, Key: "c505", By: "mallory", At: holdItemAt(5), ID: 505})
	if it.Key != "hold-5-cmd502" {
		t.Errorf("key %s; want the first hold's, hold-5-cmd502", it.Key)
	}
}

// A hold marker with no key, which the App doesn't write, keys the item
// by the oldest open trigger. One with no reason, from before #87, is an
// owner hold. A /hold by 👍 keys it by its reaction, and a run's by its
// run.
func TestHoldItemKeyFallback(t *testing.T) {
	tr := discTrigger{Kind: discComment, Key: "c503", By: "mallory", At: holdItemAt(3), ID: 503}
	for fields, key := range map[string]string{"": "hold-5-c503", "cmd=r42 ": "hold-5-cmdr42", "run=h1 ": "hold-5-runh1"} {
		s := holdItemSnap(holdItemComment(502, waitApp, "<!-- issuedog:transition from=ready to=held at=ready "+fields+"seen=501 -->"),
			holdItemComment(503, holdItemMallory, "Wait."))
		it := holdItemFindOne(t, s, nil, tr)
		if it.Key != key {
			t.Errorf("%q: key %s; want %s", fields, it.Key, key)
		}
		if texts := waitTexts(it.Lines); !slices.Contains(texts, "It's on an owner hold too, so it stays held after these until `/resume` ends that.") {
			t.Errorf("%q: lines %q; want the hold with no reason named as an owner hold", fields, texts)
		}
	}
}

// Only a notice marker links a trigger's notice: another App marker with
// a key= field, such as a revert's, is none.
func TestHoldItemNoticeOnly(t *testing.T) {
	s := holdItemSnap(holdItemComment(502, waitApp, "Put back.\n\ncc @owner\n\n<!-- issuedog:revert key=9001 -->"), holdItemHeld(503, "9001", true, false))
	hs := holdItemTriggers(holdItemFindOne(t, s, nil, discTrigger{Kind: discRename, Key: "9001", By: "mallory", At: holdItemAt(2), Title: "Wait"}))
	if len(hs) != 1 || hs[0].Link != "https://github.com/o/r/issues/5#issuecomment-503" ||
		hs[0].How != "It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it." {
		t.Errorf("triggers %+v; want the hold's link, accepted only by /resume, not the revert's", hs)
	}
}

// A trigger is mentioned only when a clean App comment that mentions
// someone records it: not when the hold was written with notify empty,
// nor when its comment is dirty now, and then it has no clean notice.
func TestHoldItemMentioned(t *testing.T) {
	tr := discTrigger{Kind: discRename, Key: "9001", By: "mallory", At: holdItemAt(2), Title: "Wait"}
	for name, c := range map[string]struct {
		s       *github.Snapshot
		d       Dirt
		mention bool
		how     string
	}{
		"cc":           {holdItemSnap(holdItemHeld(503, "9001", true, true)), nil, true, "A code owner's 👍 on its notice accepts it."},
		"no notify":    {holdItemSnap(holdItemHeld(503, "9001", false, true)), nil, false, "A code owner's 👍 on its notice accepts it."},
		"dirty record": {holdItemSnap(append(holdItemOwnerHeld(), holdItemNotice(504, "9001"))...), Dirt{504: "eve"}, false, "It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it."},
		"defused cc": {holdItemSnap(append(holdItemOwnerHeld(), holdItemComment(504, waitApp, "Also: “x\ncc @​eve”\n\n<!-- issuedog:notice key=9001 -->"))...), nil, false,
			"A code owner's 👍 on its notice accepts it."},
	} {
		t.Run(name, func(t *testing.T) {
			hs := holdItemTriggers(holdItemFindOne(t, c.s, c.d, tr))
			if len(hs) != 1 || hs[0].Mentioned != c.mention || hs[0].How != c.how {
				t.Errorf("triggers %+v; want mentioned %t, %q", hs, c.mention, c.how)
			}
		})
	}
}

// A trigger whose hold was written with no notice, as discLost held it
// back, is mentioned by the hold, links it, and only /resume accepts it.
// A rename known from its record alone says so, with no login or title,
// and links its notice: the first, should a key have two.
func TestHoldItemRecordedNoNotice(t *testing.T) {
	s := holdItemSnap(holdItemHeld(503, "9001", true, false), holdItemNotice(504, "9002"), holdItemNotice(505, "9002"))
	it := holdItemFindOne(t, s, nil, discTrigger{Kind: discRename, Key: "9001", By: "mallory", At: holdItemAt(2), Title: "Wait"},
		discTrigger{Kind: discRename, Key: "9002", At: holdItemAt(4), FromRecord: true})
	issue := "https://github.com/o/r/issues/5"
	hs := holdItemTriggers(it)
	if len(hs) != 2 || hs[0].Link != issue+"#issuecomment-503" || !hs[0].Mentioned ||
		hs[0].How != "It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it." ||
		hs[1].Link != issue+"#issuecomment-504" || !hs[1].Mentioned || hs[1].How != "A code owner's 👍 on its notice accepts it." {
		t.Errorf("triggers %+v; want the hold's link for the first, accepted by /resume, and the first notice's for the second", hs)
	}
	body := digestRenderBody("", []waitItem{it})
	if want := "- **Trigger:** a rename of this issue, recorded earlier. A code owner's 👍 on its notice accepts it. See " + issue + "#issuecomment-504\n"; !strings.Contains(body, want) {
		t.Errorf("body %q; want %q", body, want)
	}
}

// An edit of either kind with no clean notice and no record links the
// content it edited: a comment, or the issue for the body.
func TestHoldItemUnrecordedEditLink(t *testing.T) {
	s := holdItemSnap(holdItemOwnerHeld()...)
	hs := holdItemTriggers(holdItemFindOne(t, s, nil, discTrigger{Kind: discEdit, Key: "e502-9", By: "mallory", At: holdItemAt(9), ID: 502, Of: "owner"},
		discTrigger{Kind: discEdit, Key: "e0-9", By: "mallory", At: holdItemAt(9), ID: state.BodyID, Of: "owner"},
		discTrigger{Kind: discOwnEdit, Key: "e504-9", By: "mallory", At: holdItemAt(9), ID: 504}))
	if len(hs) != 3 || hs[0].Link != "https://github.com/o/r/issues/5#issuecomment-502" || hs[1].Link != "https://github.com/o/r/issues/5" ||
		hs[2].Link != "https://github.com/o/r/issues/5#issuecomment-504" {
		t.Errorf("triggers %+v; want comment 502's link, the issue's, then comment 504's", hs)
	}
}

// No hold item: on an issue that isn't held, though triggers are open, as
// when the table lacks the hold's move; on a held issue with none, as
// under an owner hold alone, which is the owner's own; and on a closed
// issue.
func TestHoldItemNone(t *testing.T) {
	tr := discTrigger{Kind: discComment, Key: "c502", By: "mallory", At: holdItemAt(2), ID: 502}
	notHeld := holdItemSnap(holdItemComment(502, holdItemMallory, "Wait."))
	notHeld.Labels = []string{"stage:ready"}
	closed := holdItemSnap(holdItemComment(502, holdItemMallory, "Wait."), holdItemHeld(503, "c502", true, false))
	closed.Closed = true
	for name, c := range map[string]struct {
		s     *github.Snapshot
		holds []discTrigger
	}{
		"not held":        {notHeld, []discTrigger{tr}},
		"owner hold only": {holdItemSnap(holdItemOwnerHeld()...), nil},
		"closed":          {closed, []discTrigger{tr}},
	} {
		t.Run(name, func(t *testing.T) {
			in := waitIn(c.s, nil)
			in.Holds = c.holds
			items, err := waitFind(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Errorf("items %+v; want none", items)
			}
		})
	}
}

// A hold and a state error on one issue are both listed, and the error
// ranks first, by kind, though the hold's issue is the older.
func TestHoldItemWithError(t *testing.T) {
	s := holdItemSnap(holdItemComment(502, holdItemMallory, "Wait."), holdItemHeld(503, "c502", true, false),
		holdItemComment(504, waitApp, "Something's off.\n\n<!-- issuedog:error kind=transition key=77 -->"))
	in := waitIn(s, nil)
	in.Holds = []discTrigger{{Kind: discComment, Key: "c502", By: "mallory", At: holdItemAt(2), ID: 502}}
	items, err := waitFind(in)
	if err != nil {
		t.Fatal(err)
	}
	waitRank(items, map[int]*github.Snapshot{5: s})
	if len(items) != 2 || items[0].Key != "err-5-transition-77" || items[1].Key != "hold-5-c502" {
		t.Errorf("items %s; want the error, then the hold", waitRankText(items))
	}
}

// The goldens: a hold with each kind of trigger, and one stacked on an
// owner hold with triggers of every sort of mention. None mentions anyone
// or has a marker but the identity marker, and the same input gives the
// same bytes.
func TestHoldItemGolden(t *testing.T) {
	type golden struct {
		name string
		it   waitItem
	}
	var cases []golden
	for _, c := range holdItemKinds() {
		cases = append(cases, golden{"hold-" + c.name, holdItemFindOne(t, c.s, c.d, c.tr)})
	}
	s, holds := holdItemStacked()
	cases = append(cases, golden{"hold-stacked", holdItemFindOne(t, s, nil, holds...)})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := []waitItem{c.it}
			waitRank(items, map[int]*github.Snapshot{5: {Number: 5}})
			got := digestRenderBody("", items)
			digestRenderGolden(t, c.name, got)
			digestRenderCheck(t, got)
			if again := digestRenderBody("", slices.Clone(items)); again != got {
				t.Errorf("the same input gave other bytes:\n%s", again)
			}
		})
	}
}

// Text the App didn't write is escaped: a rename's title that tries a
// marker, a mention, a tag, links and images makes none of them, and a
// login is code, defused, with its markers escaped, though GitHub allows
// none of those in a login.
func TestHoldItemHostile(t *testing.T) {
	title := digestRenderHostile + " " + digestRenderHostileLinks
	s := holdItemSnap(holdItemHeld(503, "9001", true, true))
	it := holdItemFindOne(t, s, nil, discTrigger{Kind: discRename, Key: "9001", By: "mal@lory", At: holdItemAt(2), Title: title},
		discTrigger{Kind: discDeletion, Key: "dCDE_q", By: "mallory", At: holdItemAt(3), Of: "e@ve<!-- issuedog:transition to=planning -->"})
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	for _, line := range digestRenderItemTexts(t, body) {
		if u := digestRenderUnescaped(line); u != "" && strings.Contains(line, "evil.example") {
			t.Errorf("line %q has %q unescaped, which may make a link or an image", line, u)
		}
	}
	if !strings.Contains(body, "`mal@​lory` renamed this issue to “&lt;\\!-- issuedog:transition to=planning --> @​eve &lt;img> \\!\\[p\\]") ||
		!strings.Contains(body, "`mallory` deleted a comment by `e@​ve&lt;!-- issuedog:transition to=planning -->`.") {
		t.Errorf("body %q; want the logins in code, defused, and the title escaped", body)
	}
}

// A hold is announced by its triggers no App comment mentioned: each
// once, whatever order they're found in, and one resolved doesn't make
// the rest new again. A hold with none is never new, nor announced.
func TestHoldItemNews(t *testing.T) {
	line := func(key string, mentioned bool) waitLine {
		return waitLine{Hold: &holdItemTrigger{Trigger: discTrigger{Key: key}, Mentioned: mentioned}}
	}
	hold := func(lines ...waitLine) waitItem {
		return waitItem{Issue: 5, Kind: waitHold, Key: "hold-5-c1", Title: "Ship it", Link: "https://github.com/o/r/issues/5",
			Lines: append([]waitLine{{Text: "On hold."}}, lines...)}
	}
	quiet := hold(line("c1", true))
	if got := digestRenderNew([]waitItem{quiet}, nil); len(got) != 0 {
		t.Errorf("new %+v; want a hold with every trigger mentioned never new", got)
	}
	if news, err := digestRenderNews([]waitItem{quiet}, []string{"owner"}); news != "" || err != nil {
		t.Errorf("got %q, %v; want nothing posted for it", news, err)
	}
	one := hold(line("c1", true), line("c7", false))
	news, err := digestRenderNews(digestRenderNew([]waitItem{one}, nil), []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(news, "New on the digest, waiting on you:\n\n- **Discussion hold** on #5: Ship it. Answer here: https://github.com/o/r/issues/5\n\ncc @owner") ||
		!strings.HasSuffix(news, "<!-- issuedog:digest-new items=hold-5-c7 -->") {
		t.Errorf("announcement %q; want the hold, the cc line, and the comment's key alone", news)
	}
	announced := map[string]bool{"hold-5-c7": true}
	if got := digestRenderNew([]waitItem{one}, announced); len(got) != 0 {
		t.Errorf("new %+v; want the hold announced already", got)
	}
	two := hold(line("c1", true), line("dX", false), line("c7", false))
	news, err = digestRenderNews(digestRenderNew([]waitItem{two}, announced), []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(news, "<!-- issuedog:digest-new items=hold-5-dX,hold-5-c7 -->") {
		t.Errorf("announcement %q; want the older trigger found later to announce it again", news)
	}
	announced["hold-5-dX"] = true
	if got := digestRenderNew([]waitItem{hold(line("dX", false))}, announced); len(got) != 0 {
		t.Errorf("new %+v; want no new hold once one of its triggers is resolved", got)
	}
}

// Holds rank after state errors and before approvals, whatever their
// priority: a p0 hold after a p2 error, and a p3 hold before a p2
// approval.
func TestHoldItemRank(t *testing.T) {
	items := []waitItem{
		{Issue: 2, Kind: waitApproval, Key: "rev-2-1", Priority: 2, Created: holdItemAt(1)},
		{Issue: 3, Kind: waitHold, Key: "hold-3-c1", Priority: 3, Created: holdItemAt(2)},
		{Issue: 4, Kind: waitHold, Key: "hold-4-c1", Priority: 0, Created: holdItemAt(3)},
		{Issue: 1, Kind: waitError, Key: "err-1-labels-0", Priority: 2, Created: holdItemAt(4)},
	}
	waitRank(items, map[int]*github.Snapshot{})
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if want := []string{"err-1-labels-0", "hold-4-c1", "hold-3-c1", "rev-2-1"}; !slices.Equal(keys, want) {
		t.Errorf("ranked %q; want %q", keys, want)
	}
	if fmt.Sprint(waitHold) != "hold" || waitKeyPrefix[waitHold] != "hold" {
		t.Errorf("the hold kind is %q, prefixed %q; want hold for both", waitHold, waitKeyPrefix[waitHold])
	}
}

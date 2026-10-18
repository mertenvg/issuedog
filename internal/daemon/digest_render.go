package daemon

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// Rendering the digest (SPEC §9.4; plan #7, T6, and assumptions 6, 10, 11
// and 12, Q1a and Q2a). It's pure, with no I/O:
//   - digestRenderBody is the digest's body: the pin note if there is one,
//     then every item in the order given, then the identity marker last.
//     The digest's pass (T8, #160) ranks the items with waitRank first.
//   - digestRenderNews is the comment that announces new items, with the
//     cc line and a digest-new marker naming their keys.
//   - digestRenderNew picks the items no digest-new comment announced yet.
//
// Both texts are the same bytes for the same input: they hold no times,
// relative or not.
//
// Whose text is what (assumption 12):
//   - an item's Title is raw, so it goes through text and github.Escape,
//     with its "[", "]", "!" and "\" escaped too, so it makes no Markdown
//     link, image or tracking pixel (digestRenderTitle, the owner's call
//     on #160). GitHub still autolinks a bare https://… or www.… in it,
//     and owner/repo#1 still makes a cross-reference, each showing its
//     whole target and loading nothing (SPEC §16, "Links in titles");
//   - a line the App copied from its own comment (waitLine.Copied) was only
//     made safe for the middle of a line, and a state error's lines were
//     escaped but never defused, so every copied line is inline'd and
//     escaped again, and then gets the line-start defences
//     (digestRenderLead), as it starts a list item here;
//   - a hold's trigger (waitLine.Hold) names logins and a rename's title,
//     which aren't the App's: logins go in code, defused, and the title
//     is escaped as an item's Title is (digestRenderHold);
//   - waiting.go's own lines, the pin note and the link are the App's.
//
// The body mentions nobody: titles and copied lines are defused, and the
// pin note is defused too, which changes nothing in digestPinNote's
// (#159). Only the announcement's cc line mentions anyone.

// digestRenderIdentity is the digest's identity marker, as
// github.FormatMarker writes kind "digest" with no fields. It's the one
// constant for it: digest.go writes it through digestBody, and finds the
// digest by it (digestIdentity).
const digestRenderIdentity = "<!-- issuedog:digest -->"

// digestRenderLimit is what a body or comment may come to, by
// digestRenderSize: GitHub's 65,536 characters, with 1,024 to spare, as
// SPEC §9.1 keeps for the interviewer's texts.
const digestRenderLimit = 65536 - 1024

// digestRenderSize is how much s counts toward digestRenderLimit, counted
// so that it's never less than GitHub's count, however GitHub counts: its
// UTF-8 bytes, which are never fewer than its UTF-16 code units or its
// characters, plus one more for each "\n", in case it's stored as "\r\n".
func digestRenderSize(s string) int {
	return len(s) + strings.Count(s, "\n")
}

// digestRenderBody renders the digest's body (assumption 10): note, the
// pin note, at the top if it isn't "", as digestPin (#159) needs the body
// to start with it; then how many items wait, and each of items in the
// order given; then the identity marker. With no items it says "Nothing
// is waiting on you." If the body would go over digestRenderLimit, the
// items at the end, the lowest ranked, are cut, and a line says how many.
func digestRenderBody(note string, items []waitItem) string {
	var head []string
	if note = strings.TrimSpace(note); note != "" {
		head = append(head, github.Escape(defuse(note)))
	}
	if len(items) == 0 {
		return strings.Join(append(head, "Nothing is waiting on you.", digestRenderIdentity), "\n\n")
	}
	intro := "1 item waits on you."
	if len(items) > 1 {
		intro = fmt.Sprintf("%d items wait on you, the ones that unblock the most first.", len(items))
	}
	head = append(head, intro)
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = digestRenderItem(i+1, it)
	}
	render := func(k int) string {
		body := append(append([]string{}, head...), parts[:k]...)
		if k < len(items) {
			body = append(body, fmt.Sprintf("…and %d more, cut to fit GitHub's limit on an issue body.", len(items)-k))
		}
		return strings.Join(append(body, digestRenderIdentity), "\n\n")
	}
	return render(digestRenderFit(len(items), render))
}

// digestRenderFit returns n if render(n) fits digestRenderLimit, and
// otherwise the largest k below n for which render(k) fits, or 0 if none
// does. render(k) is a text with the first k of n items, and, for k below
// n, a line saying how many more there are. Each item adds more than that
// line can lose in digits, so the size grows with k below n, and k is
// found by binary search.
func digestRenderFit(n int, render func(k int) string) int {
	if digestRenderSize(render(n)) <= digestRenderLimit {
		return n
	}
	return max(sort.Search(n, func(k int) bool { return digestRenderSize(render(k)) > digestRenderLimit })-1, 0)
}

// digestRenderItem renders it, ranked rank: a heading with its issue and
// escaped title, what it is, its lines as a list, with each question's
// options under it, the recommended one first and marked, and the link to
// answer at.
func digestRenderItem(rank int, it waitItem) string {
	heading := fmt.Sprintf("### %d. #%d", rank, it.Issue)
	if title := digestRenderTitle(it.Title); title != "" {
		// A heading drops the #s that end it after a space, so an escaped
		// title keeps its last one.
		if strings.HasSuffix(title, "#") {
			title = title[:len(title)-1] + `\#`
		}
		heading += ": " + title
	}
	about := fmt.Sprintf("**%s**, priority p%d", digestRenderKindName(it.Kind), it.Priority)
	if it.Unblocks > 0 {
		about += fmt.Sprintf(", unblocks %s (itself included)", waitCount(it.Unblocks, "open issue"))
	}
	var lines []string
	for _, l := range it.Lines {
		t := l.Text
		if l.Copied {
			t = digestRenderLead(digestRenderText(t))
		}
		switch {
		case l.Hold != nil:
			lines = append(lines, "- "+digestRenderHold(*l.Hold))
		case l.Question > 0:
			lines = append(lines, fmt.Sprintf("- **Question %d:** %s", l.Question, t))
		case l.Option != "" && l.Recommended:
			// Its text says "(recommended)" too, as the comment does.
			lines = append(lines, fmt.Sprintf("  - **%s.** %s", l.Option, t))
		case l.Option != "":
			lines = append(lines, fmt.Sprintf("  - %s. %s", l.Option, t))
		default:
			lines = append(lines, "- "+t)
		}
	}
	lines = append(lines, "- Answer here: "+it.Link)
	return strings.Join([]string{heading, about, strings.Join(lines, "\n")}, "\n\n")
}

// digestRenderText is text the App didn't write, or copied from its own
// comment, made safe for the middle of a line: one line, its tags escaped,
// its mentions defused and its task boxes escaped (inline), then its
// markers escaped (SPEC §5.1). Both are the same again on text they
// already made safe, so App text copied as posted comes out unchanged.
func digestRenderText(s string) string {
	return github.Escape(inline(s))
}

// digestRenderTitle is an issue's raw title made safe for the middle of a
// line: one line, its tags escaped and its mentions defused (text), then
// each "[", "]" and "!" backslash-escaped, so a title makes no Markdown
// link, image or tracking pixel, and each "\" too, so none of those
// escapes can be undone, and the title reads as typed. Then its markers
// are escaped (SPEC §5.1). With every "[" escaped, no task box is left
// (escapeBoxes). A bare https://… or www.… still autolinks, and
// owner/repo#1 still makes a cross-reference: each shows its whole target
// and loads nothing (SPEC §16, "Links in titles").
func digestRenderTitle(s string) string {
	var b strings.Builder
	for _, r := range text(s) {
		if strings.ContainsRune(`\[]!`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return github.Escape(b.String())
}

// digestRenderOrderedRE starts an ordered list item.
var digestRenderOrderedRE = regexp.MustCompile(`^[0-9]{1,9}([.)])`)

// digestRenderLeadChars are the characters that may start a block when
// they start a list item's text: a heading, a block quote, a list item, a
// thematic break or setext underline, a table and a link reference or
// footnote definition. A fence is three backticks or tildes, but one
// backtick may start a code span, which an escape would break.
// digestRenderText already escapes the "<" of an HTML block.
const digestRenderLeadChars = "#>-+*=_|["

// digestRenderLead backslash-escapes what could make s, digestRenderText's
// output, start a block where it starts a list item's text, so it's always
// a paragraph: a first character in digestRenderLeadChars, a fence's
// first character, or an ordered list's "." or ")". Mid-line, an escaped
// character reads as itself.
func digestRenderLead(s string) string {
	if m := digestRenderOrderedRE.FindStringSubmatchIndex(s); m != nil {
		return s[:m[2]] + `\` + s[m[2]:]
	}
	if s != "" && strings.IndexByte(digestRenderLeadChars, s[0]) >= 0 || strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
		return `\` + s
	}
	return s
}

// digestRenderKindName names kind k for the owner.
func digestRenderKindName(k waitKind) string {
	switch k {
	case waitError:
		return "State error"
	case waitHold:
		return "Discussion hold"
	case waitApproval:
		return "Approval"
	case waitPlanApproval:
		return "Plan approval"
	case waitQuestion:
		return "Question round"
	case waitFlag:
		return "Triage flag"
	case waitHuman:
		return "Human task"
	case waitDone:
		return "Finished story"
	}
	return "Item (" + k.String() + ")"
}

// digestRenderMentions reports whether new item it gets the digest's
// mention (Q2a): only one whose own comment doesn't mention notify
// already. A question round, a spec's revision, a plan's revision (the
// plan comment ends in one, #270) and a triage flag end in a cc line, and
// so does a human task's body (#287, SPEC §9.4), so
// that leaves state errors, and a discussion hold with a trigger that no
// App comment mentioning notify names (waitItem.news, #192): a comment on
// an issue held already (Q2b), or a trigger whose notice discLost held
// back, unless the hold's own transition marker records it. A hold's
// other triggers, named by the hold's comment or a notice, each ending in
// a cc line (#87's assumption 5), give it none. A finished story (#278)
// gets it, as it isn't listed here: no comment says its children have
// all closed. A kind added later gets the mention unless it's listed here.
func digestRenderMentions(it waitItem) bool {
	switch it.Kind {
	case waitApproval, waitPlanApproval, waitQuestion, waitFlag, waitHuman:
		return false
	case waitHold:
		return len(it.news()) > 0
	}
	return true
}

// digestRenderHold renders h, an open trigger on a hold item, for its line
// in the list: what it is, how a code owner resolves it, and its link.
// What it is says who did it and to whose content, as the hold's comment
// and its notice do (discWhat), with each login in code and defused, and a
// rename's title escaped as an item's Title is (digestRenderTitle), so it
// makes no link, image, mention or marker. One known from its record alone
// says it was recorded earlier (discWhatRecorded), in the App's words
// only, and links the comment that holds the record (holdItemTrigger.Link).
func digestRenderHold(h holdItemTrigger) string {
	tr := h.Trigger
	what := discWhat(tr) // discWhatRecorded for one known from its record alone
	if tr.Kind == discRename && !tr.FromRecord {
		what = fmt.Sprintf("%s renamed this issue to “%s”", discLogin(tr.By), digestRenderTitle(tr.Title))
	}
	return fmt.Sprintf("**Trigger:** %s. %s See %s", github.Escape(what), h.How, h.Link)
}

// digestRenderNew returns the items, in order, with a key that announces
// them (waitItem.news), as github.EncodeValue writes it, that isn't in
// announced: the keys the App's digest-new comments that were never
// edited hold (#159's digestState.announced). That's an item's own key,
// or for a hold, one for each trigger no App comment mentioned (#192). An
// item announced before isn't new, even if it went away and came back
// with the same key (assumption 11).
//
// A key is one of a comma-separated list that github.ParseMarkers reads
// back and #159's digestState.comment splits on ",". The encoding leaves
// every key waitFind makes today as it is, since an error's kind and key
// come from a marker, which holds no space, "<" or ">", and the App's
// keys hold no "," or "%". It's never split on "-", which an error's key
// may hold.
func digestRenderNew(items []waitItem, announced map[string]bool) []waitItem {
	var fresh []waitItem
	for _, it := range items {
		if slices.ContainsFunc(it.news(), func(k string) bool { return !announced[github.EncodeValue(k)] }) {
			fresh = append(fresh, it)
		}
	}
	return fresh
}

// digestRenderNews renders the comment that announces the new items
// (Q1a), or "" if there's nothing to post. items are the new ones in rank
// order (digestRenderNew), and it chooses the ones that get the mention
// itself (digestRenderMentions, Q2a), so the caller passes every new
// item. With notify empty, or no item that gets the mention, it's "", and
// nothing is posted: an item it leaves out stays unannounced, which costs
// nothing, as it never gets a mention.
//
// The comment lists each item it announces on one line, with its issue,
// escaped title, kind and link, then the cc line for notify, then a
// digest-new marker with the keys that announce them (waitItem.news,
// github.EncodeValue): a hold's keys name each trigger no App comment
// mentioned, so a later one makes the hold new again, and one resolved
// doesn't (#192). If it would go over digestRenderLimit, the
// lowest-ranked items are left out of the list and
// the marker both, and a line says so: they're new on the next pass, and
// the next comment announces them.
func digestRenderNews(items []waitItem, notify []string) (string, error) {
	var mention []waitItem
	for _, it := range items {
		if digestRenderMentions(it) {
			mention = append(mention, it)
		}
	}
	if len(notify) == 0 || len(mention) == 0 {
		return "", nil
	}
	lines := make([]string, len(mention))
	keys := make([]string, len(mention))
	for i, it := range mention {
		title := ""
		if t := digestRenderTitle(it.Title); t != "" {
			title = ": " + t
		}
		lines[i] = fmt.Sprintf("- **%s** on #%d%s. Answer here: %s", digestRenderKindName(it.Kind), it.Issue, title, it.Link)
		var news []string
		for _, k := range it.news() {
			news = append(news, github.EncodeValue(k))
		}
		keys[i] = strings.Join(news, ",")
	}
	// The marker is formatted with one key at least, so k = 0 is sized
	// with one too much, rather than failing.
	var ferr error
	render := func(k int) string {
		marker, err := github.FormatMarker(github.Marker{Kind: "digest-new", Fields: map[string]string{"items": strings.Join(keys[:max(k, 1)], ",")}})
		if err != nil {
			ferr = cmp.Or(ferr, err)
			return ""
		}
		list := strings.Join(lines[:k], "\n")
		if k < len(mention) {
			list += fmt.Sprintf("\n\n…and %d more, which I'll announce in the next comment.", len(mention)-k)
		}
		parts := append([]string{"New on the digest, waiting on you:", list}, ccLines(notify)...)
		return strings.Join(append(parts, marker), "\n\n")
	}
	k := digestRenderFit(len(mention), render)
	text := render(k)
	switch {
	case ferr != nil:
		// A key github.EncodeValue wrote is never one the marker can't hold, but
		// for an empty one: none is ever given.
		return "", fmt.Errorf("announce %s on the digest: %w", waitCount(len(mention), "new item"), ferr)
	case k == 0:
		return "", fmt.Errorf("announce %s on the digest: the first, %s, doesn't fit in a comment", waitCount(len(mention), "new item"), mention[0].Key)
	}
	return text, nil
}

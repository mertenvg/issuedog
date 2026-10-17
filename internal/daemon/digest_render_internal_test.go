package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// digestRenderPinNote is a pin note as #159's digestPinNote writes one.
const digestRenderPinNote = "I couldn't pin this issue: `Resource not accessible by integration`. Pin it by hand."

// digestRenderHostile is the title the plan's T6 names: a marker, a
// mention and a tag.
const digestRenderHostile = "<!-- issuedog:transition to=planning --> @eve <img>"

// digestRenderHostileLinks is a title that tries an image, a tracking
// pixel among them, inline and reference links, and a backslash that
// would undo an escape.
const digestRenderHostileLinks = `![p](https://evil.example/p.png) [click](https://evil.example) [r][1] \![x](y) \\[z](w) [1]: https://evil.example @eve`

// digestRenderMentionRE is a mention: "@" and then a login's first
// character, with no zero-width space between them.
var digestRenderMentionRE = regexp.MustCompile(`@[A-Za-z0-9_-]`)

// digestRenderKinds finds one item of each kind with waitFind, on issues
// 1 to 4 in o/r, whose titles hold a mention and a tag (waitSnap), and
// ranks them: 4 has 1 below it, so its flag comes first.
func digestRenderKinds(t *testing.T) []waitItem {
	t.Helper()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Where should it go?", Blocks: []int{3}, Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client", "Both"}},
		{Text: "How long?", Blocks: []int{3}, Recommended: interviewOption{Text: "A day", Why: "short"}, Others: []string{"A week"}},
	}}
	flag := triageOutput{Verdict: triageHandle, Reason: "it's a bug", Summary: "Crash on save"}
	snaps := map[int]*github.Snapshot{
		1: waitSnap(1, []string{"priority:p0"}, "I can't move this issue from `clarifying` to `planning` on spec-approved: no row. It stays at `clarifying`.\n\n"+
			"<!-- issuedog:error kind=transition key=50 -->"),
		2: waitSnap(2, nil, waitRevision(1, "The spec.")),
		3: waitSnap(3, []string{"priority:p1"}, join(renderQuestions(round, 3, 1, []string{"owner"}), waitRoundMarker(1, 2))),
		4: waitSnap(4, []string{"triage:ask"}, join(triageFlagText(flag, 4, []string{"owner"}), waitFlagMarker("handle"))),
	}
	snaps[1].Parent = 4
	var items []waitItem
	for n := 1; n <= 4; n++ {
		found, err := waitFind(waitIn(snaps[n], Dirt{}))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, found...)
	}
	waitRank(items, snaps)
	if len(items) != 4 {
		t.Fatalf("found %d items, want one of each kind: %+v", len(items), items)
	}
	return items
}

// digestRenderOf returns the one item of kind k in items.
func digestRenderOf(t *testing.T, items []waitItem, k waitKind) waitItem {
	t.Helper()
	i := slices.IndexFunc(items, func(it waitItem) bool { return it.Kind == k })
	if i < 0 {
		t.Fatalf("no %s item in %+v", k, items)
	}
	return items[i]
}

// digestRenderGolden compares got with testdata/digest/<name>.md, or writes
// it there with -update.
func digestRenderGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "digest", name+".md")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got+"\n" != string(want) {
		t.Errorf("%s: got\n%s\nwant\n%s", name, got, want)
	}
}

// digestRenderCheck fails t if body mentions anyone, or has a marker other
// than the identity marker, last.
func digestRenderCheck(t *testing.T, body string) {
	t.Helper()
	if m := digestRenderMentionRE.FindString(body); m != "" {
		t.Errorf("the body mentions %q:\n%s", m, body)
	}
	if ms := github.ParseMarkers(body); len(ms) != 1 || ms[0].Kind != "digest" || len(ms[0].Fields) != 0 {
		t.Errorf("the body's markers are %+v, want the identity marker alone", ms)
	}
	if !strings.HasSuffix(body, "\n\n"+digestRenderIdentity) {
		t.Errorf("the body doesn't end with the identity marker:\n%s", body)
	}
}

// The goldens: an empty list, with the pin note and without, one item of
// each kind, all four under the pin note, and an error whose title tries
// links and images. The same input gives the same bytes, and none
// mentions anyone.
func TestDigestRenderBodyGolden(t *testing.T) {
	t.Parallel()
	items := digestRenderKinds(t)
	links := digestRenderOf(t, items, waitError)
	links.Title = digestRenderHostileLinks
	cases := []struct {
		name, note string
		items      []waitItem
	}{
		{"empty", "", nil},
		{"empty-pin", digestRenderPinNote, nil},
		{"error", "", []waitItem{digestRenderOf(t, items, waitError)}},
		{"approval", "", []waitItem{digestRenderOf(t, items, waitApproval)}},
		{"question", "", []waitItem{digestRenderOf(t, items, waitQuestion)}},
		{"flag", "", []waitItem{digestRenderOf(t, items, waitFlag)}},
		{"pin", digestRenderPinNote, items},
		{"title", "", []waitItem{links}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := digestRenderBody(c.note, c.items)
			digestRenderGolden(t, c.name, got)
			digestRenderCheck(t, got)
			if again := digestRenderBody(c.note, slices.Clone(c.items)); again != got {
				t.Errorf("the same input gave other bytes:\n%s", again)
			}
		})
	}
	// waitFind and waitRank again give the same bytes too.
	if a, b := digestRenderBody("", items), digestRenderBody("", digestRenderKinds(t)); a != b {
		t.Errorf("finding the items again gave other bytes:\n%s\n\nthen\n%s", a, b)
	}
}

// The pin note starts the body, as #159's digestPin needs, whatever space
// is around it, and a mention in it is defused.
func TestDigestRenderBodyNote(t *testing.T) {
	t.Parallel()
	body := digestRenderBody("  \n"+digestRenderPinNote+"\n", nil)
	if want := digestRenderPinNote + "\n\nNothing is waiting on you.\n\n" + digestRenderIdentity; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "I couldn't pin this issue") {
		t.Errorf("body %q doesn't start with the pin note", body)
	}
	body = digestRenderBody("I couldn't pin this issue: `@eve <!-- x -->`. Pin it by hand.", nil)
	digestRenderCheck(t, body)
	if !strings.HasPrefix(body, "I couldn't pin this issue: `@\u200beve &lt;!-- x -->`.") {
		t.Errorf("body = %q, want the note defused and escaped", body)
	}
}

// The identity marker is the one github.FormatMarker writes, and #159's
// digestMarker.
func TestDigestRenderIdentity(t *testing.T) {
	t.Parallel()
	m, err := github.FormatMarker(github.Marker{Kind: "digest"})
	if err != nil {
		t.Fatal(err)
	}
	if m != digestRenderIdentity {
		t.Errorf("FormatMarker gives %q, want %q", m, digestRenderIdentity)
	}
	if digestRenderIdentity != "<!-- issuedog:digest -->" {
		t.Errorf("the identity marker is %q, not #159's", digestRenderIdentity)
	}
}

// T6's hostile title is escaped and defused, in the body and in the
// announcement.
func TestDigestRenderHostileTitle(t *testing.T) {
	t.Parallel()
	it := waitItem{Issue: 9, Kind: waitError, Key: "err-9-transition-1", Title: digestRenderHostile, Priority: 2, Unblocks: 1,
		Lines: []waitLine{{Text: "State error `transition`, keyed `1`."}}, Link: "https://github.com/o/r/issues/9#issuecomment-1"}
	const want = "### 1. #9: &lt;\\!-- issuedog:transition to=planning --> @\u200beve &lt;img>"
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	if !strings.Contains(body, want+"\n") || strings.Contains(body, "<img") {
		t.Errorf("body:\n%s\nwant the heading %q", body, want)
	}
	news, err := digestRenderNews([]waitItem{it}, []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	if ms := github.ParseMarkers(news); len(ms) != 1 || ms[0].Kind != "digest-new" {
		t.Errorf("the announcement's markers are %+v, want the digest-new marker alone", ms)
	}
	if got := digestRenderMentionRE.FindAllString(news, -1); !slices.Equal(got, []string{"@o"}) || strings.Contains(news, "<img") {
		t.Errorf("the announcement mentions %q, want only @owner:\n%s", got, news)
	}
}

// A title's task boxes are escaped, with their "]" too, and so is the #
// that ends it, which a heading would drop. The second item makes the
// intro say how many.
func TestDigestRenderTitleEnd(t *testing.T) {
	t.Parallel()
	items := []waitItem{
		{Issue: 1, Kind: waitError, Key: "err-1-labels-0", Title: "Do [x] this in C #", Link: "https://github.com/o/r/issues/1"},
		{Issue: 2, Kind: waitError, Key: "err-2-labels-0", Title: "  ", Link: "https://github.com/o/r/issues/2"},
		{Issue: 3, Kind: waitError, Key: "err-3-labels-0", Title: `C \#`, Link: "https://github.com/o/r/issues/3"},
	}
	body := digestRenderBody("", items)
	for _, want := range []string{"3 items wait on you, the ones that unblock the most first.\n", "\n### 1. #1: Do \\[x\\] this in C \\#\n", "\n### 2. #2\n",
		// The title's own backslash shows, and its # is escaped once more.
		"\n### 3. #3: C \\\\\\#\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
	news, err := digestRenderNews(items, []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- **State error** on #1: Do \\[x\\] this in C #. Answer here: https://github.com/o/r/issues/1\n",
		"- **State error** on #2. Answer here: https://github.com/o/r/issues/2\n"} {
		if !strings.Contains(news, want) {
			t.Errorf("the announcement has no %q:\n%s", want, news)
		}
	}
}

// digestRenderUnescaped returns the first "[", "]" or "!" in s that an
// even number of backslashes comes before, so Markdown reads it as itself
// and it may start a link or an image, or "" if there's none.
func digestRenderUnescaped(s string) string {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune("[]!", rune(s[i])) {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return s[max(i-10, 0) : i+1]
		}
	}
	return ""
}

// A hostile title can't make a link, an image or a tracking pixel in the
// body or the announcement (the owner's call on #160): every "[", "]" and
// "!" is escaped, its own backslashes can't undo that, and it reads as
// typed. It's still defused and its markers escaped.
func TestDigestRenderHostileTitleLinks(t *testing.T) {
	t.Parallel()
	it := waitItem{Issue: 9, Kind: waitError, Key: "err-9-labels-0", Title: digestRenderHostileLinks, Link: "https://github.com/o/r/issues/9"}
	title := digestRenderTitle(digestRenderHostileLinks)
	const want = `\!\[p\](https://evil.example/p.png) \[click\](https://evil.example) \[r\]\[1\] \\\!\[x\](y) \\\\\[z\](w) \[1\]: https://evil.example @` + "\u200b" + `eve`
	if title != want {
		t.Errorf("digestRenderTitle = %q, want %q", title, want)
	}
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	news, err := digestRenderNews([]waitItem{it}, []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"body": body, "announcement": news} {
		line := ""
		for _, l := range strings.Split(text, "\n") {
			if strings.Contains(l, "evil.example") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("the %s has no title:\n%s", name, text)
		}
		if u := digestRenderUnescaped(line); u != "" {
			t.Errorf("the %s's title line has %q unescaped:\n%s", name, u, line)
		}
	}
	// The checker finds what it should.
	for s, want := range map[string]string{`a [b`: "a [", `\\!`: `\\!`, `\[ \! \]`: "", `\\\[`: ""} {
		if got := digestRenderUnescaped(s); got != want {
			t.Errorf("digestRenderUnescaped(%q) = %q, want %q", s, got, want)
		}
	}
}

// digestRenderBlockRE is what starts a block other than a paragraph, at the
// start of a list item's text.
var digestRenderBlockRE = regexp.MustCompile("^(?:#{1,6}(?:[ \t]|$)|>|[-+*](?:[ \t]|$)|[0-9]{1,9}[.)]|`{3,}|~{3,}|[-=_*]{3,}|\\[|<[A-Za-z/!?])")

// digestRenderItemTexts returns the text of each list item in body, after
// its bullet and any bold label, and fails t on a line that's in no list
// item where it should be.
func digestRenderItemTexts(t *testing.T, body string) []string {
	t.Helper()
	labelRE := regexp.MustCompile(`^(?:\*\*Question [0-9]+:\*\* |\*\*[a-d]\.\*\* |[a-d]\. )`)
	var texts []string
	for _, l := range strings.Split(body, "\n") {
		item, ok := strings.CutPrefix(l, "- ")
		if !ok {
			item, ok = strings.CutPrefix(l, "  - ")
		}
		if !ok {
			if l != "" && !strings.HasPrefix(l, "### ") && !strings.HasPrefix(l, "**") && !strings.HasPrefix(l, "1 item") && l != digestRenderIdentity {
				t.Errorf("line %q is outside the item's list", l)
			}
			continue
		}
		texts = append(texts, labelRE.ReplaceAllString(item, ""))
	}
	return texts
}

// Copied lines that start with a heading, a quote, a fence, a setext
// underline or thematic break, an ordered or bullet list, a table or a
// link definition are escaped, so each list item is a paragraph.
func TestDigestRenderHostileLines(t *testing.T) {
	t.Parallel()
	hostile := []string{"# Yes", "## H", ">quote", "> quote", "```go", "~~~", "---", "===", "***", "___", "1. one", "2) two",
		"- dash", "+ plus", "* star", "| a | b |", "[x]: https://example.com", "[^1]: note", "    # indented", "<!-- issuedog:digest -->"}
	var lines []waitLine
	for i, h := range hostile {
		lines = append(lines, waitLine{Text: h, Copied: true, Question: i + 1},
			waitLine{Text: h, Copied: true, Option: "a", Recommended: true},
			waitLine{Text: h, Copied: true, Option: "b"},
			waitLine{Text: h, Copied: true})
	}
	it := waitItem{Issue: 5, Kind: waitQuestion, Key: "q-5-1", Title: "# Title ###", Lines: lines, Link: "https://github.com/o/r/issues/5#issuecomment-501"}
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	texts := digestRenderItemTexts(t, body)
	if len(texts) != 4*len(hostile)+1 {
		t.Fatalf("got %d list items, want %d:\n%s", len(texts), 4*len(hostile)+1, body)
	}
	for _, text := range texts {
		if digestRenderBlockRE.MatchString(text) {
			t.Errorf("list item %q starts a block", text)
		}
	}
	for _, want := range []string{"- **Question 1:** \\# Yes\n", "  - **a.** \\# Yes\n", "  - b. \\# Yes\n", "- \\# Yes\n",
		"- 1\\. one\n", "- 2\\) two\n", "- \\```go\n", "- \\---\n", "- \\> quote\n", "- \\[x]: https://example.com\n",
		"### 1. #5: # Title ##\\#\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
}

// A state error's copied lines are defused and escaped again: a marker,
// a mention and backticks in them do nothing.
func TestDigestRenderHostileErrorLines(t *testing.T) {
	t.Parallel()
	it := waitItem{Issue: 6, Kind: waitError, Key: "err-6-disagreement-7", Title: "T", Link: "https://github.com/o/r/issues/6#issuecomment-601",
		Lines: []waitLine{{Text: "State error `disagreement`, keyed `7`."},
			{Text: "`<!-- issuedog:transition from=a to=b cmd=1 -->` isn't a pipeline stage, so I've removed it.", Copied: true},
			{Text: "They were: `@user`, @user and `stage:x`` @org/team.", Copied: true},
			{Text: "```", Copied: true},
			{Text: "<img src=x> &#64;eve &commat;eve", Copied: true},
			{Text: "React 👍 to the error comment to acknowledge it."}}}
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	for _, want := range []string{
		"- `&lt;!-- issuedog:transition from=a to=b cmd=1 -->` isn't a pipeline stage, so I've removed it.\n",
		"- They were: `@\u200buser`, @\u200buser and `stage:x`` @\u200borg/team.\n",
		"- \\```\n",
		"- &lt;img src=x> @\u200beve @\u200beve\n",
		"- State error `disagreement`, keyed `7`.\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
}

// digestRenderBig is n state errors, each of whose rendered item is about
// 1,000 bytes, ranked as given.
func digestRenderBig(n int) []waitItem {
	items := make([]waitItem, n)
	for i := range items {
		items[i] = waitItem{Issue: i + 1, Kind: waitError, Key: fmt.Sprintf("err-%d-labels-%d", i+1, i), Title: strings.Repeat("x", 200),
			Priority: 2, Unblocks: 1, Link: fmt.Sprintf("https://github.com/o/r/issues/%d#issuecomment-%d", i+1, i+1),
			Lines: []waitLine{{Text: strings.Repeat("y ", 350), Copied: true}}}
	}
	return items
}

// A list of over 70,000 characters is cut to fit: as many of the
// highest-ranked items as fit, then "…and N more", then the identity
// marker, and one item more wouldn't fit.
func TestDigestRenderBodyCut(t *testing.T) {
	t.Parallel()
	items := digestRenderBig(70)
	whole := 0
	for i, it := range items {
		whole += len(digestRenderItem(i+1, it))
	}
	if whole < 70000 {
		t.Fatalf("the list is %d characters, want over 70,000", whole)
	}
	body := digestRenderBody(digestRenderPinNote, items)
	digestRenderCheck(t, body)
	if digestRenderSize(body) > digestRenderLimit {
		t.Errorf("the body's size is %d, over %d", digestRenderSize(body), digestRenderLimit)
	}
	k := strings.Count(body, "\n### ")
	if k == 0 || k >= len(items) {
		t.Fatalf("the body has %d items, want some of %d", k, len(items))
	}
	for i := range k {
		if !strings.Contains(body, fmt.Sprintf("\n### %d. #%d: ", i+1, i+1)) {
			t.Errorf("item %d is missing, though it ranks above the cut", i+1)
		}
	}
	more := fmt.Sprintf("…and %d more, cut to fit GitHub's limit on an issue body.", len(items)-k)
	if !strings.HasSuffix(body, "\n\n"+more+"\n\n"+digestRenderIdentity) {
		t.Errorf("the body doesn't end with %q and the marker:\n%s", more, body[len(body)-300:])
	}
	if next := digestRenderSize(body) + 4 + digestRenderSize(digestRenderItem(k+1, items[k])); next <= digestRenderLimit {
		t.Errorf("item %d would fit too, at %d", k+1, next)
	}
	if !strings.HasPrefix(body, digestRenderPinNote+"\n\n70 items wait on you,") {
		t.Errorf("the body starts %q", body[:100])
	}
}

// digestRenderFit keeps every item when all of them fit, even if fewer
// with the "…and N more" line wouldn't, finds the last k that fits, right
// at the limit too, and gives 0 when nothing fits.
func TestDigestRenderFit(t *testing.T) {
	t.Parallel()
	const l = digestRenderLimit
	cases := []struct {
		name  string
		sizes []int // render(k)'s size, by k
		want  int
	}{
		{"all fit, the cut ones wouldn't", []int{l + 1, l + 1, l}, 2},
		{"none fits", []int{l + 1, l + 2, l + 3}, 0},
		{"exactly at the limit", []int{10, l - 5, l, l + 1, l + 2}, 2},
		{"one over the limit", []int{10, l + 1, l + 2, l + 3}, 0},
		{"all but the last", []int{10, 20, 30, l + 1}, 2},
		{"no items", []int{5}, 0},
	}
	for _, c := range cases {
		render := func(k int) string { return strings.Repeat("a", c.sizes[k]) }
		if got := digestRenderFit(len(c.sizes)-1, render); got != c.want {
			t.Errorf("%s: digestRenderFit = %d, want %d", c.name, got, c.want)
		}
	}
}

// The size counts UTF-8 bytes, and each newline twice.
func TestDigestRenderSize(t *testing.T) {
	t.Parallel()
	// GitHub's 65,536 characters, with SPEC §9.1's 1,024 to spare.
	if digestRenderLimit != 64512 {
		t.Errorf("digestRenderLimit = %d, want 64512", digestRenderLimit)
	}
	for s, want := range map[string]int{"": 0, "abc": 3, "é": 2, "👍": 4, "€": 3, "a\nb": 4, "\n\n": 4} {
		if got := digestRenderSize(s); got != want {
			t.Errorf("digestRenderSize(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestDigestRenderLead(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"": "", "plain": "plain", "# h": `\# h`, ">q": `\>q`, "-": `\-`, "+ x": `\+ x`, "* x": `\* x`, "=": `\=`, "```": "\\```",
		"~~~": `\~~~`, "~x~": "~x~", "`a` b": "`a` b", "``": "``", "___": `\___`, "|a|": `\|a|`, "[x]: y": `\[x]: y`, "1. x": `1\. x`, "123456789) x": `123456789\) x`,
		"1234567890. x": "1234567890. x", "12 x": "12 x", "1.5 x": `1\.5 x`, `\# x`: `\# x`, "a # b": "a # b", "&lt;!--": "&lt;!--",
	} {
		if got := digestRenderLead(in); got != want {
			t.Errorf("digestRenderLead(%q) = %q, want %q", in, got, want)
		}
	}
}

// The announcement lists only the items that get a mention (Q2a), in the
// order given, then the cc line, then the digest-new marker with their
// keys. With no notify, or no such item, there's nothing to post.
func TestDigestRenderNews(t *testing.T) {
	t.Parallel()
	items := digestRenderKinds(t)
	errs := []waitItem{digestRenderOf(t, items, waitError), {Issue: 7, Kind: waitError, Key: "err-7-attempts-clarifying-12",
		Title: "Second @eve", Link: "https://github.com/o/r/issues/7#issuecomment-701"}}
	all := append(slices.Clone(items), errs[1])
	got, err := digestRenderNews(all, []string{"owner", "other"})
	if err != nil {
		t.Fatal(err)
	}
	digestRenderGolden(t, "news", got)
	ms := github.ParseMarkers(got)
	if len(ms) != 1 || ms[0].Kind != "digest-new" || ms[0].Fields["items"] != "err-1-transition-50,err-7-attempts-clarifying-12" {
		t.Errorf("markers = %+v, want digest-new for the two errors", ms)
	}
	if !strings.HasSuffix(got, "\n\ncc @owner @other\n\n<!-- issuedog:digest-new items=err-1-transition-50,err-7-attempts-clarifying-12 -->") {
		t.Errorf("got:\n%s\nwant the cc line, then the marker, last", got)
	}
	if m := digestRenderMentionRE.FindAllString(got, -1); !slices.Equal(m, []string{"@o", "@o"}) {
		t.Errorf("mentions = %q, want only @owner and @other", m)
	}
	if again, err := digestRenderNews(all, []string{"owner", "other"}); err != nil || again != got {
		t.Errorf("the same input gave %q, %v", again, err)
	}
	var quiet []waitItem
	for _, it := range items {
		if it.Kind != waitError {
			quiet = append(quiet, it)
		}
	}
	for name, c := range map[string]struct {
		items  []waitItem
		notify []string
	}{"no notify": {errs, nil}, "no item": {nil, []string{"owner"}}, "only items that mention already": {quiet, []string{"owner"}}} {
		if got, err := digestRenderNews(c.items, c.notify); got != "" || err != nil {
			t.Errorf("%s: got %q, %v, want nothing to post", name, got, err)
		}
	}
}

// Which kinds get the digest's mention, and what each is called. A hold
// gets it only with a trigger no App comment mentioned (#192): one with
// none, as with no triggers at all, gets none.
func TestDigestRenderKinds(t *testing.T) {
	t.Parallel()
	for k, want := range map[waitKind]bool{waitError: true, waitHold: false, waitApproval: false, waitQuestion: false, waitFlag: false, waitKind(9): true} {
		if got := digestRenderMentions(waitItem{Kind: k}); got != want {
			t.Errorf("digestRenderMentions(%s) = %v, want %v", k, got, want)
		}
	}
	quiet := waitItem{Issue: 5, Kind: waitHold, Lines: []waitLine{{Hold: &holdItemTrigger{Trigger: discTrigger{Key: "c1"}, Mentioned: true}}}}
	if digestRenderMentions(quiet) {
		t.Errorf("a hold whose one trigger was mentioned gets the mention; want none")
	}
	loud := quiet
	loud.Lines = append(slices.Clone(quiet.Lines), waitLine{Hold: &holdItemTrigger{Trigger: discTrigger{Key: "c2"}}})
	if !digestRenderMentions(loud) {
		t.Errorf("a hold with a trigger no App comment mentioned gets no mention; want one")
	}
	for k, want := range map[waitKind]string{waitError: "State error", waitHold: "Discussion hold", waitApproval: "Approval", waitQuestion: "Question round",
		waitFlag: "Triage flag", waitKind(9): "Item (kind9)"} {
		if got := digestRenderKindName(k); got != want {
			t.Errorf("digestRenderKindName(%d) = %q, want %q", k, got, want)
		}
	}
	if got := waitKind(9).String(); got != "kind9" || waitHold.String() != "hold" {
		t.Errorf("kind names %q and %q; want kind9 and hold", got, waitHold.String())
	}
}

// Every key goes into the marker so that github.ParseMarkers reads it
// back and a split on "," gives it, as #159's digestState.comment does:
// today's keys as they are, and any other byte percent-encoded.
func TestDigestRenderKeys(t *testing.T) {
	t.Parallel()
	keys := map[string]string{
		"q-1-2": "q-1-2", "rev-2-0": "rev-2-0", "flag-3-401": "flag-3-401", "err-4-transition-50": "err-4-transition-50",
		"err-4-transition-clarifying-17-9f2a61c0": "err-4-transition-clarifying-17-9f2a61c0", "err-4-transition-r12": "err-4-transition-r12",
		"err-5-x-a,b": "err-5-x-a%2Cb", "err-5-x-50%": "err-5-x-50%25", "err-5-x-a b\tc": "err-5-x-a%20b%09c", "err-5-x-<a>": "err-5-x-%3Ca%3E",
		"err-5-x-é": "err-5-x-%C3%A9", "err-5-x-\x00\x7f": "err-5-x-%00%7F", "err-5-x-a=b/c@d-->": "err-5-x-a=b/c@d--%3E",
	}
	var items []waitItem
	var want []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	}) {
		if got := digestRenderKey(k); got != keys[k] {
			t.Errorf("digestRenderKey(%q) = %q, want %q", k, got, keys[k])
		}
		items = append(items, waitItem{Issue: 1, Kind: waitError, Key: k, Title: "T", Link: "https://github.com/o/r/issues/1"})
		want = append(want, keys[k])
	}
	news, err := digestRenderNews(items, []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	ms := github.ParseMarkers(news)
	if len(ms) != 1 || ms[0].Kind != "digest-new" {
		t.Fatalf("markers = %+v, want one digest-new", ms)
	}
	if got := strings.Split(ms[0].Fields["items"], ","); !slices.Equal(got, want) {
		t.Errorf("keys read back = %q, want %q", got, want)
	}
}

// digestRenderNew keeps the items whose key, as the marker holds it, no
// announcement has, in order.
func TestDigestRenderNew(t *testing.T) {
	t.Parallel()
	items := []waitItem{{Key: "err-1-x-a,b"}, {Key: "q-2-1"}, {Key: "err-3-labels-0"}, {Key: "rev-4-1"}}
	got := digestRenderNew(items, map[string]bool{"err-1-x-a%2Cb": true, "rev-4-1": true, "err-1-x-a,b": false})
	if keys := []string{got[0].Key, got[1].Key}; len(got) != 2 || !slices.Equal(keys, []string{"q-2-1", "err-3-labels-0"}) {
		t.Errorf("new = %+v, want q-2-1 and err-3-labels-0", got)
	}
	if got := digestRenderNew(items, nil); len(got) != 4 {
		t.Errorf("with nothing announced, got %d new, want 4", len(got))
	}
}

// An announcement over the limit lists, and its marker names, only the
// highest-ranked items that fit, and says how many more there are. One
// whose first item alone doesn't fit is an error.
func TestDigestRenderNewsCut(t *testing.T) {
	t.Parallel()
	items := digestRenderBig(400)
	for i := range items {
		items[i].Title = strings.Repeat("<b>", 70)
	}
	news, err := digestRenderNews(items, []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	if digestRenderSize(news) > digestRenderLimit {
		t.Errorf("the announcement's size is %d, over %d", digestRenderSize(news), digestRenderLimit)
	}
	k := strings.Count(news, "\n- **State error** on #")
	keys := strings.Split(github.ParseMarkers(news)[0].Fields["items"], ",")
	if k == 0 || k >= len(items) || len(keys) != k || keys[k-1] != items[k-1].Key {
		t.Fatalf("listed %d and marked %d of %d", k, len(keys), len(items))
	}
	if more := fmt.Sprintf("\n\n…and %d more, which I'll announce in the next comment.\n\ncc @owner\n\n", len(items)-k); !strings.Contains(news, more) {
		t.Errorf("the announcement has no %q", more)
	}
	huge := []waitItem{{Issue: 1, Kind: waitError, Key: "err-1-labels-0", Title: strings.Repeat("x", digestRenderLimit)}}
	if got, err := digestRenderNews(huge, []string{"owner"}); err == nil || got != "" || !strings.Contains(err.Error(), "err-1-labels-0") {
		t.Errorf("got %q, %v, want an error naming the item", got, err)
	}
	if got, err := digestRenderNews([]waitItem{{Kind: waitError}}, []string{"owner"}); err == nil || got != "" {
		t.Errorf("an item with no key gave %q, %v, want an error", got, err)
	}
}

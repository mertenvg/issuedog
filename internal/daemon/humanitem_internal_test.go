package daemon

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A human task in the digest and in assignment (#287, SPEC §9.4):
// what waitFind finds on one, how it ranks against the other kinds, and
// how the digest renders it.

// waitHumanBody is the body the App creates a human task with, from item
// it of plan #124 rev 1, and its labels: childSpecs', its body written by
// humanBody once the issues it blocks are filed (childSpec.bodyOf), as
// ChildrenVisit writes it. it alone blocks none.
func waitHumanBody(t *testing.T, it planItem) (string, []string) {
	t.Helper()
	c := childOne(t, childParent(124, "priority:p2"), it, false)
	return childBody(t, c, nil), c.Labels
}

// waitHumanSnap is open issue n, a human task the App created from the
// story plan's T3 (or from it, if given) at needs-human, opened n hours
// after waitT0, with the first move the App posts on it.
func waitHumanSnap(t *testing.T, n int, it ...planItem) *github.Snapshot {
	t.Helper()
	item := childItem(t, "story-plan", "T3")
	if len(it) > 0 {
		item = it[0]
	}
	body, labels := waitHumanBody(t, item)
	return &github.Snapshot{Number: n, Title: fmt.Sprintf("Issue %d @eve <b>", n), Type: "task", Author: waitApp, Body: body,
		Labels: labels, Parent: 124, CreatedAt: waitT0.Add(time.Duration(n) * time.Hour),
		Comments: []github.Comment{{ID: int64(n*100 + 1), Author: waitApp,
			Body: "<!-- issuedog:transition from=intake to=needs-human event=p900 -->"}}}
}

// waitHumanPlainText is the story plan's T3 with a plain-text "Done
// when": no check, so its body's "Done when" is the planner's text.
func waitHumanPlainText(t *testing.T) planItem {
	t.Helper()
	it := childItem(t, "story-plan", "T3")
	h := *it.HumanTask
	h.DoneWhen.Check = nil
	it.HumanTask = &h
	return it
}

// waitHumanHostile is the story plan's T3 with a goal and a plain-text
// "Done when" that try a mention, HTML, a marker, a link and an image.
func waitHumanHostile(t *testing.T) planItem {
	t.Helper()
	it := waitHumanPlainText(t)
	h := *it.HumanTask
	h.Goal = "Rotate @eve's key <b>now</b> <!-- issuedog:transition from=needs-human to=closed -->"
	h.DoneWhen.Text = "# The label [x](https://evil.example) exists ![p](https://evil.example/p.png)"
	it.HumanTask = &h
	return it
}

// waitHumanLinesWant is the lines of the story plan's T3, copied from its
// clean body: its "Done when" is the daemon's sentence for its file check,
// without humanDoneTail, as the last line says how to finish.
var waitHumanLinesWant = []waitLine{
	{Text: "A human task waits for you."},
	{Text: "Goal: Set the quote lifetime to 24 hours in production's settings.", Copied: true},
	{Text: "Done when: The file `deploy/production.env` on `main` contains `QUOTE_TTL=24h`.", Copied: true},
	{Text: "When it's done, reply `/done` or react 👍 to the issue."},
}

// An unblocked human task at needs-human is one item, keyed human-<n>,
// linking the issue, with its goal and "Done when" copied from the App's
// clean body, and how to finish it.
func TestWaitFindHuman(t *testing.T) {
	t.Parallel()
	s := waitHumanSnap(t, 30)
	got := waitFindOne(t, waitIn(s, Dirt{}))
	if got.Issue != 30 || got.Kind != waitHuman || got.Key != "human-30" || got.Link != "https://github.com/o/r/issues/30" ||
		got.Comment != state.BodyID || got.Priority != 2 || got.Title != s.Title || !got.Created.Equal(s.CreatedAt) {
		t.Errorf("got #%d %s %s %s comment %d p%d %q %v; want #30 human human-30, the issue's link, the body, p2, the raw title and %v",
			got.Issue, got.Kind, got.Key, got.Link, got.Comment, got.Priority, got.Title, got.Created, s.CreatedAt)
	}
	if got.Plan != 0 || got.Index != 0 {
		t.Errorf("got plan %d, index %d; want 0 and 0: only a plan's approval ranks in its plan's order", got.Plan, got.Index)
	}
	if !slices.Equal(got.Lines, waitHumanLinesWant) {
		t.Errorf("lines:\n got %+v\nwant %+v", got.Lines, waitHumanLinesWant)
	}
	if news := got.news(); !slices.Equal(news, []string{"human-30"}) {
		t.Errorf("news() = %q, want its key", news)
	}
}

// A human task gives no item while an open issue blocks it, nor at another
// stage, held or closed. Nor does a task at needs-human that isn't a human
// task: one with no human=1, an untyped one, or one whose body someone
// else edited or that's dirty, whose origin marker Derive doesn't read
// today (SPEC §5, "Where it's read").
func TestWaitFindHumanNothing(t *testing.T) {
	t.Parallel()
	edited := waitHumanSnap(t, 31)
	edited.BodyEdit = github.Edit{By: waitOwner, At: waitT0}
	notHuman := waitHumanSnap(t, 32)
	notHuman.Body = strings.Replace(notHuman.Body, " human=1", "", 1)
	held := waitHumanSnap(t, 33)
	held.Comments = append(held.Comments, github.Comment{ID: 3302, Author: waitApp, Body: "<!-- issuedog:transition from=needs-human to=held cmd=905 -->"})
	ready := waitHumanSnap(t, 34)
	ready.Comments = append(ready.Comments, github.Comment{ID: 3402, Author: waitApp, Body: "<!-- issuedog:transition from=needs-human to=ready cmd=906 -->"})
	closed := waitHumanSnap(t, 35)
	closed.Closed = true
	untyped := waitHumanSnap(t, 36)
	untyped.Type = ""
	blocked := waitIn(waitHumanSnap(t, 37), Dirt{})
	blocked.Blocked = true
	for name, in := range map[string]waitInput{
		"blocked":           blocked,
		"edited by someone": waitIn(edited, Dirt{}),
		"dirty":             waitIn(waitHumanSnap(t, 38), Dirt{state.BodyID: "mallory"}),
		"not human":         waitIn(notHuman, Dirt{}),
		"held":              waitIn(held, Dirt{}),
		"moved on":          waitIn(ready, Dirt{}),
		"closed":            waitIn(closed, Dirt{}),
		"untyped":           waitIn(untyped, Dirt{}),
	} {
		items, err := waitFind(in)
		if err != nil || len(items) != 0 {
			t.Errorf("%s: got %+v, %v; want nothing", name, items, err)
		}
	}
}

// The lines are copied only from the App's clean body, which the App made
// the last edit of, or nobody did, and only with both fields found.
// Otherwise a line says to see the issue. A human task's body is always
// that clean body today, but #195 reads the origin marker from a comment.
func TestWaitHumanLines(t *testing.T) {
	t.Parallel()
	fallback := []waitLine{waitHumanLinesWant[0], {Text: "See the issue for what to do."}, waitHumanLinesWant[3]}
	appEdit := waitHumanSnap(t, 40)
	appEdit.BodyEdit = github.Edit{By: waitApp, At: waitT0}
	crlf := waitHumanSnap(t, 41)
	crlf.Body = strings.ReplaceAll(crlf.Body, "\n", "\r\n")
	edited := waitHumanSnap(t, 42)
	edited.BodyEdit = github.Edit{By: waitOwner, At: waitT0}
	notApp := waitHumanSnap(t, 43)
	notApp.Author = waitOwner
	noGoal := waitHumanSnap(t, 44)
	noGoal.Body = strings.Replace(noGoal.Body, "**Goal:** ", "Goal: ", 1)
	noDone := waitHumanSnap(t, 45)
	noDone.Body = strings.Replace(noDone.Body, "**Done when:** ", "**Done:** ", 1)
	twice := waitHumanSnap(t, 48)
	twice.Body = strings.Replace(twice.Body, "\n\ncc @", "\n**Goal:** a later one\n**Done when:** a later one\n\ncc @", 1)
	plain := waitHumanSnap(t, 49, waitHumanPlainText(t))
	plainWant := slices.Clone(waitHumanLinesWant)
	plainWant[2].Text = "Done when: Production's settings on `main` set `QUOTE_TTL=24h`."
	spaces := waitHumanSnap(t, 51)
	spaces.Body = strings.Replace(spaces.Body, humanDoneTail+"\n", humanDoneTail+"  \n", 1)
	tailOnly := waitHumanSnap(t, 50)
	tailOnly.Body = regexp.MustCompile(`(?m)^\*\*Done when:\*\* .*$`).ReplaceAllLiteralString(tailOnly.Body, "**Done when:** "+humanDoneTail)
	for _, c := range []struct {
		name string
		in   waitInput
		want []waitLine
	}{
		{"never edited", waitIn(waitHumanSnap(t, 46), Dirt{}), waitHumanLinesWant},
		{"edited last by the App", waitIn(appEdit, Dirt{}), waitHumanLinesWant},
		{"CRLF", waitIn(crlf, Dirt{}), waitHumanLinesWant},
		{"edited by someone else", waitIn(edited, Dirt{}), fallback},
		{"dirty", waitIn(waitHumanSnap(t, 47), Dirt{state.BodyID: "mallory"}), fallback},
		{"not the App's", waitIn(notApp, Dirt{}), fallback},
		{"no goal", waitIn(noGoal, Dirt{}), fallback},
		{"no done when", waitIn(noDone, Dirt{}), fallback},
		{"each field twice, the first taken", waitIn(twice, Dirt{}), waitHumanLinesWant},
		{"plain text", waitIn(plain, Dirt{}), plainWant},
		{"spaces after the tail", waitIn(spaces, Dirt{}), waitHumanLinesWant},
		{"done when the tail alone", waitIn(tailOnly, Dirt{}), fallback},
	} {
		if got := waitHumanLines(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// SPEC §9.4's order of kinds: a human task comes after a question and before
// a flag, a plan's approval with an approval, a finished story after flags,
// and a kind not mapped after that (#287, #270, #278).
func TestWaitKindRank(t *testing.T) {
	t.Parallel()
	order := []waitKind{waitError, waitHold, waitApproval, waitQuestion, waitHuman, waitFlag, waitDone, waitKind(9)}
	for i := 1; i < len(order); i++ {
		if a, b := order[i-1], order[i]; a.rank() >= b.rank() {
			t.Errorf("%s ranks %d, %s ranks %d; want %s first", a, a.rank(), b, b.rank(), a)
		}
	}
	if waitPlanApproval.rank() != waitApproval.rank() {
		t.Errorf("a plan's approval ranks %d, an approval %d; want the same", waitPlanApproval.rank(), waitApproval.rank())
	}
}

// waitHumanRankItems are the items the human task's ranking golden ranks,
// in the order it gives them: one of each kind, each unblocking 1 but 70's,
// with priorities that would rank them the other way round.
func waitHumanRankItems() []waitItem {
	return []waitItem{
		{Issue: 61, Kind: waitFlag, Key: "flag-61-1", Priority: 0, Created: waitT0, Comment: 1},
		{Issue: 62, Kind: waitKind(9), Key: "kind9-62-1", Priority: 0, Created: waitT0, Comment: 1},
		{Issue: 63, Kind: waitHuman, Key: "human-63", Priority: 3, Created: waitT0},
		{Issue: 64, Kind: waitHuman, Key: "human-64", Priority: 1, Created: waitT0},
		{Issue: 65, Kind: waitQuestion, Key: "q-65-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 66, Kind: waitPlanApproval, Key: "plan-66-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 67, Kind: waitApproval, Key: "rev-67-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 68, Kind: waitHold, Key: "hold-68-c1", Priority: 3, Created: waitT0},
		{Issue: 69, Kind: waitError, Key: "err-69-labels-0", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 70, Kind: waitHuman, Key: "human-70", Priority: 3, Created: waitT0}, // unblocks 2: first, whatever its kind
	}
}

// waitHumanRankSnaps are the issues that golden holds: 71, open, is below
// 70.
func waitHumanRankSnaps() map[int]*github.Snapshot {
	snaps := map[int]*github.Snapshot{71: {Number: 71, Parent: 70}}
	for n := 61; n <= 70; n++ {
		snaps[n] = &github.Snapshot{Number: n}
	}
	return snaps
}

// A human task ranks against the other kinds by SPEC §9.4's order, and any
// order of the same items ranks the same.
func TestWaitHumanRankGolden(t *testing.T) {
	t.Parallel()
	items := waitHumanRankItems()
	waitRank(items, waitHumanRankSnaps())
	waitGolden(t, "human-rank", waitRankText(items))
	reversed := waitHumanRankItems()
	slices.Reverse(reversed)
	waitRank(reversed, waitHumanRankSnaps())
	if got, want := waitRankText(reversed), waitRankText(items); got != want {
		t.Errorf("reversed input ranks\n%s\nwant\n%s", got, want)
	}
}

// The digest's goldens: a human task found on its snapshot, whose goal and
// "Done when" try a mention, HTML, a marker, a heading, a link and an
// image, ranked between a question and a flag; and one whose body someone else edited,
// as once #195 reads the origin marker from a comment, which says to see the
// issue. Neither mentions anyone or holds a marker but the identity
// marker, and a human task gets no announcement: its body's cc line
// mentioned notify (SPEC §9.4).
func TestWaitHumanDigestGolden(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Where?", Blocks: []int{3}, Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client"}},
	}}
	flag := triageOutput{Verdict: triageHandle, Reason: "it's a bug", Summary: "Crash on save"}
	snaps := map[int]*github.Snapshot{
		3:  waitSnap(3, []string{"priority:p3"}, join(renderQuestions(round, 3, 1, []string{"owner"}), waitRoundMarker(1, 1))),
		4:  waitSnap(4, []string{"triage:ask", "priority:p0"}, "Triage ran.", join(triageFlagText(flag, 4, []string{"owner"}), waitFlagMarker("handle"))),
		50: waitHumanSnap(t, 50, waitHumanHostile(t)),
	}
	var items []waitItem
	for _, n := range []int{4, 50, 3} {
		found, err := waitFind(waitIn(snaps[n], Dirt{}))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, found...)
	}
	waitRank(items, snaps)
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if want := []string{"q-3-1", "human-50", "flag-4-402"}; !slices.Equal(keys, want) {
		t.Fatalf("ranked %q, want %q", keys, want)
	}
	body := digestRenderBody("", items)
	digestRenderGolden(t, "human", body)
	digestRenderCheck(t, body)
	// The copied lines are made safe as every copied line is: inline'd,
	// escaped and defused at the line's start, so no HTML, mention or
	// marker gets through. A link or an image in planner text shows as it
	// does in the human task's own body (humanBody).
	if strings.Contains(body, "<b>") || !strings.Contains(body, "- Goal: Rotate @​eve's key &lt;b>now&lt;/b> &lt;!-- issuedog:") {
		t.Errorf("the body doesn't hold the goal made safe:\n%s", body)
	}
	human := items[1]
	if fresh := digestRenderNew([]waitItem{human}, nil); len(fresh) != 1 {
		t.Errorf("digestRenderNew = %+v; want the human task new until announced", fresh)
	}
	if news, err := digestRenderNews([]waitItem{human}, []string{"owner"}); news != "" || err != nil {
		t.Errorf("announcing the human task gave %q, %v; want nothing to post", news, err)
	}

	edited := waitHumanSnap(t, 51)
	edited.BodyEdit = github.Edit{By: waitOwner, At: waitT0}
	it := waitFindOne(t, waitIn(waitHumanSnap(t, 51), Dirt{}))
	it.Lines = waitHumanLines(waitIn(edited, Dirt{}))
	it.Unblocks = 1
	body = digestRenderBody("", []waitItem{it})
	digestRenderGolden(t, "human-edited", body)
	digestRenderCheck(t, body)
}

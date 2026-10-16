package daemon

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// What waits on the owner (SPEC §9.4, §9.6; plan #7, assumptions 5, 6 and
// 12, and Q3a). It's two pure functions, which read only what they're
// given and do no I/O:
//   - waitFind lists one issue's items. The digest (T8, #160) calls it on
//     each issue right after step 1 succeeds on it, with the snapshot as
//     step 1 left it, the Inputs it returned and the issue's Dirt, and
//     caches the items by issue. An issue whose step 1 fails keeps the
//     items it had.
//   - waitRank orders every issue's items, once per pass: it counts each
//     item's open descendants from the parents in all the snapshots held.
//
// Assignment (§9.6, #87) can take the same items.
//
// Which text is whose (assumption 12): an item's Title is raw, and T6
// escapes it. No line is raw: each is the App's, either this file's own
// words or copied from the App's own clean comment (waitLine.Copied).

// waitKind is a kind of item. Its order is Q3a's tie-break after the
// unblock count: state errors, then approvals, then questions, then flags.
type waitKind int

const (
	waitError waitKind = iota
	waitApproval
	waitQuestion
	waitFlag
)

func (k waitKind) String() string {
	switch k {
	case waitError:
		return "error"
	case waitApproval:
		return "approval"
	case waitQuestion:
		return "question"
	case waitFlag:
		return "flag"
	}
	return "kind" + strconv.Itoa(int(k))
}

// waitItem is one thing that waits on the owner.
type waitItem struct {
	Issue int
	Kind  waitKind
	// Key names the item across passes and restarts (assumption 5), so T8
	// can tell a new one: q-<n>-<round>, rev-<n>-<rev> (rev-<n>-0 when
	// there's no revision yet), flag-<n>-<comment id> or
	// err-<n>-<kind>-<key>.
	Key string
	// Title is the issue's title. It's raw: anyone may have written it, so
	// T6 runs it through inline and github.Escape.
	Title string
	// Lines say what's being decided, the options with the recommended one
	// first and marked, and how to answer. None is raw (waitLine).
	Lines []waitLine
	// Link is where to answer: the comment that asks, or the issue itself
	// when that's in the body (waitLink).
	Link string
	// Priority is the issue's priority:pN, 0 to 3, or 2 with none.
	Priority int
	// Unblocks is 1 plus the issue's open descendants. waitRank sets it,
	// and waitFind leaves it 0.
	Unblocks int
	// Created is when the issue was opened, for Q3a's last tie-break, the
	// oldest issue. Comment is the ID of the comment Link names, or
	// state.BodyID, which orders two items of one kind on one issue.
	Created time.Time
	Comment int64
}

// waitLine is one line of an item. Its Text is the App's, never raw:
//   - Copied unset: this file's own words, with only numbers, stage names,
//     hashes and the kind and key of an App marker in them, none of which
//     holds a space or an angle bracket (github.FormatMarker).
//   - Copied set: a line copied from the App's own clean comment, as
//     posted. The App wrote it, but agent text in it was only made safe
//     for that comment (inline, github.Escape), and a state error's text
//     may quote a label or an error, so T6 defuses and escapes it again
//     (assumption 12).
type waitLine struct {
	Text   string
	Copied bool
	// Question is a question line's number, from 1, or 0 for any other
	// line. Its Text is the question alone, without the number, the bold
	// or what it blocks.
	Question int
	// Option is an option line's letter, "a" to "d", under the question
	// line before it, or "". Its Text is what follows "a. ".
	Option string
	// Recommended marks the recommended option, which is always "a". Its
	// Text still says "(recommended)", and why, as the comment does.
	Recommended bool
}

// waitInput is what waitFind reads of one issue.
type waitInput struct {
	Owner, Repo, AppSlug string
	// S is the snapshot as step 1 left it, not a fresh read: a fresh one has
	// no SettingsEvent, which would end every labels error (#155's verify).
	S *github.Snapshot
	// Dirt is S's dirty content (Applier.Dirty).
	Dirt Dirt
	// Inputs are what step 1 returned on S (Step.Run).
	Inputs []github.Comment
}

// waitFind returns what on in.S waits on the owner (assumption 5), in no
// set order (waitRank orders them). A closed issue has nothing. Otherwise:
//   - a state error for each open one (openErrors), linked to its comment;
//   - at spec-review, an approval: of the latest revision, or, with none,
//     of no revision. One that can't be approved yet, for a dirty body, a
//     Hidden revision, a body that no longer has its hash or a type with
//     no approval to planning, says why;
//   - at clarifying, a question round: when the latest transition is an
//     interviewer round with round= and questions= above 0, and no
//     trusted answer has come since (no Inputs). A typed bug or task's
//     move carries neither, so it waits for phase 3 with no item;
//   - at intake with no stage label, a flag: when the latest triage
//     marker has verdict=ask, as FlagByReaction answers it.
//
// A round's or flag's questions and options are copied from its comment
// (waitQuestions). Its error is openErrors', which with Inputs given has
// none to give today.
func waitFind(in waitInput) ([]waitItem, error) {
	s := in.S
	if s.Closed {
		return nil, nil
	}
	is := state.Derive(s, in.AppSlug, in.Dirt.Has)
	var items []waitItem
	add := func(kind waitKind, key string, comment int64, lines []waitLine) {
		items = append(items, waitItem{Issue: s.Number, Kind: kind, Key: fmt.Sprintf("%s-%d-%s", waitKeyPrefix[kind], s.Number, key),
			Title: s.Title, Lines: lines, Link: waitLink(in.Owner, in.Repo, s.Number, comment),
			Priority: priority(s), Created: s.CreatedAt, Comment: comment})
	}
	open, err := openErrors(s, is, func() ([]github.Comment, error) { return in.Inputs, nil })
	if err != nil {
		return nil, fmt.Errorf("what waits on %s/%s#%d: %w", in.Owner, in.Repo, s.Number, err)
	}
	for _, e := range open {
		add(waitError, e.Kind+"-"+e.Key, e.Comment, waitErrorLines(s, e))
	}
	switch {
	case is.Stage == state.SpecReview:
		rev, comment, lines := waitApprovalLines(in, is)
		add(waitApproval, strconv.Itoa(rev), comment, lines)
	case is.Stage == state.Clarifying && len(in.Inputs) == 0:
		m := ivLatestMove(is)
		round, rerr := strconv.Atoi(m.Fields["round"])
		n, nerr := strconv.Atoi(m.Fields["questions"])
		if !ivRoundMove(m) || rerr != nil || nerr != nil || round < 1 || n < 1 {
			break
		}
		lines := []waitLine{{Text: fmt.Sprintf("Round %d asks %s, and waits for an answer.", round, waitCount(n, "question"))}}
		heading := fmt.Sprintf("### Questions for #%d (round %d)", s.Number, round)
		lines = append(lines, waitQuestions(waitBody(s, is.TransitionComment), heading, n,
			"I couldn't read them back from the round comment, so see it there.")...)
		lines = append(lines, waitLine{Text: "Reply in one comment, e.g. `1a 2c 3: free text`."})
		add(waitQuestion, strconv.Itoa(round), is.TransitionComment, lines)
	case is.Stage == state.Intake && stageOf(s) == "" && is.Triage.Verdict == triageAsk:
		lines := []waitLine{{Text: "Triage flagged this issue: should issuedog handle it?"}}
		heading := fmt.Sprintf("### Should issuedog handle #%d?", s.Number)
		lines = append(lines, waitQuestions(waitBody(s, is.Triage.Comment), heading, 1,
			"I couldn't read the question back from the flag, so see it there.")...)
		lines = append(lines, waitLine{Text: "React 👍 to the flag to take the recommendation, or post `/track` or `/ignore`."})
		add(waitFlag, strconv.FormatInt(is.Triage.Comment, 10), is.Triage.Comment, lines)
	}
	return items, nil
}

// waitKeyPrefix starts each kind's key.
var waitKeyPrefix = map[waitKind]string{waitError: "err", waitApproval: "rev", waitQuestion: "q", waitFlag: "flag"}

// waitLink is the link to comment id on issue n, or to the issue itself
// when id is state.BodyID: an error in the body (#155's verify).
func waitLink(owner, repo string, n int, id int64) string {
	link := fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
	if id == state.BodyID {
		return link
	}
	return link + "#issuecomment-" + strconv.FormatInt(id, 10)
}

// waitBody returns the body of s's comment id, or "" if s has none, as
// for state.BodyID.
func waitBody(s *github.Snapshot, id int64) string {
	for _, c := range s.Comments {
		if c.ID == id && id != state.BodyID {
			return c.Body
		}
	}
	return ""
}

// waitCount is "1 <noun>" or "<n> <noun>s".
func waitCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// waitErrorLines says what the open state error e on s is: its kind and
// key, the lines of its comment's text, and how to acknowledge it. An
// error in the body only gets no text from it, since the App never posts
// one there, and a 👍 can't acknowledge it (ErrorByReaction).
func waitErrorLines(s *github.Snapshot, e openErr) []waitLine {
	lines := []waitLine{{Text: fmt.Sprintf("State error `%s`, keyed `%s`.", e.Kind, e.Key)}}
	if e.Comment == state.BodyID {
		return append(lines, waitLine{Text: "It's in the issue description, where a 👍 can't acknowledge it, so it ends when its cause is gone."})
	}
	// The App escaped every "<!--" it didn't write, so a line that starts
	// with one is its own marker.
	for _, l := range strings.Split(strings.ReplaceAll(waitBody(s, e.Comment), "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "<!--") {
			lines = append(lines, waitLine{Text: l, Copied: true})
		}
	}
	return append(lines, waitLine{Text: "React 👍 to the error comment to acknowledge it."})
}

// waitApprovalLines returns the spec's approval at spec-review: the latest
// revision's number, or 0 with none, the comment to link (the revision's,
// or else the transition's), and its lines. The reasons it can't be
// approved yet are ApproveCommand's and approve's, in their order, without
// the login of who made the body dirty, which isn't the App's text, and,
// for a type that can't be at spec-review, Transition's hold-back.
func waitApprovalLines(in waitInput, is state.Issue) (int, int64, []waitLine) {
	s := in.S
	latest, ok := state.LatestRevision(s, in.AppSlug, in.Dirt.Has)
	lines := []waitLine{{Text: "The spec waits for review, with no revision to approve yet."}}
	comment := is.TransitionComment
	var why []string
	if ok {
		lines[0].Text = fmt.Sprintf("Revision %d of the spec, `%s`, waits for approval.", latest.Rev, latest.Hash)
		comment = latest.Comment
	}
	if in.Dirt.Has(state.BodyID) {
		why = append(why, "the issue description has an edit that wasn't approved (see the note on it)")
	}
	switch {
	case !ok:
	case latest.Hidden:
		why = append(why, fmt.Sprintf("a comment of mine after revision %d was edited without approval or deleted, so it may hide a newer revision", latest.Rev))
	case state.BodyHash(s.Body) != latest.Hash:
		why = append(why, fmt.Sprintf("the issue description changed after revision %d, so it no longer matches `%s`", latest.Rev, latest.Hash))
	}
	// A type change that stands is the cause (typeErrStands): Transition
	// holds the 👍's move back, and the type error, listed beside this,
	// says what to do (#156's verify).
	if typeErrStands(s, is) {
		why = append(why, "its type doesn't fit `spec-review`, so a 👍 is held back until the type changes (see the type error)")
	} else if class, err := state.ClassOf(is.Item); err != nil {
		why = append(why, "I can't tell what kind of issue this is from its type")
	} else if _, err := state.Lookup(state.SpecReview, state.Planning, class, state.TriggerSpecApproved); err != nil {
		why = append(why, "an issue of its type doesn't go on to `planning` on approval")
	}
	for _, w := range why {
		lines = append(lines, waitLine{Text: "It can't be approved yet: " + w + "."})
	}
	if ok && len(why) == 0 {
		lines = append(lines, waitLine{Text: "React 👍 to the revision comment, or post `/approve`, to approve it."})
	}
	return latest.Rev, comment, append(lines, waitLine{Text: "Post `/revise <notes>`, or any other comment, to send it back."})
}

// The lines of a question in the SPEC §9.1 format, as renderQuestions and
// triageFlagText write them. The question's text, inline'd, is all of its
// line up to the last "** Blocks: " that only a list of issues follows.
var (
	waitQuestionRE = regexp.MustCompile(`^\*\*([1-9][0-9]*)\. (.*)\*\* Blocks: #[0-9]+(?:, #[0-9]+)*$`)
	waitOptionRE   = regexp.MustCompile(`^   ([a-d])\. (.*)$`)
)

// waitQuestions returns the n questions under heading in body, the App's
// round or flag comment, each with its options in order, copied
// (waitLine.Copied). They follow heading, with blank lines between them,
// up to the first line that's neither: each numbered in turn, with 2 to 4
// options lettered in turn, of which the first, and only the first, must
// be the recommended one. Anything else, such as a format that has
// changed, gives one line of this file's own instead: missing.
func waitQuestions(body, heading string, n int, missing string) []waitLine {
	fallback := []waitLine{{Text: missing}}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	at := slices.Index(lines, heading)
	if at < 0 {
		return fallback
	}
	var out []waitLine
	questions, options := 0, 0 // options is the current question's
	for _, l := range lines[at+1:] {
		if q := waitQuestionRE.FindStringSubmatch(l); q != nil {
			if questions > 0 && options < 2 || q[1] != strconv.Itoa(questions+1) {
				return fallback
			}
			questions, options = questions+1, 0
			out = append(out, waitLine{Text: q[2], Copied: true, Question: questions})
			continue
		}
		if o := waitOptionRE.FindStringSubmatch(l); o != nil {
			rec := options == 0
			if questions == 0 || o[1] != string(rune('a'+options)) || rec && !strings.Contains(o[2], " (recommended): ") {
				return fallback
			}
			options++
			out = append(out, waitLine{Text: o[2], Copied: true, Option: o[1], Recommended: rec})
			continue
		}
		if l != "" {
			break
		}
	}
	if questions != n || options < 2 {
		return fallback
	}
	return out
}

// waitRank sets each item's Unblocks, 1 plus the open issues below its
// issue, and sorts items in place by Q3a: the most unblocked first, then
// by kind (waitKind), then by priority, p0 first, then the oldest issue,
// and last by issue number, the comment linked and the key, so the order
// is total and the same input always gives the same order. Descendants
// are counted from the parents in snaps, every issue the daemon holds, by
// number. The poll drops a closed issue's snapshot, so the snapshots hold
// only open issues, and the walk never reaches a closed issue or what's
// below it: an open issue below a closed one doesn't count toward the
// closed one's ancestors (SPEC §9.4, "The order"). A snapshot read as its
// issue closed is closed till the next poll drops it: it's walked
// through, but not counted.
//
// Each issue has one parent, so the walk down from an issue meets each
// issue below it once, unless the parents loop back to the issue itself,
// which GitHub shouldn't allow: the walk stops there.
func waitRank(items []waitItem, snaps map[int]*github.Snapshot) {
	children := map[int][]int{}
	for n, s := range snaps {
		if p := s.Parent; p != 0 {
			children[p] = append(children[p], n)
		}
	}
	count := func(n int) int {
		todo, open := []int{n}, 0
		for len(todo) > 0 {
			m := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			for _, c := range children[m] {
				if c == n {
					continue
				}
				todo = append(todo, c)
				if !snaps[c].Closed {
					open++
				}
			}
		}
		return open
	}
	for i := range items {
		items[i].Unblocks = 1 + count(items[i].Issue)
	}
	slices.SortFunc(items, func(a, b waitItem) int {
		return cmp.Or(cmp.Compare(b.Unblocks, a.Unblocks), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Priority, b.Priority),
			a.Created.Compare(b.Created), cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Comment, b.Comment), cmp.Compare(a.Key, b.Key))
	})
}

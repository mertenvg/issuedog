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

// What waits on the owner (SPEC §9.4, §9.6): which items there are, what
// each says, whose text is whose, and their order. It's two pure
// functions, which read only what they're given and do no I/O:
//   - waitFind lists one issue's items. Iterate calls it on each issue
//     right after step 1 succeeds on it (digestCacheSet, #160), with
//     the snapshot as step 1 left it, the Inputs it returned, the
//     issue's Dirt and, if it's held, its open discussion triggers
//     (Step.holdItemFind, #192), and caches the items by issue, whether
//     or not the digest is on. An issue whose step 1 fails keeps the
//     items it had.
//   - waitRank orders every issue's items, once per pass: it counts each
//     item's open descendants from the parents in all the snapshots held.
//
// Assignment (§9.6, #191) takes the same items, from the same cache, and
// assigns for any kind (assign.go), a hold among them (#192). A finished
// story's item (#278) is the one both keep only while it stands, which
// they judge on each pass from what the poll listed
// (Daemon.digestDoneStands).
//
// Which text is whose (SPEC §9.4): an item's Title is raw, and the
// digest's renderer (digestRenderTitle) escapes it. No line's Text is raw:
// each is the App's, either this file's own words or copied from the App's
// own clean comment (waitLine.Copied). A hold's trigger line
// (waitLine.Hold) has no Text: its trigger's logins and title are raw, and
// digestRenderHold escapes them.

// waitKind is a kind of item. The order's tie-break after the unblock
// count (SPEC §9.4, The order) goes by waitKind.rank: state errors, then
// discussion holds (#192), then approvals, then questions, then human
// tasks (#287), then flags, then finished stories (#278). A plan's
// approval (#270) ranks as an approval: it's a kind of its own only for
// its key and its name.
type waitKind int

const (
	waitError waitKind = iota
	waitHold
	waitApproval
	waitQuestion
	waitFlag
	waitPlanApproval
	waitHuman
	waitDone
)

// rank is k's place in SPEC §9.4's order of kinds, lowest first: a plan's
// approval is an approval's, a human task's comes after a question's and
// before a flag's (#287), a flag's after that, and a finished story's
// after every other kind's (#278), as story verification unblocks no
// other issue. Any other kind ranks by its own value, so a kind appended
// later, unless it's mapped here, ranks after finished stories: each
// after waitDone is above waitDone's rank.
func (k waitKind) rank() int {
	switch k {
	case waitPlanApproval:
		return int(waitApproval)
	case waitHuman:
		return int(waitQuestion) + 1
	case waitFlag:
		return int(waitQuestion) + 2
	case waitDone:
		return int(waitQuestion) + 3
	}
	return int(k)
}

func (k waitKind) String() string {
	switch k {
	case waitError:
		return "error"
	case waitHold:
		return "hold"
	case waitApproval:
		return "approval"
	case waitQuestion:
		return "question"
	case waitFlag:
		return "flag"
	case waitPlanApproval:
		return "plan-approval"
	case waitHuman:
		return "human"
	case waitDone:
		return "done"
	}
	return "kind" + strconv.Itoa(int(k))
}

// waitItem is one thing that waits on the owner.
type waitItem struct {
	Issue int
	Kind  waitKind
	// Key names the item across passes and restarts (SPEC §9.4), so the
	// digest can tell a new one (digestRenderNew): q-<n>-<round>,
	// rev-<n>-<rev> (rev-<n>-0 when there's no revision yet),
	// plan-<n>-<rev> for a plan's (plan-<n>-0 with no clean plan revision,
	// #270), flag-<n>-<comment id>,
	// err-<n>-<kind>-<key>, hold-<n>-<hold marker id> (holdItemOf),
	// human-<n> for a human task's (#287) or done-<n> for a finished
	// story's (#278), of each of which an issue has one at most. A hold is
	// announced by other keys than this one (waitItem.news).
	Key string
	// Title is the issue's title. It's raw: anyone may have written it, so
	// the digest's renderer runs it through inline and github.Escape.
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
	// Created is when the issue was opened, for SPEC §9.4's last
	// tie-break, the oldest issue. Comment is the ID of the comment Link
	// names, or state.BodyID, which orders two items of one kind on one
	// issue.
	Created time.Time
	Comment int64
	// Plan and Index place a plan's approval in the plan its issue was
	// filed from (#270, SPEC §9.4, Plan order): Plan is that plan's
	// issue number and Index the item's number in it, as 2 for S2, both
	// from the issue's origin marker (state.Issue.Origin, waitPlanOrder).
	// waitRank lists the approvals of one plan together, in item order.
	// They're 0 for every other item, and for an issue with no origin
	// marker, such as an epic, which ranks alone.
	Plan, Index int
	// Children are the children a finished story's record names (waitDone,
	// #278), in its order, and nil for every other item. waitFind doesn't
	// know whether they're closed, so it finds the item whatever they are,
	// and the digest and assignment keep it only while every one is listed
	// closed and no open issue is left below the story
	// (Daemon.digestDoneStands).
	Children []int
	// Unassigned marks an open closer state error posted while its issue
	// was closed (closerErrPostedClosed): the digest lists it, but it
	// assigns nobody (assign.go). It's unset for every other item.
	Unassigned bool
}

// waitLine is one line of an item. Its Text is the App's, never raw:
//   - Copied unset: this file's own words, with only numbers, stage names,
//     hashes and the kind and key of an App marker in them, none of which
//     holds a space or an angle bracket (github.FormatMarker).
//   - Copied set: a line copied from the App's own clean comment, as
//     posted. The App wrote it, but agent text in it was only made safe
//     for that comment (inline, github.Escape), and a state error's text
//     may quote a label or an error, so the digest's renderer
//     (digestRenderText) defuses and escapes it again (SPEC §9.4).
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
	// Hold is a hold item's open trigger, or nil for any other line. Its
	// Text is "": digestRenderHold renders the trigger, whose By, Of and
	// Title are raw, and escapes them.
	Hold *holdItemTrigger
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
	// CloserErrEnded, if set, reports whether the closer state error keyed
	// key on S has ended: the question of its close was answered while the
	// daemon runs, or step 1 no longer asks it (Applier.closerErrEnded).
	// Unset, none ends so.
	CloserErrEnded func(key string) bool
	// Holds are the open discussion triggers on S after its visit, if S is
	// held, and nil otherwise (Step.holdItemFind).
	Holds []discTrigger
	// OwnerHeld is set when S, held with Holds open, also has an owner hold
	// that stays once they're resolved, as resolveLeave judges it
	// (Applier.resolveHoldsLeft, Step.holdItemFind).
	OwnerHeld bool
	// Blocked is set when S is a human task at needs-human (waitHumanTask)
	// that an open issue blocks, as its visit read them (Daemon.humanBlocked,
	// #287), and unset for any other issue.
	Blocked bool
}

// waitFind returns what on in.S waits on the owner (SPEC §9.4), in no
// set order (waitRank orders them). A closed issue has nothing. Otherwise:
//   - a state error for each open one (openErrors), linked to its comment;
//   - while held, a discussion hold, when in.Holds has an open trigger
//     (holdItemOf). An issue held only by owner holds has none: a /hold,
//     or a stage:held set by hand, is the owner's own, and waits on
//     nobody else (SPEC §9.6 lists discussion holds alone);
//   - at spec-review, an approval: of the latest revision, or, with none,
//     of no revision. One that can't be approved yet, for a dirty body, a
//     Hidden revision, a body that no longer has its hash or a type with
//     no approval to planning, says why;
//   - at clarifying, a question round: when the latest transition, looking
//     through a hold the issue went back from (ivHoldLatest), is an
//     interviewer round with round= and questions= above 0, and no
//     trusted answer has come since (no Inputs). A typed bug or task's
//     move carries neither, so it waits for phase 3 with no item;
//   - at planning, a question round in the same way, when that latest
//     transition is a planner round (planRoundMove, #253). Its key and
//     rank are an interviewer round's, as the rounds share one numbering;
//   - at plan-review, a plan's approval (#270): of the latest plan
//     revision, or, with no clean one, of none. One that can't be approved
//     yet says why, a reason a line (waitPlanApprovalLines). It's placed
//     in its plan's order (waitPlanOrder);
//   - at needs-human, a human task (#287, SPEC §9.6's row), when it is
//     one (waitHumanTask) and no open issue blocks it (in.Blocked). It
//     links the issue, whose body says what to do (waitHumanLines);
//   - at planned, a finished story (#278), when the record of its approved
//     plan revision's children stands (waitDoneChildren), whether or not
//     they're closed: the digest and assignment judge that on each pass
//     (Daemon.digestDoneStands). It links the issue;
//   - at intake with no stage label, a flag: when the latest triage
//     marker has verdict=ask, as FlagByReaction answers it.
//
// A round's or flag's questions and options are copied from its comment
// (waitQuestions). Its error is openErrors', which with Inputs given has
// none to give today, or a finished epic's plan data that doesn't read
// back (waitDoneChildren).
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
	open, err := openErrors(s, is, func() ([]github.Comment, error) { return in.Inputs, nil }, in.CloserErrEnded)
	if err != nil {
		return nil, fmt.Errorf("what waits on %s/%s#%d: %w", in.Owner, in.Repo, s.Number, err)
	}
	for _, e := range open {
		add(waitError, e.Kind+"-"+e.Key, e.Comment, waitErrorLines(s, e))
		items[len(items)-1].Unassigned = closerErrPostedClosed(s, e)
	}
	switch {
	case is.Stage == state.Held && len(in.Holds) > 0:
		id, lines := holdItemOf(in, is)
		add(waitHold, id, state.BodyID, lines)
	case is.Stage == state.SpecReview:
		rev, comment, lines := waitApprovalLines(in, is)
		add(waitApproval, strconv.Itoa(rev), comment, lines)
	case (is.Stage == state.Clarifying || is.Stage == state.Planning) && len(in.Inputs) == 0:
		// Through a hold the issue went back from (#185's verify note):
		// after /resume back to clarifying or planning, the round still
		// waits. The round is the stage's own: the interviewer's at
		// clarifying, the planner's at planning (#253).
		isRound := ivRoundMove
		if is.Stage == state.Planning {
			isRound = planRoundMove
		}
		m := ivHoldLatest(is, true)
		round, rerr := strconv.Atoi(m.Fields["round"])
		n, nerr := strconv.Atoi(m.Fields["questions"])
		if !isRound(m) || rerr != nil || nerr != nil || round < 1 || n < 1 {
			break
		}
		comment := is.TransitionComment
		for _, r := range questionRounds(s, in.AppSlug, in.Dirt.Has, isRound) {
			if r.Number == round {
				comment = r.Comment
			}
		}
		lines := []waitLine{{Text: fmt.Sprintf("Round %d asks %s, and waits for an answer.", round, waitCount(n, "question"))}}
		heading := fmt.Sprintf("### Questions for #%d (round %d)", s.Number, round)
		lines = append(lines, waitQuestions(waitBody(s, comment), heading, n,
			"I couldn't read them back from the round comment, so see it there.")...)
		lines = append(lines, waitLine{Text: "Reply in one comment, e.g. `1a 2c 3: free text`."})
		add(waitQuestion, strconv.Itoa(round), comment, lines)
	case is.Stage == state.PlanReview:
		rev, comment, lines := waitPlanApprovalLines(in, is)
		add(waitPlanApproval, strconv.Itoa(rev), comment, lines)
		items[len(items)-1].Plan, items[len(items)-1].Index = waitPlanOrder(is.Origin)
	case waitHumanTask(is) && !in.Blocked:
		// An issue has one human task at most: its key is human-<n>.
		add(waitHuman, "", state.BodyID, waitHumanLines(in))
		items[len(items)-1].Key = fmt.Sprintf("%s-%d", waitKeyPrefix[waitHuman], s.Number)
	case is.Stage == state.Planned:
		children, err := waitDoneChildren(in, is)
		if err != nil {
			return nil, fmt.Errorf("what waits on %s/%s#%d: %w", in.Owner, in.Repo, s.Number, err)
		}
		if children == nil {
			break
		}
		// An issue is one story at most: its key is done-<n>.
		add(waitDone, "", state.BodyID, waitDoneLines(children))
		items[len(items)-1].Key = fmt.Sprintf("%s-%d", waitKeyPrefix[waitDone], s.Number)
		items[len(items)-1].Children = children
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
var waitKeyPrefix = map[waitKind]string{waitError: "err", waitHold: "hold", waitApproval: "rev", waitQuestion: "q", waitFlag: "flag",
	waitPlanApproval: "plan", waitHuman: "human", waitDone: "done"}

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
// spec revision's number, or 0 with none, the comment to link (the revision's,
// or else the transition's), and its lines. The reasons it can't be
// approved yet are ApproveCommand's and approve's, in their order, without
// the login of who made the body dirty, which isn't the App's text, and,
// for a type that can't be at spec-review, Transition's hold-back.
func waitApprovalLines(in waitInput, is state.Issue) (int, int64, []waitLine) {
	s := in.S
	latest, ok := state.LatestRevision(s, in.AppSlug, state.SpecRevision, in.Dirt.Has)
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
	why = append(why, waitTypeWhy(s, is, state.Planning, state.TriggerSpecApproved)...)
	for _, w := range why {
		lines = append(lines, waitLine{Text: "It can't be approved yet: " + w + "."})
	}
	if ok && len(why) == 0 {
		lines = append(lines, waitLine{Text: "React 👍 to the revision comment, or post `/approve`, to approve it."})
	}
	return latest.Rev, comment, append(lines, waitLine{Text: "Post `/revise <notes>`, or any other comment, to send it back."})
}

// waitTypeWhy says why is's type keeps an approval at its stage, a review
// stage, from moving it on to next on trigger, or nothing. A type change
// that stands is the cause (typeErrStands): Transition holds the 👍's move
// back, and the type error, listed beside the approval, says what to do
// (#156's verify). Else the type may not say what kind of issue it is, or
// its kind may have no move on to next.
func waitTypeWhy(s *github.Snapshot, is state.Issue, next state.Stage, trigger state.Trigger) []string {
	if typeErrStands(s, is) {
		return []string{fmt.Sprintf("its type doesn't fit `%s`, so a 👍 is held back until the type changes (see the type error)", is.Stage)}
	}
	class, err := state.ClassOf(is.Item)
	if err != nil {
		return []string{"I can't tell what kind of issue this is from its type"}
	}
	if _, err := state.Lookup(is.Stage, next, class, trigger); err != nil {
		return []string{fmt.Sprintf("an issue of its type doesn't go on to `%s` on approval", next)}
	}
	return nil
}

// waitPlanApprovalLines returns the plan's approval at plan-review (#270):
// the latest plan revision's number, or 0 with no clean one, the comment
// to link (the plan comment, or else the transition's), and its lines.
// The reasons it can't be approved yet are state.PlanApproval's, in its
// order, then the type's, as at spec-review (waitTypeWhy). A plan that
// can't be approved as it stands, with no clean revision, a hidden one or
// one whose comment changed, needs a new plan, which only a planner run
// posts: the daemon doesn't repost one (SPEC §7.4). So the last line
// says that sending it back gets one.
func waitPlanApprovalLines(in waitInput, is state.Issue) (int, int64, []waitLine) {
	s := in.S
	latest, reasons := state.PlanApproval(s, in.AppSlug, in.Dirt.Has)
	lines := []waitLine{{Text: "The plan waits for review, with no clean revision to approve."}}
	comment := is.TransitionComment
	if latest.Comment != 0 {
		lines[0].Text = fmt.Sprintf("Revision %d of the plan, `%s`, waits for approval.", latest.Rev, latest.Hash)
		comment = latest.Comment
	}
	var why []string
	renew := false // only a new plan can be approved
	for _, r := range reasons {
		switch r {
		case state.PlanNoRevision:
			// The only plan comment may have been made dirty or deleted,
			// not just never posted (#267's verify).
			why = append(why, "there's no clean plan revision to approve: none was posted, or it was edited without approval or deleted")
			renew = true
		case state.PlanHidden:
			why = append(why, fmt.Sprintf("a comment of mine after plan revision %d was edited without approval or deleted, so it may hide a newer revision", latest.Rev))
			renew = true
		case state.PlanChanged:
			why = append(why, fmt.Sprintf("the comment of plan revision %d changed after it was posted, so it no longer matches `%s`", latest.Rev, latest.Hash))
			renew = true
		case state.PlanBodyDirty:
			why = append(why, "the issue description has an edit that wasn't approved (see the note on it)")
		}
	}
	why = append(why, waitTypeWhy(s, is, state.Planned, state.TriggerPlanApproved)...)
	for _, w := range why {
		lines = append(lines, waitLine{Text: "It can't be approved yet: " + w + "."})
	}
	if len(why) == 0 {
		lines = append(lines, waitLine{Text: "React 👍 to the plan comment, or post `/approve`, to approve it."})
	}
	back := "Post `/revise <notes>`, or any other comment, to send it back."
	if renew {
		back = "Post `/revise <notes>`, or any other comment, to send it back and get a new plan."
	}
	return latest.Rev, comment, append(lines, waitLine{Text: back})
}

// waitHumanTask reports whether is is a human task at needs-human (SPEC
// §9.3): a task from a plan whose clean origin marker has human=1
// (state.ClassHumanTask). A task at needs-human after a hand-off, which
// phase 3 adds, isn't one.
func waitHumanTask(is state.Issue) bool {
	class, err := state.ClassOf(is.Item)
	return is.Stage == state.NeedsHuman && err == nil && class == state.ClassHumanTask
}

// waitHumanLines returns a human task's lines (#287): that it waits, then
// its Goal and "Done when", copied from its body (waitLine.Copied), and
// last how to finish it. They're copied only from the App's clean body:
// the App's, not dirty, and edited last by the App or by nobody. That's
// the body Derive reads the origin marker from, so today a human task's
// body is always one, but once the marker can be read from a comment
// (SPEC §5, Where it's read; #195), a body someone else edited isn't the
// App's text. Then, or with either field not found, as for a format that
// has changed, a line says to see the issue instead. Each field is the
// first line that starts with its bold name, as humanBody writes them
// (#284): no line above "Done when" starts so, as each field is one line,
// steps are numbered and links are bullets. A check's "Done when" is
// copied without humanDoneTail, which says to react on "this issue": in
// the digest that's the digest, and the last line says how to finish.
func waitHumanLines(in waitInput) []waitLine {
	s := in.S
	lines := []waitLine{{Text: "A human task waits for you."}}
	var goal, done string
	if e := s.BodyEdit; state.IsApp(s.Author, in.AppSlug) && !in.Dirt.Has(state.BodyID) && (e.At.IsZero() || state.IsApp(e.By, in.AppSlug)) {
		for _, l := range strings.Split(strings.ReplaceAll(s.Body, "\r\n", "\n"), "\n") {
			if v, ok := strings.CutPrefix(l, "**Goal:** "); ok && goal == "" {
				goal = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(l, "**Done when:** "); ok && done == "" {
				done = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), humanDoneTail))
			}
		}
	}
	if goal != "" && done != "" {
		lines = append(lines, waitLine{Text: "Goal: " + goal, Copied: true}, waitLine{Text: "Done when: " + done, Copied: true})
	} else {
		lines = append(lines, waitLine{Text: "See the issue for what to do."})
	}
	return append(lines, waitLine{Text: "When it's done, reply `/done` or react 👍 to the issue."})
}

// waitDoneChildren returns the children of in.S, an issue at planned
// with state is, for its finished story's item (#278, SPEC §9.4, Finished
// story): those the clean record of its approved plan revision's children
// names (state.CreatedRecord, #277), in its order. That's for a story, or
// an epic whose approved plan is a plan of tasks, which acts as its own
// story (SPEC §4). It's nil for an epic planned into stories, an issue of
// any other type, or one with no approved plan revision or no record of it
// yet, as while its children are still being created. Only an epic's plan
// data is read back, and data that doesn't read back is an error. The
// reading is epicCloseChildren's, which an epic planned into stories
// closes by (#251).
func waitDoneChildren(in waitInput, is state.Issue) ([]int, error) {
	children, stories, err := epicCloseChildren(in.S, in.AppSlug, in.Dirt.Has, is)
	if err != nil {
		return nil, fmt.Errorf("a finished epic's plan: %w", err)
	}
	if stories {
		return nil, nil
	}
	return children, nil
}

// waitDoneLines are a finished story's lines (#278): that it waits for
// story verification, then the children its record names, or that its
// record names none, as after a revision that drops every child.
func waitDoneLines(children []int) []waitLine {
	refs := make([]string, len(children))
	for i, c := range children {
		refs[i] = "#" + strconv.Itoa(c)
	}
	those := "Its children: " + strings.Join(refs, ", ") + "."
	if len(children) == 0 { // a revision left none (state.CreatedNone, #297)
		those = "Its plan's latest revision left it no children."
	}
	return []waitLine{
		{Text: "Every child of this story is closed, so it waits for story verification, which runs by hand for now. Close it once that passes."},
		{Text: those},
	}
}

// waitPlanItemRE is a plan item's ID, as the planner's schema has it: S
// for a story or T for a task, then its number in the plan, from 1.
var waitPlanItemRE = regexp.MustCompile(`^[ST]([1-9][0-9]{0,8})$`)

// waitPlanOrder returns the plan and item index of a plan's approval on
// an issue with origin o (waitItem.Plan, waitItem.Index): o's plan, and
// its item's number. With no origin, or an item ID that isn't a plan
// item's, it's 0 and 0, and the item ranks alone.
func waitPlanOrder(o state.Origin) (int, int) {
	m := waitPlanItemRE.FindStringSubmatch(o.Item)
	if o.Plan == 0 || m == nil {
		return 0, 0
	}
	// At most 9 digits, so it always fits.
	index, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0
	}
	return o.Plan, index
}

// holdItemTrigger is one open discussion trigger on a hold item's issue
// (waitLine.Hold). Its Trigger's By, Of and Title are raw, and
// digestRenderHold escapes them. Link and How are the App's.
type holdItemTrigger struct {
	Trigger discTrigger
	// Link is where to see the trigger and resolve it: its notice, if a
	// clean one names it; else the comment it is; else the comment that
	// records it (discRecords), as for a trigger known from its record
	// alone (discTrigger.FromRecord); else the content it edited; else the
	// issue.
	Link string
	// How says how a code owner resolves it (holdItemOf).
	How string
	// Mentioned is set when an App comment that mentions notify names it
	// (SPEC §9.4, Announcing new items): a clean App marker records its
	// key (discRecords), the hold's transition or a notice, in a comment
	// with a cc line (holdItemCCRE), as the hold's comment and each notice
	// end in one (SPEC §6.4). A comment trigger on an issue held already
	// has no record, as a discussion hold on a held issue writes no hold
	// marker (SPEC §6.4), nor has a trigger whose notice discLost held
	// back on an issue held already, nor one whose record is in content
	// made dirty or deleted since, so none of those is mentioned. The
	// trigger whose hold moved the issue into held is mentioned, notice or
	// not: the hold's transition marker records it, as event=, in a
	// comment with a cc line. Nor is one recorded while notify was empty
	// mentioned, whose comment has no cc line: that mentioned nobody.
	Mentioned bool
}

// holdItemCCRE finds a cc line that mentions someone, as ccLines writes
// it: "cc @" at the start of a line, then a login. Text the App didn't
// write can't make one: it's inline'd onto the line of the App's own words
// (discWhat), and a mention in it is defused, with a zero-width space
// after its "@".
var holdItemCCRE = regexp.MustCompile(`(?m)^cc @[^\s\x{200b}]`)

// holdItemOf returns the hold item of in.S, which is held with in.Holds
// open, oldest first (#192): the id its key ends in, and its lines.
//
// The key is hold-<n>-<hold marker id>, where the id names the transition
// marker that moved the issue into held, whatever hold it was
// (holdItemMarkerID): the first hold wins (SPEC §6.4), so that marker
// stands while the issue is held, and a discussion hold on an issue held
// already writes none. So the key stays the same from pass to pass, and
// while triggers come and go under one hold, as when the oldest is
// resolved or a comment trigger is deleted, and a hold after a move out of
// held has a key of its own, as its marker has a key of its own (SPEC
// §5.2). Only a marker with no key, which the App doesn't write, falls
// back to the oldest open trigger's key, which changes as that trigger
// goes. Which triggers to announce isn't the key's job: see waitItem.news.
//
// The lines say the issue is held for discussion, from where, and how
// many triggers are open; then one line for each (waitLine.Hold), with a
// link and how it's resolved; then, with an owner hold that stays
// (in.OwnerHeld), that the issue stays held after them until that ends;
// and last, that /resume accepts every trigger. How each is resolved
// follows SPEC §6.4's notices and its rules for resolving a hold, as #188
// resolves them (resolve.go):
//   - a comment: a code owner's 👍 on it approves it, and their later
//     comment counters it, but a /hold or a /resume, which counter nothing
//     (resolver.comment). One known from its record alone is gone, so only
//     a counter is left;
//   - any other kind resolveByNotice allows: a code owner's 👍 on its
//     notice, the clean App comment with its notice marker, which may be
//     the hold's own;
//   - one that edited or deleted what may have been the App's content
//     (resolveHitsApp), notice or not, or one with no clean notice,
//     deleted, made dirty or held back (discLost): only /resume on the
//     issue.
func holdItemOf(in waitInput, is state.Issue) (string, []waitLine) {
	s := in.S
	id := in.Holds[0].Key
	if hs := is.HeldStack.Holds; len(hs) > 0 && holdItemMarkerID(hs[0]) != "" {
		id = holdItemMarkerID(hs[0])
	}
	records := discRecords(s, in.AppSlug, in.Dirt.Has)
	period := resolvePeriodOf(s, in.AppSlug, in.Dirt.Has)
	notices := map[string]int64{}
	discMarkers(s, in.AppSlug, in.Dirt.Has, func(c int64, _ time.Time, m github.Marker) {
		if k := m.Fields["key"]; m.Kind == "notice" && k != "" {
			if _, seen := notices[k]; !seen {
				notices[k] = c
			}
		}
	})
	link := func(c int64) string { return waitLink(in.Owner, in.Repo, s.Number, c) }
	from := ""
	if at := is.HeldStack.At; at.Open() && at != state.Held {
		from = fmt.Sprintf(", from `%s`", at)
	}
	lines := []waitLine{{Text: fmt.Sprintf("On hold for discussion%s, with %s that only a code owner can resolve.", from,
		waitCount(len(in.Holds), "open trigger"))}}
	for _, tr := range in.Holds {
		h := &holdItemTrigger{Trigger: tr}
		rec, recorded := records[tr.Key]
		h.Mentioned = recorded && holdItemCCRE.MatchString(waitBody(s, rec))
		notice, noticed := notices[tr.Key]
		switch {
		case tr.Kind == discComment && !tr.FromRecord:
			h.Link, h.How = link(tr.ID), "A code owner's 👍 on the comment approves it, and any later comment of theirs here, other than `/hold` or `/resume`, counters it."
		case tr.Kind == discComment:
			h.Link, h.How = link(rec), "A later comment by a code owner here, other than `/hold` or `/resume`, counters it."
		case noticed && resolveByNotice(tr, s, in.AppSlug, period.at):
			h.Link, h.How = link(notice), "A code owner's 👍 on its notice accepts it."
		default:
			h.Link, h.How = link(state.BodyID), "It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it."
			switch {
			case resolveHitsApp(tr, s, in.AppSlug, period.at):
				h.How = "It edited or deleted what may have been the App's record or a code owner's `/hold`, so only `/resume` here accepts it, not a 👍 on a notice."
			case noticed:
				h.How = "Only `/resume` here accepts it."
			}
			switch {
			case noticed:
				h.Link = link(notice)
			case recorded:
				h.Link = link(rec)
			case tr.Kind == discEdit || tr.Kind == discOwnEdit:
				h.Link = link(tr.ID)
			}
		}
		lines = append(lines, waitLine{Hold: h})
	}
	// An owner hold that stays: in.OwnerHeld, from what only a request can
	// tell, or one a marker records that no /resume ended (resolveLeft).
	if in.OwnerHeld || resolveOwnerHeld(resolveLeft(is.HeldStack.Holds, period, 0)) {
		lines = append(lines, waitLine{Text: "It's on an owner hold too, so it stays held after these until `/resume` ends that."})
	}
	return id, append(lines, waitLine{Text: "Post `/resume` here to accept every open trigger at once."})
}

// holdItemMarkerID names m, the transition marker that moved an issue into
// held, by the key that makes it unique (SPEC §5.2), or "" for none:
//   - event= as it is: a discussion hold's trigger key (SPEC §6.4, Keys
//     and records), such as c<comment id>, or a hand-set hold's label event ID;
//   - else cmd= after "cmd": a /hold's comment, or r<reaction id>, as
//     cmd502, which no trigger key looks like;
//   - else run= after "run".
func holdItemMarkerID(m github.Marker) string {
	switch {
	case m.Fields["event"] != "":
		return m.Fields["event"]
	case m.Fields["cmd"] != "":
		return "cmd" + m.Fields["cmd"]
	case m.Fields["run"] != "":
		return "run" + m.Fields["run"]
	}
	return ""
}

// news returns the keys that announce it (digestRenderNew): its Key, or,
// for a hold, hold-<n>-<trigger key> for each open trigger that no App
// comment mentioning notify names (holdItemTrigger.Mentioned), which may
// be none (SPEC §9.4). Each such trigger is a discussion hold of its own
// that nothing else mentioned, as a hold on a held issue writes no marker
// (SPEC §6.4, discHeldStack), so each is announced once, whatever order
// they're found in, and one resolved doesn't make the rest new again. A
// hold whose triggers are all mentioned is never new.
func (it waitItem) news() []string {
	if it.Kind != waitHold {
		return []string{it.Key}
	}
	var keys []string
	for _, l := range it.Lines {
		if l.Hold != nil && !l.Hold.Mentioned {
			keys = append(keys, fmt.Sprintf("%s-%d-%s", waitKeyPrefix[waitHold], it.Issue, l.Hold.Trigger.Key))
		}
	}
	return keys
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
// issue, and sorts items in place by SPEC §9.4's order: the most unblocked
// first, then by kind (waitKind), then by priority, p0 first, then the
// oldest issue, and last by issue number, the comment linked and the key,
// so the order is total and the same input always gives the same order.
// Descendants are counted from the parents in snaps, every issue the
// daemon holds, by number. The poll drops a closed issue's snapshot, so
// the snapshots hold only open issues, and the walk never reaches a closed
// issue or what's below it: an open issue below a closed one doesn't count
// toward the closed one's ancestors (SPEC §9.4, "The order"). A snapshot
// read as its issue closed is closed till the next poll drops it: it's
// walked through, but not counted.
//
// Each issue has one parent, so the walk down from an issue meets each
// issue below it once, unless the parents loop back to the issue itself,
// which GitHub shouldn't allow: the walk stops there.
//
// A plan's approvals with the same Plan form a group (#270, SPEC §9.4,
// Plan order): the stories one plan filed are listed together, in the
// plan's item order (Index), whatever their priorities. The group sits
// where its best member would rank on its own by the rules above, and
// within it, members go by Index, then by those rules, so the order stays
// total. Every item outside a group ranks as it did, and an approval with
// no Plan, such as an epic's, ranks on its own.
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
	// SPEC §9.4's order of two items on their own.
	q3a := func(a, b waitItem) int {
		return cmp.Or(cmp.Compare(b.Unblocks, a.Unblocks), cmp.Compare(a.Kind.rank(), b.Kind.rank()), cmp.Compare(a.Priority, b.Priority),
			a.Created.Compare(b.Created), cmp.Compare(a.Issue, b.Issue), cmp.Compare(a.Comment, b.Comment), cmp.Compare(a.Key, b.Key))
	}
	best := map[int]waitItem{} // each group's best member, by Plan
	for _, it := range items {
		if b, ok := best[it.Plan]; it.Plan != 0 && (!ok || q3a(it, b) < 0) {
			best[it.Plan] = it
		}
	}
	// place is where it ranks: as its group's best member, or as itself.
	place := func(it waitItem) waitItem {
		if it.Plan != 0 {
			return best[it.Plan]
		}
		return it
	}
	slices.SortFunc(items, func(a, b waitItem) int {
		return cmp.Or(q3a(place(a), place(b)), cmp.Compare(a.Plan, b.Plan), cmp.Compare(a.Index, b.Index), q3a(a, b))
	})
}

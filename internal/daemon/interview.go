package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// The interviewer's outcomes.
const (
	outcomeQuestions = "questions"
	outcomeSpec      = "spec"
	outcomeTyped     = "typed"
	// outcomeUnchanged is only for the clarifying after a discussion hold
	// (#189, SPEC §6.4): nothing in the discussion changes the issue, so it
	// goes back to its origin.
	outcomeUnchanged = "unchanged"
	// The interviewer's plan outcome is outcomePlan, the planner's name
	// for a plan, as outcomeQuestions is both stages' question round. It's
	// only for the clarifying after a discussion hold from planning,
	// plan-review or planned (ivPlanOrigin), on an epic or a story (#294):
	// the spec stands, but its plan changes, so the issue goes to
	// planning, with the message as the planner's notes.
)

// interviewOutput is the interviewer's structured output, as
// internal/agent/schemas/interviewer.json gives it. It's one flat object,
// and checkInterview matches its parts to its outcome.
type interviewOutput struct {
	Outcome   string              `json:"outcome"`
	Type      *interviewType      `json:"type,omitempty"`
	Message   string              `json:"message,omitempty"`
	Questions []interviewQuestion `json:"questions,omitempty"`
	Spec      *interviewSpec      `json:"spec,omitempty"`
}

// interviewType is the type the interviewer sets, and why.
type interviewType struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// interviewQuestion is one question of a round. Its options are
// Recommended, then Others.
type interviewQuestion struct {
	Text        string          `json:"text"`
	Blocks      []int           `json:"blocks"`
	Recommended interviewOption `json:"recommended"`
	Others      []string        `json:"others"`
}

// interviewOption is a question's recommended option.
type interviewOption struct {
	Text string `json:"text"`
	Why  string `json:"why"`
}

// interviewSpec is the spec's sections (SPEC §11.1).
type interviewSpec struct {
	Problem            string              `json:"problem"`
	Goals              []string            `json:"goals"`
	OutOfScope         []string            `json:"out_of_scope"`
	UsersAndFlows      string              `json:"users_and_flows"`
	Constraints        []string            `json:"constraints"`
	AcceptanceCriteria []string            `json:"acceptance_criteria"`
	Decisions          []interviewDecision `json:"decisions"`
}

// interviewDecision is one Decisions line. Answer is a–d or free for an
// answer, superseded (Summary is the later question's ID) or dropped
// (Summary is why), or "" for a decision made without asking, whose
// Question is "". By and Round are "" and 0 when nobody answered.
type interviewDecision struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Summary  string `json:"summary"`
	By       string `json:"by"`
	Round    int    `json:"round"`
}

// decodeInterview decodes a run's output. A field the types don't have is
// an error.
func decodeInterview(raw json.RawMessage) (interviewOutput, error) {
	var out interviewOutput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return interviewOutput{}, fmt.Errorf("decode interviewer output: %w", err)
	}
	return out, nil
}

// interviewFacts is what checkInterview checks an output against.
type interviewFacts struct {
	Issue        int
	MaxQuestions int  // loop.max_questions_per_round
	MayType      bool // the interviewer may set the type (SPEC §4)
	Untyped      bool // the issue has no type, so a spec must set one
	// Rounds is the question count of each round so far: Rounds[0] is
	// round 1's. A round is asked as round len(Rounds)+1.
	Rounds []int
	// Trusted is the logins with trusted comments on the issue, the only
	// ones a decision may name.
	Trusted []string
	Notify  []string // config's notify, for the cc line
	// Origin is the stage the issue was at before its first discussion
	// hold, while it's at the clarifying after one (ivHoldOrigin), or "".
	// Only then may the interviewer return unchanged, and not when it's
	// clarifying itself: the issue is there already. Only after a hold
	// from planning, plan-review or planned may it return plan.
	Origin state.Stage
	// Class is the issue's class (state.ClassOf), or 0 if it has none the
	// table knows. Only an epic or a story may get plan, and a story with
	// a parent (ClassStory) gets no spec and no typed outcome (SPEC §4,
	// §9.1): its body is its parent's plan item.
	Class state.Class
	// Options is the option count of each question asked whose round
	// comment reads back (ivWordingOptions), so a decision's letter must
	// name one of its options (#229). A question missing from it may be
	// answered a to d, as its comment may not read back.
	Options map[questionID]int
}

// The rules checkInterview enforces. Each failure names one.
const (
	ruleOutcome            = "outcome"
	ruleQuestionCount      = "question-count"
	ruleBlocks             = "blocks"
	ruleOptions            = "options"
	ruleOneLine            = "one-line"
	ruleTypeAllowed        = "type-allowed"
	ruleTypeValue          = "type-value"
	ruleSpecType           = "spec-type"
	ruleCriteria           = "criteria"
	ruleDecisionQuestion   = "decision-question"
	ruleDecisionAnswer     = "decision-answer"
	ruleDecisionBy         = "decision-by"
	ruleDecisionRound      = "decision-round"
	ruleDecisionSuperseded = "decision-superseded"
	ruleDecisionDropped    = "decision-dropped"
	ruleDecisionCoverage   = "decision-coverage"
	ruleLength             = "length"
	ruleUnchanged          = "unchanged"
	rulePlan               = "plan"
	ruleParentStory        = "parent-story"
)

// ruleError is an output that breaks a rule.
type ruleError struct {
	Rule, Detail string
}

func (e *ruleError) Error() string { return "rule " + e.Rule + ": " + e.Detail }

// maxText is GitHub's limit on a comment or body, in characters. textRoom
// is kept free of it for the markers, the revision text and a dirty note
// that go with the rendered text.
const (
	maxText  = 65536
	textRoom = 1024
)

// checkInterview checks out against f after the schema (SPEC §9.1's
// Checks, and §11.1's Decisions rules). Its error joins a *ruleError for every rule
// broken. Nothing may be written from an output that fails.
func checkInterview(out interviewOutput, f interviewFacts) error {
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &ruleError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
	}
	switch out.Outcome {
	case outcomeQuestions:
		if out.Spec != nil || out.Message != "" || len(out.Questions) == 0 {
			fail(ruleOutcome, "a question round has questions, and a type at most")
		}
		checkQuestions(out.Questions, f, fail)
	case outcomeSpec:
		if out.Spec == nil || len(out.Questions) > 0 {
			fail(ruleOutcome, "a spec has a spec, and a type and a message at most")
		}
		if out.Spec != nil {
			checkSpec(*out.Spec, f, fail)
		}
		if f.Untyped && out.Type == nil {
			fail(ruleSpecType, "a spec on an issue with no type sets it to epic or story")
		}
	case outcomeTyped:
		if out.Type == nil || out.Spec != nil || out.Message != "" || len(out.Questions) > 0 {
			fail(ruleOutcome, "typed has a type and nothing else")
		}
	case outcomeUnchanged:
		if out.Type != nil || out.Spec != nil || len(out.Questions) > 0 {
			fail(ruleOutcome, "unchanged has a message at most")
		}
		if f.Origin == "" || f.Origin == state.Clarifying {
			fail(ruleUnchanged, "unchanged is only for the clarifying after a discussion hold from another stage")
		}
	case outcomePlan:
		if out.Type != nil || out.Spec != nil || len(out.Questions) > 0 || strings.TrimSpace(out.Message) == "" {
			fail(ruleOutcome, "plan has a message and nothing else")
		}
		if !ivPlanOrigin(f.Origin) || f.Class&(state.ClassEpic|state.AnyStory) == 0 {
			fail(rulePlan, "plan is only for an epic or a story at the clarifying after a discussion hold from planning, plan-review or planned")
		}
	default:
		fail(ruleOutcome, "unknown outcome %q", out.Outcome)
	}
	if f.Class == state.ClassStory && (out.Outcome == outcomeSpec || out.Outcome == outcomeTyped) {
		fail(ruleParentStory, "a story with a parent gets only questions, unchanged or plan, never %s", out.Outcome)
	}
	if t := out.Type; t != nil {
		if !f.MayType {
			fail(ruleTypeAllowed, "the interviewer may not set this issue's type")
		}
		if !oneLine(t.Reason) {
			fail(ruleOneLine, "the type's reason")
		}
		if bugOrTask := t.Value == "bug" || t.Value == "task"; out.Outcome == outcomeTyped && !bugOrTask {
			fail(ruleTypeValue, "typed sets bug or task, not %q", t.Value)
		} else if out.Outcome != outcomeTyped && t.Value != "epic" && t.Value != "story" {
			fail(ruleTypeValue, "a %s output sets epic or story, not %q", out.Outcome, t.Value)
		}
	}
	for _, t := range renderedTexts(out, f) {
		if n := textLen(t.text); n > maxText-textRoom {
			fail(ruleLength, "the %s is %d characters, over %d", t.what, n, maxText-textRoom)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("check interviewer output: %w", errors.Join(errs...))
}

func checkQuestions(qs []interviewQuestion, f interviewFacts, fail func(string, string, ...any)) {
	if len(qs) > f.MaxQuestions {
		fail(ruleQuestionCount, "%d questions, over the limit of %d", len(qs), f.MaxQuestions)
	}
	for i, q := range qs {
		at := "question " + strconv.Itoa(i+1)
		if len(q.Blocks) == 0 {
			fail(ruleBlocks, "%s blocks nothing", at)
		}
		if len(q.Others) == 0 || len(q.Others) > 3 {
			fail(ruleOptions, "%s has %d options, not 2 to 4", at, len(q.Others)+1)
		}
		ivWordingDuplicates(at, append([]string{q.Recommended.Text}, q.Others...), fail)
		for _, s := range append([]string{q.Text, q.Recommended.Text, q.Recommended.Why}, q.Others...) {
			if !oneLine(s) {
				fail(ruleOneLine, "%s: %q", at, s)
			}
		}
	}
}

// ivWordingDuplicates fails rule options for each of a question's options,
// opts, in order, the recommended one first, that is the same as an
// earlier one (#229): the same text once case is ignored, leading and
// trailing space is dropped and each run of spaces is one space. at names
// the question.
func ivWordingDuplicates(at string, opts []string, fail func(string, string, ...any)) {
	for j, o := range opts {
		for k := range j {
			if strings.EqualFold(strings.Join(strings.Fields(opts[k]), " "), strings.Join(strings.Fields(o), " ")) {
				fail(ruleOptions, "%s: option %c is the same as option %c, %q", at, rune('a'+j), rune('a'+k), opts[k])
				break
			}
		}
	}
}

func checkSpec(s interviewSpec, f interviewFacts, fail func(string, string, ...any)) {
	if len(s.AcceptanceCriteria) == 0 {
		fail(ruleCriteria, "a spec has at least one acceptance criterion")
	}
	for _, list := range [][]string{s.Goals, s.OutOfScope, s.Constraints, s.AcceptanceCriteria} {
		for _, item := range list {
			if !oneLine(item) {
				fail(ruleOneLine, "list item %q", item)
			}
		}
	}
	answered := map[questionID]bool{}
	for i, d := range s.Decisions {
		at := "decision " + strconv.Itoa(i+1)
		if d.Question == "" {
			if d.Answer != "" || d.By != "" || d.Round != 0 {
				fail(ruleDecisionAnswer, "%s: a decision made without asking has no answer, by or round", at)
			}
			if !oneLine(d.Summary) {
				fail(ruleOneLine, "%s: %q", at, d.Summary)
			}
			continue
		}
		q, ok := parseQuestionID(d.Question)
		if !ok || !q.askedBy(f.Rounds) {
			fail(ruleDecisionQuestion, "%s: %s wasn't asked", at, d.Question)
			continue
		}
		answered[q] = true
		switch d.Answer {
		case "a", "b", "c", "d", "free":
			if n, ok := f.Options[q]; ok && d.Answer != "free" && int(d.Answer[0]-'a') >= n {
				fail(ruleDecisionAnswer, "%s: %s had %d options, a to %c, so it has no option %q", at, d.Question, n, rune('a'+n-1), d.Answer)
			}
			if !oneLine(d.Summary) {
				fail(ruleOneLine, "%s: %q", at, d.Summary)
			}
			if !slices.ContainsFunc(f.Trusted, func(l string) bool { return strings.EqualFold(l, d.By) }) {
				fail(ruleDecisionBy, "%s: %q has no trusted comment on the issue", at, d.By)
			}
			if d.Round < q.round || d.Round > len(f.Rounds) {
				fail(ruleDecisionRound, "%s: round %d isn't from %d to %d", at, d.Round, q.round, len(f.Rounds))
			}
			continue
		case "superseded":
			if later, ok := parseQuestionID(d.Summary); !ok || !later.askedBy(f.Rounds) || later.round <= q.round {
				fail(ruleDecisionSuperseded, "%s: %s isn't superseded by %q, a question asked in a later round", at, d.Question, d.Summary)
			}
		case "dropped":
			if strings.TrimSpace(d.Summary) == "" {
				fail(ruleDecisionDropped, "%s: %s is dropped with no reason", at, d.Question)
			} else if !oneLine(d.Summary) {
				fail(ruleOneLine, "%s: %q", at, d.Summary)
			}
		default:
			fail(ruleDecisionAnswer, "%s: unknown answer %q", at, d.Answer)
			continue
		}
		if d.By != "" || d.Round != 0 {
			fail(ruleDecisionAnswer, "%s: a %s question has no by or round", at, d.Answer)
		}
	}
	var missing []string
	for r, n := range f.Rounds {
		for i := 1; i <= n; i++ {
			if q := (questionID{r + 1, i}); !answered[q] {
				missing = append(missing, q.String())
			}
		}
	}
	if len(missing) > 0 {
		fail(ruleDecisionCoverage, "%s asked but not in decisions", strings.Join(missing, ", "))
	}
}

// rendered is a text an output renders to, and what it is.
type rendered struct{ what, text string }

// renderedTexts is what out renders to under f.
func renderedTexts(out interviewOutput, f interviewFacts) []rendered {
	if out.Outcome == outcomeUnchanged {
		return []rendered{{"comment", renderUnchanged(out, f.Origin, "", f.Notify)}}
	}
	if out.Outcome == outcomePlan {
		return []rendered{{"comment", renderPlanChange(out, f.Notify)}}
	}
	if out.Outcome != outcomeSpec {
		return []rendered{{"comment", renderQuestions(out, f.Issue, len(f.Rounds)+1, f.Notify)}}
	}
	texts := []rendered{{"revision comment", renderSpecNote(out, f.Notify)}}
	if out.Spec != nil {
		texts = append(texts, rendered{"body", renderSpec(*out.Spec)})
	}
	return texts
}

// textLen is text's length in UTF-16 code units, which is never less than
// its length in characters, however GitHub counts them.
func textLen(text string) int {
	n := 0
	for _, r := range text {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}

// oneLine reports whether s is one line that isn't blank.
func oneLine(s string) bool {
	return !strings.ContainsAny(s, "\r\n") && strings.TrimSpace(s) != ""
}

// questionID is the ID Q<round>.<n> of a round's nth question.
type questionID struct{ round, n int }

var questionIDRE = regexp.MustCompile(`^Q([1-9][0-9]*)\.([1-9][0-9]*)$`)

func parseQuestionID(s string) (questionID, bool) {
	m := questionIDRE.FindStringSubmatch(s)
	if m == nil {
		return questionID{}, false
	}
	r, err := strconv.Atoi(m[1])
	if err != nil {
		return questionID{}, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return questionID{}, false
	}
	return questionID{r, n}, true
}

func (q questionID) askedBy(rounds []int) bool {
	return q.round <= len(rounds) && q.n <= rounds[q.round-1]
}

func (q questionID) String() string { return fmt.Sprintf("Q%d.%d", q.round, q.n) }

// renderQuestions renders a question round of out, as round round on issue
// (SPEC §9.1): the type line first when out sets a type, then the
// questions with the recommended option first, then the cc line for
// notify (SPEC §9). A typed output renders as the type
// line and the cc line. A question's text goes inside the App's bold, so
// it's escaped for it first (ivWordingStrong).
func renderQuestions(out interviewOutput, issue, round int, notify []string) string {
	var parts []string
	if t := out.Type; t != nil {
		parts = append(parts, renderTypeLine(*t))
	}
	if len(out.Questions) > 0 {
		lines := []string{fmt.Sprintf("### Questions for #%d (round %d)", issue, round)}
		for i, q := range out.Questions {
			blocks := make([]string, len(q.Blocks))
			for j, b := range q.Blocks {
				blocks[j] = "#" + strconv.Itoa(b)
			}
			lines = append(lines, "", fmt.Sprintf("**%d. %s** Blocks: %s", i+1, inline(ivWordingStrong(q.Text)), strings.Join(blocks, ", ")),
				"   a. "+inline(q.Recommended.Text)+" (recommended): "+inline(q.Recommended.Why))
			for j, o := range q.Others {
				lines = append(lines, "   "+string(rune('b'+j))+". "+inline(o))
			}
		}
		parts = append(parts, strings.Join(append(lines, "", "Reply in one comment, e.g. `1a 2c 3: free text`."), "\n"))
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n"))
}

// renderSpec renders s as the issue body, in the SPEC §11.1 template.
func renderSpec(s interviewSpec) string {
	decisions := make([]string, len(s.Decisions))
	for i, d := range s.Decisions {
		q, summary := inline(d.Question), inline(d.Summary)
		switch d.Answer {
		case "":
			decisions[i] = "- Added, not asked: " + summary
		case "superseded":
			decisions[i] = "- " + q + " → superseded by " + summary
		case "dropped":
			decisions[i] = "- " + q + " → dropped: " + summary
		default:
			answer := inline(d.Answer)
			if answer == "free" {
				answer = "free text"
			}
			decisions[i] = fmt.Sprintf("- %s → %s: %s (%s, round %d)", q, answer, summary, inline(d.By), d.Round)
		}
	}
	return github.Escape(strings.Join([]string{
		"## Problem\n\n" + block(s.Problem),
		"## Goals\n\n" + bullets(s.Goals),
		"## Out of scope\n\n" + bullets(s.OutOfScope),
		"## Users and flows\n\n" + block(s.UsersAndFlows),
		"## Constraints\n\n" + bullets(s.Constraints),
		"## Acceptance criteria\n\n" + bullets(s.AcceptanceCriteria),
		"## Decisions\n\n" + orNone(strings.Join(decisions, "\n")),
	}, "\n\n"))
}

// renderTypeLine is the line naming the type t sets, and why, that starts
// the comment of an output that sets one (SPEC §9.1).
func renderTypeLine(t interviewType) string {
	return "Type: " + inline(t.Value) + ", because " + lowerFirst(inline(t.Reason))
}

// renderSpecNote renders the text of a spec's revision comment: the type
// line when out sets a type, out's message, the decisions made without
// asking, and the cc line for notify.
func renderSpecNote(out interviewOutput, notify []string) string {
	var parts, added []string
	if t := out.Type; t != nil {
		parts = append(parts, renderTypeLine(*t))
	}
	if strings.TrimSpace(out.Message) != "" {
		parts = append(parts, block(out.Message))
	}
	if out.Spec != nil {
		for _, d := range out.Spec.Decisions {
			if d.Question == "" {
				added = append(added, d.Summary)
			}
		}
	}
	if len(added) > 0 {
		parts = append(parts, "Added, not asked:\n\n"+bullets(added))
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n"))
}

// renderUnchanged renders an unchanged outcome's comment (#189): that the
// discussion changed nothing, so the issue goes back to origin, then
// standing, the App's line on a revision that still stands, if any, then
// out's message, then the cc line for notify when the issue waits on the
// owner there, at a review stage.
func renderUnchanged(out interviewOutput, origin state.Stage, standing string, notify []string) string {
	parts := []string{fmt.Sprintf("Nothing in the discussion during the hold changes this issue, so it goes back to `%s`.", origin)}
	if standing != "" {
		parts = append(parts, standing)
	}
	if strings.TrimSpace(out.Message) != "" {
		parts = append(parts, block(out.Message))
	}
	if origin == state.SpecReview || origin == state.PlanReview {
		parts = append(parts, ccLines(notify)...)
	}
	return github.Escape(strings.Join(parts, "\n\n"))
}

// renderPlanChange renders a plan outcome's comment (#294): that the
// discussion changes the plan, so the issue goes to planning, then out's
// message, then that the planner posts a new plan for the owner's 👍,
// then the cc line for notify.
func renderPlanChange(out interviewOutput, notify []string) string {
	parts := []string{
		"The discussion during the hold changes this issue's plan, but not its spec, so it goes to `planning`.",
		block(out.Message),
		"The planner reads these notes and the discussion, and posts a new plan for your 👍.",
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n"))
}

func ccLines(notify []string) []string {
	if len(notify) == 0 {
		return nil
	}
	return []string{"cc @" + strings.Join(notify, " @")}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "None."
	}
	return s
}

// Agent text is made safe to post with these. github.Escape does markers.
var (
	// atEntityRE is an HTML entity for "@", which GitHub may mention on.
	atEntityRE = regexp.MustCompile(`(?i)&(?:#0*64|#x0*40|commat);`)
	// tagRE starts an HTML tag, comment or declaration. Inline, it could
	// add a heading or hide what follows, and at the start of a line it
	// starts an HTML block that could swallow the template's headings.
	tagRE = regexp.MustCompile(`<([A-Za-z/!?])`)
	// container is the block quote, list item and footnote definition
	// markers before a line's text: GFM's containers, any of which may hold
	// a heading, a fence or a setext underline.
	container = `(?:[ \t]*(?:>|[-*+][ \t]|\d{1,9}[.)][ \t]|\[\^[^\]]*\]:))*[ \t]*`
	headingRE = regexp.MustCompile(`^(` + container + `)#{1,2}([ \t]|$)`)
	taskRE    = regexp.MustCompile(`^(` + container + `(?:[-*+]|\d{1,9}[.)])[ \t]+)\[[ \txX]\](?:[ \t]+|$)`)
	// breakRE is a line a CommonMark parser could read as a code fence or
	// a setext heading's underline, at any indent and in any container.
	// Its second group starts at the character block escapes.
	breakRE = regexp.MustCompile("^(" + container + ")(`{3,}|~{3,}|=+[ \t]*$|-+[ \t]*$)")
	// fenceRE is the only fence block keeps: one at column 0, outside any
	// container, whose info string has no backtick if it's a backtick
	// fence. CommonMark reads such a line as a fence wherever block meets
	// it, since block escapes every other fence and HTML block.
	fenceRE = regexp.MustCompile("^(?:(`{3,})[^`]*|(~{3,}).*)$")
)

// defuse puts a zero-width space after each "@", or HTML entity for one,
// that isn't followed by a space or the end of text, so agent text can't
// mention anyone (SPEC §9).
func defuse(text string) string {
	parts := strings.Split(atEntityRE.ReplaceAllString(text, "@"), "@")
	for i := 1; i < len(parts); i++ {
		if r, _ := utf8.DecodeRuneInString(parts[i]); parts[i] != "" && !unicode.IsSpace(r) && r != '\u200b' {
			parts[i] = "\u200b" + parts[i]
		}
	}
	return strings.Join(parts, "@")
}

// normalize drops every U+FEFF from text and turns each C0 control
// character, other than those in keep, into a space. GitHub's parser
// drops a BOM at the start of a document, turns NUL into U+FFFD and reads
// \v and \f as spaces in some places but not others. After this, the text
// posted is the text block reads line by line, so their line starts can't
// differ, and no pattern here has to know about those characters.
func normalize(text, keep string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\ufeff':
			return -1
		case r < 0x20 && !strings.ContainsRune(keep, r):
			return ' '
		}
		return r
	}, text)
}

// escapeBoxes backslash-escapes the "[" of every task list box in line,
// "[ ]", "[x]", "[X]" or "[\t]", wherever it is, unless a backslash is
// before it already. A box with a backslash before its "[" is never a
// checkbox, in any container or position, and reads as "[ ]".
func escapeBoxes(line string) string {
	out := make([]byte, 0, len(line))
	for i := 0; i < len(line); i++ {
		if line[i] == '[' && i+2 < len(line) && line[i+2] == ']' && strings.IndexByte(" \txX", line[i+1]) >= 0 && (i == 0 || line[i-1] != '\\') {
			out = append(out, '\\')
		}
		out = append(out, line[i])
	}
	return string(out)
}

// text is agent text on one line: normalized, with its HTML tags escaped
// and its mentions defused.
func text(s string) string {
	s = normalize(strings.ReplaceAll(s, "\r\n", " "), "\t")
	return defuse(tagRE.ReplaceAllString(strings.TrimSpace(s), "&lt;${1}"))
}

// ivWordingStrongChars are the characters ivWordingStrong escapes outside
// a code span: those that could open or close emphasis, a link or a
// strikethrough, and the backslash, so no backslash in the text can undo
// an escape, its own or the App's.
const ivWordingStrongChars = `\*_[]~`

// ivWordingStrong is raw agent text made safe to go inside the App's
// "**…**" (#229), before inline: each of ivWordingStrongChars outside a
// code span is backslash-escaped, so no "**" or other delimiter in the
// text can close the App's bold early, or open something that swallows
// its close. A code span, a backtick run that the next run of the same
// length in the text closes, as CommonMark reads it, is kept as it is:
// its text is literal, and it ends before the App's "**". Any other
// backtick run is escaped, so it can't open a span that runs past them.
// A bare URL outside a code span (ivWordingURL) keeps its
// ivWordingURLChars unescaped, so GitHub links it whole (#399).
// The text reads as typed: only its own emphasis and links don't render.
func ivWordingStrong(s string) string {
	var b strings.Builder
	url := 0 // the end of the bare URL that s[i] is in, while i is below it
	for i := 0; i < len(s); {
		if s[i] != '`' {
			if i >= url {
				url = ivWordingURL(s, i)
			}
			if strings.IndexByte(ivWordingStrongChars, s[i]) >= 0 && (i >= url || strings.IndexByte(ivWordingURLChars, s[i]) < 0) {
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		n, end := ivWordingTicks(s, i), -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := ivWordingTicks(s, j)
			if m == n {
				end = j + m
				break
			}
			j += m
		}
		if end < 0 {
			b.WriteString(strings.Repeat("\\`", n))
			i += n
			continue
		}
		b.WriteString(s[i:end])
		i = end
	}
	return b.String()
}

// ivWordingURLChars are the characters of ivWordingStrongChars that a bare
// URL keeps unescaped (ivWordingStrong): a backslash before one would cut
// GitHub's link short, and neither can close the App's "**". An "_" or "~"
// pairs only with its own kind, and the App's close pairs with its open
// first, which drops any "_" or "~" left open between them. The URL's "*",
// "\", "[" and "]" stay escaped, as an issue title's "[", "]" and "\" are
// (digestRenderTitle): a bare "*" can close the App's bold where a parser
// ends the link before it, and "[" and "]" can make a link or an image
// where a parser reads them before the URL, so those still cut it short.
const ivWordingURLChars = "_~"

// ivWordingURLRE starts a bare URL that GFM links (its "Autolinks
// (extension)"): "http://" or "https://", in any case, or "www.".
var ivWordingURLRE = regexp.MustCompile(`^(?:(?i:https?://)|www\.)`)

// ivWordingURL returns the end of the bare URL that starts at s[i], as GFM
// links it, or 0 if none does. One starts with ivWordingURLRE at the start
// of s, or after a space, a tab, "*", "_", "~" or "(", and runs to the next
// space, control character or "<". GFM then leaves trailing punctuation out
// of the link: each "?", "!", ".", ",", ":", "*", "_", "~", "'" and "\"" at
// the end, a ")" with more ")" than "(" before it, and a ";", with the
// entity it ends, if it ends one, such as "&amp;". A URL with nothing left
// after its start is none.
func ivWordingURL(s string, i int) int {
	if i > 0 && strings.IndexByte(" \t*_~(", s[i-1]) < 0 {
		return 0
	}
	m := ivWordingURLRE.FindStringIndex(s[i:])
	if m == nil {
		return 0
	}
	start, end := i+m[1], i+m[1]
	for end < len(s) && s[end] > ' ' && s[end] != '<' && s[end] != 0x7f {
		end++
	}
	for end > start {
		c := s[end-1]
		switch {
		case strings.IndexByte(`?!.,:*_~'"`, c) >= 0:
			end--
			continue
		case c == ')' && strings.Count(s[i:end], ")") > strings.Count(s[i:end], "("):
			end--
			continue
		case c == ';':
			j := end - 1
			for j > start && ivWordingAlnum(s[j-1]) {
				j--
			}
			if j < end-1 && j > start && s[j-1] == '&' {
				end = j - 1
			} else {
				end--
			}
			continue
		}
		break
	}
	if end == start {
		return 0
	}
	return end
}

// ivWordingAlnum reports whether c is an ASCII letter or digit.
func ivWordingAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ivWordingTicks is the length of the backtick run that starts at s[i].
func ivWordingTicks(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// inline is agent text for the middle of a line: text, with its task list
// boxes escaped.
func inline(s string) string {
	return escapeBoxes(text(s))
}

// block is agent Markdown for a section, with its line breaks made "\n"
// and the rest normalized, keeping tabs. Only a fence fenceRE
// matches is kept as one, and it ends where CommonMark ends it: at a line
// of at most 3 spaces, then at least as many of its characters, then only
// spaces or tabs. A fence left open is closed, so it can't swallow what
// follows. A line inside a kept fence is code, which GitHub never
// mentions on, so it's kept as it is, apart from github.Escape. Every
// other line is defused, and cleaned as clean says.
func block(text string) string {
	var out []string
	fence := ""
	text = normalize(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text), "\t\n")
	for _, line := range strings.Split(text, "\n") {
		switch {
		case fence != "":
			if closesFence(line, fence) {
				fence = ""
			}
		case fenceRE.MatchString(line):
			m := fenceRE.FindStringSubmatch(line)
			fence, line = m[1]+m[2], defuse(line)
		default:
			line = clean(defuse(line))
		}
		out = append(out, line)
	}
	if fence != "" {
		out = append(out, fence)
	}
	return orNone(strings.TrimLeft(strings.TrimRight(strings.Join(out, "\n"), " \t\n"), "\n"))
}

// closesFence reports whether line closes a fence opened by fence, by
// CommonMark's rule. A tab counts as 4 spaces of indent, so it never does.
func closesFence(line, fence string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return len(t) >= len(fence) && strings.Trim(t, fence[:1]) == ""
}

// bullets renders items as a plain "- " list, never a task list: a box
// the item starts with is stripped, and clean escapes any other.
func bullets(items []string) string {
	lines := make([]string, len(items))
	for i, s := range items {
		lines[i] = clean("- " + text(s))
	}
	return orNone(strings.Join(lines, "\n"))
}

// clean makes a line of agent Markdown outside a kept fence safe: HTML
// tags escaped, # and ## headings demoted to ###, and task list boxes
// after the line's markers stripped, until there are none. Every other
// box is escaped (escapeBoxes), wherever it is. Then a line that could be
// a fence or a setext underline gets its first such character
// backslash-escaped, so no parser reads a fence block doesn't track, or a
// heading.
func clean(line string) string {
	line = tagRE.ReplaceAllString(line, "&lt;${1}")
	for {
		next := taskRE.ReplaceAllString(headingRE.ReplaceAllString(line, "${1}###${2}"), "${1}")
		if next == line {
			break
		}
		line = next
	}
	line = escapeBoxes(line)
	if m := breakRE.FindStringSubmatchIndex(line); m != nil {
		line = line[:m[4]] + `\` + line[m[4]:]
	}
	return line
}

// lowerWords are the first words lowerFirst lowercases. Any other word may
// be a name, such as "Alice" or "GitHub", so it keeps its case.
var lowerWords = []string{"A", "An", "The", "It", "It's", "Its", "This", "That", "These", "Those", "There", "They", "Each", "Every", "Some", "Most", "All", "No", "One"}

// lowerFirst lowercases text's first word if it's one of lowerWords, so a
// reason reads on after "because".
func lowerFirst(text string) string {
	word, _, _ := strings.Cut(text, " ")
	if !slices.Contains(lowerWords, word) {
		return text
	}
	return strings.ToLower(word) + text[len(word):]
}

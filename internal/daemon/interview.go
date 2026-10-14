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
)

// The interviewer's outcomes.
const (
	outcomeQuestions = "questions"
	outcomeSpec      = "spec"
	outcomeTyped     = "typed"
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
	MayType      bool // the interviewer may set the type (plan for #6, assumption 13)
	Untyped      bool // the issue has no type, so a spec must set one
	// Rounds is the question count of each round so far: Rounds[0] is
	// round 1's. A round is asked as round len(Rounds)+1.
	Rounds []int
	// Trusted is the logins with trusted comments on the issue, the only
	// ones a decision may name.
	Trusted []string
	Notify  []string // config's notify, for the cc line
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
)

// checkError is an output that breaks a rule.
type checkError struct {
	Rule, Detail string
}

func (e *checkError) Error() string { return "rule " + e.Rule + ": " + e.Detail }

// maxText is GitHub's limit on a comment or body, in characters. textRoom
// is kept free of it for the markers, the revision text and a dirty note
// that go with the rendered text.
const (
	maxText  = 65536
	textRoom = 1024
)

// checkInterview checks out against f after the schema (plan for #6,
// assumptions 6, 7 and 10). Its error joins a *checkError for every rule
// broken. Nothing may be written from an output that fails.
func checkInterview(out interviewOutput, f interviewFacts) error {
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &checkError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
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
	default:
		fail(ruleOutcome, "unknown outcome %q", out.Outcome)
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
		for _, s := range append([]string{q.Text, q.Recommended.Text, q.Recommended.Why}, q.Others...) {
			if !oneLine(s) {
				fail(ruleOneLine, "%s: %q", at, s)
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
// notify (plan for #6, assumption 14). A typed output renders as the type
// line and the cc line.
func renderQuestions(out interviewOutput, issue, round int, notify []string) string {
	var parts []string
	if t := out.Type; t != nil {
		parts = append(parts, "Type: "+inline(t.Value)+", because "+lowerFirst(inline(t.Reason)))
	}
	if len(out.Questions) > 0 {
		lines := []string{fmt.Sprintf("### Questions for #%d (round %d)", issue, round)}
		for i, q := range out.Questions {
			blocks := make([]string, len(q.Blocks))
			for j, b := range q.Blocks {
				blocks[j] = "#" + strconv.Itoa(b)
			}
			lines = append(lines, "", fmt.Sprintf("**%d. %s** Blocks: %s", i+1, inline(q.Text), strings.Join(blocks, ", ")),
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

// renderSpecNote renders the text of a spec's revision comment: out's
// message, the decisions made without asking, and the cc line for notify.
func renderSpecNote(out interviewOutput, notify []string) string {
	var parts, added []string
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
	mentionRE  = regexp.MustCompile(`@([A-Za-z0-9])`)
	// container is the block quote and list markers before a line's text.
	container = `(?:[ \t]*(?:>|[-*+](?:[ \t]|$)|\d{1,9}[.)](?:[ \t]|$)))*[ \t]*`
	headingRE = regexp.MustCompile(`^(` + container + `)#{1,2}([ \t]|$)`)
	taskRE    = regexp.MustCompile(`^(` + container + `(?:[-*+]|\d{1,9}[.)])[ \t]+)\[[ xX]\](?:[ \t]+|$)`)
	// htmlRE starts an HTML block, which could swallow the template's
	// headings after it.
	htmlRE   = regexp.MustCompile(`^((?: {0,3}>)* {0,3})<([A-Za-z/!?])`)
	fenceRE  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	setextRE = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
)

// defuse puts a zero-width space after each "@" that could start a
// mention, so agent text can't notify anyone (plan for #6, assumption 14).
func defuse(text string) string {
	return mentionRE.ReplaceAllString(atEntityRE.ReplaceAllString(text, "@"), "@\u200b${1}")
}

// inline is agent text for the middle of a line: defused, on one line.
func inline(text string) string {
	return defuse(strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(text)))
}

// block is agent Markdown for a section: defused, with headings demoted to
// ###, setext headings broken up, task list boxes stripped and HTML blocks
// escaped, except in code fences. A fence left open is closed, so it
// can't swallow what follows.
func block(text string) string {
	var out []string
	fence := ""
	for _, line := range strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text), "\n") {
		line = defuse(line)
		switch {
		case fence != "":
			if t := strings.TrimSpace(line); strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
				fence = ""
			}
		case fenceRE.MatchString(line):
			fence = fenceRE.FindStringSubmatch(line)[1]
		default:
			if setextRE.MatchString(line) && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			line = htmlRE.ReplaceAllString(item(line), "${1}&lt;${2}")
		}
		out = append(out, line)
	}
	if fence != "" {
		out = append(out, fence)
	}
	return orNone(strings.TrimLeft(strings.TrimRight(strings.Join(out, "\n"), " \t\n"), "\n"))
}

// bullets renders items as a plain "- " list, never a task list.
func bullets(items []string) string {
	lines := make([]string, len(items))
	for i, s := range items {
		lines[i] = item("- " + inline(s))
	}
	return orNone(strings.Join(lines, "\n"))
}

// item demotes a heading and strips task list boxes at the start of line,
// until there are none.
func item(line string) string {
	for {
		next := taskRE.ReplaceAllString(headingRE.ReplaceAllString(line, "${1}###${2}"), "${1}")
		if next == line {
			return line
		}
		line = next
	}
}

// lowerFirst lowercases text's first letter when its first word is
// otherwise lowercase, so a reason reads on after "because". "I" and
// words like "PWA" or "GitHub" keep their case.
func lowerFirst(text string) string {
	word, _, _ := strings.Cut(text, " ")
	r, n := utf8.DecodeRuneInString(word)
	if !unicode.IsUpper(r) || word == "I" || strings.HasPrefix(word, "I'") || word[n:] != strings.ToLower(word[n:]) {
		return text
	}
	return string(unicode.ToLower(r)) + text[n:]
}

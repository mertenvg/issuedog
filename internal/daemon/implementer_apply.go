package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file checks and applies the implementer's results (SPEC §7.2, §12):
// its commits, which move the task to verifying with its head, and its
// question round, which moves it to clarifying (SPEC §9.1).

// The implementer's outcomes, as its schema names them.
const (
	implementerCommits  = "commits"
	implementerQuestion = "question"
	implementerHandoff  = "handoff"
)

// implementerOutput is the implementer's structured output, as
// internal/agent/schemas/implementer.json gives it. It's one flat object,
// with no conditional at its top level, so checkImplementer pairs each
// outcome with its fields (SPEC §12).
type implementerOutput struct {
	Outcome   string                    `json:"outcome"`
	Title     *implementerTitle         `json:"title,omitempty"`
	Summary   *implementerSummary       `json:"summary,omitempty"`
	Questions []interviewQuestion       `json:"questions,omitempty"`
	Handoff   *implementerHandoffFields `json:"handoff,omitempty"`
}

// implementerTitle is the PR title, <Type>: <Sentence> (SPEC §11.5).
type implementerTitle struct {
	Type     string `json:"type"`
	Sentence string `json:"sentence"`
}

// String is the title as the PR gets it.
func (t implementerTitle) String() string { return t.Type + ": " + t.Sentence }

// implementerSummary is the PR's summary of the change (SPEC §11.5).
type implementerSummary struct {
	WhatChanged  string   `json:"what_changed"`
	Why          string   `json:"why"`
	TestsAdded   []string `json:"tests_added"`
	TestsUpdated []string `json:"tests_updated"`
}

// implementerHandoffFields is a hand-off's fields (SPEC §9.3), as the schema
// gives them.
type implementerHandoffFields struct {
	Goal          string   `json:"goal"`
	WhyBlocks     string   `json:"why_blocks"`
	Steps         []string `json:"steps"`
	Links         []string `json:"links"`
	DoneWhen      string   `json:"done_when"`
	EstimatedTime string   `json:"estimated_time"`
}

// decodeImplementer decodes a run's output. A field the types don't have
// is an error (decodeCause).
func decodeImplementer(raw json.RawMessage) (implementerOutput, error) {
	var out implementerOutput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return implementerOutput{}, decodeCause("the implementer's", err)
	}
	return out, nil
}

// implementerMaxTitle is GitHub's limit on a PR title, in characters.
const implementerMaxTitle = 256

// The rules checkImplementer enforces beside the interviewer's, which it
// shares where they're the same: outcome, one-line and length.
const (
	ruleCommits    = "commits"
	ruleTitleType  = "title-type"
	ruleTitleLimit = "title-length"
)

// implementerFacts is what checkImplementer checks an output against.
type implementerFacts struct {
	TitleTypes []string // pr.title_types
	// Head is the commit the run left (Result.Head), or "" when it made
	// none past its start.
	Head string
	// Issue is the task's number, MaxQuestions loop.max_questions_per_round
	// and Notify the config's notify, for a question round and its cc line.
	Issue        int
	MaxQuestions int
	Notify       []string
	// Rounds is the question count of each round of every stage on the
	// task so far (ivRoundCounts of allRounds): a question round is asked
	// as round len(Rounds)+1, as nextRound numbers it (SPEC §9.1).
	Rounds []int
}

// checkImplementer checks out against f after the schema. Its error joins
// a *ruleError for every rule broken, and nothing may be written from an
// output that fails. Each outcome is paired with its fields here, as the
// schema has no conditional at its top level (SPEC §12):
//   - commits has a title and a summary, and no questions or hand-off. It
//     needs commits past the run's start, a title type on pr.title_types,
//     exactly as written, and a title on one line within GitHub's limit,
//     and its comment (renderCommits) within maxText less textRoom.
//   - question has questions, and no title, summary or hand-off. Its
//     questions follow the interviewer's question rules (checkQuestions),
//     and its comment (renderQuestions) fits within maxText less textRoom.
//   - handoff isn't handled yet, so it's a failed attempt, as an unknown
//     outcome is.
func checkImplementer(out implementerOutput, f implementerFacts) error {
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &ruleError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
	}
	switch out.Outcome {
	case implementerCommits:
		checkCommits(out, f, fail)
	case implementerQuestion:
		checkImplementerQuestion(out, f, fail)
	case implementerHandoff:
		fail(ruleOutcome, "the %s outcome isn't handled yet", out.Outcome)
	default:
		fail(ruleOutcome, "unknown outcome %q", out.Outcome)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("check implementer output: %w", errors.Join(errs...))
}

// checkCommits is checkImplementer's rules for a commits outcome.
func checkCommits(out implementerOutput, f implementerFacts, fail func(string, string, ...any)) {
	if out.Title == nil {
		fail(ruleOutcome, "commits has a title")
	}
	if out.Summary == nil {
		fail(ruleOutcome, "commits has a summary")
	}
	if out.Questions != nil {
		fail(ruleOutcome, "commits has no questions")
	}
	if out.Handoff != nil {
		fail(ruleOutcome, "commits has no hand-off")
	}
	if f.Head == "" {
		fail(ruleCommits, "no commits past the run's start")
	}
	if t := out.Title; t != nil {
		if !slices.Contains(f.TitleTypes, t.Type) {
			fail(ruleTitleType, "the PR title's type %q isn't one of pr.title_types, %s", t.Type, strings.Join(f.TitleTypes, ", "))
		}
		if !oneLine(t.Type) || !oneLine(t.Sentence) {
			fail(ruleOneLine, "the PR title %q", t.String())
		}
		if n := textLen(t.String()); n > implementerMaxTitle {
			fail(ruleTitleLimit, "the PR title is %d characters, over %d", n, implementerMaxTitle)
		}
	}
	if out.Title != nil && out.Summary != nil {
		if n := textLen(renderCommits(*out.Title, *out.Summary, f.Head)); n > maxText-textRoom {
			fail(ruleLength, "the comment is %d characters, over %d", n, maxText-textRoom)
		}
	}
}

// checkImplementerQuestion is checkImplementer's rules for a question
// outcome, as the interviewer's question round has them (SPEC §9.1).
func checkImplementerQuestion(out implementerOutput, f implementerFacts, fail func(string, string, ...any)) {
	if len(out.Questions) == 0 {
		fail(ruleOutcome, "question has questions")
	}
	if out.Title != nil {
		fail(ruleOutcome, "question has no title")
	}
	if out.Summary != nil {
		fail(ruleOutcome, "question has no summary")
	}
	if out.Handoff != nil {
		fail(ruleOutcome, "question has no hand-off")
	}
	checkQuestions(out.Questions, interviewFacts{MaxQuestions: f.MaxQuestions}, fail)
	text := renderQuestions(interviewOutput{Questions: out.Questions}, f.Issue, len(f.Rounds)+1, f.Notify)
	if n := textLen(text); n > maxText-textRoom {
		fail(ruleLength, "the question round's comment is %d characters, over %d", n, maxText-textRoom)
	}
}

// facts are what checkImplementer holds r's output to: the config's
// pr.title_types, loop.max_questions_per_round and notify, the run's head,
// and the rounds of every stage on r.S so far.
func (im *Implementer) facts(r Result) implementerFacts {
	c := im.Daemon.Config
	return implementerFacts{
		TitleTypes:   c.PR.TitleTypes,
		Head:         r.Head,
		Issue:        r.S.Number,
		MaxQuestions: int(c.Loop.MaxQuestionsPerRound),
		Notify:       c.Notify,
		Rounds:       ivRoundCounts(allRounds(r.S, im.Daemon.Step.AppSlug, r.Dirt.Has)),
	}
}

// Check checks r's output after its schema (checkImplementer): a broken
// rule is a failed attempt, with nothing written. It reads nothing.
func (im *Implementer) Check(_ context.Context, r Result) error {
	out, err := decodeImplementer(r.Output)
	if err != nil {
		return fmt.Errorf("check implementer run %s: %w", r.RunID, err)
	}
	return checkImplementer(out, im.facts(r))
}

// renderCommits renders the comment of a commits outcome, on the move to
// verifying, in a fixed format: the head the run committed, the PR title,
// then the summary under the PR's headings (SPEC §11.5), the agent's text
// escaped (text, block). The head is git's, so it's the App's text.
func renderCommits(title implementerTitle, sum implementerSummary, head string) string {
	var tests []string
	for _, t := range sum.TestsAdded {
		tests = append(tests, "- added: "+inline(t))
	}
	for _, t := range sum.TestsUpdated {
		tests = append(tests, "- updated: "+inline(t))
	}
	return github.Escape(strings.Join([]string{
		"The implementer committed `" + head + "`.",
		"**PR title:** " + inline(title.Type) + ": " + inline(title.Sentence),
		"## What changed\n\n" + block(sum.WhatChanged),
		"## Why\n\n" + block(sum.Why),
		"## Tests\n\n" + orNone(strings.Join(tests, "\n")),
	}, "\n\n"))
}

// implementerTriggers are the triggers of the implementer's moves, by the
// stage each moves a task to from in-progress (SPEC §7.2).
var implementerTriggers = map[state.Stage]state.Trigger{
	state.Verifying:  state.TriggerImplemented,
	state.Clarifying: state.TriggerImplementerQuestion,
	state.NeedsHuman: state.TriggerImplementerBlocked,
}

// implementerMoved returns the move r's run wrote on an issue in state
// is, as a Move to finish (Transition): to the stage its marker names, on
// the trigger of that move (implementerTriggers), whichever outcome it
// was. It reports false if no transition marker carries the run's key.
func implementerMoved(is state.Issue, r Result) (Move, bool) {
	for _, m := range is.Markers {
		if m.Kind == "transition" && m.Fields["run"] == r.RunID {
			to := state.Stage(m.Fields["to"])
			return r.Move(to, implementerTriggers[to]), true
		}
	}
	return Move{}, false
}

// Apply writes r's result, which Check passed, on r.S, as r.Move's move,
// keyed by the run, with seen= and the run's model=, effort= and ran=:
//   - commits: in-progress → verifying on implemented, with head=, the
//     run's head (Result.Head), and its comment renderCommits'.
//   - question: in-progress → clarifying on implementer-question, with
//     round= and questions=, the round the next of every stage's
//     (nextRound, SPEC §9.1), and its comment the round in the SPEC §9.1
//     format, then the cc line (renderQuestions). The task waits there for
//     its answer (Step.AnswerVisit).
//
// Each write is checked first, so it can run again after a crash part way
// (SPEC §5.2). Once the run's move is written, it only finishes the move's
// labels (Transition), at the stage the move put the issue at, whichever
// outcome it was (implementerMoved). Nothing is rendered from output that
// fails checkImplementer.
func (im *Implementer) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply implementer run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	if m, moved := implementerMoved(is, r); moved {
		logr.Debugf("%s/%s#%d: run %s's move is written already, so only its labels are left", a.Owner, a.Repo, r.S.Number, r.RunID)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return nil
	}
	out, err := decodeImplementer(r.Output)
	if err != nil {
		return wrap(err)
	}
	f := im.facts(r)
	if err := checkImplementer(out, f); err != nil {
		return wrap(err)
	}
	if out.Outcome == implementerQuestion {
		round := nextRound(allRounds(r.S, a.AppSlug, r.Dirt.Has))
		m := r.Move(state.Clarifying, state.TriggerImplementerQuestion)
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields["round"], m.Fields["questions"] = strconv.Itoa(round), strconv.Itoa(len(out.Questions))
		m.Text = renderQuestions(interviewOutput{Questions: out.Questions}, r.S.Number, round, f.Notify)
		logr.Infof("%s/%s#%d: run %s asks round %d, of %d questions, moving to %s", a.Owner, a.Repo, r.S.Number, r.RunID, round, len(out.Questions), state.Clarifying)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return nil
	}
	m := r.Move(state.Verifying, state.TriggerImplemented)
	if m.Fields == nil {
		m.Fields = map[string]string{}
	}
	m.Fields["head"] = r.Head
	m.Text = renderCommits(*out.Title, *out.Summary, r.Head)
	logr.Infof("%s/%s#%d: run %s committed %s, moving to %s", a.Owner, a.Repo, r.S.Number, r.RunID, r.Head, state.Verifying)
	if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
		return wrap(err)
	}
	return nil
}

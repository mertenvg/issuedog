package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file checks and applies the implementer's results (SPEC §7.2, §12):
// for now its commits, which move the task to verifying with its head.

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
}

// checkImplementer checks out against f after the schema. Its error joins
// a *ruleError for every rule broken, and nothing may be written from an
// output that fails. Each outcome is paired with its fields here, as the
// schema has no conditional at its top level (SPEC §12):
//   - commits has a title and a summary, and no questions or hand-off. It
//     needs commits past the run's start, a title type on pr.title_types,
//     exactly as written, and a title on one line within GitHub's limit,
//     and its comment (renderCommits) within maxText less textRoom.
//   - question and handoff aren't handled yet, so either is a failed
//     attempt, as an unknown outcome is.
func checkImplementer(out implementerOutput, f implementerFacts) error {
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &ruleError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
	}
	switch out.Outcome {
	case implementerCommits:
		checkCommits(out, f, fail)
	case implementerQuestion, implementerHandoff:
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

// facts are what checkImplementer holds r's output to: the config's
// pr.title_types, and the run's head.
func (im *Implementer) facts(r Result) implementerFacts {
	return implementerFacts{TitleTypes: im.Daemon.Config.PR.TitleTypes, Head: r.Head}
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

// Apply writes r's commits, which Check passed, on r.S: the move
// in-progress → verifying on implemented, r.Move's, keyed by the run, with
// seen= and the run's model=, effort= and ran=, and head=, the run's head
// (Result.Head), and its comment renderCommits'. Each write is checked
// first, so it can run again after a crash part way (SPEC §5.2). Once the
// run's move is written, it only finishes the move's labels (Transition),
// at the stage the move put the issue at. Nothing is rendered from output
// that fails checkImplementer.
func (im *Implementer) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply implementer run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	if is.Moved(state.Key{Field: "run", ID: r.RunID}) {
		logr.Debugf("%s/%s#%d: run %s's move is written already, so only its labels are left", a.Owner, a.Repo, r.S.Number, r.RunID)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, r.Move(is.Stage, state.TriggerImplemented)); err != nil {
			return wrap(err)
		}
		return nil
	}
	out, err := decodeImplementer(r.Output)
	if err != nil {
		return wrap(err)
	}
	if err := checkImplementer(out, im.facts(r)); err != nil {
		return wrap(err)
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

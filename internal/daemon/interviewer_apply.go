package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file applies the interviewer's results (plan #6, T5): a question
// round, and under Q1a the typing of a bug or task. #135 (plan #6, T6)
// completes it with the spec: Check already checks a spec in full, then
// fails it as an attempt, and Apply refuses one, so #135 drops those two
// refusals and adds the spec's writes (assumption 9) to Apply.

// Check checks r's output after its schema (checkInterview, plan #6,
// assumptions 7 and 10) against the facts of r.S, the issue read again
// (ivFacts). A broken rule is a failed attempt, with nothing written. A
// read that fails while gathering the facts is a *checkReadError, so the
// result is kept and applied again on the next pass. Until #135, a spec
// that passes every rule fails too, as nothing can write it yet.
func (iv *Interviewer) Check(ctx context.Context, r Result) error {
	out, err := decodeInterview(r.Output)
	if err != nil {
		return err
	}
	f, err := iv.facts(ctx, r)
	if err != nil {
		return &checkReadError{fmt.Errorf("the interviewer's facts on #%d: %w", r.S.Number, err)}
	}
	if err := checkInterview(out, f); err != nil {
		return err
	}
	if out.Outcome == outcomeSpec {
		return errors.New("the interviewer can't write a spec yet: #135 builds that")
	}
	return nil
}

// facts are what Check holds r's output to, from r.S:
//   - the rounds so far, from the round= and questions= of the
//     interviewer's clean round moves (interviewerRounds);
//   - whether the interviewer may set the type, from the timeline
//     (typing), and whether the issue has one;
//   - the logins a spec's decision may name;
//   - loop.max_questions_per_round, and notify for the cc line.
func (iv *Interviewer) facts(ctx context.Context, r Result) (interviewFacts, error) {
	d := iv.Daemon
	a := d.Step.Applier
	_, may, err := iv.typing(ctx, r.S, &ivTimeline{a: a, number: r.S.Number})
	if err != nil {
		return interviewFacts{}, err
	}
	logins, err := ivTrustedLogins(ctx, r.S, r.Dirt.Has, a.AppSlug, trustCache{st: d.Step, verdicts: map[string]bool{}})
	if err != nil {
		return interviewFacts{}, err
	}
	return interviewFacts{
		Issue:        r.S.Number,
		MaxQuestions: int(d.Config.Loop.MaxQuestionsPerRound),
		MayType:      may,
		Untyped:      r.S.Type == "",
		Rounds:       ivRoundCounts(interviewerRounds(r.S, a.AppSlug, r.Dirt.Has)),
		Trusted:      logins,
		Notify:       d.Config.Notify,
	}, nil
}

// ivTrustedLogins returns the logins, each once, ignoring case, with a
// clean comment on s by a trusted user: the people a spec's decision may
// name, as Context lists them in the preamble. dirty is as for
// state.Derive. The App's comments, and other bots', don't count.
func ivTrustedLogins(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, appSlug string, tc trustCache) ([]string, error) {
	var logins []string
	for _, c := range s.Comments {
		if dirty(c.ID) || state.IsApp(c.Author, appSlug) ||
			slices.ContainsFunc(logins, func(l string) bool { return strings.EqualFold(l, c.Author.Login) }) {
			continue
		}
		trusted, err := tc.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("comment %d: %w", c.ID, err)
		}
		if trusted {
			logins = append(logins, c.Author.Login)
		}
	}
	return logins, nil
}

// ivRoundCounts is the question count of each round in rounds, by its
// number: counts[0] is round 1's, so the next round is len(counts)+1. A
// round no clean comment records, such as one someone made dirty, counts
// 0, so none of its questions was asked (questionID.askedBy).
func ivRoundCounts(rounds []interviewerRound) []int {
	var counts []int
	for _, r := range rounds {
		for len(counts) < r.Number {
			counts = append(counts, 0)
		}
		counts[r.Number-1] = r.Questions
	}
	return counts
}

// Apply writes r's question round, or its typed outcome (plan #6, Q1a),
// which Check passed, on r.S. Each write is checked first, so it can run
// again after a crash part way (SPEC §5.2):
//  1. If the output sets a type, the App adds its type:* label and removes
//     the issue's other type:* labels (ivSetType), which updates r.S.Type.
//     A move written already means the type was set before it, so this is
//     skipped, and a type the owner set since stands.
//  2. The move, clarifying → clarifying on more-questions, keyed by the
//     run, with seen= and the run's model= and effort= (Result.Move). A
//     question round's carries round= and questions= (plan #6, assumption
//     8), and its text is the round in the SPEC §9.1 format, the type line
//     first when it sets one, then the cc line (renderQuestions). A typed
//     outcome's carries neither, and its text is the type line, that a bug
//     or task waits here until phase 3, and the cc line.
//
// A spec is #135's (see the top of this file), so it's refused.
func (iv *Interviewer) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply interviewer run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	out, err := decodeInterview(r.Output)
	if err != nil {
		return wrap(err)
	}
	if out.Outcome != outcomeQuestions && out.Outcome != outcomeTyped {
		return wrap(fmt.Errorf("can't apply a %q outcome yet: #135 builds the spec", out.Outcome))
	}
	moved := state.Derive(r.S, a.AppSlug, r.Dirt.Has).Moved(state.Key{Field: "run", ID: r.RunID})
	if out.Type != nil && !moved {
		if err := ivSetType(ctx, a, r.S, out.Type.Value); err != nil {
			return wrap(err)
		}
	}
	// Once the move is written, its round is among these, so the number is
	// one too high, but Transition then writes no text or fields.
	round := len(ivRoundCounts(interviewerRounds(r.S, a.AppSlug, r.Dirt.Has))) + 1
	notify := iv.Daemon.Config.Notify
	m := r.Move(state.Clarifying, state.TriggerMoreQuestions)
	m.Text = renderQuestions(out, r.S.Number, round, notify)
	if out.Outcome == outcomeTyped {
		m.Text = strings.Join(append([]string{renderQuestions(out, r.S.Number, round, nil), ivTypedWaits}, ccLines(notify)...), "\n\n")
	} else {
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields["round"], m.Fields["questions"] = strconv.Itoa(round), strconv.Itoa(len(out.Questions))
	}
	if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
		return wrap(err)
	}
	return nil
}

// ivTypedWaits follows the type line of a bug or task the interviewer
// typed (plan #6, Q1a), as IntakeVisit says it for one filed so.
const ivTypedWaits = "Bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3), so I won't ask anything more here."

// ivTypes are the types the interviewer sets, as type:<type> labels.
var ivTypes = []string{"epic", "story", "bug", "task"}

// ivSetType makes type:<typ> the only type:* label on s, as setStage does
// for stage labels: it adds it, then removes the others s had before the
// add. One the add's answer shows for the first time was set since s was
// read, so it's left for the next CheckLabels. s.Type follows the labels
// (Snapshot.SetLabels). Only an issue typed by label can be typed so: on
// one with an issue type, it fails with nothing written, as it does for a
// type that isn't one of ivTypes.
func ivSetType(ctx context.Context, a *Applier, s *github.Snapshot, typ string) error {
	if !slices.Contains(ivTypes, typ) {
		return fmt.Errorf("set the type: %q isn't one of %s", typ, strings.Join(ivTypes, ", "))
	}
	if !s.TypeFromLabels {
		return fmt.Errorf("set the type %s: the issue has an issue type, %s, which only the owner sets", typ, s.Type)
	}
	want := "type:" + typ
	known := slices.Clone(s.Labels)
	if err := a.addLabel(ctx, s, want); err != nil {
		return fmt.Errorf("set the type %s: %w", typ, err)
	}
	for _, l := range known {
		if !strings.HasPrefix(strings.ToLower(l), "type:") || strings.EqualFold(l, want) {
			continue
		}
		if err := a.removeLabel(ctx, s, l); err != nil {
			return fmt.Errorf("set the type %s: %w", typ, err)
		}
	}
	return nil
}

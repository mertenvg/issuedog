package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file applies the interviewer's results (plan #6, T5 and T6): a
// question round, under Q1a the typing of a bug or task, and the spec
// (assumption 9), which is written into the body and posted as a revision.

// Check checks r's output after its schema (checkInterview, plan #6,
// assumptions 7 and 10) against the facts of r.S, the issue read again
// (ivFacts). A broken rule is a failed attempt, with nothing written. So is
//   - an issue the interviewer no longer runs on (ivStillRuns), such as one
//     the owner retyped to a task or bug, or gave a parent, during the run:
//     Pending is false for it from then on, so no run follows;
//   - output whose move doesn't fit the issue once the output's type is set
//     (ivMoveFits), such as a spec typing an issue with a parent a story,
//     which the table moves to spec-review only with no parent. Checked
//     here, it writes nothing, where Transition would report it as a state
//     error only after the type label and the body.
//
// A read that fails while gathering the facts is a *checkReadError, so the
// result is kept and applied again on the next pass.
func (iv *Interviewer) Check(ctx context.Context, r Result) error {
	out, err := decodeInterview(r.Output)
	if err != nil {
		return err
	}
	f, err := iv.facts(ctx, r)
	if err != nil {
		return &checkReadError{fmt.Errorf("the interviewer's facts on #%d: %w", r.S.Number, err)}
	}
	it := state.Derive(r.S, iv.Daemon.Step.AppSlug, r.Dirt.Has).Item
	if !ivStillRuns(it, out, f.MayType) {
		return fmt.Errorf("the interviewer no longer runs on #%d, with type %q and a parent %t: "+
			"it runs only on an epic, a story with no parent or an untyped issue", r.S.Number, it.Type, it.HasParent)
	}
	if err := checkInterview(out, f); err != nil {
		return err
	}
	if err := ivMoveFits(it, out); err != nil {
		return fmt.Errorf("the interviewer's %s on #%d: %w", out.Outcome, r.S.Number, err)
	}
	return nil
}

// ivStillRuns reports whether the interviewer still runs on an issue like
// it, read again after a run whose output is out (interviewerClass), or
// whether out, a typed outcome, made it the task or bug it is with its own
// type, set before a crash: the type is out's, and the App set it (may, as
// typing says), so it isn't the owner's retype. Pending is false for such
// an issue, so the only run on it is the one that typed it. Only a typed
// outcome sets a type the interviewer doesn't run on, so a story with a
// parent never passes, whoever typed it.
func ivStillRuns(it state.Item, out interviewOutput, may bool) bool {
	return interviewerClass(it) ||
		out.Outcome == outcomeTyped && out.Type != nil && may && strings.EqualFold(it.Type, out.Type.Value)
}

// ivMoveFits returns an error unless out's move fits an issue like it once
// out's type, if it sets one, is set: the table has the move for its class
// (SPEC §7.2), clarifying → spec-review on spec-written for a spec, and
// clarifying → clarifying on more-questions otherwise. A question round
// must also leave the issue one the interviewer runs on, or no run would
// read its answers: a story with a parent waits at clarifying for good.
// out has passed checkInterview, so its type is a known one.
func ivMoveFits(it state.Item, out interviewOutput) error {
	if out.Type != nil {
		it.Type = out.Type.Value
	}
	class, err := state.ClassOf(it)
	if err != nil {
		return fmt.Errorf("the issue's class once typed: %w", err)
	}
	to, trigger := state.Clarifying, state.TriggerMoreQuestions
	if out.Outcome == outcomeSpec {
		to, trigger = state.SpecReview, state.TriggerSpecWritten
	}
	if _, err := state.Lookup(state.Clarifying, to, class, trigger); err != nil {
		return fmt.Errorf("an issue of type %q with a parent %t can't move: %w", it.Type, it.HasParent, err)
	}
	if out.Outcome == outcomeQuestions && !interviewerClass(it) {
		return fmt.Errorf("a round would leave an issue of type %q with a parent %t, which the interviewer doesn't run on, "+
			"so nothing would read its answers", it.Type, it.HasParent)
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

// Apply writes r's question round, its typed outcome (plan #6, Q1a) or its
// spec (assumption 9), which Check passed, on r.S. Each write is checked
// first, so it can run again after a crash part way (SPEC §5.2):
//  1. If the output sets a type, the App adds its type:* label and removes
//     the issue's other type:* labels (ivSetType), which updates r.S.Type,
//     so an untyped issue is typed before its move. A move written already
//     means the type was set before it, so this is skipped, and a type the
//     owner set since stands.
//  2. A spec is written into the body (specWriteBody), unless the move is
//     written already, so an edit made since stands.
//  3. The move, keyed by the run, with seen= and the run's model= and
//     effort= (Result.Move):
//     - A question round's is clarifying → clarifying on more-questions.
//     It carries round= and questions= (plan #6, assumption 8), and its
//     text is the round in the SPEC §9.1 format, the type line first when
//     it sets one, then the cc line (renderQuestions).
//     - A typed outcome's is the same move with neither, and its text is
//     the type line, that a bug or task waits here until phase 3, and the
//     cc line.
//     - A spec's is clarifying → spec-review on spec-written, posted with
//     the next revision (PostRevision), so its marker carries rev= and
//     body=, the hash of the body GitHub returned. Its text is the type
//     line when it sets one, the agent's message (the answers to the
//     revision notes, or what changed), the decisions added without asking,
//     and the cc line (renderSpecNote). On a dirty body, PostRevision posts
//     no revision, and the move's text says why.
//
// A type:* label can be left without its type line: when the comment after
// it fails, and a trusted comment comes before the retry, the result is
// dropped (Runs.apply) with the label added. The next run's preamble says
// issuedog set the type, so it may change it, which posts the line again,
// but if it keeps the type, no line names it. That's left as it is: the
// line's reason was the dropped run's, so only an invented one could stand
// in for it, and the label's timeline event shows the App set it.
func (iv *Interviewer) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply interviewer run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	out, err := decodeInterview(r.Output)
	if err != nil {
		return wrap(err)
	}
	if !slices.Contains([]string{outcomeQuestions, outcomeTyped, outcomeSpec}, out.Outcome) {
		return wrap(fmt.Errorf("unknown outcome %q", out.Outcome))
	}
	moved := state.Derive(r.S, a.AppSlug, r.Dirt.Has).Moved(state.Key{Field: "run", ID: r.RunID})
	if out.Type != nil && !moved {
		if err := ivSetType(ctx, a, r.S, out.Type.Value); err != nil {
			return wrap(err)
		}
	}
	notify := iv.Daemon.Config.Notify
	if out.Outcome == outcomeSpec {
		if out.Spec == nil {
			return wrap(errors.New("the spec outcome has no spec"))
		}
		if !moved {
			if err := specWriteBody(ctx, a, r.S, r.Dirt, renderSpec(*out.Spec)); err != nil {
				return wrap(err)
			}
		}
		m := r.Move(state.SpecReview, state.TriggerSpecWritten)
		m.Text = renderSpecNote(out, notify)
		if err := a.PostRevision(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return nil
	}
	// Once the move is written, its round is among these, so the number is
	// one too high, but Transition then writes no text or fields.
	round := len(ivRoundCounts(interviewerRounds(r.S, a.AppSlug, r.Dirt.Has))) + 1
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

// specWriteBody makes spec, the rendered spec, s's whole body, with s's
// title left as it is (plan #6, assumption 9). On a dirty body (d), the
// dirty note stays at the top, naming the same login, so the body stays
// dirty and nothing approves it (SPEC §5.1). A body that has that text
// already, as after a crash just after the edit, isn't edited again. s
// takes the body GitHub returns, which the revision's hash is taken from,
// and the App as its last editor.
func specWriteBody(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, spec string) error {
	body := spec
	if by, dirty := d[state.BodyID]; dirty {
		var err error
		if body, err = note(state.BodyID, by, spec); err != nil {
			return fmt.Errorf("write the spec into the body: %w", err)
		}
	}
	if specSameText(s.Body, body) {
		logr.Debugf("%s/%s#%d: the body has the spec already", a.Owner, a.Repo, s.Number)
		return nil
	}
	logr.Infof("%s/%s#%d: writing the spec into the body", a.Owner, a.Repo, s.Number)
	is, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.Number, gogithub.UpdateIssueRequest{Body: &body})
	if err != nil {
		return fmt.Errorf("write the spec into the body: %w", err)
	}
	s.Body, s.BodyEdit = is.GetBody(), github.Edit{By: github.Author{Login: a.AppSlug + "[bot]", Type: "Bot"}, At: is.GetUpdatedAt().Time}
	return nil
}

// specSameText reports whether a and b are the same text, with CRLF and CR
// line endings taken as LF, as state.BodyHash takes them.
func specSameText(a, b string) bool {
	lf := strings.NewReplacer("\r\n", "\n", "\r", "\n")
	return lf.Replace(a) == lf.Replace(b)
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

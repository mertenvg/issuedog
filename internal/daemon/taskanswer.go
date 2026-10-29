package daemon

import (
	"context"
	"fmt"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file sends a task back to ready once the owner answers the
// implementer's question round (SPEC §7.2, §9.1), and finds those answers
// again for the implementer's next run (implementerAnswers).

// AnswerVisit ends a visit to an open task at clarifying whose round is
// the implementer's: the loop calls it after FlagVisit. The latest
// transition, looking through a hold the task went back from by /resume
// (ivHoldLatest), as the digest's question item does, is the implementer's
// question round (implRoundMove). Then the task's answers are its trusted,
// clean comments above the round bound that aren't commands, as step 1
// gives them (state.Issue.Inputs): no App comment, no other bot's and no
// untrusted one counts. With any, it moves the task clarifying → ready on
// answered, keyed cmd=<the first answer's ID>, with seen= the newest
// answer's ID, so the next round's input starts after them. Step 1 has
// 👍'd each answer already (Step.handle), and 👍s one a marker names that
// it hasn't, so the move adds none (NoAck).
//
// At any other stage, or with no answer, it writes nothing. A move the
// table lacks, as for an issue retyped from a task since its round, is
// reported once by Transition, and the issue stays. Once the move is
// written, the task is at ready, so a crash before its labels leaves an
// Unfinished disagreement, which CheckLabels finishes (SPEC §5.2).
func (st *Step) AnswerVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("answer on %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	if s.Closed {
		return nil
	}
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	round := ivHoldLatest(is, true)
	if is.Stage != state.Clarifying || !implRoundMove(round) {
		return nil
	}
	counts, err := st.counted(ctx, s, d)
	if err != nil {
		return wrap(err)
	}
	answers := is.Inputs(s, func(c github.Comment) bool { return counts[c.ID] })
	if len(answers) == 0 {
		return nil
	}
	first, newest := answers[0], answers[len(answers)-1]
	m := Move{To: state.Ready, Trigger: state.TriggerAnswered, Key: state.CmdKey(first.ID),
		Fields: map[string]string{state.FieldSeen: strconv.FormatInt(newest.ID, 10)},
		Text:   taskAnswerText(round.Fields["round"], len(answers)), NoAck: true}
	err = st.Transition(ctx, s, d.Has, m)
	switch {
	case StateError(err):
		return nil // reported once by Transition
	case err != nil:
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: round %s has %d answers, so the task goes back to %s, keyed cmd=%d", st.Owner, st.Repo, s.Number,
		round.Fields["round"], len(answers), state.Ready, first.ID)
	return nil
}

// taskAnswerText is the comment on a task's move back to ready, for n
// answers to round, the round= of the implementer's round move, which the
// App wrote, or a round with no number if it isn't one.
func taskAnswerText(round string, n int) string {
	what, them := "an answer", "it"
	if n > 1 {
		what, them = "answers", "them"
	}
	if _, err := strconv.Atoi(round); err != nil {
		return fmt.Sprintf("The implementer's question round has %s, so the task goes back to `ready`. Its next run gets %s.", what, them)
	}
	return fmt.Sprintf("Round %s has %s, so the task goes back to `ready`. The implementer's next run gets %s.", round, what, them)
}

// answerMove reports whether m is the move AnswerVisit writes: clarifying
// → ready, keyed by a comment's cmd=, and by no run.
func answerMove(m github.Marker) bool {
	_, keyed := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
	return m.Kind == "transition" && keyed && m.Fields["run"] == "" &&
		state.Stage(m.Fields["from"]) == state.Clarifying && state.Stage(m.Fields["to"]) == state.Ready
}

// implementerAnswers returns the answers the task in state is went back
// to ready on, for the implementer's next run (SPEC §12): when the latest
// move to ready was an answer's (answerMove), and no implementer run has
// moved the task since, the comments on s from that move's key, the first
// answer, to its seen, the newest, that are clean, trusted and not
// commands, as AnswerVisit counted them. Otherwise, as on a task the
// scheduler picked or the owner moved with no question, it's none. Trust
// is tc's, so it reads no more than the trust of their authors.
func implementerAnswers(ctx context.Context, s *github.Snapshot, d Dirt, tc trustCache, is state.Issue) ([]github.Comment, error) {
	var answered github.Marker
	for _, m := range is.Markers {
		switch {
		case answerMove(m):
			answered = m
		case m.Kind == "transition" && (state.Stage(m.Fields["to"]) == state.Ready ||
			m.Fields["run"] != "" && state.Stage(m.Fields["from"]) == state.InProgress):
			answered = github.Marker{}
		}
	}
	first, ok := state.Key{Field: "cmd", ID: answered.Fields["cmd"]}.Comment()
	if !ok {
		return nil, nil
	}
	newest, err := strconv.ParseInt(answered.Fields[state.FieldSeen], 10, 64)
	if err != nil {
		newest = first // a move written with no seen= read only its key
	}
	var answers []github.Comment
	for _, c := range s.Comments {
		if c.ID < first || c.ID > newest || d.Has(c.ID) {
			continue
		}
		if _, cmd := state.ParseCommand(c.Body); cmd {
			continue
		}
		trusted, err := tc.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("the answers' trust, comment %d: %w", c.ID, err)
		}
		if trusted {
			answers = append(answers, c)
		}
	}
	return answers, nil
}

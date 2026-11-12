package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file applies the interviewer's results (SPEC §9.1): a question
// round, the typing of a bug or task, the spec (§11.1), which is
// written into the body and posted as a revision, a bug's or an owner
// task's body (§11.2), written into the body before its move to ready,
// and at the clarifying after a hold, unchanged (#189) and plan (#294).

// Check checks r's output after its schema (checkInterview, SPEC §9.1's
// Checks) against the facts of r.S, the issue read again
// (ivFacts). A broken rule is a failed attempt, with nothing written. So is
//   - an issue the interviewer no longer runs on (ivStillRuns), such as one
//     the owner gave a type of a story with a parent, or a parent, during
//     the run, away from the clarifying after a hold from a stage of its
//     plan (ivRunsOn): Pending is false for it from then on, so no run
//     follows; and a typed outcome on a bug or a task, which gets only
//     questions and task (ivOutcomes), such as one the owner typed during
//     the run;
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
	if !ivStillRuns(it, f.Origin, out, f.MayType) {
		return &daemonCause{fmt.Errorf("the interviewer no longer runs on #%d, with type %q and a parent %t, for its %s: "+
			"it runs only on an epic, a story with no parent or an untyped issue, "+
			"on a bug or a task with questions and task only, "+
			"and after a hold from planning, plan-review or planned on a story with a parent", r.S.Number, it.Type, it.HasParent, out.Outcome)}
	}
	if err := checkInterview(out, f); err != nil {
		return err
	}
	if err := ivMoveFits(it, out, f.Origin); err != nil {
		return &daemonCause{fmt.Errorf("the interviewer's %s on #%d: %w", out.Outcome, r.S.Number, err)}
	}
	return nil
}

// ivStillRuns reports whether the interviewer still runs on an issue like
// it, read again after a run whose output is out, with origin the stage it
// came from to the clarifying after a hold, or "" (ivRunsOn), but for a
// typed outcome on a bug or a task (ivNewTask), whose outcomes are only
// questions and task (ivOutcomes); or whether out, a typed outcome, made
// it the task or bug it is with its own type, set before a crash: the
// type is out's, and the App set it (may, as typing says), so it isn't the
// owner's retype. Its move, the typed one, is an entry for the run that
// writes its task (Pending). Only a typed outcome sets a type the
// interviewer runs on with other outcomes, so a story with a parent never
// passes, whoever typed it.
func ivStillRuns(it state.Item, origin state.Stage, out interviewOutput, may bool) bool {
	if out.Outcome == outcomeTyped && out.Type != nil && may && strings.EqualFold(it.Type, out.Type.Value) {
		return true
	}
	return ivRunsOn(it, origin) && (out.Outcome != outcomeTyped || !ivNewTask(it))
}

// ivMoveFits returns an error unless out's move fits an issue like it once
// out's type, if it sets one, is set: the table has the move for its class
// (SPEC §7.2), clarifying → spec-review on spec-written for a spec, or on
// spec-rewritten at the clarifying after a hold, whose origin is set
// (ivHoldOrigin); clarifying → ready on task-written for a task, which only
// a bug or a task the owner filed has before ready, or on task-rewritten
// at the clarifying after a hold; clarifying → origin on discussion-no-change for
// unchanged; clarifying → planning on plan-changed for plan, which only an
// epic or a story has (#294); and clarifying → clarifying on
// more-questions otherwise. A question round must also leave the issue one
// the interviewer runs on (ivRunsOn), or no run would read its answers: a
// story with a parent waits at clarifying for good, but at the clarifying
// after a hold from a stage of its plan (SPEC §9.1). out has passed
// checkInterview, so its type is a known one.
func ivMoveFits(it state.Item, out interviewOutput, origin state.Stage) error {
	if out.Type != nil {
		it.Type = out.Type.Value
	}
	class, err := state.ClassOf(it)
	if err != nil {
		return fmt.Errorf("the issue's class once typed: %w", err)
	}
	to, trigger := ivHoldMove(out.Outcome, origin)
	if _, err := state.Lookup(state.Clarifying, to, class, trigger); err != nil {
		return fmt.Errorf("an issue of type %q with a parent %t can't move: %w", it.Type, it.HasParent, err)
	}
	if out.Outcome == outcomeQuestions && !ivRunsOn(it, origin) {
		return fmt.Errorf("a round would leave an issue of type %q with a parent %t, which the interviewer doesn't run on, "+
			"so nothing would read its answers", it.Type, it.HasParent)
	}
	return nil
}

// facts are what Check holds r's output to, from r.S:
//   - the rounds so far, from the round= and questions= of the
//     interviewer's clean round moves (interviewerRounds), up to the
//     latest round of every stage: a planner round's number counts 0
//     there, so a decision names none of its questions, and the next
//     round's number follows it (ivFactRounds, #253);
//   - whether the interviewer may set the type, from the timeline
//     (typing), and whether the issue has one;
//   - the logins a spec's decision may name;
//   - loop.max_questions_per_round, and notify for the cc line;
//   - the stage the issue came from, at the clarifying after a hold
//     (ivHoldOrigin), which an unchanged or plan outcome needs;
//   - the issue's class, which a plan outcome needs, and on a story with
//     a parent (SPEC §9.1) refuses a spec, typed and any type: whatever
//     typing says, it may not set the type there;
//   - the option count of each question its round comments read back
//     with, which a decision's letter must name one of (ivWordingOptions);
//   - config's scopes, which a task's must be one of, and the parent and
//     the priority its header names (taskParent, priority), for its length.
func (iv *Interviewer) facts(ctx context.Context, r Result) (interviewFacts, error) {
	d := iv.Daemon
	a := d.Step.Applier
	_, may, err := iv.typing(ctx, r.S, &ivTimeline{a: a, number: r.S.Number})
	if err != nil {
		return interviewFacts{}, err
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	class, err := state.ClassOf(is.Item)
	if err != nil {
		class = 0 // a type the table doesn't know: plan is refused, and Check refuses the run
	}
	if class == state.ClassStory {
		may = false
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
		Rounds:       ivFactRounds(r.S, a.AppSlug, r.Dirt.Has),
		Trusted:      logins,
		Notify:       d.Config.Notify,
		Origin:       ivHoldOrigin(is),
		Class:        class,
		Options:      ivWordingOptions(r.S, a.AppSlug, r.Dirt.Has),
		Scopes:       d.Config.Scopes,
		Parent:       taskParent(r.S),
		Priority:     "p" + strconv.Itoa(priority(r.S)),
	}, nil
}

// ivWordingOptions returns the option count of each question of the
// interviewer's rounds on s (interviewerRounds), by its ID, as its clean
// round comment reads back in the SPEC §9.1 format (waitQuestions), which
// the digest copies its questions from too (#229). A round whose comment
// doesn't read back, such as one whose format has changed, adds nothing,
// so its questions' letters go unchecked, as they did before. dirty is as
// for state.Derive.
func ivWordingOptions(s *github.Snapshot, appSlug string, dirty func(int64) bool) map[questionID]int {
	opts := map[questionID]int{}
	for _, r := range interviewerRounds(s, appSlug, dirty) {
		heading := fmt.Sprintf("### Questions for #%d (round %d)", s.Number, r.Number)
		round, n := map[questionID]int{}, 0
		for _, l := range waitQuestions(waitBody(s, r.Comment), heading, r.Questions, "") {
			switch {
			case l.Question > 0:
				n = l.Question
			case l.Option != "" && n > 0:
				round[questionID{r.Number, n}]++
			}
		}
		maps.Copy(opts, round)
	}
	return opts
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

// ivFactRounds is interviewFacts.Rounds for s: the question count of each
// interviewer round by its number (ivRoundCounts), with a 0 for each
// round of another stage's, the planner's, up to the latest round of
// every stage (allRounds). So the interviewer's decisions cover only its
// own questions, and its next round, len(counts)+1, is the next of every
// stage's (nextRound, SPEC §9.1). With no planner rounds, it's
// ivRoundCounts of the interviewer's alone.
func ivFactRounds(s *github.Snapshot, appSlug string, dirty func(int64) bool) []int {
	counts := ivRoundCounts(interviewerRounds(s, appSlug, dirty))
	for next := nextRound(allRounds(s, appSlug, dirty)); len(counts) < next-1; {
		counts = append(counts, 0)
	}
	return counts
}

// Apply writes r's question round, its typed outcome (SPEC §9.1), its
// spec (§11.1) or its task (§11.2), which Check passed, on r.S. Each write
// is checked first, so it can run again after a crash part way (SPEC §5.2):
//  1. If the output sets a type, the App adds its type:* label and removes
//     the issue's other type:* labels (ivSetType), which updates r.S.Type,
//     so an untyped issue is typed before its move. A move written already
//     means the type was set before it, so this is skipped, and a type the
//     owner set since stands.
//  2. A spec is written into the body (specWriteBody), unless the move is
//     written already, so an edit made since stands. It is the App's full
//     rewrite, so a body that was dirty is clean from then on (#169,
//     SPEC §5.1). A task's scope:* label is set, then the task is written
//     into the body (taskWriteBody) the same way.
//  3. The move, keyed by the run, with seen= and the run's model= and
//     effort= (Result.Move):
//     - A question round's is clarifying → clarifying on more-questions.
//     It carries round= and questions= (SPEC §5, §9.1), and its
//     text is the round in the SPEC §9.1 format, the type line first when
//     it sets one, then the cc line (renderQuestions).
//     - A typed outcome's is the same move with neither, and its text is
//     the type line, that the next run writes it up as a task
//     (ivTypedWaits), and the cc line.
//     - A spec's is clarifying → spec-review on spec-written, posted with
//     the next revision (PostRevision), so its marker carries rev= and
//     body=, the hash of the body GitHub returned. Its text is the line
//     naming whose edit the spec replaced, if the body was dirty
//     (rewriteReplacedText), the type line when it sets one, the agent's
//     message (the answers to the revision notes, or what changed), and
//     the decisions added without asking (renderSpecNote), then the
//     revision, and last the cc line.
//     The body is clean after step 2, so it is a normal revision.
//     - A task's is clarifying → ready on task-written (SPEC §7.2), or on
//     task-rewritten at the clarifying after a hold, with no revision: a
//     task gets none. Before it, unless the move is written already, the
//     App sets the task's scope:* label (taskSetScope), then writes the
//     task into the body (taskWriteBody), the App's full rewrite, as a
//     spec's. Its text is the line naming whose edit the task replaced, if
//     the body was dirty (rewriteReplacedText), then ivTaskWritten.
//     - At the clarifying after a discussion hold (#189), an unchanged
//     outcome's is clarifying → origin on discussion-no-change, with no
//     revision: the latest one, if the body still has its hash, still
//     stands, and says so (ivHoldStanding), so a 👍 or /approve on it
//     approves it (SPEC §7.4). Its text is renderUnchanged's. A spec's
//     there is on spec-rewritten.
//     - There, after a hold from planning, plan-review or planned, a plan
//     outcome's is clarifying → planning on plan-changed (#294), with no
//     revision: the planner posts the next plan revision from the move's
//     message, the discussion's owner input and the /revise it names
//     (plannerHoldOf). It carries replan=1, which tells it from
//     unchanged's move after a hold from planning, the same clarifying →
//     planning (SPEC §5). Its text is renderPlanChange's.
//  4. At the clarifying after a hold, every move carries origin
//     (ivHoldOrigin, SPEC §6.4), and one after a hold from spec-review or
//     plan-review names the /revise that waited through it, if the run
//     read one
//     (ivHoldRevise, as the run's seen bounds it), with cmd= beside run=
//     (Move.check), then 👍s it, so it's processed and isn't applied
//     again. Once the move is written, its marker says where it went and
//     which /revise it named, so a replay only finishes the labels and the
//     👍, and nothing it reads since can change them.
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
	if !slices.Contains([]string{outcomeQuestions, outcomeTyped, outcomeSpec, outcomeUnchanged, outcomePlan, outcomeTask}, out.Outcome) {
		return wrap(fmt.Errorf("unknown outcome %q", out.Outcome))
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	moved := is.Moved(state.Key{Field: "run", ID: r.RunID})
	hold, err := iv.holdFields(ctx, a, r, is, moved)
	if err != nil {
		return wrap(err)
	}
	if out.Type != nil && !moved {
		if err := ivSetType(ctx, a, r.S, out.Type.Value); err != nil {
			return wrap(err)
		}
	}
	notify := iv.Daemon.Config.Notify
	to, trigger := ivHoldMove(out.Outcome, hold.origin)
	if out.Outcome == outcomeUnchanged {
		m := hold.set(r.Move(to, trigger))
		m.Text = renderUnchanged(out, to, ivHoldStanding(r.S, a.AppSlug, r.Dirt, to), notify)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return hold.ack(ctx, a, wrap)
	}
	if out.Outcome == outcomePlan {
		m := hold.set(r.Move(to, trigger))
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields[ivReplanField] = "1" // tells it from unchanged after a hold from planning (ivPlanChanged)
		m.Text = renderPlanChange(out, notify)
		if !moved {
			logr.Infof("%s/%s#%d: the discussion changes the plan, so it goes to planning", a.Owner, a.Repo, r.S.Number)
		}
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return hold.ack(ctx, a, wrap)
	}
	if out.Outcome == outcomeSpec {
		if out.Spec == nil {
			return wrap(errors.New("the spec outcome has no spec"))
		}
		m := hold.set(r.Move(to, trigger))
		m.Text = renderSpecNote(out)
		dirt := r.Dirt
		if !moved {
			replaced, err := specWriteBody(ctx, a, r.S, r.Dirt, r.RunID, renderSpec(*out.Spec))
			if err != nil {
				return wrap(err)
			}
			if replaced != "" {
				m.Text = strings.TrimSpace(rewriteReplacedText("spec", replaced) + "\n\n" + m.Text)
			}
			// The body is the App's full rewrite now, so it's clean
			// (Applier.Dirty), and the spec gets a normal revision.
			dirt = maps.Clone(r.Dirt)
			delete(dirt, state.BodyID)
		}
		if err := a.PostRevision(ctx, r.S, dirt.Has, m, notify); err != nil {
			return wrap(err)
		}
		return hold.ack(ctx, a, wrap)
	}
	if out.Outcome == outcomeTask {
		if out.Task == nil {
			return wrap(errors.New("the task outcome has no task"))
		}
		m := hold.set(r.Move(to, trigger))
		m.Text = ivTaskWritten
		if !moved {
			if err := taskSetScope(ctx, a, r.S, out.Task.Scope); err != nil {
				return wrap(err)
			}
			replaced, err := taskWriteBody(ctx, a, r.S, r.Dirt, r.RunID, renderTask(*out.Task, taskParent(r.S), "p"+strconv.Itoa(priority(r.S))))
			if err != nil {
				return wrap(err)
			}
			if replaced != "" {
				m.Text = rewriteReplacedText("task", replaced) + "\n\n" + m.Text
			}
		}
		// The move holds no revision, unlike a spec's, and the body has no
		// marker but its rewrite marker, which no state is read from, so
		// whether r.Dirt still counts the body dirty changes nothing here.
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return hold.ack(ctx, a, wrap)
	}
	// Once the move is written, its round is among these, so the number is
	// one too high, but Transition then writes no text or fields. The
	// planner's rounds share the numbering (#253).
	round := nextRound(allRounds(r.S, a.AppSlug, r.Dirt.Has))
	m := hold.set(r.Move(to, trigger))
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
	return hold.ack(ctx, a, wrap)
}

// specWriteBody makes spec, the rendered spec, s's whole body, with s's
// title left as it is (SPEC §11.1), and returns who made the
// body it replaced dirty, if anyone did, from d. The App's full rewrite
// replaces all text in the body, so it cleans it (#169, SPEC §5.1): the body
// starts with the rewrite marker (state.RewriteKind), with run= and, on a
// dirty body, replaced=, naming who made it dirty, and has no dirty note.
// A body that is the spec already, as the App's last edit, isn't edited
// again. That is the case after a crash just after the edit, when its
// marker names this run: its replaced= is returned, so the move's text is
// the same. When an earlier run wrote it, the body was clean from then on
// (Applier.Dirty), so this run replaced nothing. s takes the body GitHub
// returns, which the revision's hash is taken from, and the App as its
// last editor.
func specWriteBody(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, run, spec string) (string, error) {
	return rewriteBody(ctx, a, s, d, run, spec, "spec", false)
}

// taskWriteBody makes task, the rendered task (renderTask), s's whole
// body, with s's title left as it is (SPEC §11.2), as specWriteBody does
// for a spec: the App's full rewrite, with the rewrite marker, so a dirty
// body is clean from then on, and the same check before it, so a replay
// writes nothing twice. A body whose rewrite marker names run, as the App's
// last edit, isn't edited again either, though it isn't task: this run
// wrote it before a crash, and its header's parent or priority changed
// since, so it stays as the run wrote it. It returns who made the body it
// replaced dirty, if anyone did, from d.
func taskWriteBody(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, run, task string) (string, error) {
	return rewriteBody(ctx, a, s, d, run, task, "task", true)
}

// rewriteBody is specWriteBody and taskWriteBody: it makes text, the
// rendered what, "spec" or "task", s's whole body, the App's full rewrite
// (SPEC §5.1), and returns who made the body it replaced dirty, if anyone
// did, from d. A body whose last edit is the App's rewrite isn't edited
// again when it's text already, or, with byRun, when its marker names run.
// Either way, the marker's replaced= is returned when it names run, and
// else nothing.
func rewriteBody(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, run, text, what string, byRun bool) (string, error) {
	if m, rest, ok := state.ParseRewrite(s.Body); ok && state.IsApp(s.BodyEdit.By, a.AppSlug) {
		if specSameText(rest, text) || byRun && m.Fields["run"] == run {
			logr.Debugf("%s/%s#%d: the body has the %s already", a.Owner, a.Repo, s.Number, what)
			if m.Fields["run"] != run {
				return "", nil
			}
			return m.Fields["replaced"], nil
		}
	}
	replaced := ""
	if by, dirty := d[state.BodyID]; dirty {
		replaced = rewriteLogin(by, s.BodyEdit, a.AppSlug)
	}
	marker := github.Marker{Kind: state.RewriteKind, Fields: map[string]string{"run": run}}
	if replaced != "" {
		marker.Fields["replaced"] = replaced
	}
	f, err := github.FormatMarker(marker)
	if err != nil {
		return "", fmt.Errorf("write the %s into the body: %w", what, err)
	}
	body := f + "\n" + text
	if replaced != "" {
		logr.Infof("%s/%s#%d: writing the %s into the body, replacing %s's edit, which wasn't approved", a.Owner, a.Repo, s.Number, what, replaced)
	} else {
		logr.Infof("%s/%s#%d: writing the %s into the body", a.Owner, a.Repo, s.Number, what)
	}
	is, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.Number, gogithub.UpdateIssueRequest{Body: &body})
	if err != nil {
		return "", fmt.Errorf("write the %s into the body: %w", what, err)
	}
	s.Body, s.BodyEdit = is.GetBody(), github.Edit{By: github.Author{Login: a.AppSlug + "[bot]", Type: "Bot"}, At: is.GetUpdatedAt().Time}
	return replaced, nil
}

// rewriteLogin is by, a login as Dirt gives it for a dirty body whose last
// edit is last, for a rewrite marker's replaced=: by if the App may name
// them (dirtyNamed), as /approve's rejections do, else "unknown", as Dirt
// names an editor GitHub doesn't name. Dirt's login may come from a note
// the author typed on content nobody edited, which can name anyone, so it
// isn't the App's text (#638).
func rewriteLogin(by string, last github.Edit, appSlug string) string {
	if !dirtyNamed(by, last, appSlug) {
		return "unknown"
	}
	return by
}

// rewriteReplacedText is the line a spec's or a task's move adds when
// what, "spec" or "task", replaced a body that replaced, a login
// rewriteLogin gave, made dirty. The login is defused, so it mentions
// nobody, and "unknown", an editor the App doesn't name, is "someone", as
// /approve's rejections say (#638).
func rewriteReplacedText(what, replaced string) string {
	by := "someone"
	if replaced != "unknown" {
		by = "`" + defuse(replaced) + "`"
	}
	return fmt.Sprintf("This %s replaces the whole issue description, with the edit by %s that wasn't approved, "+
		"so the description is clean again.", what, by)
}

// specSameText reports whether a and b are the same text, with CRLF and CR
// line endings taken as LF, as state.BodyHash takes them.
func specSameText(a, b string) bool {
	lf := strings.NewReplacer("\r\n", "\n", "\r", "\n")
	return lf.Replace(a) == lf.Replace(b)
}

// ivTypedWaits follows the type line of a bug or task the interviewer
// typed (SPEC §9.1): its move is an entry, so the next run comes at once
// (Pending), and writes it up as a task, or asks first.
const ivTypedWaits = "I'll write it up as a task next."

// ivTaskWritten is the text of a task's move to ready (SPEC §9.1), after
// the line naming whose edit it replaced, if any.
const ivTaskWritten = "I've written this issue up as a task, in the description above, so it's `ready` for the implementer."

// taskSetScope makes scope:<scope> the only scope:* label on s, as
// ivSetType does for the type: it adds it, then removes the others s had
// before the add, ignoring case. One the add's answer shows for the first
// time was set since s was read, so it's left for the next CheckLabels.
// scope is one from config's list (checkTask).
func taskSetScope(ctx context.Context, a *Applier, s *github.Snapshot, scope string) error {
	want := "scope:" + scope
	known := slices.Clone(s.Labels)
	if err := a.addLabel(ctx, s, want); err != nil {
		return fmt.Errorf("set the scope %s: %w", scope, err)
	}
	for _, l := range known {
		if !strings.HasPrefix(strings.ToLower(l), "scope:") || strings.EqualFold(l, want) {
			continue
		}
		if err := a.removeLabel(ctx, s, l); err != nil {
			return fmt.Errorf("set the scope %s: %w", scope, err)
		}
	}
	return nil
}

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

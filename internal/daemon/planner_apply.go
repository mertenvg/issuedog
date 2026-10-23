package daemon

import (
	"context"
	"fmt"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file checks and applies the planner's results (#247, SPEC §9.1): a
// question round, or a plan, posted as a plan revision that keeps the
// plan's data in its plan-data marker (SPEC §5, §11.4).

// planChecked is the sub-issue count Check read for run, and for a
// revision the children it read, so Apply, which follows it on the same
// issue read, renders the validation line from the facts it checked, with
// no second read: a closed child is read once per run.
type planChecked struct {
	run   string
	subs  int
	prior *planPrior // nil for a first plan
}

// Check checks r's output after its schema against the facts of r.S, the
// issue read again (facts), with checkPlan: every 11.3 rule, each
// broken one named. A broken rule is a failed attempt, with nothing
// written. So is an issue the planner no longer runs on, read again:
//   - one that isn't an epic or a story (plannerClass), such as one the
//     owner retyped to a task during the run;
//   - one planned before whose approved plan revision can't be read back
//     (planPriorOf), so a revision can't be checked.
//
// An issue planned before gets a revision, checked against its approved
// plan's children (checkStatuses). A read that fails while gathering the
// facts, of the sub-issues or the children, is a *checkReadError, so the
// result is kept and applied again on the next pass.
func (p *Planner) Check(ctx context.Context, r Result) error {
	out, err := decodePlan(r.Output)
	if err != nil {
		return err
	}
	is := state.Derive(r.S, p.Daemon.Step.Applier.AppSlug, r.Dirt.Has)
	class, err := state.ClassOf(is.Item)
	if err != nil || !plannerClass(is.Item) {
		return &daemonCause{fmt.Errorf("the planner no longer runs on #%d, with type %q and a parent %t: "+
			"it runs only on an epic or a story", r.S.Number, is.Item.Type, is.Item.HasParent)}
	}
	if _, err := planPriorOf(r.S, p.Daemon.Step.Applier.AppSlug, is, r.Dirt.Has); err != nil {
		// The error is the daemon's, or the App's plan data's decoder's.
		return &daemonCause{fmt.Errorf("the planner no longer runs on #%d: it was planned before, but %w", r.S.Number, err)}
	}
	read, err := p.read(ctx, r, is, false)
	if err != nil {
		return &checkReadError{fmt.Errorf("the planner's facts on #%d: %w", r.S.Number, err)}
	}
	return checkPlan(out, p.facts(r, is, class, read))
}

// read reads what a plan on r's issue, in state is, is checked against,
// once: the count of its sub-issues before the plan's are created
// (plannerSubIssues), or for a revision, of an issue planned before, its
// approved plan's children, with that count (planPrior.read). Check reads
// it, and keeps it for run r.RunID, and Apply takes it from there, with
// cached, on the same run. Apply reads it only if Check didn't, as when
// it's called on its own.
func (p *Planner) read(ctx context.Context, r Result, is state.Issue, cached bool) (planChecked, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached && p.checked.run == r.RunID && r.RunID != "" {
		return p.checked, nil
	}
	a := p.Daemon.Step.Applier
	prior, err := planPriorOf(r.S, a.AppSlug, is, r.Dirt.Has)
	if err != nil {
		return planChecked{}, err
	}
	read := planChecked{run: r.RunID, prior: prior}
	if prior != nil {
		if err := prior.read(ctx, p.Daemon.Step, r.S); err != nil {
			return planChecked{}, err
		}
		read.subs = prior.Subs
	} else {
		subs, err := plannerSubIssues(ctx, a, r.S.Number)
		if err != nil {
			return planChecked{}, fmt.Errorf("its sub-issues: %w", err)
		}
		read.subs = len(subs)
	}
	p.checked = read
	return read, nil
}

// facts are what checkPlan holds r's output to on an issue in state is, of
// class class, with the sub-issues and, for a revision, the children read
// read:
//   - the config's scopes, plan.max_tasks, models.allowed,
//     loop.max_questions_per_round and notify;
//   - the round a question round is asked as, the next of every stage's
//     (nextRound, #253);
//   - the issue's priority, p2 with none, as the preamble gives it;
//   - the plan revision a plan is posted as, one more than the latest
//     (state.LatestRevision of plans, which number on their own), and
//     whether it skips review (planSkipsReview), and the move's marker
//     fields (Result.Move): they change the comment, so the length rule
//     counts it as it's posted, but for the run's ID (planCommentBody);
//   - for a revision, the approved plan and its children (planPrior).
func (p *Planner) facts(r Result, is state.Issue, class state.Class, read planChecked) planFacts {
	c, a := p.Daemon.Config, p.Daemon.Step.Applier
	latest, _ := state.LatestRevision(r.S, a.AppSlug, state.PlanRevision, r.Dirt.Has)
	return planFacts{
		Issue:        r.S.Number,
		Class:        class,
		Scopes:       c.Scopes,
		MaxTasks:     int(c.Plan.MaxTasks),
		Models:       c.Models,
		MaxQuestions: int(c.Loop.MaxQuestionsPerRound),
		Round:        nextRound(allRounds(r.S, a.AppSlug, r.Dirt.Has)),
		SubIssues:    read.subs,
		Priority:     "p" + strconv.Itoa(priority(r.S)),
		Notify:       c.Notify,
		Rev:          latest.Rev + 1,
		NoReview:     planSkipsReview(c.Plan.Review, class, is),
		Fields:       r.Move(state.PlanReview, state.TriggerPlanValid).Fields,
		Prior:        read.prior,
	}
}

// planSkipsReview reports whether a plan on an issue in state is, of class
// class, skips review and goes straight to planned (SPEC §11.3, §7.2):
// only a story's, only when review, plan.review, is false, only
// when the move that put the issue at planning (planEntry) carries no
// origin, so the planning doesn't follow a discussion hold (SPEC §6.4),
// and only for a first plan, of an issue never planned before. After a
// discussion hold, and for any revision, however the issue came back to
// planning (SPEC §7.2), the owner reviews the plan, whatever plan.review
// says: a revision can close issues.
func planSkipsReview(review bool, class state.Class, is state.Issue) bool {
	return !review && class&state.AnyStory != 0 && planEntry(is).Fields["origin"] == "" && !plannedBefore(is)
}

// planEntry returns the move that put is at planning: its latest
// transition that bounds a round, looking through the planner's question
// rounds (planRoundMove), which keep the issue at planning, and through
// holds, as ivHoldLatest does with entries: a move into held, a held →
// held marker, and a /resume back to the stage the issue was at, which
// carries the round's bound (seen), keep the move from before the hold.
// A move out of held that is an entry of its own, carrying at or by, isn't
// looked through.
func planEntry(is state.Issue) github.Marker {
	var last github.Marker
	for _, m := range is.Markers {
		if _, ok := state.RoundBound(m, 0); !ok {
			continue
		}
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		out := from == state.Held && to != state.Held
		resume := out && m.Fields[state.FieldSeen] != "" && m.Fields["at"] == "" && m.Fields["by"] == "" &&
			to == state.Stage(last.Fields["to"])
		if (to == state.Held || resume || planRoundMove(m)) && last.Kind != "" {
			continue
		}
		last = m
	}
	return last
}

// Apply writes r's question round or plan, which Check passed, on r.S.
// Each write is checked first, so it can run again after a crash part way
// (SPEC §5.2). Once the run's move is written, it only finishes the move's
// labels (Transition), at the stage the move put the issue at, so it
// renders and reads nothing, and nothing it reads since can change where
// the move went. Otherwise it renders only output checkPlan passes, with
// the facts Check read (facts, subIssues), so the plan comment's
// validation line, which states the rules as facts, is true:
//   - A question round's move is planning → planning on plan-questions,
//     with round= and questions=, the round the next of every stage's
//     (#253), and its text the round in the SPEC §9.1 format, then the cc
//     line (renderQuestions).
//   - A plan's move is planning → plan-review on plan-valid, or, when it
//     skips review (planSkipsReview), which a revision never does,
//     planning → planned on plan-no-review. It is one comment: the plan (renderPlan), the move's
//     transition marker, with rev=, the plan-data marker (planDataMarker),
//     then the plan revision marker, rev= one more than the latest plan
//     revision, with the hash of everything above it (Move.PlanRev).
//
// Each move is r.Move's, keyed by the run, with seen= and the run's
// model= and effort=.
func (p *Planner) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply planner run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	out, err := decodePlan(r.Output)
	if err != nil {
		return wrap(err)
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	if is.Moved(state.Key{Field: "run", ID: r.RunID}) {
		logr.Debugf("%s/%s#%d: run %s's move is written already, so only its labels are left", a.Owner, a.Repo, r.S.Number, r.RunID)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, r.Move(is.Stage, state.TriggerPlanValid)); err != nil {
			return wrap(err)
		}
		return nil
	}
	class, err := state.ClassOf(is.Item)
	if err != nil {
		return wrap(fmt.Errorf("the issue's class: %w", err))
	}
	read, err := p.read(ctx, r, is, true)
	if err != nil {
		return wrap(err)
	}
	f := p.facts(r, is, class, read)
	// Nothing is rendered from output that fails: the comment's text
	// states what checkPlan checked.
	if err := checkPlan(out, f); err != nil {
		return wrap(err)
	}
	if out.Outcome == outcomeQuestions {
		m := r.Move(state.Planning, state.TriggerPlanQuestions)
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields["round"], m.Fields["questions"] = strconv.Itoa(f.Round), strconv.Itoa(len(out.Questions))
		m.Text = renderQuestions(interviewOutput{Questions: out.Questions}, r.S.Number, f.Round, f.Notify)
		logr.Infof("%s/%s#%d: posting planner round %d, of %d questions", a.Owner, a.Repo, r.S.Number, f.Round, len(out.Questions))
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return nil
	}
	data, err := planDataMarker(*out.Plan)
	if err != nil {
		return wrap(err)
	}
	to, trigger := state.PlanReview, state.TriggerPlanValid
	if f.NoReview {
		to, trigger = state.Planned, state.TriggerPlanNoReview
	}
	m := r.Move(to, trigger)
	m.Text, m.PlanRev, m.Markers = renderPlan(*out.Plan, f.view()), f.Rev, []github.Marker{data}
	logr.Infof("%s/%s#%d: posting plan revision %d, moving to %s", a.Owner, a.Repo, r.S.Number, f.Rev, to)
	if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
		return wrap(err)
	}
	return nil
}

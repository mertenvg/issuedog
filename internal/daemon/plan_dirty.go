package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// planDirtyText is the App's line above the move to plan-review it records
// again (planDirtyRestore).
const planDirtyText = "A comment of mine was edited without approval, and it held this issue's move to `plan-review`, so I've recorded the move again here. " +
	"There's no clean plan revision to approve. To get a new plan, post `/revise <notes>`, or any other comment."

// planDirtyKeyPrefix starts the event key of a move planDirtyRestore
// records again, before the ID of the dirty comment that hid it. No label
// event's ID, which an override's event= names, looks like it.
const planDirtyKeyPrefix = "plan-dirty-"

// planDirtyRestore records again, in an App comment of its own, the App's
// move to plan-review that a dirty plan comment hid (#315), before the
// label rules act on events: a plan comment carries its own move to
// plan-review (plan #247, assumption 1), and once anyone but the App edits
// it, it's dirty, and its markers no longer count (SPEC §5.1). The label
// rules would then put the label back to planning, with a disagreement,
// and the planner would run again, where plan #248's Q1a has the daemon
// post nothing. It reports whether it recorded the move, so the events are
// left alone: the label shows plan-review already.
//
// It acts only on a disagreement that is no override and not the App's
// own write cut short (Unfinished), where all of these hold:
//   - the one stage label is stage:plan-review. The label rules have just
//     judged it, so an untrusted change of it is reverted already, and a
//     trusted one would be an override: the App set it, with its own move
//     to plan-review (discRestore relies on the same);
//   - the markers left, in clean content, show planning, the stage that
//     move was from;
//   - an App comment after the latest move they show is dirty, so it may
//     be the one that held the move to plan-review.
//
// So it stands in for no other stage's move, and the dirty comment's text,
// whoever edited it, is never read: the move it records is the one the
// label shows. A write collaborator who isn't trusted holds the issue
// instead (discRestore, asked first), as for any edit of theirs to the
// App's content (SPEC §6.4). The move is keyed event=plan-dirty-<id>, by
// the newest such dirty comment, and its seen is that comment's ID, so a
// comment posted after the plan comment is a revision request at
// plan-review, as before the edit. It's planning → plan-review in the
// table, the planner's own move (state.TriggerPlanValid), so a type that
// has no such move gets no record, and the disagreement stands, as before.
// The move's comment is added to s, so Derive reads plan-review from it in
// the same pass, and a replay finds it and records nothing more. Should
// anyone edit it, it's dirty in turn, and recorded again, by its own ID.
// No plan revision is in it, so nothing is approvable until a new plan:
// the digest item (waitPlanApprovalLines) and /approve's reply
// (planRejection) say so.
func (a *Applier) planDirtyRestore(ctx context.Context, s *github.Snapshot, d Dirt, events []github.LabelEvent) (bool, error) {
	if !slices.ContainsFunc(events, func(ev github.LabelEvent) bool { return ev.Kind == github.LabelDisagreement }) ||
		slices.ContainsFunc(events, func(ev github.LabelEvent) bool { return ev.Kind == github.LabelOverride || ev.Unfinished }) {
		return false, nil
	}
	var stages []state.Stage
	for _, l := range s.Labels {
		if !isStageLabel(l) {
			continue
		}
		stage, ok := state.ParseLabel(l)
		if !ok {
			return false, nil
		}
		stages = append(stages, stage)
	}
	if len(stages) != 1 || stages[0] != state.PlanReview {
		return false, nil
	}
	is := state.Derive(s, a.AppSlug, d.Has)
	if is.Stage != state.Planning {
		return false, nil
	}
	var hid int64
	for _, c := range s.Comments {
		if c.ID > is.TransitionComment && state.IsApp(c.Author, a.AppSlug) && d.Has(c.ID) {
			hid = c.ID
		}
	}
	if hid == 0 {
		return false, nil
	}
	m := Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "event", ID: planDirtyKeyPrefix + strconv.FormatInt(hid, 10)},
		Fields: map[string]string{state.FieldSeen: strconv.FormatInt(hid, 10)}, Text: planDirtyText}
	// Checked here, so a move the table lacks reports no state error of
	// its own: the disagreement stands for it, as before.
	if why, err := noRow(is, m); err != nil {
		if !StateError(err) {
			return false, fmt.Errorf("record again the move to plan-review that comment %d hid: %w", hid, err)
		}
		logr.Warnf("%s/%s#%d: not recording again the move to plan-review that comment %d hid, as %s: %v", a.Owner, a.Repo, s.Number, hid, why, err)
		return false, nil
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return false, fmt.Errorf("record again the move to plan-review that comment %d hid: %w", hid, err)
	}
	logr.Infof("%s/%s#%d: recorded again the move to plan-review that comment %d, made dirty, hid", a.Owner, a.Repo, s.Number, hid)
	return true, nil
}

package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// planDirtyText is the App's line above the move to plan-review it records
// again (reviewDirtyRestore).
const planDirtyText = "A comment of mine was edited without approval, and it held this issue's move to `plan-review`, so I've recorded the move again here. " +
	"There's no clean plan revision to approve. To get a new plan, post `/revise <notes>`, or any other comment."

// specDirtyText is the App's line above the move to spec-review it records
// again (reviewDirtyRestore). The record is no spec revision: ReviewVisit
// posts the next one, as for any stale or hidden one, unless the body is
// dirty or was last edited by someone untrusted (SPEC §7.4).
const specDirtyText = "A comment of mine was edited without approval, and it held this issue's move to `spec-review`, so I've recorded the move again here. " +
	"It's no revision of the spec: to approve the spec, approve the next revision I post. To send it back, post `/revise <notes>`, or any other comment."

// planDirtyKeyPrefix and specDirtyKeyPrefix start the event key of a move
// reviewDirtyRestore records again, before the ID of the dirty comment
// that hid it. No label event's ID, which an override's event= names,
// looks like either.
const (
	planDirtyKeyPrefix = "plan-dirty-"
	specDirtyKeyPrefix = "spec-dirty-"
)

// reviewDirtyMove is a stage run's move to a review stage, which the App
// comment of the stage's revision holds, that reviewDirtyRestore records
// again once that comment is dirty: the move, its trigger in the table,
// the record's key prefix and its text.
type reviewDirtyMove struct {
	from, to state.Stage
	trigger  state.Trigger
	prefix   string
	text     string
}

// reviewDirtyMoves are the moves reviewDirtyRestore records again: the
// interviewer's spec, posted with its spec revision (SPEC §7.4), and the
// planner's plan comment, a plan revision (SPEC §11.4).
var reviewDirtyMoves = []reviewDirtyMove{
	{state.Clarifying, state.SpecReview, state.TriggerSpecWritten, specDirtyKeyPrefix, specDirtyText},
	{state.Planning, state.PlanReview, state.TriggerPlanValid, planDirtyKeyPrefix, planDirtyText},
}

// reviewDirtyRestore records again, in an App comment of its own, the
// App's move to a review stage that a dirty App comment hid (#315, #447),
// before the label rules act on events: a spec revision comment carries
// the interviewer's move to spec-review, and a plan comment the planner's
// move to plan-review (SPEC §7.4, §11.4), and once anyone but the App
// edits it, it's dirty, and its markers no longer count (SPEC §5.1). The
// label rules would then put the label back to the stage before, with a
// disagreement, and the stage would run again, where SPEC §7.4 has the
// daemon post nothing. It reports whether it recorded the move, so the
// events are left alone: the label shows the review stage already.
//
// It acts only on a disagreement that is no override and not the App's
// own write cut short (Unfinished), where all of these hold:
//   - the one stage label is spec-review or plan-review. The label rules
//     have just judged it, so an untrusted change of it is reverted
//     already, and a trusted one would be an override: the App set it,
//     with its own move there (discRestore relies on the same);
//   - the markers left, in clean content, show the stage that move was
//     from, clarifying or planning, but not the clarifying after a
//     discussion hold, whose moves carry origin and may name the /revise
//     that waited through it (SPEC §6.4), which only the dirty text holds;
//   - an App comment after the latest move they show is dirty, so it may
//     be the one that held the move.
//
// So it stands in for no other stage's move, and the dirty comment's text,
// whoever edited it, is never read: the move it records is the one the
// label shows. A write collaborator who isn't trusted holds the issue
// instead (discRestore, asked first), as for any edit of theirs to the
// App's content (SPEC §6.4). The move is keyed event=<prefix><id>, by the
// newest such dirty comment. Its seen is the bound of the latest move the
// markers show, the most the hidden move's seen can be known to cover, so
// a comment no run read stays a revision request at the review stage, as
// before the edit: one posted during the run, before the dirty comment, as
// well as one after it (SPEC §7.4, #318). One the App 👍'd as an answer
// before the dirty comment was posted may be one the run read, and isn't
// one (reviewDirtyRead). The move is in the table's row for the stage
// run's own move, so a type that has no such move gets no record, and the
// disagreement stands, as before. The move's comment is added to s, so
// Derive reads the review stage from it in the same pass, and a replay
// finds it and records nothing more. Should anyone edit it, it's dirty in
// turn, and recorded again, by its own ID. No revision is in it: at
// plan-review nothing is approvable until a new plan, as the digest item
// (waitPlanApprovalLines) and /approve's reply (planRejection) say, and at
// spec-review ReviewVisit posts the next spec revision.
func (a *Applier) reviewDirtyRestore(ctx context.Context, s *github.Snapshot, d Dirt, events []github.LabelEvent) (bool, error) {
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
	if len(stages) != 1 {
		return false, nil
	}
	i := slices.IndexFunc(reviewDirtyMoves, func(rm reviewDirtyMove) bool { return rm.to == stages[0] })
	if i < 0 {
		return false, nil
	}
	rm := reviewDirtyMoves[i]
	is := state.Derive(s, a.AppSlug, d.Has)
	if is.Stage != rm.from || is.AfterHold() {
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
	m := Move{To: rm.to, Trigger: rm.trigger, Key: state.Key{Field: "event", ID: rm.prefix + strconv.FormatInt(hid, 10)},
		Fields: map[string]string{state.FieldSeen: strconv.FormatInt(is.Round, 10)}, Text: rm.text}
	// Checked here, so a move the table lacks reports no state error of
	// its own: the disagreement stands for it, as before.
	if why, err := noRow(is, m); err != nil {
		if !StateError(err) {
			return false, fmt.Errorf("record again the move to %s that comment %d hid: %w", rm.to, hid, err)
		}
		logr.Warnf("%s/%s#%d: not recording again the move to %s that comment %d hid, as %s: %v", a.Owner, a.Repo, s.Number, rm.to, hid, why, err)
		return false, nil
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return false, fmt.Errorf("record again the move to %s that comment %d hid: %w", rm.to, hid, err)
	}
	logr.Infof("%s/%s#%d: recorded again the move to %s that comment %d, made dirty, hid", a.Owner, a.Repo, s.Number, rm.to, hid)
	return true, nil
}

// reviewDirtyRead reports whether c, a revision request the App 👍'd at
// up, may be input that the stage run read whose move a dirty comment hid
// (reviewDirtyRestore): a clean record of that move names a comment that
// c was posted before, and the App 👍'd c before that comment was posted,
// as step 1 does an answer before the run starts (SPEC §7.4). One 👍'd
// after it was 👍'd during a read that didn't list the move yet, so no run
// read it, and it's a request (#318). With the hiding comment gone, it may
// have been read. GitHub's times are to the second, so a 👍 in that
// comment's second counts as after it.
func reviewDirtyRead(s *github.Snapshot, is state.Issue, c github.Comment, up time.Time) bool {
	for _, m := range is.Markers {
		if m.Kind != "transition" {
			continue
		}
		var id string
		for _, rm := range reviewDirtyMoves {
			if v, ok := strings.CutPrefix(m.Fields["event"], rm.prefix); ok && m.Fields["to"] == string(rm.to) {
				id = v
			}
		}
		hid, err := strconv.ParseInt(id, 10, 64)
		if err != nil || c.ID >= hid {
			continue // no record, or c came after the comment it names
		}
		i := slices.IndexFunc(s.Comments, func(x github.Comment) bool { return x.ID == hid })
		if i < 0 || up.Before(s.Comments[i].CreatedAt) {
			return true
		}
	}
	return false
}

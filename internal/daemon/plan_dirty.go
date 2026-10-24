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

// planResumeDirtyText is the App's line above the /resume move back to
// plan-review from held that it records again (reviewDirtyRestore). The
// dirty comment is an App comment after the latest plan revision, so that
// revision is hidden (state.PlanHidden), as /approve's reply says.
const planResumeDirtyText = "A comment of mine was edited without approval, and it held this issue's move back to `plan-review` from `held`, so I've recorded the move again here. " +
	"That comment may have hidden a newer plan revision, so I can't approve the plan. To get a new plan, post `/revise <notes>`, or any other comment."

// planDirtyKeyPrefix and specDirtyKeyPrefix start the event key of a move
// reviewDirtyRestore records again, before the ID of the dirty comment
// that hid it. No label event's ID, which an override's event= names,
// looks like either.
const (
	planDirtyKeyPrefix = "plan-dirty-"
	specDirtyKeyPrefix = "spec-dirty-"
)

// reviewDirtyMove is an App move to a review stage, held by an App
// comment, that reviewDirtyRestore records again once that comment is
// dirty: the move, its trigger in the table, the record's key prefix and
// its text.
type reviewDirtyMove struct {
	from, to state.Stage
	trigger  state.Trigger
	prefix   string
	text     string
}

// reviewDirtyMoves are the moves reviewDirtyRestore records again: the
// interviewer's spec, posted with its spec revision (SPEC §7.4), the
// planner's plan comment, a plan revision (SPEC §11.4), and /resume's
// move back to plan-review from held, in the App's comment applying it
// (SPEC §6.4). No two have the same from and to.
var reviewDirtyMoves = []reviewDirtyMove{
	{state.Clarifying, state.SpecReview, state.TriggerSpecWritten, specDirtyKeyPrefix, specDirtyText},
	{state.Planning, state.PlanReview, state.TriggerPlanValid, planDirtyKeyPrefix, planDirtyText},
	{state.Held, state.PlanReview, state.TriggerResume, planDirtyKeyPrefix, planResumeDirtyText},
}

// reviewDirtyRestore records again, in an App comment of its own, the
// App's move to a review stage that a dirty App comment hid (#315, #447,
// #485), before the label rules act on events: a spec revision comment
// carries the interviewer's move to spec-review, a plan comment the
// planner's move to plan-review (SPEC §7.4, §11.4), and the comment
// applying a /resume its move back to plan-review from held (SPEC §6.4),
// and once anyone but the App edits it, it's dirty, and its markers no
// longer count (SPEC §5.1). The label rules would then put the label back
// to the stage before, with a disagreement, and the stage would run again,
// where SPEC §7.4 has the daemon post nothing, or the issue would be held
// again, its /resume used. It reports whether it recorded the move, so the
// events are left alone: the label shows the review stage already.
//
// It acts only on a disagreement that is no override and not the App's
// own write cut short (Unfinished), where all of these hold:
//   - the one stage label is spec-review or plan-review. The label rules
//     have just judged it, so an untrusted change of it is reverted
//     already, and a trusted one would be an override: the App set it,
//     with its own move there (discRestore relies on the same);
//   - the markers left, in clean content, show the stage that move was
//     from, clarifying, planning or held, but not the clarifying after a
//     discussion hold, whose moves carry origin and may name the /revise
//     that waited through it (SPEC §6.4), which only the dirty text holds.
//     Held must be from that review stage, its first hold's at, which a
//     /resume goes back to (heldStackResume);
//   - an App comment after the latest move they show is dirty, so it may
//     be the one that held the move.
//
// So it stands in for no other stage's move, and the dirty comment's text,
// whoever edited it, is never read: the move it records is the one the
// label shows. A write collaborator who isn't trusted holds the issue
// instead (discRestore, asked first), as for any edit of theirs to the
// App's content (SPEC §6.4). The move is keyed event=<prefix><id>, by the
// newest such dirty comment. A stage run's move's seen is the bound of the
// latest move the markers show, the most the hidden move's seen can be
// known to cover, so a comment no run read stays a revision request at
// the review stage, as before the edit: one posted during the run, before
// the dirty comment, as well as one after it (SPEC §7.4, #318). One the
// App 👍'd as an answer before the run's move was posted is one the run
// read, and isn't one (reviewDirtyRead). A /resume's move carries the
// bound from before the hold (state.HeldStack.Seen), as resume's does, so
// its seen is the hidden move's own. It also carries resolved, the time of
// the oldest dirty App comment after the latest move the markers show, at
// or before the hidden move's own, which bounds the discussion triggers
// (discFloor): so a trigger after the /resume still holds the issue, as
// under the hidden move, whose cmd named the /resume, and not only one
// after the record (#566). The time is in the record, so it stands once
// the dirty comment is deleted. The move is in the table's row for
// the hidden move, so a type that has no such move gets no record, and the
// disagreement stands, as before. The move's comment is added to s, so
// Derive reads the review stage from it in the same pass, and a replay
// finds it and records nothing more. Should anyone edit it, it's dirty in
// turn, and recorded again, by its own ID. No revision is in it: at
// plan-review nothing is approvable until a new plan, as the digest item
// (waitPlanApprovalLines) and /approve's reply (planRejection) say, since
// the dirty comment is the plan comment or an App comment after the latest
// plan revision, and at spec-review ReviewVisit posts the next spec
// revision.
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
	is := state.Derive(s, a.AppSlug, d.Has)
	i := slices.IndexFunc(reviewDirtyMoves, func(rm reviewDirtyMove) bool { return rm.to == stages[0] && rm.from == is.Stage })
	if i < 0 || is.AfterHold() || is.Stage == state.Held && is.HeldStack.At != stages[0] {
		return false, nil
	}
	rm := reviewDirtyMoves[i]
	var hid int64
	var first time.Time // when the oldest such dirty comment was posted
	for _, c := range s.Comments {
		if c.ID > is.TransitionComment && state.IsApp(c.Author, a.AppSlug) && d.Has(c.ID) {
			if hid == 0 {
				first = c.CreatedAt
			}
			hid = c.ID
		}
	}
	if hid == 0 {
		return false, nil
	}
	fields := map[string]string{state.FieldSeen: strconv.FormatInt(is.Round, 10)}
	if rm.from == state.Held {
		// The bound from before the hold, as resume's move carries, and
		// the oldest dirty comment's time as resolved, at or before the
		// hidden move's own, as the discussion floor (discFloor).
		fields = state.HeldStackFields(map[string]string{resolveField: strconv.FormatInt(first.Unix(), 10)}, is.HeldStack.Seen)
	}
	m := Move{To: rm.to, Trigger: rm.trigger, Key: state.Key{Field: "event", ID: rm.prefix + strconv.FormatInt(hid, 10)},
		Fields: fields, Text: rm.text}
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
// up, is input that the stage run read whose move a dirty comment hid
// (reviewDirtyRestore), and if so, when the clean record of that move was
// posted, which stands for the run's use of c (usedAt): an edit since gets
// the edited reply (SPEC §6.2). It is when such a record names a comment
// that c was posted before, and the App 👍'd c before any dirty App
// comment after c, up to that one, was posted. One of those is the one
// that held the run's move, so the 👍 came before the move. Step 1 👍s an
// answer before the run starts, which reads it, or while a run's result
// waits to be applied, after the run read up to its lease's seen: then the
// apply drops the result, as the issue has new input, if step 1's read or
// its own lists the answer (Runs.newInput, SPEC §8.2). So no run but one
// that read c moved the issue after that 👍. One 👍'd after was 👍'd during
// a read that didn't list the move yet, so no run read it, and it's a
// request (#318). With the hiding comment gone, and no other dirty one
// after c, it may have been read. GitHub's times are to the second, so a
// 👍 in that comment's second counts as after it. A /resume's record
// carries the hidden move's own seen, so it tells nothing more.
func reviewDirtyRead(s *github.Snapshot, appSlug string, d Dirt, c github.Comment, up time.Time) (time.Time, bool) {
	for _, r := range s.Comments {
		if d.Has(r.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(r.Author, r.Body, appSlug) {
			hid, ok := reviewDirtyHid(m)
			if !ok || c.ID >= hid {
				continue // no record of a run's move, or c came after the comment it names
			}
			i := slices.IndexFunc(s.Comments, func(x github.Comment) bool {
				return x.ID > c.ID && x.ID <= hid && state.IsApp(x.Author, appSlug) && d.Has(x.ID)
			})
			if i < 0 || up.Before(s.Comments[i].CreatedAt) {
				return r.CreatedAt, true
			}
		}
	}
	return time.Time{}, false
}

// reviewDirtyHid returns the ID of the dirty comment that m, a transition
// marker, names as the one that hid a stage run's move to a review stage,
// which reviewDirtyRestore recorded again, or reports that m is no such
// record.
func reviewDirtyHid(m github.Marker) (int64, bool) {
	if m.Kind != "transition" {
		return 0, false
	}
	for _, rm := range reviewDirtyMoves {
		v, ok := strings.CutPrefix(m.Fields["event"], rm.prefix)
		if !ok || rm.from == state.Held || m.Fields["from"] != string(rm.from) || m.Fields["to"] != string(rm.to) {
			continue
		}
		if hid, err := strconv.ParseInt(v, 10, 64); err == nil {
			return hid, true
		}
	}
	return 0, false
}

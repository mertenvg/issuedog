package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Closes, reopens and the closed stages (SPEC §6.4, §7.2, plan #87 rev 2,
// Q5 and assumption 12, T6). Only a trusted user, the App or a merged pull
// request may close an issue in the pipeline, and only a trusted user may
// reopen one. The newest closed or reopened event decides:
//   - An accepted close stands: the issue gets the closed stage its close
//     names, stage:completed or stage:discarded (state.ClosingStage), in
//     place of its other stage labels, with no comment (closingJudge).
//   - An untrusted close is reverted: the App reopens the issue, leaving its
//     stage label alone. A write collaborator's close is a discussion
//     trigger, keyed event=<its timeline event id> (#87's assumption 3), so
//     step 1 holds the reopened issue for it, with its notice, as for any
//     trigger (discFind, closingTriggers). Anyone else's close gets a short
//     comment with a revert marker, keyed by the close's event, as a label
//     revert does, before the reopen (closingRevertClose).
//   - An untrusted reopen of an issue with a closed stage is reverted: the
//     App closes it again with the same state_reason, with a revert
//     comment, and the label stays (closingReopen).
//
// A merged pull request's close is a closed event that names a commit
// (commit_id), whose actor is whoever merged it, and that commit's pull
// requests list a merged one (github.ClosingMerged): one request, asked
// only for a close nobody trusted made, and remembered per commit
// (closingMerged). The fake serves both (githubtest.CloseByCommit), and
// SPEC §16 gets the live check of that shape (T10).
//   - A trusted reopen moves the issue to held, closed → held, at the stage
//     the latest transition marker shows, as an owner hold that /resume
//     here ends, and the closed stage goes (closingReopen).
//
// Which closed issues are read. The poll lists every issue, closed ones
// too, and keeps a closed issue whose close is still to judge
// (closingList): one in the pipeline, with a stage label, or one this
// daemon held with a stage label before it closed, whose close isn't
// explained by a trusted or App event. A close is explained when:
//   - this daemon judged it, and the listing still shows the closed_at and
//     the stage labels it left (closingSeen): a comment or any other change
//     to a closed issue reads nothing;
//   - or the App closed it again itself, and the listing shows what it left
//     (closingAgain);
//   - or, on a start's first poll, the issue has exactly one stage label,
//     a closed stage: only an accepted close leaves one, which an earlier
//     run judged.
//
// Judging reads the timeline once (github.ClosingTimeline). A closed stage
// on the issue stands when the newest close is accepted and the stage's
// standing labeled event is the App's or a trusted user's
// (github.StandingLabelChange), so a trusted user's choice of the other
// closed stage by hand stands too. So a closed issue with a closed stage is
// never read again, on a pass or a start, until its closed_at or its stage
// labels change within a run. Gap, for SPEC §16 (T10, #194): what happens
// while no daemon runs to a closed issue that has one closed stage isn't
// found, as the first poll of a start can't tell who set it, nor whether
// the issue was reopened and closed again meanwhile. So a closed stage set
// by someone untrusted stands, with or without their close, and so does
// an untrusted close after a trusted reopen of a judged issue.
//
// The first start relabels the closed issues from before T6, most of them
// at stage:pr-open: each is judged once, with one timeline read and at
// most two label writes, the closed stage added and the old stage
// removed, as setStage writes them. A pass judges at most closingBatch,
// the newest closes first, so a fresh close isn't stuck behind the
// backlog, and the label writes stay well under GitHub's secondary rate
// limits. Each write is checked first, and the due issues are found from
// the listing again on each start, so a crash part way resumes: an issue
// with its closed stage is judged, and one left with two stage labels is
// judged again, which removes the old one.
//
// The digest isn't in the pipeline: the poll records it before any of
// this, and digestOpen judges its close and reopen, where the App's own
// close counts as no one's and nothing holds. They share closingTrusted,
// and the rule that a close's event must be from closed_at on.
//
// Closes aren't routed through discRestore: a close hides no marker, and
// the App's revert leaves the stage label alone, so the hold is step 1's
// ordinary discussion hold. Of the two gaps the notes on #190 name, an
// interrupted write that left two stage labels is finished by the label
// rules before step 1 holds anything, so it posts no disagreement; and a
// hold the table lacks, as for a type the pipeline doesn't know, holds
// nothing, as for any trigger (discussion): the reopen stands, and the
// type error stands for the hold.

// discClose is a close of the issue: key <timeline event id>, as a rename's
// (closingFromKey tells the two apart).
const discClose discKind = "close"

// closingBatch is how many closed issues a pass judges at most
// (closingPass).
const closingBatch = 20

// closingErrClosedAgain is wrapped by CheckLabels' error when it closed an
// issue again, after an untrusted reopen (closingReopen): nothing more is
// acted on in the issue's pass, and Iterate reads it again next pass, as
// for ErrLabelsHeld.
var closingErrClosedAgain = errors.New("closed again, as only a code owner can reopen it")

// closingListed is a closed issue as the poll listed it, for what a judged
// close leaves (closingSeen): its closed_at and its stage labels, in lower
// case, sorted and joined by ",".
type closingListed struct {
	closed time.Time
	stages string
}

// closingStages returns the stage labels among labels, in lower case,
// sorted.
func closingStages(labels []string) []string {
	var stages []string
	for _, l := range labels {
		if isStageLabel(l) {
			stages = append(stages, strings.ToLower(l))
		}
	}
	slices.Sort(stages)
	return stages
}

// closingLabel reports whether label is a closed stage's.
func closingLabel(label string) bool {
	_, ok := state.ParseClosingLabel(label)
	return ok
}

// closingList takes a closed issue from the poll's listing: it is due to
// be judged (closingDue), unless its close is explained (see above). start
// is set on a start's first poll, and had when this daemon held the issue,
// open, with a stage label, so one whose closer also took its stage label
// off is judged too.
func (d *Daemon) closingList(is *gogithub.Issue, start, had bool) {
	if d.closingSeen == nil {
		d.closingSeen, d.closingDue = map[int]closingListed{}, map[int]*gogithub.Issue{}
	}
	n := is.GetNumber()
	var labels []string
	for _, l := range is.Labels {
		labels = append(labels, l.GetName())
	}
	stages := closingStages(labels)
	listed := closingListed{closed: is.GetClosedAt().Time, stages: strings.Join(stages, ",")}
	if again, ok := d.Step.closingAgain[n]; ok {
		// The App closed it again (closingCloseAgain): judged, as it left it.
		delete(d.Step.closingAgain, n)
		d.closingSeen[n] = again
	}
	seen, known := d.closingSeen[n]
	switch {
	case known && seen == listed:
		delete(d.closingDue, n)
	case len(stages) == 0 && !had && !known:
		// Never in the pipeline. One due already, listed again, as at the
		// poll's cursor, after the first listing dropped its snapshot,
		// stays due.
	case start && len(stages) == 1 && closingLabel(stages[0]):
		d.closingSeen[n] = listed // judged on an earlier run
		delete(d.closingDue, n)
	default:
		d.closingDue[n] = is
	}
}

// closingForget forgets issue n's close, once it's listed open, or gone.
func (d *Daemon) closingForget(n int) {
	delete(d.closingSeen, n)
	delete(d.Step.closingAgain, n)
	delete(d.closingDue, n)
}

// closingPass judges the closes due (closingList), at most closingBatch,
// the newest closes first. One judged stays judged (closingSeen); one
// reverted, reopened, is read again this pass, so step 1 holds it for a
// write collaborator's close. One whose event isn't written yet, or whose
// judging failed, stays due for the next pass. With nothing due, it makes
// no request.
func (d *Daemon) closingPass(ctx context.Context) error {
	if len(d.closingDue) == 0 {
		return nil
	}
	due := slices.Collect(maps.Keys(d.closingDue))
	slices.SortFunc(due, func(x, y int) int {
		if c := d.closingDue[y].GetClosedAt().Compare(d.closingDue[x].GetClosedAt().Time); c != 0 {
			return c
		}
		return cmp.Compare(y, x)
	})
	if len(due) > closingBatch {
		logr.Infof("%s/%s: %d closed issues have a close to judge, so %d wait for the next passes", d.Step.Owner, d.Step.Repo, len(due), len(due)-closingBatch)
		due = due[:closingBatch]
	}
	var errs []error
	for _, n := range due {
		done, reopened, err := d.closingJudge(ctx, d.closingDue[n])
		if gone(err) {
			logr.Infof("%s/%s#%d is gone, deleted or transferred, so its close is forgotten: %v", d.Step.Owner, d.Step.Repo, n, err)
			d.closingForget(n)
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !done {
			continue
		}
		delete(d.closingDue, n)
		if reopened {
			d.stale[n] = true
		}
	}
	return errors.Join(errs...)
}

// closingJudge judges the close of is, a closed issue as listed, from its
// timeline: done unless the close's event isn't written yet, which is
// looked at again next pass, and reopened if it reverted the close. An
// accepted close sets the closed stage it names, unless a closed stage the
// App or a trusted user set stands alone already (closingExplained).
func (d *Daemon) closingJudge(ctx context.Context, is *gogithub.Issue) (done, reopened bool, err error) {
	a, n := d.Step.Applier, is.GetNumber()
	wrap := func(err error) error { return fmt.Errorf("the close of %s/%s#%d: %w", a.Owner, a.Repo, n, err) }
	timeline, closings, err := a.Client.ClosingTimeline(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return false, false, wrap(err)
	}
	// The newest close or reopen must be the close that closed the issue:
	// one from closed_at on, as for the digest (digestOpen). GitHub may
	// write it late.
	k := len(closings) - 1
	if k < 0 || closings[k].Reopen || closings[k].At.Before(is.GetClosedAt().Time) {
		logr.Infof("%s/%s#%d has no closed event yet, so it's looked at again next pass", a.Owner, a.Repo, n)
		return false, false, nil
	}
	cl := closings[k]
	if cl.StateReason == "" {
		cl.StateReason = is.GetStateReason()
	}
	accepted, merged, err := a.closingAccepted(ctx, d.Step.Trust, cl)
	if err != nil {
		return false, false, wrap(err)
	}
	if !accepted {
		reverted, err := d.closingRevertClose(ctx, n, cl)
		if err != nil {
			return false, false, wrap(err)
		}
		return reverted, reverted, nil
	}
	s := &github.Snapshot{Number: n, Closed: true}
	for _, l := range is.Labels {
		s.Labels = append(s.Labels, l.GetName())
	}
	explained, err := closingExplained(ctx, timeline, s.Labels, a.AppSlug, d.Step.Trust)
	if err != nil {
		return false, false, wrap(err)
	}
	if !explained {
		want := state.ClosingStage(cl.StateReason, merged)
		if err := a.setStage(ctx, s, want); err != nil {
			return false, false, wrap(err)
		}
		logr.Infof("%s/%s#%d: its close by %s stands, so it's at %s", a.Owner, a.Repo, n, cl.By.Login, want)
	}
	d.closingSeen[n] = closingListed{closed: is.GetClosedAt().Time, stages: strings.Join(closingStages(s.Labels), ",")}
	return true, false, nil
}

// closingExplained reports whether labels, a closed issue's, hold exactly
// one stage label, a closed stage, whose standing change on timeline is a
// labeled event, the App's or a trusted user's (github.StandingLabelChange).
func closingExplained(ctx context.Context, timeline []*gogithub.Timeline, labels []string, appSlug string, trust github.Trust) (bool, error) {
	stages := closingStages(labels)
	if len(stages) != 1 || !closingLabel(stages[0]) {
		return false, nil
	}
	e, err := github.StandingLabelChange(ctx, timeline, stages[0], appSlug, trust)
	if err != nil {
		return false, fmt.Errorf("who set %s: %w", stages[0], err)
	}
	return e.GetEvent() == "labeled", nil
}

// closingAccepted reports whether cl, a close, stands: the App's, a trusted
// user's (closingTrusted), or a merged pull request's, merged, which names
// its commit (closingMerged). Only a close nobody trusted made asks about
// its commit.
func (a *Applier) closingAccepted(ctx context.Context, trust github.Trust, cl github.Closing) (accepted, merged bool, err error) {
	if state.IsApp(cl.By, a.AppSlug) {
		return true, false, nil
	}
	ok, err := closingTrusted(ctx, trust, cl.By)
	if err != nil {
		return false, false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	if ok || cl.CommitID == "" {
		return ok, false, nil
	}
	if merged, err = a.closingMerged(ctx, cl.CommitID); err != nil {
		return false, false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	return merged, merged, nil
}

// closingTrusted reports whether a close or reopen by by stands for being
// theirs: a trusted user's (SPEC §6.1), not a bot's, the App's included,
// nor that of someone GitHub doesn't name. The digest's judgement shares
// it (digestTrusted).
func closingTrusted(ctx context.Context, trust github.Trust, by github.Author) (bool, error) {
	if by.Login == "" || by.Type == "Bot" {
		return false, nil
	}
	ok, err := trust.Trusted(ctx, by.Login)
	if err != nil {
		return false, fmt.Errorf("trust %s: %w", by.Login, err)
	}
	return ok, nil
}

// closingMerged reports whether commit sha is a merged pull request's
// (github.ClosingMerged), asked once per commit while the daemon runs: a
// commit's merged pull request never changes, so a held issue visited on
// every pass, with an old close that names a commit, asks nothing again.
func (a *Applier) closingMerged(ctx context.Context, sha string) (bool, error) {
	if merged, ok := a.closingPRs[sha]; ok {
		return merged, nil
	}
	merged, err := a.Client.ClosingMerged(ctx, a.Owner, a.Repo, sha)
	if err != nil {
		return false, fmt.Errorf("merged pull request: %w", err)
	}
	if a.closingPRs == nil {
		a.closingPRs = map[string]bool{}
	}
	a.closingPRs[sha] = merged
	return merged, nil
}

// closingCounts reports whether cl is a discussion trigger, as discFind
// judges one, in its order: a close, open after the floor (open), then
// recorded, with no question, or else by a write collaborator who isn't
// trusted (who.writer, with its memory of logins GitHub has no permission
// record of), and not a merged pull request's.
func (a *Applier) closingCounts(ctx context.Context, cl github.Closing, open discOpen, who discWho) (bool, error) {
	o, recorded := open(strconv.FormatInt(cl.ID, 10), cl.At, 0)
	switch {
	case cl.Reopen || !o:
		return false, nil
	case recorded:
		return true, nil
	}
	w, err := who.writer(ctx, cl.By)
	if err != nil || !w {
		if err != nil {
			return false, fmt.Errorf("who closed it at %d: %w", cl.ID, err)
		}
		return false, nil
	}
	if cl.CommitID == "" {
		return true, nil
	}
	merged, err := a.closingMerged(ctx, cl.CommitID)
	if err != nil {
		return false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	return !merged, nil
}

// closingTriggers returns the close triggers on s, from s.Closings, which
// CheckLabels read with the timeline it reads anyway, so they cost no
// request (discFind): each close closingCounts counts. The App reverted
// each of them when it judged the close (closingRevertClose).
func (a *Applier) closingTriggers(ctx context.Context, s *github.Snapshot, open discOpen, who discWho) ([]discTrigger, error) {
	var found []discTrigger
	for _, cl := range s.Closings {
		ok, err := a.closingCounts(ctx, cl, open, who)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, discTrigger{Kind: discClose, Key: strconv.FormatInt(cl.ID, 10), By: cl.By.Login, At: cl.At})
		}
	}
	return found, nil
}

// closingWhat says what tr, a close, is, for its hold's comment or its
// notice (discWhat). The login isn't the App's text: discLogin defuses it.
func closingWhat(tr discTrigger) string {
	if tr.FromRecord {
		return "a close of this issue, recorded earlier"
	}
	return discLogin(tr.By) + " closed this issue, which I reopened"
}

// closingFromKey is tr, a trigger built from its key alone (discFromKey),
// as a close if its key is a closed event's ID on s: a close and a rename
// are both keyed by their timeline event ID (#87's assumption 3), so only
// the timeline tells them apart, and it only grows. A snapshot CheckLabels
// hasn't checked has no closes, so its keys stay renames, which only words
// the trigger. Only a rename's key is a timeline event ID, so no other
// kind's matches one.
func closingFromKey(s *github.Snapshot, tr discTrigger) discTrigger {
	for _, cl := range s.Closings {
		if !cl.Reopen && strconv.FormatInt(cl.ID, 10) == tr.Key {
			tr.Kind = discClose
		}
	}
	return tr
}

// closingRevertClose reverts cl, an untrusted close of issue n: it reopens
// the issue, leaving its stage label alone. It reads the issue first, and
// reports false, with nothing written, if it's open again already. A
// write collaborator's close is a discussion trigger, so step 1's hold and
// notice record it (closingByWriter); anyone else's gets a comment with a
// revert marker keyed by the close's event first, posted once.
func (d *Daemon) closingRevertClose(ctx context.Context, n int, cl github.Closing) (bool, error) {
	a, trust := d.Step.Applier, d.Step.Trust
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return false, fmt.Errorf("revert: %w", err)
	}
	if !s.Closed {
		return false, nil
	}
	dirt, err := a.Dirty(ctx, s, trust)
	if err != nil {
		return false, fmt.Errorf("revert: %w", err)
	}
	writer, err := a.closingByWriter(ctx, s, dirt, trust, cl)
	if err != nil {
		return false, fmt.Errorf("revert: %w", err)
	}
	if !writer {
		text := fmt.Sprintf("I've reopened this issue: only a code owner, the App or a merged pull request can close it, and %s isn't one.",
			discLogin(cl.By.Login))
		if err := a.closingRevertComment(ctx, s, dirt.Has, cl.ID, text); err != nil {
			return false, fmt.Errorf("revert: %w", err)
		}
	}
	if err := a.Client.ReopenIssue(ctx, a.Owner, a.Repo, n); err != nil {
		return false, fmt.Errorf("revert: %w", err)
	}
	logr.Infof("%s/%s#%d: reopened it, as %s closed it and isn't trusted", a.Owner, a.Repo, n, cl.By.Login)
	return true, nil
}

// closingByWriter reports whether cl, an untrusted close of s, is a
// discussion trigger (closingCounts), with the floor, the records and the
// trust asked as discFind asks them. On an issue that isn't in the
// pipeline (discFloor), or with a trust that knows no write collaborator
// who isn't trusted, none is.
func (a *Applier) closingByWriter(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, cl github.Closing) (bool, error) {
	b, ok := discFloor(s, a.AppSlug, d.Has)
	if !ok || !discKnowsWriters(trust) {
		return false, nil
	}
	return a.closingCounts(ctx, cl, discOpenAfter(b, discRecords(s, a.AppSlug, d.Has)), a.discWhoOf(s, trust))
}

// closingRevertComment posts text with a revert marker keyed by event id
// on s, unless a clean App comment carries that marker already. The text
// is escaped, as it names someone else's login (SPEC §5.1).
func (a *Applier) closingRevertComment(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, id int64, text string) error {
	key := strconv.FormatInt(id, 10)
	if slices.ContainsFunc(state.Derive(s, a.AppSlug, dirty).Markers, func(m github.Marker) bool {
		return m.Kind == "revert" && m.Fields["key"] == key
	}) {
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "revert", Fields: map[string]string{"key": key}})
	if err != nil {
		return fmt.Errorf("revert comment: %w", err)
	}
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("revert comment: %w", err)
	}
	return nil
}

// closingReopen judges the reopen of s, an open issue with a closed stage
// on it (assumption 12), before the label rules act on the disagreement
// the closed stage makes with the latest transition marker. It reports
// whether it handled s, so the label rules' events are left alone. The
// reopen to judge is the newest one that isn't the App's own, which only
// ever reverts an untrusted close, and whose move no marker records:
//   - A trusted user's moves s to held (closed → held), keyed by its event,
//     at the stage the latest transition marker shows, which an accepted
//     close leaves as it was, as an owner hold with no cmd, which /resume
//     here ends (heldStackEnds); the move's labels take the closed stage
//     off. On a held issue, the first hold's at and origin stay. A move the
//     table lacks, as for a type the pipeline doesn't know, holds nothing:
//     the label rules then put the label back, as for any disagreement.
//   - Anyone else's is reverted: a revert comment keyed by its event,
//     posted once, then a close with the state_reason of the close it
//     undid, or the closed stage's, and the error wraps
//     closingErrClosedAgain. The label stays.
//
// It handles nothing when an override stands, as the owner's stage label
// wins, nor when no reopen is left to judge: then the closed stage was set
// on the open issue by hand, which the label rules put back, with a
// disagreement error for a trusted user's (SPEC §5.3), or the App's own
// move was cut short, which they finish. While the newest close or reopen
// is still a close, the reopen's event isn't written yet, so the error
// wraps ErrLabelsHeld, and the issue waits for its next pass: for
// github.HoldBound at most past updated_at, by GitHub's clock, which the
// reopen moved, as for a late label event. After that, it's taken that no
// reopen is left to judge, so a reopen whose event is later still isn't
// held.
func (a *Applier) closingReopen(ctx context.Context, s *github.Snapshot, trust github.Trust, dirty func(int64) bool, events []github.LabelEvent) (bool, error) {
	i := slices.IndexFunc(s.Labels, closingLabel)
	if s.Closed || i < 0 || slices.ContainsFunc(events, func(ev github.LabelEvent) bool { return ev.Kind == github.LabelOverride }) {
		return false, nil
	}
	label := s.Labels[i]
	if k := len(s.Closings); k > 0 && !s.Closings[k-1].Reopen && a.discNow().Before(s.UpdatedAt.Add(github.HoldBound)) {
		return false, fmt.Errorf("%s waits for the event of the reopen: %w", label, ErrLabelsHeld)
	}
	r := len(s.Closings) - 1
	for r >= 0 && (!s.Closings[r].Reopen || state.IsApp(s.Closings[r].By, a.AppSlug)) {
		r--
	}
	if r < 0 {
		return false, nil
	}
	reopen := s.Closings[r]
	key := state.Key{Field: "event", ID: strconv.FormatInt(reopen.ID, 10)}
	is := state.Derive(s, a.AppSlug, dirty)
	if is.Moved(key) {
		return false, nil
	}
	ok, err := closingTrusted(ctx, trust, reopen.By)
	if err != nil {
		return false, fmt.Errorf("reopen %d: %w", reopen.ID, err)
	}
	if !ok {
		return true, a.closingCloseAgain(ctx, s, dirty, reopen, r, label)
	}
	at, origin, seen := is.Stage, state.Stage(""), is.Round
	if is.Stage == state.Held {
		at, origin, seen = is.HeldStack.At, is.HeldStack.Origin, is.HeldStack.Seen
	}
	if !at.Open() || at == state.Held {
		at = state.Clarifying // no stage /resume could restore
	}
	fields := map[string]string{"at": string(at), "reason": "owner"}
	if origin != "" {
		fields["origin"] = string(origin)
	}
	text := fmt.Sprintf("%s reopened this issue, so it's on hold, at `%s`, where it was before it closed. `/resume` here puts it back there.",
		discLogin(reopen.By.Login), at)
	err = a.Transition(ctx, s, dirty, Move{To: state.Held, Trigger: state.TriggerReopened, Key: key,
		Fields: state.HeldStackFields(fields, seen), DiscFrom: state.Closed, Text: text})
	if StateError(err) {
		logr.Warnf("%s/%s#%d: no hold for its reopen %d: %v", a.Owner, a.Repo, s.Number, reopen.ID, err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reopen %d: %w", reopen.ID, err)
	}
	logr.Infof("%s/%s#%d: on hold at %s, as %s reopened it", a.Owner, a.Repo, s.Number, at, reopen.By.Login)
	return true, nil
}

// closingCloseAgain reverts reopen, s.Closings[r], an untrusted reopen of
// s, which has the closed stage label: a revert comment keyed by its
// event, posted once (closingRevertComment), then a close with the
// state_reason of the newest close before it, or label's, and s is closed.
// Its error wraps closingErrClosedAgain once it's closed.
func (a *Applier) closingCloseAgain(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, reopen github.Closing, r int, label string) error {
	reason := "completed"
	if stage, _ := state.ParseClosingLabel(label); stage == state.Discarded {
		reason = "not_planned"
	}
	for j := r - 1; j >= 0; j-- {
		if cl := s.Closings[j]; !cl.Reopen {
			if cl.StateReason != "" {
				reason = cl.StateReason
			}
			break
		}
	}
	text := fmt.Sprintf("I've closed this issue again: only a code owner can reopen it, and %s isn't one.", discLogin(reopen.By.Login))
	if err := a.closingRevertComment(ctx, s, dirty, reopen.ID, text); err != nil {
		return fmt.Errorf("reopen %d: %w", reopen.ID, err)
	}
	is, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.Number, gogithub.UpdateIssueRequest{State: new("closed"), StateReason: &reason})
	if err != nil {
		return fmt.Errorf("close %s/%s#%d again: %w", a.Owner, a.Repo, s.Number, err)
	}
	s.Closed = true
	// The App's own close, with the closed stage judged before, so the next
	// listing of it needs no read (closingList).
	if a.closingAgain == nil {
		a.closingAgain = map[int]closingListed{}
	}
	a.closingAgain[s.Number] = closingListed{closed: is.GetClosedAt().Time, stages: strings.Join(closingStages(s.Labels), ",")}
	logr.Infof("%s/%s#%d: closed it again, as %s, since %s reopened it and isn't trusted", a.Owner, a.Repo, s.Number, reason, reopen.By.Login)
	return fmt.Errorf("%s/%s#%d: %w", a.Owner, a.Repo, s.Number, closingErrClosedAgain)
}

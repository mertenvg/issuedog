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

// Closes, reopens and the closed stages (SPEC §6.4, §7.1, §7.2). Only a
// trusted user, the App or a merged pull request may close an issue in the
// pipeline, and only a trusted user may reopen one. The newest closed or
// reopened event decides:
//   - An accepted close stands: the issue gets the closed stage its close
//     names, stage:completed or stage:discarded (state.ClosingStage), in
//     place of its other stage labels, with no comment (closingJudge).
//   - An untrusted close is reverted: the App reopens the issue, leaving its
//     stage label alone. A write collaborator's close is a discussion
//     trigger, keyed event=<its timeline event id> (SPEC §6.4), so
//     step 1 holds the reopened issue for it, with its notice, as for any
//     trigger (discFind, closingTriggers). Anyone else's close gets a short
//     comment with a revert marker, keyed by the close's event, as a label
//     revert does, before the reopen (closingRevertClose).
//   - An untrusted reopen of an issue with a closed stage is reverted: the
//     App closes it again with the same state_reason, with a revert
//     comment, and the label stays (closingReopen).
//   - A trusted reopen moves the issue to held, closed → held, at the stage
//     the latest transition marker shows, as an owner hold that /resume
//     here ends, and the closed stage goes (closingReopen).
//
// How GitHub records a close (checked live on this repo, #185, #187 and
// #191, in #190's verify): the REST closed event names its actor, the
// merger for a pull request's merge, and its state_reason and commit_id
// are null, even for a merge, whose squash commit shows only on a separate
// referenced event. So a merge is told by GraphQL's ClosedEvent.closer
// (github.ClosingCloser), asked by the event's node_id: a merged pull
// request whose base is this repo's default branch, or a commit in this
// repo, as a push to the default branch that says "Fixes #N" makes. A
// commit or a pull request in another repo that names the issue is no
// merge, so it's reverted like any untrusted close. It's one GraphQL query,
// asked for a close nobody trusted made, and for the close an untrusted
// reopen undid, for its state reason (closingCloseAgain), and remembered
// per event while the daemon runs (closingCloser). An event GitHub can't
// resolve (NOT_FOUND under node), or whose closer it won't show the App
// (FORBIDDEN under node, as for a project's workflow on a personal
// account, or a commit in a private repo), is no merge, logged, and isn't
// asked about again, so its close is reverted. A project's workflow's closer
// (ProjectV2) is no merge either. Any other failure, a failed request, a
// rate limit, an error on the repository, or one of another type or of
// none under node (#204), is logged as the pass's error, but fails only
// that issue for the pass, its close's judging (closingPass) or its step 1
// (closingCounts), while other issues carry on, and the next pass asks
// again: none of those says how the issue was closed, and a merge reverted
// on a guess would reopen and hold a finished issue. Once the question has
// failed with an error under node on loop.max_attempts passes, the issue
// gets a state error instead, and that failure is no longer the pass's
// error: the close stays unjudged, or holds nothing on an open issue,
// still asked about once a pass (#232, closererr.go). The close's state
// reason is the issue's state_reason as listed, which is the newest
// close's, and, for a close again after an untrusted reopen, the
// ClosedEvent's stateReason of the close it undid, or the closed stage's
// if that question fails on closerErrRetries passes, logged, so the close
// again waits on it only that long (closingCloseAgain, #233).
//
// Which closed issues are read. The poll lists every issue, closed ones
// too, and keeps a closed issue whose close is still to judge
// (closingList): one in the pipeline, with a stage label, or one this
// daemon held with a stage label before it closed, judged or closed
// again, whose close isn't explained by a trusted or App event. One found
// still to judge stays so, even once its stage labels go, until it is
// judged or explained. A close is explained when:
//   - this daemon judged it, and the listing still shows the closed_at and
//     the stage labels it left (closingSeen): a comment or any other change
//     to a closed issue reads nothing;
//   - or the App closed it again itself, and the listing shows what it left
//     (closingAgain);
//   - or, on a start's first poll, the issue has exactly one stage label,
//     a closed stage: only an accepted close leaves one, which an earlier
//     run judged, so it counts as this daemon's judging of it. The poll
//     after digestDrop lists every issue again too, but it isn't a
//     start's, so an untrusted close or closed stage it lists is judged,
//     as on any later poll (#424).
//
// Judging reads the timeline once (github.ClosingTimeline). A closed stage
// on the issue stands when the newest close is accepted and the stage's
// standing labeled event is the App's or a trusted user's
// (github.StandingLabelChange), so a trusted user's choice of the other
// closed stage by hand stands too. So a closed issue with a closed stage is
// never read again, on a pass or a start, until its closed_at or its stage
// labels change within a run. Gap, for SPEC §16 (#194): what happens
// while no daemon runs to a closed issue that has one closed stage isn't
// found, as the first poll of a start can't tell who set it, nor whether
// the issue was reopened and closed again meanwhile. So a closed stage set
// by someone untrusted stands, with or without their close, and so does
// an untrusted close after a trusted reopen of a judged issue.
//
// The first start relabels the closed issues from before the closed stages
// existed, most of them at stage:pr-open: each is judged once, with one
// timeline read and at most two label writes, the closed stage added and
// the old stage removed, as setStage writes them. A pass judges at most
// closingBatch, the newest closes first, so a fresh close isn't stuck
// behind the backlog, and the label writes stay well under GitHub's
// secondary rate limits. Each write is checked first, and the due issues
// are found from the listing again on each start, so a crash part way
// resumes: an issue with its closed stage is judged, and one left with two
// stage labels is judged again, which removes the old one.
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

// closingListedOf is is, a closed issue as the poll listed it, as
// closingListed records one.
func closingListedOf(is *gogithub.Issue) closingListed {
	var labels []string
	for _, l := range is.Labels {
		labels = append(labels, l.GetName())
	}
	return closingListed{closed: is.GetClosedAt().Time, stages: strings.Join(closingStages(labels), ",")}
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
// is set on a start's first poll alone (Daemon.startPolled), and had when
// this daemon held the issue, open, with a stage label, so one whose closer
// also took its stage label off is judged too. One due already, listed
// again, is due as this newer listing shows it, so it's judged from its
// labels and closed_at now.
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
		// stays due, as newly listed.
		if _, due := d.closingDue[n]; due {
			d.closingDue[n] = is
		}
	case start && len(stages) == 1 && closingLabel(stages[0]):
		d.closingSeen[n] = listed // judged on an earlier run
		delete(d.closingDue, n)
	default:
		d.closingDue[n] = is
	}
}

// closingBelowSet keeps, for issue n, just listed closed (closingList)
// with a close still to judge (closingDue), the parent named by s, the
// snapshot held of it, which leave then drops. A parent that waits on the
// issues below it (digestDoneStands) so waits on n till that close is
// accepted (closingPass), or, once it's reverted, till the poll lists n
// open (closingForget): an untrusted close whose event is late, or how it
// was made can't be read, doesn't let the parent close (SPEC §7.3, §9.4).
// With no snapshot held, as when n is listed again, the parent kept
// stays. One with no close to judge, or no parent, is forgotten.
func (d *Daemon) closingBelowSet(n int, s *github.Snapshot) {
	p := d.closingBelow[n]
	if s != nil {
		p = s.Parent
	}
	if _, due := d.closingDue[n]; !due || p == 0 {
		delete(d.closingBelow, n)
		return
	}
	if d.closingBelow == nil {
		d.closingBelow = map[int]int{}
	}
	d.closingBelow[n] = p
}

// closingUnsettled reports whether issue n may still be open, for a
// parent that waits on the issues below it: a snapshot held shows it open,
// or closed though the poll hasn't listed it closed yet, as when it closed
// between the poll and its read, so its close isn't judged; or no snapshot
// is held of it while it's to be read again, as when its read after its
// close was reverted failed; or it's closed with a close still to judge
// (closingDue). It makes no request.
func (d *Daemon) closingUnsettled(n int) bool {
	if _, due := d.closingDue[n]; due {
		return true
	}
	s := d.snaps[n]
	if s == nil {
		return d.stale[n]
	}
	return !s.Closed || !d.closedListed[n]
}

// closingForget forgets issue n's close, once it's listed open, or gone.
func (d *Daemon) closingForget(n int) {
	delete(d.closingSeen, n)
	delete(d.Step.closingAgain, n)
	delete(d.closingDue, n)
	delete(d.closingGone, n)
	delete(d.closingBelow, n)
	d.closerErrJudged(n, nil)
}

// closingPass judges the closes due (closingList), at most closingBatch,
// the newest closes first. One judged stays judged (closingSeen); one
// reverted, reopened, is read again this pass, so step 1 holds it for a
// write collaborator's close. One whose event isn't written yet, or whose
// judging failed, stays due for the next pass. One whose judging finds it
// gone, deleted or transferred, stays due too, as one answer may be
// wrong: its close is forgotten only once a judging on a later pass finds
// it gone again, with no good one between (closingGone), as two reads
// drop an open issue (Iterate). With nothing due, it makes no request.
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
		if gone(err) && d.closingGone[n] {
			// A failed judging between the two shows nothing, so it
			// doesn't count. Its close is never judged now, as closingForget
			// forgets it was (closingSeen), so a parent that waits on it
			// doesn't take it for closed (digestDoneStands): it waits for
			// good, as after a restart, which lists it no more.
			logr.Infof("%s/%s#%d is gone, deleted or transferred, on two judgings of its close with no good one between, so its close is forgotten: %v", d.Step.Owner, d.Step.Repo, n, err)
			d.closingForget(n)
			continue
		}
		if gone(err) {
			logr.Infof("%s/%s#%d looks gone, deleted or transferred, so its close is judged again next pass: %v", d.Step.Owner, d.Step.Repo, n, err)
			if d.closingGone == nil {
				d.closingGone = map[int]bool{}
			}
			d.closingGone[n] = true
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		delete(d.closingGone, n)
		if !done {
			continue
		}
		delete(d.closingDue, n)
		if !reopened {
			delete(d.closingBelow, n) // accepted: no longer below its parent's wait
			continue
		}
		// Open again, though the poll listed it closed: a parent that
		// waits on it doesn't take it for closed, even if its read fails
		// (digestDoneStands), nor one it was below, till the poll lists it
		// open (closingForget).
		delete(d.closedListed, n)
		d.stale[n] = true
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
	// unjudged is the error of judging cl, whose question of how it was made
	// failed with err: none while its closer state error stands, as the
	// close then stays unjudged (closererr.go), and else err.
	unjudged := func(cl github.Closing, err error) error {
		stands, rerr := d.closerErrClosed(ctx, is, cl, err)
		switch {
		case rerr != nil:
			return wrap(fmt.Errorf("%w; %w", err, rerr))
		case stands:
			return nil
		}
		return wrap(err)
	}
	// While that error stands, and the listing still shows the close it's
	// for, only the question is asked, with no timeline read, until GitHub
	// answers it (closerErrClosed). The answer is kept, so judging the
	// close from the timeline then asks nothing more.
	if on, ok := d.closerErrOn[n]; ok && on.listed == closingListedOf(is) {
		if _, _, err := a.closingAccepted(ctx, d.Step.Trust, on.cl); err != nil {
			return false, false, unjudged(on.cl, err)
		}
	}
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
	d.closerErrJudged(n, &cl) // a closer state error on an older close has ended
	accepted, merged, err := a.closingAccepted(ctx, d.Step.Trust, cl)
	if err != nil {
		// A question of how it was closed that keeps failing gets a state
		// error, and the close stays unjudged with no error (closererr.go).
		return false, false, unjudged(cl, err)
	}
	d.closerErrJudged(n, nil)
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
		want := state.ClosingStage(is.GetStateReason(), merged)
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
// user's (closingTrusted), or a merge's into the default branch, merged
// (closingCloser). Only a close nobody trusted made asks how it was made.
func (a *Applier) closingAccepted(ctx context.Context, trust github.Trust, cl github.Closing) (accepted, merged bool, err error) {
	if state.IsApp(cl.By, a.AppSlug) {
		return true, false, nil
	}
	ok, err := closingTrusted(ctx, trust, cl.By)
	if err != nil {
		return false, false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	if ok {
		return true, false, nil
	}
	by, err := a.closingCloser(ctx, cl)
	if err != nil {
		return false, false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	return by.Merge, by.Merge, nil
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

// closingCloser is how cl, a close, was made (github.ClosingCloser), asked
// once per event while the daemon runs: how a close was made never changes,
// so a held issue visited on every pass, with an old close by a write
// collaborator, asks nothing again. An event with no node ID, which GitHub
// gives every event, is no merge, with no question. A failed question is
// an error, and isn't kept, so the next pass asks again; the passes it
// failed on are counted, and an answer ends the count (closererr.go).
func (a *Applier) closingCloser(ctx context.Context, cl github.Closing) (github.ClosingCloser, error) {
	if cl.NodeID == "" {
		return github.ClosingCloser{}, nil
	}
	if by, ok := a.closingBy[cl.NodeID]; ok {
		return by, nil
	}
	by, err := a.Client.ClosingCloser(ctx, a.Owner, a.Repo, cl.NodeID)
	if err != nil {
		a.closerErrFail(cl, err)
		return github.ClosingCloser{}, fmt.Errorf("how it was closed: %w", err)
	}
	if a.closingBy == nil {
		a.closingBy = map[string]github.ClosingCloser{}
	}
	a.closingBy[cl.NodeID] = by
	a.closerErrAnswer(cl)
	return by, nil
}

// closingCounts reports whether cl is a discussion trigger, as discFind
// judges one, in its order: a close, open after the floor (open), then
// recorded, with no question, or else by a write collaborator who isn't
// trusted (who.writer, with its memory of logins GitHub has no permission
// record of), and not a merge's into the default branch (closingCloser).
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
	by, err := a.closingCloser(ctx, cl)
	if err != nil {
		return false, fmt.Errorf("close %d: %w", cl.ID, err)
	}
	return !by.Merge, nil
}

// closingTriggers returns the close triggers on s, from s.Closings, which
// CheckLabels read with the timeline it reads anyway, so they cost no
// request (discFind): each close closingCounts counts. The App reverted
// each of them when it judged the close (closingRevertClose). A close
// whose question of how it was made keeps failing counts as none once its
// state error stands (closerErrStep1), with dirty as for state.Derive,
// which the visit posts (closerErrPost).
func (a *Applier) closingTriggers(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, open discOpen, who discWho) ([]discTrigger, error) {
	var found []discTrigger
	for _, cl := range s.Closings {
		ok, err := a.closingCounts(ctx, cl, open, who)
		if err != nil {
			if a.closerErrStep1(s, dirty, cl, err) {
				continue
			}
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
// are both keyed by their timeline event ID (SPEC §6.4), so only
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
// on it (SPEC §6.4, §7.2), before the label rules act on the disagreement
// the closed stage makes with the latest transition marker. It reports
// whether it handled s, so the label rules' events are left alone. The
// reopen to judge is the newest one that isn't the App's own, which only
// ever reverts an untrusted close, and whose move no marker records:
//   - A trusted user's moves s to held (closed → held), keyed by its event,
//     at the stage the latest transition marker shows, which an accepted
//     close leaves as it was, or for the App's own move to closed, the
//     stage it moved from, as an owner hold with no cmd, which /resume
//     here ends (heldStackEnds); the move's labels take the closed stage
//     off. On a held issue, the first hold's at and origin stay, and at the
//     clarifying after a hold, its origin (discOrigin), or for the App's
//     own move to closed from there, the origin before it (closingBefore).
//     A move the table lacks, as for a type the pipeline doesn't know,
//     holds nothing: the label rules then put the label back, as for any
//     disagreement.
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
	switch is.Stage {
	case state.Held:
		at, origin, seen = is.HeldStack.At, is.HeldStack.Origin, is.HeldStack.Seen
	case state.Closed:
		// The App's own move to closed (Transition), as for a human task
		// on /done: the stage it was closed from.
		m, _ := latestTransition(is)
		at = state.Stage(m.Fields["from"])
		if before := closingBefore(is, at); before.AfterHold() {
			// A close from the clarifying after a hold, as a dropped
			// child's (droppedOne), is a parent's revision's move, which
			// carries no origin (SPEC §5), so the hold takes the origin
			// of the move before it, as for an issue still there.
			origin = discOrigin(before, at)
		}
	default:
		if is.AfterHold() {
			// The clarifying after a hold carries its origin forward, a
			// later hold of either kind included (SPEC §6.4).
			origin = discOrigin(is, is.Stage)
		}
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

// closingBefore is is, an issue whose latest transition is the App's own
// move to closed (latestTransition), as it was before that move: at from,
// the stage the move came from, with only the markers before it. So
// AfterHold and discOrigin read it as they read an issue still at from.
func closingBefore(is state.Issue, from state.Stage) state.Issue {
	for i := len(is.Markers) - 1; i >= 0; i-- {
		if _, bounds := state.RoundBound(is.Markers[i], 0); bounds {
			is.Markers = is.Markers[:i]
			break
		}
	}
	is.Stage = from
	return is
}

// closingCloseAgain reverts reopen, s.Closings[r], an untrusted reopen of
// s, which has the closed stage label: a revert comment keyed by its
// event, posted once (closingRevertComment), then a close with the
// state reason of the newest close before it, as its ClosedEvent gives it
// (closingCloser), or else label's, and s is closed. A failed question of
// that close writes nothing, and is the error, until it has failed on
// closerErrRetries passes, so one transient failure doesn't change the
// reason (#233). Then it's logged, and label's reason used, so a lasting
// failure doesn't keep s open (#204).
// Its error wraps closingErrClosedAgain once it's closed.
func (a *Applier) closingCloseAgain(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, reopen github.Closing, r int, label string) error {
	reason := "completed"
	if stage, _ := state.ParseClosingLabel(label); stage == state.Discarded {
		reason = "not_planned"
	}
	for j := r - 1; j >= 0; j-- {
		if cl := s.Closings[j]; !cl.Reopen {
			by, err := a.closingCloser(ctx, cl)
			if err != nil && a.closerErrPasses(cl) < closerErrRetries {
				// Asked again next pass, with nothing written yet (#233).
				return fmt.Errorf("reopen %d: the reason of the close it undid, %d, is asked again next pass: %w", reopen.ID, cl.ID, err)
			}
			if err != nil {
				// The label's reason does no harm, where waiting for the
				// answer would leave the issue open for as long as the
				// question fails (#204).
				logr.Warnf("%s/%s#%d: closing it again as %s, the label's reason, as the reason of the close its reopen %d undid is unknown: %v",
					a.Owner, a.Repo, s.Number, reason, reopen.ID, err)
				break
			}
			if by.StateReason != "" {
				reason = by.StateReason
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

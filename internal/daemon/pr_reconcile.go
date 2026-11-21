package daemon

import (
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

// This file is step 2 for a task's PR to main (SPEC §6.4, §7.2, §8.1): a
// PR closed without merging, or reopened by someone untrusted after a
// close, hands its task at pr-open to the owner at needs-human, and a
// trusted user's reopen sends it back to pr-open. A merge is no close
// here: its "Closes #N" closes the task, which the close judge accepts
// (closingAccepted). A task at pr-open whose branch moved to a head the
// daemon didn't push goes back to verifying (pr_head.go), and one a
// trusted user's "comment" or "request changes" review sends back goes to
// in-progress (pr_review.go).
//
// What it reads:
//   - a closed PR's state, as the poll listed it (Daemon.prs), says
//     enough, with no read (SPEC §6.4);
//   - an open PR's timeline is read, every page, only when the poll listed
//     it as changed (Daemon.prRead), its updated_at moved, when its task is
//     back at pr-open by a move that isn't the one last judged, as from a
//     hold, by the interviewer's unchanged or by a stage set by hand
//     (Daemon.prJudged), or on the pass after a read that judged nothing,
//     as a failed read or a newest close on a PR listed open, whose
//     reopened event GitHub hasn't written yet. So a pass whose poll lists
//     no PR makes no request here (SPEC §8.1);
//   - that read holds the PR's reviews too, so a review costs no read of
//     its own: only the line comments of the reviews that send the task
//     back are read, on the pass that moves it (prReviewMove);
//   - whether a reopener or a reviewer is trusted is asked once per event
//     while the daemon runs (Daemon.prTrusted);
//   - a task's branch is read from the bare clone, as the pass's sync left
//     it, at no request (prNewHead), and a move waiting for the worker is
//     kept in memory (Daemon.prWaiting).

// prPass judges the PR of each task held at pr-open or at needs-human
// (Daemon.prDirt), step 2 (SPEC §8.1), after step 1's visits and before
// the digest and assignment: a task whose step 1 failed this pass, or that
// a command touched, waits for its next pass, keeping its PR's read
// (Daemon.prRead), and so does a held one, which isn't judged. At pr-open,
// the PR the task names (taskPR), one the App opened that isn't merged,
// is judged by prJudgeOpenHead, and its branch too; at needs-human, the PR
// of a hand-off for its close or untrusted reopen is judged by
// prJudgeHandoff. A move it makes gets its task's items found again, so
// the digest and assignment show it this pass, and its task is read again
// next pass and skips Work. It returns the tasks it moved, for assignment
// (assignPass). A failed read or move is the pass's error, and only that
// task's: the next pass judges it again.
//
// A move into verifying or in-progress takes the worker, so while another
// task holds it (workerFree), the move waits, kept in memory
// (Daemon.prWaiting), with no write, and is made on the first pass the
// worker is free, unless the task moved or its PR changed since
// (prWaitingFor), or the task left pr-open (SPEC §8.1), or the branch
// moved on under a new head's move (prWaitingTip). A move into verifying
// on a new head, or into in-progress on a review that carries the
// branch's tip, then moves the task's head (taskHeadFollow).
func (d *Daemon) prPass(ctx context.Context) (map[int]bool, error) {
	a := d.Step.Applier
	moved := map[int]bool{}
	var errs []error
	for n := range d.prWaiting {
		if _, ok := d.prDirt[n]; !ok { // it left pr-open (prDirtSet)
			delete(d.prWaiting, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(d.prDirt)) {
		s, dirt := d.snaps[n], d.prDirt[n]
		if s == nil || s.Closed || d.stale[n] {
			continue
		}
		is := state.Derive(s, a.AppSlug, dirt.Has)
		if class, err := state.ClassOf(is.Item); err != nil || class&state.AnyTask == 0 {
			continue
		}
		p, ok := d.prs[taskPR(is)]
		if !ok || p.Merged {
			continue
		}
		var m *Move
		var err error
		switch is.Stage {
		case state.PROpen:
			m, err = d.prJudgeOpenHead(ctx, s, dirt, is, p)
		case state.NeedsHuman:
			delete(d.prWaiting, n)
			m, err = d.prJudgeHandoff(ctx, s, dirt, is, p)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("judge PR %s/%s#%d of #%d: %w", a.Owner, a.Repo, p.Number, n, err))
			continue
		}
		if m == nil {
			continue
		}
		if prTakesWorker(*m) {
			if free, why := d.workerFree(n); !free {
				d.prWait(n, *m, why)
				continue
			}
		}
		delete(d.prWaiting, n)
		if err := a.Transition(ctx, s, dirt.Has, *m); err != nil {
			d.prReadAgain(p.Number)
			d.reread(n, false)
			errs = append(errs, fmt.Errorf("judge PR %s/%s#%d of #%d: %w", a.Owner, a.Repo, p.Number, n, err))
			continue
		}
		logr.Infof("%s/%s#%d: its PR #%d moved it to %s on %s", a.Owner, a.Repo, n, p.Number, m.To, m.Trigger)
		d.digestCacheSet(s, d.inputs[n], dirt, nil, false, false)
		d.reread(n, false)
		moved[n] = true
		// Then the task's head, which verifying finishes if a crash
		// comes between (SPEC §3.2).
		if err := d.taskHeadFollow(ctx, n, state.Derive(s, a.AppSlug, dirt.Has)); err != nil {
			errs = append(errs, fmt.Errorf("judge PR %s/%s#%d of #%d: %w", a.Owner, a.Repo, p.Number, n, err))
		}
	}
	return moved, errors.Join(errs...)
}

// prJudgeOpenHead judges task s at pr-open, whose state is is, with its
// PR p, and returns the move it makes, or nil. A move found on an earlier
// pass that waits for the worker, and still stands (prWaitingFor), made
// to fit the branch as it is now (prWaitingTip), is the one, with no
// request. Otherwise the PR is judged (prJudgeOpen), its reviews included,
// so a review that sends the task back wins over a new head in the same
// pass, and carries it (prReviewMove). When that moves nothing, and judged
// the PR, as its read isn't to be made again next pass, an open PR's
// branch is judged for a new head (prNewHead), from the bare clone alone.
// A PR the poll lists closed, its close handed off already, gets no such
// look: its close is the PR's (SPEC §6.4).
func (d *Daemon) prJudgeOpenHead(ctx context.Context, s *github.Snapshot, dirt Dirt, is state.Issue, p listedPR) (*Move, error) {
	if m, ok := d.prWaitingFor(s.Number, is, p); ok {
		m, ok, err := d.prWaitingTip(ctx, s, is, m)
		if err != nil {
			return nil, fmt.Errorf("its waiting move at %s: %w", is.Stage, err)
		}
		if ok {
			return &m, nil
		}
	}
	m, err := d.prJudgeOpen(ctx, s, dirt, is, p)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the PR's events: %w", err)
	case m != nil || !p.Open || d.prRead[p.Number]:
		return m, nil
	}
	if m, err = d.prNewHead(ctx, s, is); err != nil {
		return nil, fmt.Errorf("its branch at %s: %w", is.Stage, err)
	}
	return m, nil
}

// prJudgeOpen judges p, the PR of task s at pr-open, whose state is is
// (SPEC §6.4's A task's PR at pr-open), and returns the move it makes, or
// nil:
//   - a PR the poll lists closed without merging hands the task off,
//     pr-open → needs-human on pr-closed, keyed event=c<PR>-<closed_at in
//     Unix seconds>, with no read. A close handed off already, as when a
//     stage:pr-open set back by hand finds the PR closed still, moves it
//     no more;
//   - an open one is read only when prRead or prJudged says so (see the
//     top of this file). The newest closed or reopened event in its window
//     (prWindow) decides: a reopen by anyone but a trusted user or the App
//     hands the task off the same way, keyed event=<the reopened event's
//     ID>; a newest close, on a PR the poll lists open, is a reopen whose
//     event isn't written yet, and is read again next pass, as is a failed
//     read, which is an error;
//   - otherwise its reviews in their window (prReviewWindow), from the
//     same read, decide: a trusted user's "comment" or "request changes"
//     review that no move has named sends the task back to in-progress
//     (prReviewMove), and anything else moves nothing. Either judges the
//     PR, so a move that waits for the worker stands (prWaitingFor). A
//     failed read of a review's line comments, or of a reviewer's trust, is
//     an error, and the PR is read again next pass.
func (d *Daemon) prJudgeOpen(ctx context.Context, s *github.Snapshot, dirt Dirt, is state.Issue, p listedPR) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	link := prLink(a.Owner, a.Repo, p.Number)
	if !p.Open {
		key := state.Key{Field: "event", ID: fmt.Sprintf("c%d-%d", p.Number, p.ClosedAt.Unix())}
		if is.Moved(key) {
			logr.Debugf("%s/%s#%d: its PR #%d's close was handed off already, so it moves the task no more", a.Owner, a.Repo, n, p.Number)
			return nil, nil
		}
		return &Move{To: state.NeedsHuman, Trigger: state.TriggerPRClosed, Key: key,
			Text: renderPRClosedHandoff(n, p.Number, link, false, "", d.Step.Notify)}, nil
	}
	if !d.prRead[p.Number] && d.prJudged[n] == is.TransitionComment {
		return nil, nil
	}
	e, reviews, err := d.prEvents(ctx, p.Number, prWindow(s, a.AppSlug, dirt.Has), prReviewWindow(s, a.AppSlug, dirt.Has))
	if err != nil {
		return nil, fmt.Errorf("at %s: %w", is.Stage, err)
	}
	switch {
	case e == nil:
	case e.GetEvent() == "closed":
		logr.Debugf("%s/%s#%d: its PR #%d is open, but its newest close or reopen is a close, so its reopened event isn't written yet, and it's read again next pass",
			a.Owner, a.Repo, n, p.Number)
		d.prReadAgain(p.Number)
		return nil, nil
	default:
		by := github.Author{Login: e.GetActor().GetLogin(), Type: e.GetActor().GetType()}
		ok, err := d.prReopenTrusted(ctx, e.GetID(), by)
		if err != nil {
			d.prReadAgain(p.Number)
			return nil, fmt.Errorf("at %s: %w", is.Stage, err)
		}
		if !ok && !strings.EqualFold(by.Login, a.AppSlug+"[bot]") {
			delete(d.prRead, p.Number)
			return &Move{To: state.NeedsHuman, Trigger: state.TriggerPRClosed, Key: state.Key{Field: "event", ID: strconv.FormatInt(e.GetID(), 10)},
				Text: renderPRClosedHandoff(n, p.Number, link, true, by.Login, d.Step.Notify)}, nil
		}
	}
	m, err := d.prReviewMove(ctx, s, is, p, reviews)
	if err != nil {
		d.prReadAgain(p.Number)
		return nil, fmt.Errorf("at %s: %w", is.Stage, err)
	}
	delete(d.prRead, p.Number)
	if d.prJudged == nil {
		d.prJudged = map[int]int64{}
	}
	d.prJudged[n] = is.TransitionComment
	return m, nil
}

// prJudgeHandoff judges p, the PR of task s at needs-human, whose state is
// is, while the task waits on the hand-off for its close or its untrusted
// reopen (prClosedMove), as its latest hand-off (latestHandoff), and
// returns the move it makes, or nil (SPEC §7.2's needs-human → pr-open).
// Only an open PR the poll listed as changed, or one whose last read
// judged nothing, is read (prRead): its newest closed or reopened event in
// the window it had at pr-open (prWindow) decides, as at pr-open, so a
// trusted reopen made just before the hand-off was posted counts too. A
// reopen by a trusted user moves the task needs-human → pr-open on
// pr-reopened, keyed event=<the reopened event's ID>, with pr=, and the
// pull request item comes back. Anyone else's reopen, the App's included,
// moves nothing, and the hand-off stands. A newest close, or a failed
// read, is read again next pass, as at pr-open. A /done or 👍 on the
// hand-off, handled in step 1 first, sends the task to ready instead.
func (d *Daemon) prJudgeHandoff(ctx context.Context, s *github.Snapshot, dirt Dirt, is state.Issue, p listedPR) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	id, ok := latestHandoff(s, a.AppSlug, dirt.Has)
	if !ok || !p.Open || !d.prRead[p.Number] || !prHandoffIn(is, id) {
		return nil, nil
	}
	win := prWindow(s, a.AppSlug, dirt.Has)
	e, _, err := d.prEvents(ctx, p.Number, win, win)
	if err != nil {
		return nil, fmt.Errorf("at %s: %w", is.Stage, err)
	}
	if e == nil {
		delete(d.prRead, p.Number)
		return nil, nil
	}
	if e.GetEvent() == "closed" {
		logr.Debugf("%s/%s#%d: its PR #%d is open, but its newest close or reopen is a close, so its reopened event isn't written yet, and it's read again next pass",
			a.Owner, a.Repo, n, p.Number)
		return nil, nil
	}
	by := github.Author{Login: e.GetActor().GetLogin(), Type: e.GetActor().GetType()}
	trusted, err := d.prReopenTrusted(ctx, e.GetID(), by)
	if err != nil {
		return nil, fmt.Errorf("at %s: %w", is.Stage, err)
	}
	delete(d.prRead, p.Number)
	if !trusted {
		logr.Debugf("%s/%s#%d: its PR #%d was reopened by %s, who isn't trusted, so its hand-off stands", a.Owner, a.Repo, n, p.Number, login(by))
		return nil, nil
	}
	return &Move{To: state.PROpen, Trigger: state.TriggerPRReopened, Key: state.Key{Field: "event", ID: strconv.FormatInt(e.GetID(), 10)},
		Fields: map[string]string{"pr": strconv.Itoa(p.Number)}, Text: renderPRReopened(p.Number, by.Login)}, nil
}

// prHandoffIn reports whether comment id, a task's latest hand-off, holds
// the hand-off for its PR's close or untrusted reopen (prClosedMove), as
// is reads its markers.
func prHandoffIn(is state.Issue, id int64) bool {
	for i, m := range is.Markers {
		if is.MarkerComments[i] == id && prClosedMove(m) {
			return true
		}
	}
	return false
}

// prReadAgain marks open PR pr to be read again on the next pass, as a
// read that judged nothing, or failed, is (SPEC §6.4).
func (d *Daemon) prReadAgain(pr int) {
	if d.prRead == nil {
		d.prRead = map[int]bool{}
	}
	d.prRead[pr] = true
}

// prEvents is what step 2 judges on PR pr's timeline, read every page
// once (Client.TimelineSince), from the earlier of win and reviewWin on:
// the newest closed or reopened event from win on, the PR's window
// (prWindow), or nil for none, and the reviews submitted from reviewWin
// on, their window (prReviewWindow), oldest first, as `reviewed` events
// give them (prReviewOf). A failed read is an error, and the PR is read
// again on the next pass.
func (d *Daemon) prEvents(ctx context.Context, pr int, win, reviewWin time.Time) (*gogithub.Timeline, []prReview, error) {
	a := d.Step.Applier
	since := win
	if reviewWin.Before(since) {
		since = reviewWin
	}
	events, err := a.Client.TimelineSince(ctx, a.Owner, a.Repo, pr, since)
	if err != nil {
		d.prReadAgain(pr)
		return nil, nil, fmt.Errorf("read its closes, reopens and reviews: %w", err)
	}
	var newest *gogithub.Timeline
	var reviews []prReview
	for _, e := range events {
		switch ev := e.GetEvent(); {
		case (ev == "closed" || ev == "reopened") && !e.GetCreatedAt().Before(win):
			newest = e
		case ev == "reviewed" && !e.GetSubmittedAt().Before(reviewWin):
			reviews = append(reviews, prReviewOf(e))
		}
	}
	return newest, reviews, nil
}

// prReopenTrusted reports whether by, who made event id, a reopened event
// or a review, whose ID is its reviewed event's, is a trusted user
// (closingTrusted): not a bot, the App's included, nor someone GitHub
// doesn't name. It's a question of trust alone, asked once per event while
// the daemon runs (SPEC §6.4), so a PR read again asks nothing again. A
// failed question is an error, and isn't kept.
func (d *Daemon) prReopenTrusted(ctx context.Context, id int64, by github.Author) (bool, error) {
	if ok, asked := d.prTrusted[id]; asked {
		return ok, nil
	}
	ok, err := closingTrusted(ctx, d.Step.CurrentTrust(), by)
	if err != nil {
		return false, fmt.Errorf("the maker of event %d: %w", id, err)
	}
	if d.prTrusted == nil {
		d.prTrusted = map[int64]bool{}
	}
	d.prTrusted[id] = ok
	return ok, nil
}

// prWindow is when task s's PR window begins (SPEC §6.4): the time of the
// comment holding its latest move into pr-open from another stage, as
// discMarkers reads the markers with dirty. A pr-open → pr-open move
// doesn't narrow it. A return from a hold doesn't either, when the hold
// began at pr-open, nor does the interviewer's unchanged back there after
// a discussion hold from pr-open (origin=pr-open), so the window is the one
// the task had before the hold. A return from a hold that began at
// another stage, as only a trusted stage:pr-open ends at pr-open, begins
// it at the move to held that began that hold. With no move into pr-open,
// it's the zero time, the whole timeline.
func prWindow(s *github.Snapshot, appSlug string, dirty func(int64) bool) time.Time {
	var win, holdAt time.Time
	held, heldFrom := false, state.Stage("")
	discMarkers(s, appSlug, dirty, func(_ int64, at time.Time, m github.Marker) {
		if m.Kind != "transition" {
			return
		}
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		switch {
		case to == state.Held:
			if !held {
				held, holdAt, heldFrom = true, at, from
				if m.Fields["at"] != "" {
					heldFrom = state.Stage(m.Fields["at"])
				}
			}
			return
		case from == state.Held:
			held = false
			if to == state.PROpen && heldFrom != state.PROpen {
				win = holdAt
			}
			if to == state.PROpen && win.IsZero() {
				win = at
			}
			return
		case to != state.PROpen, from == state.PROpen:
			return
		case from == state.Clarifying && state.Stage(m.Fields["origin"]) == state.PROpen && !win.IsZero():
			return
		}
		win = at
	})
	return win
}

// prClosedMove reports whether m is the hand-off for a task's PR closed
// without merging, or reopened by someone untrusted after a close
// (prJudgeOpen): pr-open → needs-human, keyed event=. Its key tells which
// (prClosedByReopen).
func prClosedMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen &&
		state.Stage(m.Fields["to"]) == state.NeedsHuman && m.Fields["event"] != ""
}

// prClosedByReopen reports whether m, a hand-off prClosedMove reports, is
// for an untrusted reopen, keyed by the reopened event's ID, not for a
// close, keyed c<PR>-<Unix seconds>.
func prClosedByReopen(m github.Marker) bool {
	return !strings.HasPrefix(m.Fields["event"], "c")
}

// prClosedFields are the fields of the hand-off for a task's PR closed
// without merging (SPEC §9.3), in the daemon's own words, as no run gave
// any, with the PR as its link.
func prClosedFields(link string) implementerHandoffFields {
	return implementerHandoffFields{
		Goal:      "Decide what becomes of this task, whose PR was closed without merging.",
		WhyBlocks: "its PR is reopened, or the task is started over or closed",
		Steps: []string{
			"To keep the PR, reopen it: the task goes back to `pr-open`, waiting for your review and merge.",
			"To start over, say what to change in a comment, then reply `/done`: a new round from the task's head, whose pass reopens this PR.",
			"If the task isn't needed, close this issue.",
		},
		Links:         []string{link},
		DoneWhen:      "The PR is reopened, a new round is asked for, or the issue is closed.",
		EstimatedTime: "A few minutes.",
	}
}

// renderPRClosedHandoff is the hand-off comment on task n when its PR,
// pr, at link, was closed without merging, or with reopened, reopened
// after a close by by, who isn't trusted, or by nobody GitHub names for
// "" (SPEC §7.2, §9.3), escaped: renderHandoffEnding with
// prClosedFields, opening with what happened to the PR, and ending with
// the three ways on: reopen the PR, or for an untrusted reopen close and
// reopen it, which sends the task back to pr-open; /done or a 👍 for a
// new round; or close the issue. The reopener's login isn't the App's
// text: it's in code, defused.
func renderPRClosedHandoff(n, pr int, link string, reopened bool, by string, notify []string) string {
	f := prClosedFields(link)
	intro := fmt.Sprintf("PR #%d for this task was closed without merging, so the task waits on you.", pr)
	keep := "Reopening the PR sends the task back to `pr-open`."
	if reopened {
		who := "`" + defuse(by) + "`, who isn't trusted"
		if by == "" {
			who = "someone GitHub doesn't name"
		}
		intro = fmt.Sprintf("PR #%d for this task was reopened after a close by %s, and only a trusted user's reopen keeps it, so the task waits on you.", pr, who)
		f.Goal = "Decide what becomes of this task, whose PR someone who isn't trusted reopened."
		f.WhyBlocks = "its PR is closed and reopened by you, or the task is started over or closed"
		f.Steps[0] = "To keep the PR, close it and reopen it yourself: the task goes back to `pr-open`, waiting for your review and merge."
		f.DoneWhen = "The PR is closed and reopened by you, a new round is asked for, or the issue is closed."
		keep = "Closing and reopening the PR yourself sends the task back to `pr-open`."
	}
	finish := keep + " Replying `/done` on this issue or reacting 👍 to this comment sends it back to `ready` for a new round. Closing this issue ends the task."
	return renderHandoffEnding(intro, f, finish, n, notify)
}

// renderPRReopened is the comment of a task's move back to pr-open on a
// trusted user's reopen of its PR, pr, by by (SPEC §7.2): it names the
// reopener, in code, defused, and says the task waits for the owner's
// review and merge again. Transition escapes it.
func renderPRReopened(pr int, by string) string {
	return fmt.Sprintf("`%s` reopened PR #%d, so this task is back at `pr-open`. It waits for your review and merge, and merging it closes this task.", defuse(by), pr)
}

// prDirtSet keeps s's dirty content dirt, after its last good step 1,
// while s is at pr-open or needs-human, for prPass, which moves it on that
// snapshot, and forgets it otherwise. Iterate calls it beside
// digestCacheSet.
func (d *Daemon) prDirtSet(s *github.Snapshot, dirt Dirt) {
	switch state.Derive(s, d.Step.AppSlug, dirt.Has).Stage {
	case state.PROpen, state.NeedsHuman:
		if d.prDirt == nil {
			d.prDirt = map[int]Dirt{}
		}
		d.prDirt[s.Number] = dirt
	default:
		delete(d.prDirt, s.Number)
	}
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is step 4's checks on an open task PR's merge with its target
// (SPEC §7.2, §7.5, §8.1, §10.4): when the target moves, main for a task
// with no parent, or for a task in a story its story branch's tip, a task
// at pr-open gets SPEC §10.1's checks on its verified head's merge with
// that target as it is now, built in the bare repo (repo.Repo.MergeCommit,
// SPEC §3.2), and its issuedog/verify check says the result, naming that
// target in its external_id. A conflict, or any other failure, posts
// failure, then sends the task back to in-progress, whose next run names
// the target as its target, to merge first (startFrom). One code judges
// both targets, each with its own key and check ID (VerifyTarget's
// moveKey and checkID).

// mainCheckID is the external_id of each issuedog/verify check the App
// posts (SPEC §5, §10.4): main=<SHA>, the main commit its checks ran
// against.
func mainCheckID(main string) string { return "main=" + main }

// tipCheckIDOf is the external_id of the issuedog/verify check the App
// posts on a task in a story's verified head once its story branch's tip
// moved since its verify (SPEC §5, §10.4): tip=<SHA>, the story branch's
// tip its merge was checked with.
func tipCheckIDOf(tip string) string { return "tip=" + tip }

// checkedMain is the main commit head's checks ran with, which its
// issuedog/verify check names (mainCheckID): main, main's commit the
// checks ran against, when it's in head's history, and else head's merge
// base with it, the newest main commit head holds, as the commands run on
// head alone (SPEC §10.1, §10.4). With no one merge base, it's "", and the
// check names none. It reads only the bare repo, so it costs no request.
func (d *Daemon) checkedMain(ctx context.Context, head, main string) (string, error) {
	in, err := d.Clone.IsAncestor(ctx, main, head)
	if err != nil {
		return "", fmt.Errorf("read main against the head: %w", err)
	}
	if in {
		return main, nil
	}
	bases, err := d.Clone.MergeBase(ctx, head, main)
	if err != nil {
		return "", fmt.Errorf("read the head's merge base with main: %w", err)
	}
	if len(bases) != 1 {
		return "", nil
	}
	return bases[0], nil
}

// mainKey keys a task's move pr-open → in-progress when its PR's head
// fails on its merge with main's commit main (SPEC §5): event=m<main>.
func mainKey(main string) state.Key {
	return state.Key{Field: "event", ID: "m" + main}
}

// mainKeyed reports whether k is a key mainKey makes.
func mainKeyed(k state.Key) bool {
	return k.Field == "event" && len(k.ID) > 1 && k.ID[0] == 'm'
}

// mainMove reports whether m is a move pr-open → in-progress that main's
// moving made (Runs.mainSendBack), keyed event=m<SHA>.
func mainMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen && state.Stage(m.Fields["to"]) == state.InProgress &&
		mainKeyed(state.Key{Field: "event", ID: m.Fields["event"]})
}

// tipKey keys a task in a story's move pr-open → in-progress when its
// PR's head fails on its merge with its story branch's tip, tip (SPEC
// §5): event=b<tip>.
func tipKey(tip string) state.Key {
	return state.Key{Field: "event", ID: "b" + tip}
}

// tipKeyed reports whether k is a key tipKey makes.
func tipKeyed(k state.Key) bool {
	return k.Field == "event" && len(k.ID) > 1 && k.ID[0] == 'b'
}

// tipMove reports whether m is a move pr-open → in-progress that a story
// branch's tip moving made (Runs.mainSendBack), keyed event=b<SHA>.
func tipMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen && state.Stage(m.Fields["to"]) == state.InProgress &&
		tipKeyed(state.Key{Field: "event", ID: m.Fields["event"]})
}

// moveKey keys a task's move pr-open → in-progress when its PR's head
// fails on its merge with t (SPEC §5): mainKey for main's commit, and
// tipKey for a story branch's tip.
func (t VerifyTarget) moveKey() state.Key {
	if t.Branch == "" {
		return mainKey(t.SHA)
	}
	return tipKey(t.SHA)
}

// checkID is the external_id of the issuedog/verify check the App posts on
// a head once its merge with t is checked (SPEC §5, §10.4): mainCheckID
// for main's commit, and tipCheckIDOf for a story branch's tip.
func (t VerifyTarget) checkID() string {
	if t.Branch == "" {
		return mainCheckID(t.SHA)
	}
	return tipCheckIDOf(t.SHA)
}

// targetRound reports whether task is's round at in-progress (SPEC §8.3)
// began with a move mainMove or tipMove reports, its target's moving at
// pr-open: its latest transition that bounds a round, other than the
// verify loop's own moves (verifyLoopMove), as taskRoundBound and
// implementerFindings read the round's start.
func targetRound(is state.Issue) bool {
	var start github.Marker
	for i, m := range is.Markers {
		c, ok := is.MarkerComment(i)
		if !ok {
			continue
		}
		if _, bounds := state.RoundBound(m, c); bounds && !verifyLoopMove(m) {
			start = m
		}
	}
	return mainMove(start) || tipMove(start)
}

// personPROpen reports whether task is's latest move into pr-open that
// names a head, as prHeadOf reads it, is a person's head's move back on
// person-pushed (personBack): verifying → pr-open after a move into
// verifying on a new head (newHeadMove). A move out of held back to
// verifying is looked through, as a hold keeps the stage it came from.
func personPROpen(is state.Issue) bool {
	person, back := false, false
	for _, m := range is.Markers {
		if m.Kind != "transition" {
			continue
		}
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		switch {
		case to == state.Verifying && from != state.Held:
			person = newHeadMove(m)
		case to == state.PROpen && m.Fields["head"] != "":
			back = person && from == state.Verifying
		}
	}
	return back
}

// mergeTarget is what task s's PR's verified head is merged with when its
// target moves, as the pass's sync left it, at no request, with why it
// isn't judged now, or "" (SPEC §7.5, §10.4): for a task with no parent,
// main's commit, as rule 2's baseline and rule 7's target (mainTarget).
// For a task with a parent, its story branch's tip, as rule 7's target,
// with the story's baseline, the merge base of that tip and main, as rule
// 2's, only while the tip has the App's issuedog/tip naming the branch as
// success (prStoryTip): a parent that's no story s is in, two branches
// with its prefix, no branch, a tip not checked as success yet, as a new
// one, or one the App merged a task's PR onto, and a tip with no one
// merge base with main, so the story has no baseline, each give why.
func (d *Daemon) mergeTarget(ctx context.Context, s *github.Snapshot) (VerifyTarget, string, error) {
	main, err := d.Clone.Resolve(ctx, "main")
	if err != nil {
		return VerifyTarget{}, "", fmt.Errorf("read main: %w", err)
	}
	if s.Parent == 0 {
		return mainTarget(main), "", nil
	}
	branch, tip, why, err := d.prStoryTip(ctx, s)
	switch {
	case err != nil:
		return VerifyTarget{}, "", fmt.Errorf("its story branch: %w", err)
	case why != "":
		return VerifyTarget{}, why, nil
	}
	base, err := verify.BaseOf(ctx, verify.Facts{Repo: d.Clone, Head: tip, Target: main})
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		return VerifyTarget{}, fmt.Sprintf("its story branch %s's tip %s has no one baseline: %v", branch, tip, err), nil
	case err != nil:
		return VerifyTarget{}, "", fmt.Errorf("read its story's baseline: %w", err)
	}
	return VerifyTarget{Branch: branch, SHA: tip, Baseline: base}, "", nil
}

// mainMoved is step 4's checks on a task's merge with its target (SPEC
// §8.1, §10.4), main, or for a task in a story its story branch's tip
// (SPEC §7.5), which Work runs after the checks at verifying (verifyTask)
// and before the pick, only on a pass where no task work ran, so the
// commands never overlap a claude run. It looks at each of ready, by
// priority then number, but issue skip, whose snapshot is from before the
// result resume just applied to it, that is an open task at pr-open, by
// its stage label and its markers, whose PR the poll lists open (taskPR),
// and reports whether it ran the checks on one: one task a pass.
//
// A task is judged on the head its PR was verified at (prHeadOf) and its
// target as the pass's sync left it (mergeTarget), at no request, and
// skipped when:
//   - it has no target now, as for a task in a story while its story
//     branch's tip has no success, as a new tip, after a sibling's merge,
//     till step 4 checks it, uncounted, with no request;
//   - for a task in a story, its story branch's tip is the tip= of its
//     move into pr-open (prMoveTip), the tip the head was verified
//     against, so it's step 2's to merge (prMerge);
//   - that head passed the checks against that target, at verifying, as
//     the PR step's check says (Runs.passed), while that target is in the
//     head's history, as the commands ran on the head alone, or it passed
//     them on its merge (Daemon.mergeJudged);
//   - a move keyed by that target sent it back once already (moveKey), as
//     before a stage set back by hand: that key moves it once (SPEC §5.2);
//   - a move out of pr-open waits for the worker (Daemon.prWaiting), as
//     the task then leaves pr-open: step 2's, or its own for that target,
//     as step 2 drops one for an older target before this runs
//     (prWaitingMain);
//   - no move names a head, or the bare repo lacks it (repo.Repo.HasCommit),
//     as for a marker written by hand, which is warned of once.
//
// Otherwise the App's issuedog/verify checks on the head are read, one
// request: when the newest names that target (checkID) and is a success,
// as after a restart, the head is judged, and nothing runs. So is a head a
// person pushed that failed verify, whose latest move into pr-open is on
// person-pushed (personPROpen) while the newest is a failure, whatever
// target it names: it never goes to in-progress, and its failure stands
// until its next push or the owner's review moves the task on (SPEC
// §7.2). Either is kept in mergeJudged, so it's read once a target. A
// check with no external_id, as one posted before main was named there,
// names none. Then the merge is built (repo.Repo.MergeCommit). A conflict
// posts failure, naming the files, and sends the task back
// (mainSendBack). A clean merge is checked as a head, with the target's
// baseline as rule 2's and the target as rule 7's (verifyChecks): a pass
// posts success with the commands' results, and is kept in mergeJudged,
// by head and target, so the checks don't run again until either changes,
// and for a task in a story, step 2 merges the PR onto that tip
// (prMergeOnto); a failure posts failure with the findings, and sends the
// task back the same way (SPEC §7.2). Each post is checked first
// (prMainCheck). The target moving again during the checks changes nothing
// here: the result names the target the merge was built on, and the next
// pass, whose sync brings the newer one, judges the head again, once a
// story branch's newer tip has its success.
func (r *Runs) mainMoved(ctx context.Context, ready []Ready, skip int) (bool, error) {
	if r.Commands == nil {
		return false, nil
	}
	a, d := r.Daemon.Step.Applier, r.Daemon
	for _, rd := range ready {
		s, n := rd.S, rd.S.Number
		if n == skip || s.Closed || stageOf(s) != state.PROpen {
			continue
		}
		dirt, ok := d.prDirt[n]
		if !ok {
			continue
		}
		is := state.Derive(s, a.AppSlug, dirt.Has)
		if class, err := state.ClassOf(is.Item); is.Stage != state.PROpen || err != nil || class&state.AnyTask == 0 {
			continue
		}
		if p, ok := d.prs[taskPR(is)]; !ok || !p.Open || p.Merged {
			continue
		}
		head := prHeadOf(is)
		t, why, err := d.mergeTarget(ctx, s)
		if err != nil {
			return false, fmt.Errorf("the merge with its target of #%d: %w", n, err)
		}
		if why != "" {
			logr.Debugf("%s/%s#%d: its head %s's merge isn't checked: %s", a.Owner, a.Repo, n, head, why)
			continue
		}
		if t.Branch != "" && t.SHA == prMoveTip(is) {
			continue
		}
		if d.mergeJudged[n].holds(head, t.SHA) {
			continue
		}
		if r.passed[n].holds(head, t.SHA) {
			// The commands ran on the head alone, so they ran with this
			// target only when it's in the head's history.
			in, err := d.Clone.IsAncestor(ctx, t.SHA, head)
			if err != nil {
				return false, fmt.Errorf("the merge with %s of #%d: read the target against the head: %w", t, n, err)
			}
			if in {
				continue
			}
		}
		if is.Moved(t.moveKey()) {
			logr.Debugf("%s/%s#%d: %s sent it back to %s once already, so its head %s isn't checked on it again",
				a.Owner, a.Repo, n, t, state.InProgress, head)
			continue
		}
		if m, ok := d.prWaiting[n]; ok {
			logr.Debugf("%s/%s#%d: its move to %s waits for the worker, so its merge with %s isn't checked", a.Owner, a.Repo, n, m.To, t)
			continue
		}
		has, err := d.Clone.HasCommit(ctx, head)
		if err != nil {
			return false, fmt.Errorf("the merge with %s of #%d: read the head: %w", t, n, err)
		}
		if !has {
			r.prWarnOnce(n, "main-head:"+head, "%s/%s#%d: its PR's verified head %q isn't in the bare repo, so its merge with %s isn't checked",
				a.Owner, a.Repo, n, head, t)
			continue
		}
		runs, err := a.Client.VerifyChecks(ctx, a.Owner, a.Repo, head)
		if err != nil {
			return false, fmt.Errorf("the merge with %s of #%d: read the head's checks: %w", t, n, err)
		}
		c, checked := mainChecked(runs, a.Client.AppID())
		if checked && c.Conclusion == checkFailure && personPROpen(is) {
			d.mergeJudge(n, head, t, nil)
			logr.Debugf("%s/%s#%d: its head %s, which the daemon didn't push, failed verify, as the App's %s says, so its merge with %s isn't checked: its next push or the owner's review moves it on",
				a.Owner, a.Repo, n, head, github.VerifyCheck, t)
			continue
		}
		if checked && c.ExternalID == t.checkID() && c.Conclusion == checkSuccess {
			d.mergeJudge(n, head, t, nil)
			logr.Debugf("%s/%s#%d: its head %s has the App's %s naming %s as success already, so nothing runs",
				a.Owner, a.Repo, n, head, github.VerifyCheck, t)
			continue
		}
		if err := r.mainJudge(ctx, rd, is, head, t, runs); err != nil {
			return true, fmt.Errorf("the merge with %s of #%d: %w", t, n, err)
		}
		return true, nil
	}
	return false, nil
}

// mainJudge builds the merge of rd's task's head, head, with its target t,
// main's commit or its story branch's tip, checks it, and writes what that
// gives: the check, then for a failure the move back (mainMoved). runs are
// the App's checks on head, newest first, as read before the merge, which
// the post is checked against (prMainCheck).
func (r *Runs) mainJudge(ctx context.Context, rd Ready, is state.Issue, head string, t VerifyTarget, runs []github.CheckRun) error {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	link := waitLink(a.Owner, a.Repo, n, state.BodyID)
	merge, conflicts, err := r.Daemon.Clone.MergeCommit(ctx, n, t.SHA, head)
	if err != nil {
		return fmt.Errorf("build the merge: %w", err)
	}
	// A conflict leaves any older merge result's ref as it was, which
	// nothing here reads.
	if len(conflicts) > 0 {
		logr.Infof("%s/%s#%d: its PR's head %s doesn't merge cleanly with %s: %q", a.Owner, a.Repo, n, head, t, conflicts)
		if err := r.prMainCheck(ctx, n, head, t, "failure", mergeConflictTitle(t), renderMainConflictCheck(n, head, t, conflicts), link, runs); err != nil {
			return fmt.Errorf("on a conflict: %w", err)
		}
		if err := r.mainSendBack(ctx, rd, is, t, renderMainConflict(head, t, conflicts)); err != nil {
			return fmt.Errorf("on a conflict: %w", err)
		}
		return nil
	}
	findings, results, err := r.verifyChecks(ctx, rd.S, merge, t.Baseline, t.SHA)
	if err != nil {
		return fmt.Errorf("check the merge %s: %w", merge, err)
	}
	if len(findings) == 0 && commandsFailed(results) == 0 {
		if err := r.prMainCheck(ctx, n, head, t, "success", mergePassTitle(t), renderMainPassCheck(n, head, t, results), link, runs); err != nil {
			return fmt.Errorf("on a pass: %w", err)
		}
		r.Daemon.mergeJudge(n, head, t, results)
		logr.Infof("%s/%s#%d: its PR's head %s passes the checks on its merge with %s, %s", a.Owner, a.Repo, n, head, t, merge)
		return nil
	}
	logr.Infof("%s/%s#%d: its PR's head %s fails the checks on its merge with %s, %s, in %d rules and %d commands",
		a.Owner, a.Repo, n, head, t, merge, len(findings), commandsFailed(results))
	sum := github.Escape(mainFindingsIntro(head, t) + "\n\n" + findingsText(head, t.label(), 0, 0, findings, results))
	if err := r.prMainCheck(ctx, n, head, t, "failure", mergeFailTitle(t), sum, link, runs); err != nil {
		return fmt.Errorf("on findings: %w", err)
	}
	if err := r.mainSendBack(ctx, rd, is, t, mainFindingsIntro(head, t)+"\n\n"+renderFindings(head, t.label(), 0, 0, findings, results)); err != nil {
		return fmt.Errorf("on findings: %w", err)
	}
	return nil
}

// mergeJudge keeps task n's head as judged against its target t on its
// merge, with what the commands gave, or none when a check names it
// (Runs.mainMoved), so the checks don't run again until either changes,
// and step 2 may merge a task in a story's PR onto that tip (prMergeOnto).
func (d *Daemon) mergeJudge(n int, head string, t VerifyTarget, results []verify.CommandResult) {
	if d.mergeJudged == nil {
		d.mergeJudged = map[int]checksPassed{}
	}
	d.mergeJudged[n] = checksPassed{Head: head, Target: t, Results: results}
}

// mainChecked is the newest of runs, newest first, that's the App's, app's,
// and named issuedog/verify, and true, or false with none. Only the App's
// count (SPEC §10.4), as for prChecked.
func mainChecked(runs []github.CheckRun, app int64) (github.CheckRun, bool) {
	for _, c := range runs {
		if c.AppID == app && c.Name == github.VerifyCheck {
			return c, true
		}
	}
	return github.CheckRun{}, false
}

// mergeWith names t in the titles of the issuedog/verify check the App
// posts on a head once its target moves (SPEC §10.4): "main", or "its
// story branch's tip".
func mergeWith(t VerifyTarget) string {
	if t.Branch == "" {
		return "main"
	}
	return "its story branch's tip"
}

// mergePassTitle is the title of the issuedog/verify check the App posts
// on a head once its merge with its target t passes the checks (SPEC
// §10.4).
func mergePassTitle(t VerifyTarget) string {
	return "issuedog verified this head's merge with " + mergeWith(t)
}

// mergeConflictTitle is the title of that check when the head doesn't
// merge cleanly with t.
func mergeConflictTitle(t VerifyTarget) string {
	return "This head doesn't merge cleanly with " + mergeWith(t)
}

// mergeFailTitle is the title of that check when the head's merge with t
// fails the checks.
func mergeFailTitle(t VerifyTarget) string {
	return "This head fails verify on its merge with " + mergeWith(t)
}

// prMainCheck posts the issuedog/verify check run, with conclusion, on
// head, task n's PR's verified head, never its merge (SPEC §10.4), naming
// its target t in its external_id (checkID), with title, which says what
// the merge gave, summary, an escaped text, and its details URL link, the
// task. It's checked first, against runs, the App's checks on head as
// mainMoved read them before the merge: it posts none when the newest of
// them names t with that conclusion already, so a replay or a restart
// posts one.
func (r *Runs) prMainCheck(ctx context.Context, n int, head string, t VerifyTarget, conclusion, title, summary, link string, runs []github.CheckRun) error {
	a := r.Daemon.Step.Applier
	if c, ok := mainChecked(runs, a.Client.AppID()); ok && c.ExternalID == t.checkID() && c.Conclusion == conclusion {
		logr.Debugf("%s/%s#%d: its head %s has the App's %s naming %s as %s already", a.Owner, a.Repo, n, head, github.VerifyCheck, t, conclusion)
		return nil
	}
	run, err := a.Client.PostVerifyCheck(ctx, a.Owner, a.Repo, github.NewCheckRun{HeadSHA: head, Conclusion: conclusion,
		Title: title, Summary: summary, DetailsURL: link, ExternalID: t.checkID()})
	if err != nil {
		return fmt.Errorf("check the head on its merge with %s: %w", mergeWith(t), err)
	}
	logr.Infof("%s/%s#%d: its head %s has %s as %s on its merge with %s, check run %d", a.Owner, a.Repo, n, head, github.VerifyCheck, conclusion, t, run.ID)
	return nil
}

// mainSendBack moves rd's task, whose state was is when its merge with its
// target t was judged, pr-open → in-progress, keyed event=m<SHA> for main,
// or event=b<SHA> for its story branch's tip (moveKey), with seen=, the
// newest comment on step 1's snapshot, so a comment posted during the
// checks is above the move's bound, and text, what failed, as its comment
// (SPEC §7.2). The table's pr-open → in-progress row is a review's
// trigger, which the move shares.
//
// The move takes the worker, so while another task holds it (workerFree),
// it waits, kept in memory (Daemon.prWaiting), with no write, and step 2
// makes it on the first pass the worker is free, while the target is still
// at t (prWaitingMain). A task in a story's move waits the same way while
// a failure on its story branch is open and the task isn't its bug
// (prReviewTipWait), so step 2 makes it once the failure closes (SPEC
// §3.4's A story's waits). Otherwise, before the move, it reads the issue
// again, and runs CheckLabels and the look for discussion triggers on it,
// as verifyFindings does (SPEC §8.2): a task closed, gone, held, or moved
// since the checks started gets no move.
func (r *Runs) mainSendBack(ctx context.Context, rd Ready, is state.Issue, t VerifyTarget, text string) error {
	a, d, n := r.Daemon.Step.Applier, r.Daemon, rd.S.Number
	wrap := func(err error) error {
		return fmt.Errorf("move the task back on its merge with %s: %w", mergeWith(t), err)
	}
	m := Move{To: state.InProgress, Trigger: state.TriggerReviewChanges, Key: t.moveKey(), Fields: map[string]string{}, Text: text}
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	if seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	switch waits, err := d.prReviewTipWait(ctx, rd.S, m); {
	case err != nil:
		return wrap(err)
	case waits:
		return nil
	}
	if free, why := d.workerFree(n); !free {
		d.prWait(n, m, why)
		return nil
	}
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, so it gets no move on its merge with %s, and is read again: %v", a.Owner, a.Repo, n, t, err)
		d.reread(n, true)
		return nil
	case err != nil:
		return wrap(err)
	}
	defer d.reread(n, false)
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err == nil {
		err = a.discCheckLabels(ctx, s, d.Step.Trust, dirt)
	}
	if err != nil {
		return wrap(err)
	}
	if s.Closed {
		logr.Infof("%s/%s#%d was closed during the checks on its merge with %s, so it gets no move", a.Owner, a.Repo, n, t)
		return nil
	}
	if _, err := d.Step.discussion(ctx, s, dirt); err != nil {
		return wrap(err)
	}
	now := state.Derive(s, a.AppSlug, dirt.Has)
	if now.TransitionComment != is.TransitionComment || now.Stage != state.PROpen {
		logr.Infof("%s/%s#%d moved to %s during the checks on its merge with %s, so it gets no move", a.Owner, a.Repo, n, now.Stage, t)
		return nil
	}
	if err := a.Transition(ctx, s, dirt.Has, m); err != nil {
		return wrap(err)
	}
	delete(d.prWaiting, n)
	logr.Infof("%s/%s#%d: its PR's head fails on its merge with %s, so it's back at %s, to merge it first", a.Owner, a.Repo, n, t, state.InProgress)
	return nil
}

// prWaitingMain reports whether m, a move on its target's moving, main's
// (mainKeyed) or its story branch's tip's (tipKeyed), found for task s at
// pr-open, that waits for the worker and still stands (prWaitingFor),
// stands still: while the task's target, as the pass's sync left it, at no
// request (mergeTarget), is at the commit its key names (moveKey).
// Otherwise it's dropped, and step 4 judges the head again on the newer
// target (Runs.mainMoved), as for a story branch's newer tip, once its
// check is success. One that stands, of a task in a story, isn't made
// while a failure on the story branch is open and the task isn't its bug
// (prReviewTipWait): it's kept, with no write, and this gives false, so
// it's made on the first pass the failure is closed. A push to the task's
// branch is a change the poll lists, so prWaitingFor drops the move for it
// already.
func (d *Daemon) prWaitingMain(ctx context.Context, s *github.Snapshot, m Move) (bool, error) {
	a, n := d.Step.Applier, s.Number
	t, why, err := d.mergeTarget(ctx, s)
	if err != nil {
		return false, fmt.Errorf("read the target of its waiting move: %w", err)
	}
	switch {
	case why != "":
		logr.Debugf("%s/%s#%d: its move to %s on %s, which waited for the worker, is dropped, as %s, so it's judged again",
			a.Owner, a.Repo, n, m.To, m.Key.ID[1:], why)
	case m.Key != t.moveKey():
		logr.Debugf("%s/%s#%d: its move to %s on %s, which waited for the worker, is dropped, as its target is %s now, so it's judged again",
			a.Owner, a.Repo, n, m.To, m.Key.ID[1:], t)
	default:
		// It stands, and is made once no failure on its story branch holds
		// it (prReviewTipWait), which keeps it meanwhile.
		waits, err := d.prReviewTipWait(ctx, s, m)
		if err != nil {
			return false, fmt.Errorf("its waiting move's story branch: %w", err)
		}
		return !waits, nil
	}
	delete(d.prWaiting, n)
	return false, nil
}

// targetMoved opens the comment of a task's move back, and its check's
// summary, once its target t moved and its PR's head failed on its merge
// with it: "`main` moved to `<SHA>`", or "The story branch `<name>` moved
// to `<SHA>`", the name, which an admin may have chosen, in a code span
// with its markers escaped (checkCode, SPEC §5.1).
func targetMoved(t VerifyTarget) string {
	if t.Branch == "" {
		return fmt.Sprintf("`main` moved to `%s`", t.SHA)
	}
	return fmt.Sprintf("The story branch %s moved to `%s`", checkCode(t.Branch), t.SHA)
}

// targetName names t's branch as the move back's comment says the next run
// merges it: "`main`", or "the story branch `<name>`" (checkCode).
func targetName(t VerifyTarget) string {
	if t.Branch == "" {
		return "`main`"
	}
	return "the story branch " + checkCode(t.Branch)
}

// renderMainConflictCheck is the summary of task n's issuedog/verify check
// on head when it doesn't merge cleanly with its target t (SPEC §10.4),
// escaped: it names both (VerifyTarget.label), and each conflicting file
// of files, each in a code span (checkCode), as they're paths from the
// repo.
func renderMainConflictCheck(n int, head string, t VerifyTarget, files []string) string {
	return github.Escape(fmt.Sprintf("issuedog couldn't merge `%s` for #%d with %s: these files conflict.\n\n%s",
		head, n, t.label(), mainFiles(files)))
}

// renderMainConflict is the comment of a task's move pr-open → in-progress
// when its PR's head, head, doesn't merge cleanly with its target t (SPEC
// §7.2): it names both, and each conflicting file of files, each in a code
// span (checkCode), and says the next run merges the target first.
// Transition escapes it.
func renderMainConflict(head string, t VerifyTarget, files []string) string {
	return fmt.Sprintf("%s, and this task's PR, verified at `%s`, doesn't merge cleanly with it. These files conflict:\n\n%s\n\n"+
		"So the task is back at `in-progress`, for the implementer to merge %s first and resolve the conflicts.", targetMoved(t), head, mainFiles(files), targetName(t))
}

// mainFiles lists files, each a path in a code span, one a line.
func mainFiles(files []string) string {
	lines := make([]string, len(files))
	for i, f := range files {
		lines[i] = "- " + checkCode(f)
	}
	return strings.Join(lines, "\n")
}

// mainFindingsIntro opens the check's summary and the move's comment when
// head's merge with its target t fails the checks (SPEC §7.2, §10.4): the
// findings that follow are the merge's, whose next run merges the target
// first.
func mainFindingsIntro(head string, t VerifyTarget) string {
	return fmt.Sprintf("%s, and this task's PR, verified at `%s`, fails verify once merged with it. "+
		"The findings below are the merge's. The next run merges %s first.", targetMoved(t), head, targetName(t))
}

// renderMainPassCheck is the summary of task n's issuedog/verify check on
// head when its merge with its target t passes the checks (SPEC §10.4),
// escaped: it names both (VerifyTarget.label), then the commands' results
// as the PR's Results has them (prResults).
func renderMainPassCheck(n int, head string, t VerifyTarget, results []verify.CommandResult) string {
	return github.Escape(fmt.Sprintf("issuedog checked `%s` for #%d on its merge with %s: the checks passed.\n\n%s",
		head, n, t.label(), prResults(results)))
}

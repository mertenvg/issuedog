package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is step 4's checks on an open task PR's merge with main (SPEC
// §7.2, §8.1, §10.4): when main moves, a task at pr-open gets SPEC §10.1's
// checks on its verified head's merge with main as it is now, built in the
// bare repo (repo.Repo.MergeCommit, SPEC §3.2), and its issuedog/verify
// check says the result, naming that main in its external_id. A conflict,
// or any other failure, posts failure, then sends the task back to
// in-progress, whose next run names main as its target, to merge first
// (startFrom).

// mainCheckID is the external_id of each issuedog/verify check the App
// posts (SPEC §5, §10.4): main=<SHA>, the main commit its checks ran
// against.
func mainCheckID(main string) string { return "main=" + main }

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

// mainRound reports whether task is's round at in-progress (SPEC §8.3)
// began with a move mainMove reports: its latest transition that bounds a
// round, other than the verify loop's own moves (verifyLoopMove), as
// taskRoundBound and implementerFindings read the round's start.
func mainRound(is state.Issue) bool {
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
	return mainMove(start)
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

// mainMoved is step 4's checks on a task's merge with main (SPEC §8.1,
// §10.4), which Work runs after the checks at verifying (verifyTask) and
// before the pick, only on a pass where no task work ran, so the commands
// never overlap a claude run. It looks at each of ready, by priority then
// number, but issue skip, whose snapshot is from before the result resume
// just applied to it, that is an open task at pr-open, by its stage label
// and its markers, with no parent, whose PR the poll lists open (taskPR),
// and reports whether it ran the checks on one: one task a pass.
//
// A task is judged on the head its PR was verified at (prHeadOf) and main's
// commit as the pass's sync left it, at no request, and skipped when:
//   - that head passed the checks against that main, at verifying, as the
//     PR step's check says (Runs.passed), while that main is in the
//     head's history, as the commands ran on the head alone, or it passed
//     them on its merge (mergeJudged);
//   - a move keyed by that main sent it back once already (mainKey), as
//     before a stage set back by hand: that key moves it once (SPEC §5.2);
//   - a move out of pr-open waits for the worker (Daemon.prWaiting), as
//     the task then leaves pr-open: step 2's, or its own for that main, as
//     step 2 drops one for an older main before this runs (prWaitingMain);
//   - no move names a head, or the bare repo lacks it (repo.Repo.HasCommit),
//     as for a marker written by hand, which is warned of once.
//
// Otherwise the App's issuedog/verify checks on the head are read, one
// request: when the newest names that main (mainCheckID) and is a
// success, as after a restart, the head is judged, and nothing runs. So
// is a head a person pushed that failed verify, whose latest move into
// pr-open is on person-pushed (personPROpen) while the newest is a
// failure, whatever main it names: it never goes to in-progress, and its
// failure stands until its next push or the owner's review moves the task
// on (SPEC §7.2). Either is kept in mergeJudged, so it's read once a main. A
// check with no external_id, as one posted before main was named there,
// names none. Then the merge is built (repo.Repo.MergeCommit). A conflict
// posts failure, naming the files, and sends the task back
// (mainSendBack). A clean merge is checked as a head, with main as rule
// 2's baseline and rule 7's target (verifyChecks): a pass posts success
// with the commands' results, and is kept in mergeJudged, by head and
// main, so the checks don't run again until either changes; a failure
// posts failure with the findings, and sends the task back the same way
// (SPEC §7.2). Each post is checked first (prMainCheck). main
// moving again during the checks changes nothing here: the result names
// the main the merge was built on, and the next pass, whose sync brings
// the newer one, judges the head again.
func (r *Runs) mainMoved(ctx context.Context, ready []Ready, skip int) (bool, error) {
	if r.Commands == nil {
		return false, nil
	}
	a, d := r.Daemon.Step.Applier, r.Daemon
	for _, rd := range ready {
		s, n := rd.S, rd.S.Number
		if n == skip || s.Closed || s.Parent != 0 || stageOf(s) != state.PROpen {
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
		main, err := d.Clone.Resolve(ctx, "main")
		if err != nil {
			return false, fmt.Errorf("the merge with main of #%d: read main: %w", n, err)
		}
		if r.mergeJudged[n].holds(head, main) {
			continue
		}
		if r.passed[n].holds(head, main) {
			// The commands ran on the head alone, so they ran with this
			// main only when it's in the head's history.
			in, err := d.Clone.IsAncestor(ctx, main, head)
			if err != nil {
				return false, fmt.Errorf("the merge with main of #%d: read main against the head: %w", n, err)
			}
			if in {
				continue
			}
		}
		if is.Moved(mainKey(main)) {
			logr.Debugf("%s/%s#%d: main's %s sent it back to %s once already, so its head %s isn't checked on it again",
				a.Owner, a.Repo, n, main, state.InProgress, head)
			continue
		}
		if m, ok := d.prWaiting[n]; ok {
			logr.Debugf("%s/%s#%d: its move to %s waits for the worker, so its merge with main's %s isn't checked", a.Owner, a.Repo, n, m.To, main)
			continue
		}
		has, err := d.Clone.HasCommit(ctx, head)
		if err != nil {
			return false, fmt.Errorf("the merge with main of #%d: read the head: %w", n, err)
		}
		if !has {
			r.prWarnOnce(n, "main-head:"+head, "%s/%s#%d: its PR's verified head %q isn't in the bare repo, so its merge with main isn't checked",
				a.Owner, a.Repo, n, head)
			continue
		}
		runs, err := a.Client.VerifyChecks(ctx, a.Owner, a.Repo, head)
		if err != nil {
			return false, fmt.Errorf("the merge with main of #%d: read the head's checks: %w", n, err)
		}
		c, checked := mainChecked(runs, a.Client.AppID())
		if checked && c.Conclusion == checkFailure && personPROpen(is) {
			r.mergeJudge(n, head, main, nil)
			logr.Debugf("%s/%s#%d: its head %s, which the daemon didn't push, failed verify, as the App's %s says, so its merge with main's %s isn't checked: its next push or the owner's review moves it on",
				a.Owner, a.Repo, n, head, github.VerifyCheck, main)
			continue
		}
		if checked && c.ExternalID == mainCheckID(main) && c.Conclusion == checkSuccess {
			r.mergeJudge(n, head, main, nil)
			logr.Debugf("%s/%s#%d: its head %s has the App's %s naming main's %s as success already, so nothing runs",
				a.Owner, a.Repo, n, head, github.VerifyCheck, main)
			continue
		}
		if err := r.mainJudge(ctx, rd, is, head, main, runs); err != nil {
			return true, fmt.Errorf("the merge with main of #%d: %w", n, err)
		}
		return true, nil
	}
	return false, nil
}

// mainJudge builds the merge of rd's task's head, head, with main's commit
// main, checks it, and writes what that gives: the check, then for a
// failure the move back (mainMoved). runs are the App's checks on head,
// newest first, as read before the merge, which the post is checked
// against (prMainCheck).
func (r *Runs) mainJudge(ctx context.Context, rd Ready, is state.Issue, head, main string, runs []github.CheckRun) error {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	link := waitLink(a.Owner, a.Repo, n, state.BodyID)
	merge, conflicts, err := r.Daemon.Clone.MergeCommit(ctx, n, main, head)
	if err != nil {
		return fmt.Errorf("build the merge: %w", err)
	}
	// A conflict leaves any older merge result's ref as it was, which
	// nothing here reads.
	if len(conflicts) > 0 {
		logr.Infof("%s/%s#%d: its PR's head %s doesn't merge cleanly with main's %s: %q", a.Owner, a.Repo, n, head, main, conflicts)
		if err := r.prMainCheck(ctx, n, head, main, "failure", mainConflictTitle, renderMainConflictCheck(n, head, main, conflicts), link, runs); err != nil {
			return fmt.Errorf("on a conflict: %w", err)
		}
		if err := r.mainSendBack(ctx, rd, is, main, renderMainConflict(head, main, conflicts)); err != nil {
			return fmt.Errorf("on a conflict: %w", err)
		}
		return nil
	}
	findings, results, err := r.verifyChecks(ctx, rd.S, merge, main, main)
	if err != nil {
		return fmt.Errorf("check the merge %s: %w", merge, err)
	}
	if len(findings) == 0 && commandsFailed(results) == 0 {
		if err := r.prMainCheck(ctx, n, head, main, "success", mainPassTitle, renderMainPassCheck(n, head, main, results), link, runs); err != nil {
			return fmt.Errorf("on a pass: %w", err)
		}
		r.mergeJudge(n, head, main, results)
		logr.Infof("%s/%s#%d: its PR's head %s passes the checks on its merge with main's %s, %s", a.Owner, a.Repo, n, head, main, merge)
		return nil
	}
	logr.Infof("%s/%s#%d: its PR's head %s fails the checks on its merge with main's %s, %s, in %d rules and %d commands",
		a.Owner, a.Repo, n, head, main, merge, len(findings), commandsFailed(results))
	sum := github.Escape(mainFindingsIntro(head, main) + "\n\n" + findingsText(head, mainTarget(main).label(), 0, 0, findings, results))
	if err := r.prMainCheck(ctx, n, head, main, "failure", mainFailTitle, sum, link, runs); err != nil {
		return fmt.Errorf("on findings: %w", err)
	}
	if err := r.mainSendBack(ctx, rd, is, main, mainFindingsIntro(head, main)+"\n\n"+renderFindings(head, mainTarget(main).label(), 0, 0, findings, results)); err != nil {
		return fmt.Errorf("on findings: %w", err)
	}
	return nil
}

// mergeJudge keeps task n's head as judged against main's commit main on
// its merge, with what the commands gave, or none when a check names it
// (mainMoved), so the checks don't run again until either changes.
func (r *Runs) mergeJudge(n int, head, main string, results []verify.CommandResult) {
	if r.mergeJudged == nil {
		r.mergeJudged = map[int]checksPassed{}
	}
	r.mergeJudged[n] = checksPassed{Head: head, Target: mainTarget(main), Results: results}
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

// Titles of the issuedog/verify check the App posts on a head once main
// moves (SPEC §10.4).
const (
	mainPassTitle     = "issuedog verified this head's merge with main"
	mainConflictTitle = "This head doesn't merge cleanly with main"
	mainFailTitle     = "This head fails verify on its merge with main"
)

// prMainCheck posts the issuedog/verify check run, with conclusion, on
// head, task n's PR's verified head, never its merge (SPEC §10.4), naming
// main's commit main in its external_id (mainCheckID), with title, which
// says what the merge gave, summary, an escaped text, and its details URL
// link, the task. It's checked first, against runs, the App's checks on
// head as mainMoved read them before the merge: it posts none when the
// newest of them names main with that conclusion already, so a replay or a
// restart posts one.
func (r *Runs) prMainCheck(ctx context.Context, n int, head, main, conclusion, title, summary, link string, runs []github.CheckRun) error {
	a := r.Daemon.Step.Applier
	if c, ok := mainChecked(runs, a.Client.AppID()); ok && c.ExternalID == mainCheckID(main) && c.Conclusion == conclusion {
		logr.Debugf("%s/%s#%d: its head %s has the App's %s naming main's %s as %s already", a.Owner, a.Repo, n, head, github.VerifyCheck, main, conclusion)
		return nil
	}
	run, err := a.Client.PostVerifyCheck(ctx, a.Owner, a.Repo, github.NewCheckRun{HeadSHA: head, Conclusion: conclusion,
		Title: title, Summary: summary, DetailsURL: link, ExternalID: mainCheckID(main)})
	if err != nil {
		return fmt.Errorf("check the head on its merge with main: %w", err)
	}
	logr.Infof("%s/%s#%d: its head %s has %s as %s on its merge with main's %s, check run %d", a.Owner, a.Repo, n, head, github.VerifyCheck, conclusion, main, run.ID)
	return nil
}

// mainSendBack moves rd's task, whose state was is when its merge with
// main's commit main was judged, pr-open → in-progress, keyed event=m<main>
// (mainKey), with seen=, the newest comment on step 1's snapshot, so a
// comment posted during the checks is above the move's bound, and text,
// what failed, as its comment (SPEC §7.2). The table's pr-open →
// in-progress row is a review's trigger, which the move shares.
//
// The move takes the worker, so while another task holds it (workerFree),
// it waits, kept in memory (Daemon.prWaiting), with no write, and step 2
// makes it on the first pass the worker is free, while main is still at
// main (prWaitingMain). Otherwise, before the move, it reads the issue
// again, and runs CheckLabels and the look for discussion triggers on it,
// as verifyFindings does (SPEC §8.2): a task closed, gone, held, or moved
// since the checks started gets no move.
func (r *Runs) mainSendBack(ctx context.Context, rd Ready, is state.Issue, main, text string) error {
	a, d, n := r.Daemon.Step.Applier, r.Daemon, rd.S.Number
	wrap := func(err error) error { return fmt.Errorf("move the task back on its merge with main: %w", err) }
	m := Move{To: state.InProgress, Trigger: state.TriggerReviewChanges, Key: mainKey(main), Fields: map[string]string{}, Text: text}
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	if seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	if free, why := d.workerFree(n); !free {
		d.prWait(n, m, why)
		return nil
	}
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, so it gets no move on its merge with main, and is read again: %v", a.Owner, a.Repo, n, err)
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
		logr.Infof("%s/%s#%d was closed during the checks on its merge with main, so it gets no move", a.Owner, a.Repo, n)
		return nil
	}
	if _, err := d.Step.discussion(ctx, s, dirt); err != nil {
		return wrap(err)
	}
	now := state.Derive(s, a.AppSlug, dirt.Has)
	if now.TransitionComment != is.TransitionComment || now.Stage != state.PROpen {
		logr.Infof("%s/%s#%d moved to %s during the checks on its merge with main, so it gets no move", a.Owner, a.Repo, n, now.Stage)
		return nil
	}
	if err := a.Transition(ctx, s, dirt.Has, m); err != nil {
		return wrap(err)
	}
	delete(d.prWaiting, n)
	logr.Infof("%s/%s#%d: its PR's head fails on its merge with main's %s, so it's back at %s, to merge main first", a.Owner, a.Repo, n, main, state.InProgress)
	return nil
}

// prWaitingMain reports whether m, a move on main's moving (mainKeyed)
// found for task s at pr-open, that waits for the worker and still stands
// (prWaitingFor), stands still: while main, as the pass's sync left it, at
// no request, is at the commit its key names. Otherwise it's dropped, and
// step 4 judges the head again on the newer main (Runs.mainMoved). A push
// to the task's branch is a change the poll lists, so prWaitingFor drops
// the move for it already.
func (d *Daemon) prWaitingMain(ctx context.Context, s *github.Snapshot, m Move) (bool, error) {
	a, n := d.Step.Applier, s.Number
	main, err := d.Clone.Resolve(ctx, "main")
	if err != nil {
		return false, fmt.Errorf("read main for its waiting move: %w", err)
	}
	if m.Key != mainKey(main) {
		logr.Debugf("%s/%s#%d: its move to %s on main's %s, which waited for the worker, is dropped, as main is at %s now, so it's judged again",
			a.Owner, a.Repo, n, m.To, m.Key.ID[1:], main)
		delete(d.prWaiting, n)
		return false, nil
	}
	return true, nil
}

// renderMainConflictCheck is the summary of task n's issuedog/verify check
// on head when it doesn't merge cleanly with main's commit main (SPEC
// §10.4), escaped: it names both, and each conflicting file of files, each
// in a code span (checkCode), as they're paths from the repo.
func renderMainConflictCheck(n int, head, main string, files []string) string {
	return github.Escape(fmt.Sprintf("issuedog couldn't merge `%s` for #%d with `main` at `%s`: these files conflict.\n\n%s",
		head, n, main, mainFiles(files)))
}

// renderMainConflict is the comment of a task's move pr-open → in-progress
// when its PR's head, head, doesn't merge cleanly with main's commit main
// (SPEC §7.2): it names both, and each conflicting file of files, each in a
// code span (checkCode), and says the next run merges main first.
// Transition escapes it.
func renderMainConflict(head, main string, files []string) string {
	return fmt.Sprintf("`main` moved to `%s`, and this task's PR, verified at `%s`, doesn't merge cleanly with it. These files conflict:\n\n%s\n\n"+
		"So the task is back at `in-progress`, for the implementer to merge `main` first and resolve the conflicts.", main, head, mainFiles(files))
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
// head's merge with main's commit main fails the checks (SPEC §7.2,
// §10.4): the findings that follow are the merge's, whose next run merges
// main first.
func mainFindingsIntro(head, main string) string {
	return fmt.Sprintf("`main` moved to `%s`, and this task's PR, verified at `%s`, fails verify once merged with it. "+
		"The findings below are the merge's. The next run merges `main` first.", main, head)
}

// renderMainPassCheck is the summary of task n's issuedog/verify check on
// head when its merge with main's commit main passes the checks (SPEC
// §10.4), escaped: it names both, then the commands' results as the PR's
// Results has them (prResults).
func renderMainPassCheck(n int, head, main string, results []verify.CommandResult) string {
	return github.Escape(fmt.Sprintf("issuedog checked `%s` for #%d on its merge with `main` at `%s`: the checks passed.\n\n%s",
		head, n, main, prResults(results)))
}

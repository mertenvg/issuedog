package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file tells a task in a story, its story's branch, and whether that
// branch's tip is checked (SPEC §7.5): a task or a bug whose parent is an
// open story, or an open epic planned into tasks, in this repo, is in that
// story, and works on its branch, issuedog/<story>-<slug>, found by its
// prefix in the bare clone. Its tip is checked once the App's issuedog/tip
// on it names the branch (tip.go).

// storyOf returns s's story and true: s's parent, as snaps, the snapshots
// the daemon holds, by number, has it, when that parent is open, in this
// repo, and a story, or an epic whose approved plan is of tasks
// (storyIssue). Otherwise it returns nil and false, as for s with no
// parent, one in another repo, one snaps lacks, a closed one, or an epic
// planned into stories, whose stories each have their own branch. dirty is
// the parent's dirty content, as for state.Derive, which only an epic's
// plan is read with. An epic's plan data that doesn't read back is an
// error. It reads only what it's given, so it costs no request.
func storyOf(snaps map[int]*github.Snapshot, s *github.Snapshot, appSlug string, dirty func(int64) bool) (*github.Snapshot, bool, error) {
	if s.Parent == 0 || s.ParentRepo != "" {
		return nil, false, nil
	}
	p := snaps[s.Parent]
	if p == nil || p.Closed {
		return nil, false, nil
	}
	ok, err := storyIssue(p, appSlug, dirty)
	if err != nil {
		return nil, false, fmt.Errorf("#%d's story: %w", s.Number, err)
	}
	if !ok {
		return nil, false, nil
	}
	return p, true, nil
}

// storyIssue reports whether s is an issue with a story branch (SPEC §4,
// §7.5): a story, or an epic whose approved plan revision is of tasks, as
// its plan data says (approvedPlanData), read with dirty, s's dirty content,
// as epicCloseChildren tells one. An epic planned into stories, one with no
// approved plan revision or no plan data yet, and any other type, have
// none. Plan data that doesn't read back is an error. It doesn't look at
// whether s is open.
func storyIssue(s *github.Snapshot, appSlug string, dirty func(int64) bool) (bool, error) {
	class, err := state.ClassOf(state.Item{Type: s.Type, HasParent: s.Parent != 0})
	switch {
	case err != nil: // a type that names no kind of issue is no story
		return false, nil
	case class&state.AnyStory != 0:
		return true, nil
	case class != state.ClassEpic:
		return false, nil
	}
	_, plan, ok, err := approvedPlanData(s, appSlug, dirty)
	if err != nil {
		return false, fmt.Errorf("an epic's plan: %w", err)
	}
	return ok && plan.Kind == planTasks, nil
}

// storyBranch names story's branch and its tip, among the bare clone's
// branches as the last sync left them, at no request (SPEC §7.5): the one
// branch with the story's prefix, issuedog/<story>-, whatever its slug, as
// taskBranch finds a task's. With none, it's the name a new one would get,
// from the story's title, with no tip, "". With more than one, the error
// matches errTwoBranches and names them, and no branch is the story's.
func (d *Daemon) storyBranch(ctx context.Context, story *github.Snapshot) (name, tip string, err error) {
	branches, err := d.Clone.Branches(ctx, taskBranchPrefix(story.Number))
	if err != nil {
		return "", "", fmt.Errorf("read story #%d's branches: %w", story.Number, err)
	}
	name, tip, err = taskBranch(story.Number, story.Title, branches)
	if err != nil {
		return "", "", fmt.Errorf("story #%d's branch: %w", story.Number, err)
	}
	return name, tip, nil
}

// tipJudged is a story branch's tip as the daemon last judged it: the tip,
// and the conclusion of the App's issuedog/tip on it that names the branch,
// "success" or "failure" (SPEC §7.5, §10.4).
type tipJudged struct {
	Tip, Conclusion string
}

// tipSuccess reports whether tip, branch's tip, has the App's issuedog/tip
// naming branch as success, as the daemon judged or read it (Daemon.tips,
// Runs.tipCheck). It answers from memory alone, at no request: a tip not
// judged yet, as a new one, or after a restart before step 4 reads its
// check, isn't success, and nor is a failure. Nor is a tip the App merged
// a task's PR onto (tipMergedOnto): the branch is past it, though the bare
// clone holds it till the next sync.
func (d *Daemon) tipSuccess(branch, tip string) bool {
	j, ok := d.tips[branch]
	return ok && tip != "" && j.Tip == tip && j.Conclusion == checkSuccess && !d.tipMergedOnto(branch, tip)
}

// tipMergedOnto reports whether the App merged a task's PR onto tip, as
// branch's tip (Daemon.tipsMerged, prMerge), so the branch's tip is that
// merge's squash commit, not tip, though the bare clone names tip till a
// sync brings the new one (SPEC §7.5). So for the rest of the pass that
// merged, and any pass whose sync fails, nothing names tip as the story's
// tip: not another task's merge, the pick, a run's start, nor the checks
// at verifying (tipSuccess). The next good sync forgets it (reload), as
// the bare clone names the new tip then, so on the pass after the merge
// the tip's success is what the check carried over to the merge commit is
// judged by (tipCarry), and a branch pushed back to tip by force isn't held
// for good. The tip's judgment stays (Daemon.tips), so step 4 checks
// nothing again for it, and makes no request for it.
func (d *Daemon) tipMergedOnto(branch, tip string) bool {
	return tip != "" && d.tipsMerged[branch] == tip
}

// tipMerged keeps tip as the tip of branch the App merged a task's PR
// onto (tipMergedOnto).
func (d *Daemon) tipMerged(branch, tip string) {
	if d.tipsMerged == nil {
		d.tipsMerged = map[string]string{}
	}
	d.tipsMerged[branch] = tip
}

// tipJudge keeps tip as branch's tip judged with conclusion (Daemon.tips).
func (d *Daemon) tipJudge(branch, tip, conclusion string) {
	if d.tips == nil {
		d.tips = map[string]tipJudged{}
	}
	d.tips[branch] = tipJudged{Tip: tip, Conclusion: conclusion}
}

// storyWarnOnce logs a warning for story n, or for task n in a story,
// unless the last one logged for it had the same key, so a wait that
// lasts, as on two branches with its prefix, or a task's for its story
// branch's tip (storyTarget), is warned of once, not every pass, and a new
// cause is warned of again. The repeats are logged at debug.
func (d *Daemon) storyWarnOnce(n int, key, format string, args ...any) {
	logOnce(&d.storyWarned, n, key, logr.Warnf, format, args...)
}

// logOnce logs, with log, the line for n that format and args make,
// unless *last records key as n's last such log's, so a wait that lasts
// is logged once, not every pass, and a new cause is logged again: then
// it records key, making *last if it's nil. The repeats are logged at
// debug. startWaitOnce, storyWarnOnce, pickStoryOnce and prMergeOnce each
// keep their own *last.
func logOnce(last *map[int]string, n int, key string, log func(string, ...any) string, format string, args ...any) {
	if (*last)[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if *last == nil {
		*last = map[int]string{}
	}
	(*last)[n] = key
	log(format, args...)
}

// tipWhy is why tip, branch's tip, has no success, for the log, when
// tipSuccess says it hasn't: it isn't judged yet, it's judged with another
// conclusion, or the App merged a task's PR onto it (tipMergedOnto).
func (d *Daemon) tipWhy(branch, tip string) string {
	why := "has no " + github.TipCheck + " as success yet"
	if j, ok := d.tips[branch]; ok && j.Tip == tip {
		why = "has " + github.TipCheck + " as " + j.Conclusion
	}
	if d.tipMergedOnto(branch, tip) {
		why = "had a task's PR merged onto it, so it's the branch's tip no more"
	}
	return why
}

// failureBugOf reports whether is, an issue's state, is a failure's bug of
// story (SPEC §5, Story branch failures; §7.5): a bug whose App origin
// names a failure on story's branch, a conflict bug's included. The human
// task filed for rule 2 is no bug, and a person's issue with the same
// marker has no origin (state.Derive).
func failureBugOf(is state.Issue, story int) bool {
	return story > 0 && is.Origin.Story == story && strings.EqualFold(is.Item.Type, "bug")
}

// failureBugHeld reports whether s, a snapshot the daemon holds, is a
// failure's bug of story (failureBugOf), below it in this repo, its origin
// read as failureOpenBug reads it (failureHeldOrigin), at no request.
func (d *Daemon) failureBugHeld(s *github.Snapshot, story int) bool {
	slug := d.Step.AppSlug
	return s.Parent == story && s.ParentRepo == "" && failureBugOf(state.Derive(s, slug, failureEdited(s, slug)), story)
}

// storyFailure reports whether a failure on story's branch, branch, whose
// tip is tip, is open, and names its bug, or 0 for none (SPEC §7.5, A
// failure on the story branch): it's open while a failure's bug of story
// is open among the snapshots the daemon holds (failureBugHeld), the
// lowest number named, or while step 4 judged tip failure (Daemon.tips).
// A bug written to this pass still counts till it's read again, unlike
// for failureOpenBug: the App's close marks its snapshot closed at once
// (Applier.closeFinish), so only one still open is waited on, and a close
// on its PR's merge leaves the branch a new tip, which the story's tasks
// wait for the check of anyway. An open human task doesn't count: the tip
// it was filed for fails, so they wait for a later tip that passes,
// whatever happens to it. It reads only memory, at no request.
func (d *Daemon) storyFailure(story *github.Snapshot, branch, tip string) (bug int, open bool) {
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		if s := d.snaps[n]; s != nil && !s.Closed && d.failureBugHeld(s, story.Number) {
			return n, true
		}
	}
	j, ok := d.tips[branch]
	return 0, ok && tip != "" && j.Tip == tip && j.Conclusion == checkFailure
}

// failureAside reports whether holder, an open issue the daemon holds at
// in-progress or verifying, by its stage label, which holds the one worker
// (SPEC §8.1's One task at a time), steps aside for taker, which would
// take it: taker is an open failure's bug of its story, an open story the
// daemon holds (failureBugHeld), and holder is another task below that
// story in this repo that isn't a failure's bug of it. While that bug is
// open, a failure on the story's branch is open (storyFailure), so
// holder's stage work waits: no run of its starts (storyStartTip), and
// its checks at verifying don't run (storyTarget). So the bug, which is
// the failure's fix, takes the worker instead of the story stalling. Once
// the bug is at in-progress or verifying, it holds the worker as any task
// does, as no task steps aside for another failure's bug's holder, and
// holder's work goes on only once the failure is closed. A lease is never
// stepped aside for: pickHeld and workerFree judge it apart. It reads only
// the snapshots held, at no request.
func (d *Daemon) failureAside(holder, taker *github.Snapshot) bool {
	if holder == nil || taker == nil || taker.Closed || holder.Number == taker.Number {
		return false
	}
	sn := taker.Parent
	story := d.snaps[sn]
	return sn != 0 && story != nil && !story.Closed && holder.Parent == sn && holder.ParentRepo == "" &&
		d.failureBugHeld(taker, sn) && !d.failureBugHeld(holder, sn)
}

// failureWait is why task s, in its story, its parent among the snapshots
// the daemon holds, whose branch is branch at tip, waits on an open
// failure on that branch (storyFailure), or "" when none is open, or s is
// a failure's bug of its story (failureBugHeld), which is excused (SPEC
// §3.4's A story's waits): no other task in the story is picked, starts a
// run or moves into in-progress meanwhile. It costs no request.
func (d *Daemon) failureWait(s *github.Snapshot, branch, tip string) string {
	story := d.snaps[s.Parent]
	if s.Parent == 0 || story == nil || d.failureBugHeld(s, s.Parent) {
		return ""
	}
	bug, open := d.storyFailure(story, branch, tip)
	switch {
	case !open:
		return ""
	case bug == 0:
		return fmt.Sprintf("its story branch %s's tip %s has %s as failure, so a failure on the branch is open", branch, tip, github.TipCheck)
	}
	return fmt.Sprintf("a failure on its story branch %s is open, as its bug #%d is, which goes first", branch, bug)
}

// failureTip reports whether tip, branch's tip, is one a failure's bug of
// its story takes (SPEC §3.4's A story's waits, §7.5's A checked base):
// one step 4 judged (Daemon.tips), whatever its conclusion, that the App
// merged no task's PR onto (tipMergedOnto), and that passes rule 2 against
// the story's baseline, the one merge base of tip and main's commit main.
// A tip judged success passed rule 2 among its checks. Any other is
// checked in the bare repo, at no request, and one with no one merge base
// with main fails, as it has no baseline. So it's the tip the bug was
// filed for, or once the branch moves past it, the newest tip that passes
// rule 2, even if other rules fail there, as the bug is their fix.
func (d *Daemon) failureTip(ctx context.Context, branch, tip, main string) (bool, error) {
	j, ok := d.tips[branch]
	switch {
	case !ok || tip == "" || j.Tip != tip || d.tipMergedOnto(branch, tip):
		return false, nil
	case j.Conclusion == checkSuccess:
		return true, nil
	}
	f := verify.Facts{Repo: d.Clone, Head: tip, Target: main, Config: d.Config.Verify}
	base, err := verify.BaseOf(ctx, f)
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the story's baseline at %s: %w", tip, err)
	}
	f.Baseline, f.Base = base, base
	finding, err := verify.Rule2(ctx, f)
	if err != nil {
		return false, fmt.Errorf("check rule 2 on %s: %w", tip, err)
	}
	return finding == nil, nil
}

// storyTipTaken reports whether task s, in story, whose branch is branch
// at tip, takes tip as its story branch's tip, and whether s is a
// failure's bug of story (SPEC §3.4, §7.5): a failure's bug takes a tip
// failureTip passes, with main's commit as the pass's sync left it, and
// any other task one whose check is success (tipSuccess). It reads memory
// and the bare repo, at no request.
func (d *Daemon) storyTipTaken(ctx context.Context, s, story *github.Snapshot, branch, tip string) (taken, bug bool, err error) {
	if !d.failureBugHeld(s, story.Number) {
		return d.tipSuccess(branch, tip), false, nil
	}
	main, err := d.Clone.Resolve(ctx, "main")
	if err != nil {
		return false, true, fmt.Errorf("read main: %w", err)
	}
	taken, err = d.failureTip(ctx, branch, tip, main)
	if err != nil {
		return false, true, fmt.Errorf("judge the tip its failure's bug takes: %w", err)
	}
	return taken, true, nil
}

// failureTipWhy is why a failure's bug waits on its story branch's tip,
// for the log, when failureTip doesn't pass it.
const failureTipWhy = "isn't one step 4 judged that passes rule 2, which a failure's bug takes"

// failureMovedPast reports whether is, a failure's bug's state, names tip,
// its story branch's tip, as its run's target to merge (SPEC §3.4, §7.5,
// When the branch moves under a failure's bug): tip isn't the failing tip
// its origin names, as the branch moved past it, so the bug's fix needs
// the newer tip's changes in its checkout. Any other issue names no
// target for that.
func failureMovedPast(is state.Issue, tip string) bool {
	return tip != "" && failureBugOf(is, is.Origin.Story) && is.Origin.Tip != tip
}

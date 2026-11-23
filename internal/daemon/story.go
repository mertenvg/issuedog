package daemon

import (
	"context"
	"fmt"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
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
// check, isn't success, and nor is a failure.
func (d *Daemon) tipSuccess(branch, tip string) bool {
	j, ok := d.tips[branch]
	return ok && tip != "" && j.Tip == tip && j.Conclusion == checkSuccess
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
	if d.storyWarned[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if d.storyWarned == nil {
		d.storyWarned = map[int]string{}
	}
	d.storyWarned[n] = key
	logr.Warnf(format, args...)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is step 4's checks on a story branch's tip (SPEC §7.5, §8.1,
// §10.1, §10.4): each new tip of an open story's branch, whoever made it,
// gets SPEC §10.1's rules 1, 2, 3 and 7, on the diff from the story's
// baseline, the merge base of the tip and main, and its issuedog/tip check
// says the result, naming the branch in its external_id. Rules 4 to 6 are
// about one task's diff, so they don't run on a story's.

// tipCheckID is the external_id of each issuedog/tip check the App posts
// (SPEC §5, §10.4): branch=<name>, the story branch the commit is the tip
// of, as only a check naming the branch counts for it (SPEC §7.5).
func tipCheckID(branch string) string { return "branch=" + branch }

// tipChecked is the newest of runs, newest first, that's the App's, app's,
// named issuedog/tip, and names branch in its external_id (tipCheckID), and
// true, or false with none. Another App's check, one naming another
// branch, as on a commit that's the tip of two, and an issuedog/verify,
// such as a task's, don't count (SPEC §7.5).
func tipChecked(runs []github.CheckRun, app int64, branch string) (github.CheckRun, bool) {
	for _, c := range runs {
		if c.AppID == app && c.Name == github.TipCheck && c.ExternalID == tipCheckID(branch) {
			return c, true
		}
	}
	return github.CheckRun{}, false
}

// Titles of the issuedog/tip check the App posts on a story branch's tip
// (SPEC §10.4).
const (
	tipPassTitle = "issuedog checked this story branch's tip"
	tipFailTitle = "This story branch's tip fails its checks"
)

// tipCheck is step 4's checks on a story branch's tip (SPEC §7.5, §8.1),
// which Work runs after task work at in-progress and before the checks at
// verifying, only on a pass where no task work ran, so the commands never
// overlap a claude run. It looks at each of ready, by priority then
// number, but issue skip, whose snapshot is from before the result resume
// just applied to it, that is an open story or epic, by its type, not held,
// and reports whether it checked one's tip: one tip a pass.
//
// Its branch is the one with its prefix in the bare clone, as the pass's
// sync left it (storyBranch), and its tip that branch's, at no request.
// It's skipped, with no request, when it has no branch yet, or when its
// tip is the one judged last (Daemon.tips), so an unchanged tip costs
// nothing. Two branches with its prefix get no check, and that's warned of
// once (storyWarnOnce). Then an epic, read with its dirty content, counts
// only when its approved plan is of tasks (storyIssue); one whose plan
// data doesn't read back is warned of once too, and gets no check.
//
// Otherwise the App's issuedog/tip checks on the tip are read, one
// request: the newest naming the branch (tipChecked), as after a restart,
// or for a tip the App's merge carried a check to, judges the tip, and
// nothing runs. A tip pushed by a person is checked as any other is. Then
// the tip is checked (tipJudge), and its check posted.
func (r *Runs) tipCheck(ctx context.Context, ready []Ready, skip int) (bool, error) {
	if r.Commands == nil {
		return false, nil
	}
	a, d := r.Daemon.Step.Applier, r.Daemon
	for _, rd := range ready {
		s, n := rd.S, rd.S.Number
		if n == skip || s.Closed || stageOf(s) == state.Held {
			continue
		}
		if class, err := state.ClassOf(state.Item{Type: s.Type, HasParent: s.Parent != 0}); err != nil || class&(state.AnyStory|state.ClassEpic) == 0 {
			continue
		}
		branch, tip, err := d.storyBranch(ctx, s)
		switch {
		case errors.Is(err, errTwoBranches):
			d.storyWarnOnce(n, "branches:"+err.Error(), "%s/%s#%d: its branch's tip isn't checked, as it can't tell which is its branch: %v",
				a.Owner, a.Repo, n, err)
			continue
		case err != nil:
			return false, fmt.Errorf("the tip of #%d's branch: %w", n, err)
		case tip == "":
			continue
		}
		if j, ok := d.tips[branch]; ok && j.Tip == tip {
			continue
		}
		dirt, err := a.Dirty(ctx, s, d.Step.Trust)
		if err != nil {
			return false, fmt.Errorf("the tip of #%d's branch %s: %w", n, branch, err)
		}
		story, err := storyIssue(s, a.AppSlug, dirt.Has)
		if err != nil {
			d.storyWarnOnce(n, "plan:"+err.Error(), "%s/%s#%d: its branch %s's tip isn't checked, as it can't tell what its plan is of: %v",
				a.Owner, a.Repo, n, branch, err)
			continue
		}
		if !story {
			logr.Debugf("%s/%s#%d isn't a story or an epic planned into tasks, so its branch %s's tip isn't checked", a.Owner, a.Repo, n, branch)
			continue
		}
		runs, err := a.Client.CheckRuns(ctx, a.Owner, a.Repo, tip, github.TipCheck)
		if err != nil {
			return false, fmt.Errorf("the tip of #%d's branch %s: read its checks: %w", n, branch, err)
		}
		if c, ok := tipChecked(runs, a.Client.AppID(), branch); ok {
			d.tipJudge(branch, tip, c.Conclusion)
			logr.Debugf("%s/%s#%d: its branch %s's tip %s has the App's %s naming it as %s already, so nothing runs",
				a.Owner, a.Repo, n, branch, tip, github.TipCheck, c.Conclusion)
			continue
		}
		main, err := d.Clone.Resolve(ctx, "main")
		if err != nil {
			return false, fmt.Errorf("the tip of #%d's branch %s: read main: %w", n, branch, err)
		}
		if err := r.tipJudge(ctx, s, branch, tip, main); err != nil {
			return true, fmt.Errorf("the tip of #%d's branch %s: %w", n, branch, err)
		}
		return true, nil
	}
	return false, nil
}

// tipJudge checks tip, story's branch's tip, by SPEC §10.1's rules 1, 2, 3
// and 7, in the bare repo, and posts its issuedog/tip check (tipPost). The
// story's baseline is the one merge base of tip and main's commit main
// (verify.BaseOf): rule 2's baseline, and the start of rule 3's diff. A
// tip with no merge base with main, or more than one, fails rule 2, as it
// has no baseline, as the checks at verifying judge such a head
// (verifyChecks). Rule 2 runs first, and its failure stops the checks, so
// no command runs on a tip that changes agent config. Otherwise the
// commands run in a fresh checkout of tip, named by a new ID, every one
// even after one fails, then rule 3 and rule 7, with main as its target. A
// failure to read the bare repo, or to run the commands, is an error, and
// posts nothing.
func (r *Runs) tipJudge(ctx context.Context, story *github.Snapshot, branch, tip, main string) error {
	a, n := r.Daemon.Step.Applier, story.Number
	cfg := r.Daemon.Config.Verify
	f := verify.Facts{Repo: r.Daemon.Clone, Head: tip, Target: main, Config: cfg}
	var (
		findings []verify.Finding
		results  []verify.CommandResult
	)
	base, err := verify.BaseOf(ctx, f)
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		findings = []verify.Finding{{Rule: 2, Name: "protected paths", Detail: tipMergeBasesDetail(main, err)}}
	case err != nil:
		return fmt.Errorf("read the story's baseline: %w", err)
	default:
		f.Baseline, f.Base = base, base
		findings, results, err = r.tipRules(ctx, f)
		if err != nil {
			return fmt.Errorf("check the tip: %w", err)
		}
	}
	conclusion := checkSuccess
	if len(findings) > 0 || commandsFailed(results) > 0 {
		conclusion = checkFailure
		logr.Infof("%s/%s#%d: its branch %s's tip %s fails its checks against main's %s, in %d rules and %d commands",
			a.Owner, a.Repo, n, branch, tip, main, len(findings), commandsFailed(results))
	} else {
		logr.Infof("%s/%s#%d: its branch %s's tip %s passes its checks against main's %s", a.Owner, a.Repo, n, branch, tip, main)
	}
	if err := r.tipPost(ctx, n, branch, tip, conclusion, renderTipCheck(n, branch, tip, base, findings, results)); err != nil {
		return fmt.Errorf("post its check: %w", err)
	}
	return nil
}

// tipRules runs rule 2 on f, a story branch's tip with its baseline, then
// unless that fails, rule 1's commands, then rules 3 and 7, and returns
// each rule's finding, and the commands' results (tipJudge).
func (r *Runs) tipRules(ctx context.Context, f verify.Facts) ([]verify.Finding, []verify.CommandResult, error) {
	protected, err := verify.Rule2(ctx, f)
	if err != nil {
		return nil, nil, fmt.Errorf("check rule 2: %w", err)
	}
	if protected != nil {
		return []verify.Finding{*protected}, nil, nil
	}
	results, err := r.Commands.Run(ctx, r.newID(), f.Head, f.Config.Commands)
	if err != nil {
		return nil, nil, fmt.Errorf("rule 1: %w", err)
	}
	var findings []verify.Finding
	for _, rule := range []func(context.Context, verify.Facts) (*verify.Finding, error){verify.Rule3, verify.Rule7} {
		finding, err := rule(ctx, f)
		if err != nil {
			return nil, nil, fmt.Errorf("check the rules: %w", err)
		}
		if finding != nil {
			findings = append(findings, *finding)
		}
	}
	return findings, results, nil
}

// tipMergeBasesDetail is rule 2's finding on a story branch's tip that
// doesn't have exactly one merge base with main's commit main, as err,
// verify.BaseOf's, says: the story then has no baseline (SPEC §10.1).
func tipMergeBasesDetail(main string, err error) string {
	return fmt.Sprintf("The tip doesn't have exactly one merge base with main's %s, so the story has no baseline: %v", main, err)
}

// tipPost posts the issuedog/tip check run on tip, story n's branch's tip,
// with conclusion and summary, an escaped text, naming branch in its
// external_id (tipCheckID), titled for its conclusion, with its details
// URL linking the story (SPEC §10.4). It never posts issuedog/verify. It's
// checked first: it reads the App's issuedog/tip checks on tip, and posts
// none when the newest naming branch has that conclusion already
// (tipChecked), so a replay, or a restart after a crash, posts one. Either
// way the tip is then judged with conclusion (Daemon.tips).
func (r *Runs) tipPost(ctx context.Context, n int, branch, tip, conclusion, summary string) error {
	a, d := r.Daemon.Step.Applier, r.Daemon
	runs, err := a.Client.CheckRuns(ctx, a.Owner, a.Repo, tip, github.TipCheck)
	if err != nil {
		return fmt.Errorf("read the tip's checks: %w", err)
	}
	if c, ok := tipChecked(runs, a.Client.AppID(), branch); ok && c.Conclusion == conclusion {
		logr.Debugf("%s/%s#%d: its branch %s's tip %s has the App's %s as %s already", a.Owner, a.Repo, n, branch, tip, github.TipCheck, conclusion)
		d.tipJudge(branch, tip, conclusion)
		return nil
	}
	title := tipPassTitle
	if conclusion != checkSuccess {
		title = tipFailTitle
	}
	run, err := a.Client.PostCheck(ctx, a.Owner, a.Repo, github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: conclusion,
		Title: title, Summary: summary, DetailsURL: waitLink(a.Owner, a.Repo, n, state.BodyID), ExternalID: tipCheckID(branch)})
	if err != nil {
		return fmt.Errorf("check the tip: %w", err)
	}
	d.tipJudge(branch, tip, conclusion)
	logr.Infof("%s/%s#%d: its branch %s's tip %s has %s as %s, check run %d", a.Owner, a.Repo, n, branch, tip, github.TipCheck, conclusion, run.ID)
	return nil
}

// renderTipCheck is the summary of the issuedog/tip check on tip, story's
// branch's tip (SPEC §10.4), escaped (SPEC §5.1), and cut to
// checkSummaryMax, ending in "…", as GitHub refuses a longer summary. It
// names the tip, the branch, in a code span (checkCode), the story, and
// base, the story's baseline, or says it has none, when base is "". A pass
// then gives each command's result. A failure gives each failed check a
// section, in the order checked, as findingsText does: rule 2's, then rule
// 1's, from cmds, then rules 3 and 7, each detail and output cut to its
// last findingChars characters and fenced, and at most findingCommands
// failed commands.
func renderTipCheck(story int, branch, tip, base string, fs []verify.Finding, cmds []verify.CommandResult) string {
	intro := fmt.Sprintf("issuedog checked `%s`, the tip of %s for story #%d, against the story's baseline, `main` at `%s`.", tip, checkCode(branch), story, base)
	if base == "" {
		intro = fmt.Sprintf("issuedog checked `%s`, the tip of %s for story #%d, which has no one merge base with `main`, so no baseline.", tip, checkCode(branch), story)
	}
	if len(fs) == 0 && commandsFailed(cmds) == 0 {
		return cutText(github.Escape(intro+" The checks passed.\n\n"+tipResults(cmds)), checkSummaryMax)
	}
	parts := []string{intro + " It fails its checks."}
	for _, f := range fs {
		if f.Rule == 2 {
			parts = append(parts, findingSection(f))
		}
	}
	if s := commandsSection(cmds); s != "" {
		parts = append(parts, s)
	}
	for _, f := range fs {
		if f.Rule != 2 {
			parts = append(parts, findingSection(f))
		}
	}
	return cutText(github.Escape(strings.Join(parts, "\n\n")), checkSummaryMax)
}

// tipResults is a passing tip's results, as a table: each of
// verify.commands, in a code span, with its result, as a PR's Results
// gives them (prResults), then rules 2, 3 and 7, the only other rules a
// tip gets.
func tipResults(results []verify.CommandResult) string {
	rows := []string{"| Check | Result |", "|---|---|"}
	for _, r := range results {
		cmd := strings.ReplaceAll(checkCode(defuseRefs(firstChars(r.Command, commandChars))), "|", `\|`)
		rows = append(rows, "| "+cmd+" | pass |")
	}
	rows = append(rows, "| Rules 2, 3 and 7 | pass |")
	return strings.Join(rows, "\n")
}

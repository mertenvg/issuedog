package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is step 2 for a task in a story whose PR the poll lists merged
// into its story branch (SPEC §6.4's Merges into a story branch, §7.2,
// §7.5): GitHub's closing keywords act only on a merge into the default
// branch, and the PR names its task without one (SPEC §11.5), so the App
// closes the task itself, or hands it to the owner. It's judged at
// pr-open, needs-human, in-progress or verifying (prDirtSet), and a task
// held for discussion or by the owner once it's back at one of them; a
// task held for its PR, as a revision dropped it, is dropped_pass.go's.
//
// The PR is read once, for its base, its merger and its merge commit
// (prMergeRead). Then, each checked first, so a replay, a restart or a
// crash between any two repeats none and loses none (SPEC §5.2):
//   - for the App's merge, issuedog/tip as success on the merge commit,
//     when its parent is the tip the task's head was verified against and
//     that tip is success (tipCarry);
//   - for the App's or a trusted user's merge, the task's branch deleted,
//     while its tip is the merged head (prBranchDelete);
//   - last, the move, keyed event=g<PR> (mergedKey): to closed as
//     completed, or for anyone else's merge, with neither write before
//     it, to needs-human with a hand-off naming the merger. Once it's
//     written, the task is closed, or waits on the hand-off with the PR
//     read no more, so a pass after costs nothing here (SPEC §8.1).

// mergedKey keys task's move on pr, its PR merged into its story branch
// (SPEC §5): event=g<PR>. A PR merges once, so the PR alone keys it, where
// the merged event's ID would need a timeline read beside the PR's.
func mergedKey(pr int) state.Key {
	return state.Key{Field: "event", ID: fmt.Sprintf("g%d", pr)}
}

// mergedHandoffMove reports whether m is the hand-off for a task's PR
// merged into its story branch by someone untrusted (prMerged): a move to
// needs-human keyed event=g<PR> (mergedKey).
func mergedHandoffMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.NeedsHuman && strings.HasPrefix(m.Fields["event"], "g")
}

// prMerge is a task's PR merged into its story branch, as one read of it
// gave it, and who merged it: the App, a trusted user, or, with neither,
// anyone else, someone GitHub doesn't name included.
type prMerge struct {
	PR           github.PullRequest
	App, Trusted bool
}

// prMerged judges task s, at pr-open, needs-human, in-progress or
// verifying, whose state is is, and whose PR, p, the poll lists merged,
// and returns the move it makes, or nil (SPEC §7.2, §7.5). A move
// keyed event=g<PR> written already, as after the hand-off of an untrusted
// merge, makes none, with no request. A task with no story, or a PR merged
// into main, which closes its task by its "Closes #N", makes none either
// (prMergeRead). Otherwise, for the App's merge or a trusted user's, the
// check is carried over (tipCarry), for the App's, and the task's branch
// deleted (prBranchDelete), and the move closes the task as completed: on
// merged from pr-open or needs-human, and from in-progress or verifying
// on closed, the any open → closed row, as after a review or a new head
// sent it back from pr-open. Anyone else's merge closes nothing: the move
// hands the task to the owner at needs-human, on untrusted-merge, naming
// the merger.
func (d *Daemon) prMerged(ctx context.Context, s *github.Snapshot, is state.Issue, p listedPR) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	key := mergedKey(p.Number)
	if is.Moved(key) {
		logr.Debugf("%s/%s#%d: its PR #%d's merge moved it once already, so it moves it no more", a.Owner, a.Repo, n, p.Number)
		return nil, nil
	}
	mg, ok, err := d.prMergeRead(ctx, s, p)
	if err != nil {
		return nil, fmt.Errorf("its merge at %s: %w", is.Stage, err)
	}
	if !ok {
		return nil, nil
	}
	if !mg.App && !mg.Trusted {
		logr.Infof("%s/%s#%d: its PR #%d was merged into %s by %q, who isn't trusted, so it closes nothing, and the task is handed off",
			a.Owner, a.Repo, n, p.Number, mg.PR.Base, mg.PR.MergedBy)
		return &Move{To: state.NeedsHuman, Trigger: state.TriggerUntrustedMerge, Key: key,
			Text: renderUntrustedMergeHandoff(n, p.Number, prLink(a.Owner, a.Repo, p.Number), mg.PR.MergedBy, d.Step.Notify)}, nil
	}
	if err := d.prMergedWrites(ctx, s, is, mg); err != nil {
		return nil, fmt.Errorf("its merge at %s: %w", is.Stage, err)
	}
	trigger := state.TriggerMerged
	if is.Stage == state.InProgress || is.Stage == state.Verifying {
		trigger = state.TriggerClosed
	}
	by := mg.PR.MergedBy
	if mg.App {
		by = ""
	}
	return &Move{To: state.Closed, StateReason: "completed", Trigger: trigger, Key: key,
		Text: renderMergedClose(p.Number, mg.PR.MergeCommitSHA, by)}, nil
}

// prMergeRead reads p, the PR of task s, which the poll lists merged, and
// returns it with its merger judged, and true, when it merged into s's
// story branch. It returns false, with no read, for s with no parent in
// this repo, whose PR is to main, and for s whose parent isn't its story
// (mergedInStory), which is logged, and after the read for a PR merged
// into main (prBase), whose "Closes #N" closes the task, or one that
// reads as not merged yet. The read is one conditional request, for the PR's base, its
// merger, merged_by, and its merge commit, merge_commit_sha (SPEC §7.5).
// The App's login judges its own merge; anyone else's is a question of
// trust alone (closingTrusted), a bot's or nobody's no trusted user's,
// which a login ending in [bot] names, as GitHub's read gives no type. A
// failed read, or a failed question, is an error, and the next pass reads
// again.
func (d *Daemon) prMergeRead(ctx context.Context, s *github.Snapshot, p listedPR) (prMerge, bool, error) {
	a, n := d.Step.Applier, s.Number
	if s.Parent == 0 || s.ParentRepo != "" {
		return prMerge{}, false, nil
	}
	ok, err := d.mergedInStory(ctx, s)
	if err != nil {
		return prMerge{}, false, fmt.Errorf("its story: %w", err)
	}
	if !ok {
		return prMerge{}, false, nil
	}
	pr, err := a.Client.PullRequest(ctx, a.Owner, a.Repo, p.Number)
	if err != nil {
		return prMerge{}, false, fmt.Errorf("read the merged PR #%d: %w", p.Number, err)
	}
	switch {
	case !pr.Merged:
		logr.Debugf("%s/%s#%d: its PR #%d reads as not merged, though the poll lists it merged, so it's judged again next pass", a.Owner, a.Repo, n, p.Number)
		return prMerge{}, false, nil
	case pr.Base == prBase:
		logr.Debugf("%s/%s#%d: its PR #%d merged into %s, whose \"Closes #%d\" closes it", a.Owner, a.Repo, n, p.Number, prBase, n)
		return prMerge{}, false, nil
	}
	m := prMerge{PR: pr, App: strings.EqualFold(pr.MergedBy, a.AppSlug+"[bot]")}
	if m.App {
		return m, true, nil
	}
	by := github.Author{Login: pr.MergedBy}
	if strings.HasSuffix(strings.ToLower(by.Login), "[bot]") {
		by.Type = "Bot"
	}
	if m.Trusted, err = closingTrusted(ctx, d.Step.CurrentTrust(), by); err != nil {
		return prMerge{}, false, fmt.Errorf("the merger of PR #%d: %w", p.Number, err)
	}
	return m, true, nil
}

// mergedInStory reports whether s, a task with a parent in this repo, is
// in a story, its parent's (storyOf), from the snapshots held: the
// parent's dirty content, which only an epic's plan is read with, is read
// only then, as the checks at verifying read it. A parent whose plan data
// doesn't read back is warned of once
// (storyWarnOnce), and s is judged no task in a story till it does, as
// the checks at verifying judge it (Runs.storyTarget). A failed read of the
// parent's dirty content is an error.
func (d *Daemon) mergedInStory(ctx context.Context, s *github.Snapshot) (bool, error) {
	a, n := d.Step.Applier, s.Number
	parent := d.snaps[s.Parent]
	var parentDirt Dirt
	var dirtErr error
	read := false
	dirty := func(id int64) bool {
		if !read {
			read = true
			parentDirt, dirtErr = a.Dirty(ctx, parent, d.Step.Trust)
		}
		return parentDirt.Has(id)
	}
	_, ok, err := storyOf(d.snaps, s, a.AppSlug, dirty)
	switch {
	case dirtErr != nil:
		return false, fmt.Errorf("read its parent #%d: %w", s.Parent, dirtErr)
	case err != nil:
		d.storyWarnOnce(n, "plan:"+err.Error(), "%s/%s#%d: its PR's merge isn't judged, as it can't tell what its parent #%d's plan is of: %v",
			a.Owner, a.Repo, n, s.Parent, err)
		return false, nil
	case !ok:
		logr.Debugf("%s/%s#%d: its parent, #%d, isn't a story it's worked in, so its PR's merge is left alone", a.Owner, a.Repo, n, s.Parent)
		return false, nil
	}
	return true, nil
}

// prMergedWrites are the writes before the close of task s, whose state is
// is, once mg, its PR, merged into its story branch by the App or a
// trusted user (SPEC §7.5), each checked first: for the App's merge, the
// check carried over to the merge commit (tipCarry), then the task's
// branch deleted (prBranchDelete).
func (d *Daemon) prMergedWrites(ctx context.Context, s *github.Snapshot, is state.Issue, mg prMerge) error {
	if mg.App {
		if err := d.tipCarry(ctx, s, is, mg.PR); err != nil {
			return fmt.Errorf("carry the tip's check over: %w", err)
		}
	}
	if err := d.prBranchDelete(ctx, s, mg.PR.HeadSHA); err != nil {
		return fmt.Errorf("delete its branch: %w", err)
	}
	return nil
}

// tipCarry carries the issuedog/tip check over to the merge commit of pr,
// task s's PR, which the App merged into its story branch, pr.Base (SPEC
// §7.5's Carrying the check over): the squash commit's tree is the
// verified head's, so when pr merged the head the task's move to pr-open
// names (prHeadOf), and the merge commit's parent is the tip that head
// was verified against, the move's tip= (prMoveTip), and that tip is
// success as the daemon judged it (tipSuccess), the merge commit gets
// issuedog/tip as success, naming the branch, its summary linking the
// verifier's pass on the head (tipPost, renderTipCarry). Any other merge
// commit gets none, and step 4 checks it as a push the App didn't make
// (Runs.tipCheck): one whose parent is another commit, as the App's merge
// onto a tip that moved since the verify, once the head's merge with it
// passed (prMergeOnto), one merged at another head, a failure's bug's
// (failureBugOf), as it may have gone onto a tip that fails other rules
// than rule 2 (SPEC §7.5's A checked base), and, after a restart,
// one whose parent's success isn't
// judged yet, as Daemon.tips starts empty. So does a merge commit the bare
// clone lacks, as for a merge made after the pass's sync. The parent is
// read from the bare clone, at no request, and the post is checked first.
func (d *Daemon) tipCarry(ctx context.Context, s *github.Snapshot, is state.Issue, pr github.PullRequest) error {
	a, n := d.Step.Applier, s.Number
	branch, commit, tip, head := pr.Base, pr.MergeCommitSHA, prMoveTip(is), prHeadOf(is)
	switch {
	case tip == "" || commit == "" || pr.HeadSHA != head:
		logr.Infof("%s/%s#%d: its PR #%d merged %s as %q, not the head %s verified against a tip of %s, so no check is carried over to the merge commit",
			a.Owner, a.Repo, n, pr.Number, pr.HeadSHA, commit, head, branch)
		return nil
	case failureBugOf(is, s.Parent):
		logr.Infof("%s/%s#%d: it's a failure's bug of story #%d, so no check is carried over to its PR #%d's merge commit %s, which is checked afresh",
			a.Owner, a.Repo, n, s.Parent, pr.Number, commit)
		return nil
	case !d.tipSuccess(branch, tip):
		if j, ok := d.tips[branch]; ok && j.Tip != tip {
			logr.Infof("%s/%s#%d: its PR #%d went onto %s's tip %s, which moved since its head %s was verified against %s, so no check is carried over to the merge commit %s, which is checked as any push",
				a.Owner, a.Repo, n, pr.Number, branch, j.Tip, head, tip, commit)
			return nil
		}
		logr.Infof("%s/%s#%d: %s's tip %s, which its head %s was verified against, isn't judged success, as after a restart, so no check is carried over to the merge commit %s, which is checked as any push",
			a.Owner, a.Repo, n, branch, tip, head, commit)
		return nil
	}
	has, err := d.Clone.HasCommit(ctx, commit)
	if err != nil {
		return fmt.Errorf("read the merge commit %s: %w", commit, err)
	}
	if !has {
		logr.Infof("%s/%s#%d: the bare clone lacks its PR #%d's merge commit %s, so no check is carried over to it, and it's checked as any push",
			a.Owner, a.Repo, n, pr.Number, commit)
		return nil
	}
	parent, err := d.Clone.Resolve(ctx, commit+"^")
	if err != nil {
		return fmt.Errorf("read the merge commit %s's parent: %w", commit, err)
	}
	if parent != tip {
		logr.Infof("%s/%s#%d: its PR #%d's merge commit %s is on %s, not on the tip %s its head was verified against, so no check is carried over to it",
			a.Owner, a.Repo, n, pr.Number, commit, parent, tip)
		return nil
	}
	link := ""
	if c, ok := markerCommentOf(s, is, passOn(is, head)); ok {
		link = waitLink(a.Owner, a.Repo, n, c.ID)
	}
	if err := d.tipPost(ctx, s.Parent, branch, commit, checkSuccess, renderTipCarry(s.Parent, n, pr.Number, branch, commit, tip, head, link)); err != nil {
		return fmt.Errorf("carry %s's check over to %s: %w", tip, commit, err)
	}
	return nil
}

// renderTipCarry is the summary of the issuedog/tip check the App carries
// over to commit, its squash merge of task n's PR, pr, onto tip, the tip of
// branch, story's branch, that the task's head was verified against (SPEC
// §7.5, §10.4), escaped (SPEC §5.1), and cut to checkSummaryMax. It names
// the commit, the branch, in a code span (checkCode), the story, the task,
// the head and the tip, and links link, the verifier's pass on the head,
// unless it's "".
func renderTipCarry(story, n, pr int, branch, commit, tip, head, link string) string {
	text := fmt.Sprintf("issuedog carried this check over to `%s`, the tip of %s for story #%d: it's the App's squash merge of PR #%d, task #%d's head `%s`, onto `%s`, "+
		"the tip the head was verified against, whose check passed, so its tree is the verified head's.", commit, checkCode(branch), story, pr, n, head, tip)
	if link != "" {
		text += fmt.Sprintf(" The verifier passed the head in [its comment](%s).", link)
	}
	return cutText(github.Escape(text), checkSummaryMax)
}

// prBranchDelete deletes the branch of task s once its PR merged at head
// into its story branch (SPEC §7.5's Branches after a merge): the branch
// with its prefix in the bare clone, as the pass's sync left it
// (taskBranch), at no request, while its tip is head. A branch gone
// already, as GitHub's "Automatically delete head branches" may delete
// it, is fine, and so is a delete GitHub answers for one gone since. A
// branch whose tip isn't head, as one pushed to since the merge, is kept,
// and so are two with its prefix, each warned of once (storyWarnOnce).
func (d *Daemon) prBranchDelete(ctx context.Context, s *github.Snapshot, head string) error {
	a, n := d.Step.Applier, s.Number
	branches, err := d.Clone.Branches(ctx, taskBranchPrefix(n))
	if err != nil {
		return fmt.Errorf("read the task's branches: %w", err)
	}
	name, tip, err := taskBranch(n, s.Title, branches)
	switch {
	case errors.Is(err, errTwoBranches):
		d.storyWarnOnce(n, "merged-branches:"+err.Error(), "%s/%s#%d: its PR merged, but no branch is deleted, as it can't tell which is its branch: %v",
			a.Owner, a.Repo, n, err)
		return nil
	case err != nil:
		return fmt.Errorf("name the task's branch: %w", err)
	case tip == "":
		logr.Debugf("%s/%s#%d: its branch is gone already", a.Owner, a.Repo, n)
		return nil
	case tip != head:
		d.storyWarnOnce(n, "merged-branch:"+name+"@"+tip, "%s/%s#%d: its branch %s is at %s, not its PR's merged head %s, so it's kept",
			a.Owner, a.Repo, n, name, tip, head)
		return nil
	}
	if err := a.Client.DeleteBranch(ctx, a.Owner, a.Repo, name); err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	logr.Infof("%s/%s#%d: its PR merged, so its branch %s is deleted", a.Owner, a.Repo, n, name)
	return nil
}

// renderMergedClose is the comment of a task's close once its PR, pr,
// merged into its story branch as commit, by by, a trusted user, or by the
// App for "" (SPEC §7.2): the merger's login isn't the App's text, so it's
// in code, defused. Transition escapes it.
func renderMergedClose(pr int, commit, by string) string {
	if by == "" {
		return fmt.Sprintf("The App merged PR #%d into the story branch as `%s`, so this task is complete.", pr, commit)
	}
	return fmt.Sprintf("`%s` merged PR #%d into the story branch as `%s`, so this task is complete.", defuse(by), pr, commit)
}

// mergedHandoffFields are the fields of the hand-off for a task's PR
// merged into its story branch by someone untrusted (SPEC §9.3), in the
// daemon's own words, as no run gave any, with the PR as its link.
func mergedHandoffFields(link string) implementerHandoffFields {
	return implementerHandoffFields{
		Goal:      "Decide what becomes of this task, whose PR someone who isn't trusted merged into the story branch.",
		WhyBlocks: "its merge is accepted, or the task is started over",
		Steps: []string{
			"Check the merged work on the story branch.",
			"If it's right, close this issue as completed.",
			"If it isn't, say what to change in a comment, then reply `/done`: a new round on this task.",
		},
		Links:         []string{link},
		DoneWhen:      "The issue is closed, or a new round is asked for.",
		EstimatedTime: "A few minutes.",
	}
}

// renderUntrustedMergeHandoff is the hand-off comment on task n when its
// PR, pr, at link, was merged into its story branch by by, who isn't
// trusted, or by nobody GitHub names for "" (SPEC §7.5, §9.3), escaped:
// renderHandoffEnding with mergedHandoffFields, opening with the merge,
// and ending with the ways on: close the issue to accept the merge, or
// /done or a 👍 for a new round. The merger's login isn't the App's text:
// it's in code, defused.
func renderUntrustedMergeHandoff(n, pr int, link, by string, notify []string) string {
	who := "`" + defuse(by) + "`, who isn't trusted"
	if by == "" {
		who = "someone GitHub doesn't name"
	}
	intro := fmt.Sprintf("PR #%d for this task was merged into the story branch by %s, and only the App's or a trusted user's merge closes the task, so it waits on you.", pr, who)
	finish := "Closing this issue accepts the merge. Replying `/done` on this issue or reacting 👍 to this comment sends it back to `ready` for a new round."
	return renderHandoffEnding(intro, mergedHandoffFields(link), finish, n, notify)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is step 2's merge of a task's PR into its story branch (SPEC
// §6.4, §7.5, §8.1): once step 2 has judged the PR of a task in a story at
// pr-open, and nothing moved the task, the App squash-merges the PR at the
// head it verified, onto the story branch's tip it verified the head
// against. Every guard but the read just before the merge is the bare
// clone's, as the pass's sync left it, or the daemon's memory, so a task
// whose merge waits makes no request.

// prMoveTip is the story branch's tip task is's PR's head was verified
// against (SPEC §5): the tip= of the move into pr-open that prHeadOf reads
// its head= from, the latest that names a head, or "" when that move names
// none. A person's head that failed verify goes back to pr-open with no
// tip= (personFailed), so it has none, and nothing merges it: an older
// move's tip= is never taken in its place, as it was the tip of another
// head. The check carried over once the PR merges reads it too
// (tipCarry).
func prMoveTip(is state.Issue) string {
	tip := ""
	for _, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.PROpen && m.Fields["head"] != "" {
			tip = m.Fields[fieldStoryTip]
		}
	}
	return tip
}

// prMerge merges p, the open PR of task s at pr-open, whose state is is,
// into its story branch, as the App, once step 2 judged the PR and nothing
// moved the task (prJudgeOpenHead), and returns the move that judging it
// again just before the merge makes, or nil (SPEC §6.4, §7.5's The merge's
// guards). It merges only when, each read at no request:
//   - s is a task in a story (storyOf), with no move of its waiting
//     (Daemon.prWaiting), which stands instead;
//   - its PR's verified head (prHeadOf) has the tip= of its move (prMoveTip):
//     a person's head that failed verify has none;
//   - the task's branch's tip (prTip) is that head: a new head is step 2's
//     to judge (prNewHead), and a branch that's gone, or one of two with the
//     task's prefix, merges nothing;
//   - its story branch's tip has the App's issuedog/tip naming the branch
//     as success (prStoryTip), and is that tip=, or a tip the head's merge
//     with has passed the checks on since, as step 4 judged it
//     (prMergeOnto). Otherwise it waits at pr-open, with no request, logged
//     once a cause (prMergeOnce).
//
// A head a person pushed (personPROpen) is merged only while the App's
// newest issuedog/verify on it isn't a failure (prPersonFailed). Then the
// PR is read again, one conditional request (Client.PullRequest). One that
// merged or closed since the poll is left to the next poll, and one whose
// head isn't the verified head, or whose base isn't the story branch, as
// after a person changed its base, waits, logged once. One that changed
// since the poll, its updated_at moved, is judged again (prJudgeOpen) on
// that read: a move it makes is returned, and stops the merge, and so does
// a read that judges nothing, as for a newest close whose reopened event
// isn't written yet, as the next pass judges it again. Then the merge: a
// squash at the head, with the PR's title and number, GitHub's own title
// for a squash, as the commit's title, and its body as its message
// (Client.MergePullRequest). GitHub's refusal, a 409 for a head or a base
// that moved, or a 405 for a PR it won't merge, writes nothing and closes
// nothing, and is logged once a head. It's kept (Daemon.prMergeRefused),
// and the merge waits, with no request, till the head, the PR's updated_at
// as the poll lists it, or the story branch's tip changes: a 409's head or
// base that moved shows so, and a 405 that lasts, as for a rule GitHub
// holds the merge by, costs no request a pass. A merged PR is kept as
// merged (Daemon.prs), so it's never merged again, nor judged again
// (prPass), and the tip it went onto is kept too (tipMerged), so nothing
// else names that tip as the story's tip for the rest of the pass, as a
// sibling task's merge would, onto a tip neither its head nor its merge
// was checked against. Any
// other failure is an error, and the next pass tries again.
func (d *Daemon) prMerge(ctx context.Context, s *github.Snapshot, dirt Dirt, is state.Issue, p listedPR) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	if s.Parent == 0 || !p.Open || p.Merged {
		return nil, nil
	}
	if m, ok := d.prWaiting[n]; ok {
		logr.Debugf("%s/%s#%d: its move to %s on %s waits, so its PR #%d isn't merged", a.Owner, a.Repo, n, m.To, m.Trigger, p.Number)
		return nil, nil
	}
	head, onto := prHeadOf(is), prMoveTip(is)
	if head == "" || onto == "" {
		d.prMergeOnce(n, "unverified:"+head, logr.Infof, "%s/%s#%d: its PR #%d isn't merged: the move into %s that names its head %q names no story branch tip it was verified against, as for a head that failed verify",
			a.Owner, a.Repo, n, p.Number, state.PROpen, head)
		return nil, nil
	}
	branch, tip, why, err := d.prStoryTip(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("its story branch: %w", err)
	}
	if why != "" {
		d.prMergeOnce(n, "story:"+why, logr.Infof, "%s/%s#%d: its PR #%d isn't merged yet: %s", a.Owner, a.Repo, n, p.Number, why)
		return nil, nil
	}
	_, at, ok, err := d.prTip(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("its branch: %w", err)
	}
	if !ok || at != head {
		logr.Debugf("%s/%s#%d: its branch is at %q, not at %s, its PR #%d's verified head, so the PR isn't merged", a.Owner, a.Repo, n, at, head, p.Number)
		return nil, nil
	}
	if tip != onto && !d.prMergeOnto(n, head, tip) {
		d.prMergeOnce(n, "tip:"+tip, logr.Infof, "%s/%s#%d: its PR #%d waits: its story branch %s is at %s, not at %s, the tip its head %s was verified against, and its merge with the new tip hasn't passed the checks",
			a.Owner, a.Repo, n, p.Number, branch, tip, onto, head)
		return nil, nil
	}
	if r, ok := d.prMergeRefused[n]; ok && r.Head == head && r.Tip == tip && r.UpdatedAt.Equal(p.UpdatedAt) {
		logr.Debugf("%s/%s#%d: GitHub refused to merge its PR #%d at %s onto %s, and neither the head, the PR nor the story branch's tip changed since, so it isn't tried again yet",
			a.Owner, a.Repo, n, p.Number, head, tip)
		return nil, nil
	}
	if personPROpen(is) {
		failed, err := d.prPersonFailed(ctx, n, head)
		if err != nil {
			return nil, fmt.Errorf("its head's check: %w", err)
		}
		if failed {
			d.prMergeOnce(n, "failed:"+head, logr.Infof, "%s/%s#%d: its PR #%d isn't merged: its head %s, which the daemon didn't push, has the App's %s as failure",
				a.Owner, a.Repo, n, p.Number, head, github.VerifyCheck)
			return nil, nil
		}
	}
	pr, err := a.Client.PullRequest(ctx, a.Owner, a.Repo, p.Number)
	if err != nil {
		return nil, fmt.Errorf("read its PR again before the merge: %w", err)
	}
	switch {
	case pr.Merged || pr.State != "open":
		logr.Debugf("%s/%s#%d: its PR #%d is %s now, merged %t, so it isn't merged, and the next poll lists it", a.Owner, a.Repo, n, p.Number, pr.State, pr.Merged)
		return nil, nil
	case pr.HeadSHA != head || pr.Base != branch:
		d.prMergeOnce(n, "pr:"+pr.HeadSHA+":"+pr.Base, logr.Warnf, "%s/%s#%d: its PR #%d is at %s into %s, not at %s, its verified head, into %s, its story branch, so it isn't merged",
			a.Owner, a.Repo, n, p.Number, pr.HeadSHA, pr.Base, head, branch)
		return nil, nil
	case !pr.UpdatedAt.Equal(p.UpdatedAt):
		logr.Debugf("%s/%s#%d: its PR #%d changed since the poll, so it's judged again before the merge", a.Owner, a.Repo, n, p.Number)
		d.prReadAgain(p.Number)
		m, err := d.prJudgeOpen(ctx, s, dirt, is, p)
		if err != nil {
			return nil, fmt.Errorf("judge its PR again before the merge: %w", err)
		}
		if m != nil || d.prRead[p.Number] {
			return m, nil
		}
	}
	commit, err := a.Client.MergePullRequest(ctx, a.Owner, a.Repo, p.Number, head, fmt.Sprintf("%s (#%d)", pr.Title, p.Number), pr.Body)
	switch {
	case errors.Is(err, github.ErrHeadMoved), errors.Is(err, github.ErrNotMergeable):
		d.prMergeOnce(n, "refused:"+head, logr.Warnf, "%s/%s#%d: GitHub refused to merge its PR #%d at %s into %s, so nothing merged, and it's tried again once the head, the PR or the story branch's tip changes: %v",
			a.Owner, a.Repo, n, p.Number, head, branch, err)
		if d.prMergeRefused == nil {
			d.prMergeRefused = map[int]prRefusal{}
		}
		d.prMergeRefused[n] = prRefusal{Head: head, Tip: tip, UpdatedAt: pr.UpdatedAt}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("merge its PR at %s: %w", head, err)
	}
	p.Open, p.Merged = false, true
	d.prs[p.Number] = p
	delete(d.prMergeLogged, n)
	delete(d.prMergeRefused, n)
	d.tipMerged(branch, tip)
	logr.Infof("%s/%s#%d: merged its PR #%d at %s into %s, onto %s, as %s", a.Owner, a.Repo, n, p.Number, head, branch, tip, commit)
	return nil, nil
}

// prRefusal is GitHub's refusal of a task's PR's merge (prMerge): the head
// it was asked at, the story branch's tip then, and the PR's updated_at,
// as read just before.
type prRefusal struct {
	Head, Tip string
	UpdatedAt time.Time
}

// prStoryTip is task s's story branch and its tip, as the pass's sync
// left them, with why a move onto it waits, or "" when it needn't (SPEC
// §7.5): s's story (storyOf), its one branch (storyBranch), and that tip's
// App issuedog/tip naming the branch as success (tipSuccess). A parent
// that's no story s is in, one whose plan data doesn't read back, two
// branches with its prefix, no branch, a tip not checked as success yet,
// after a restart too until step 4 reads its check, and a tip the App
// merged a task's PR onto (tipMergedOnto), as a sibling's in the same
// pass, each give why.
// It reads only the snapshots step 1 left, the bare repo and what step 4
// judged, so it costs no request: the parent's dirty content is kept from
// step 1 (Applier.Dirty), and read only for an epic's plan.
func (d *Daemon) prStoryTip(ctx context.Context, s *github.Snapshot) (branch, tip, why string, err error) {
	a := d.Step.Applier
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
	story, ok, err := storyOf(d.snaps, s, a.AppSlug, dirty)
	switch {
	case dirtErr != nil:
		return "", "", "", fmt.Errorf("read its parent #%d: %w", s.Parent, dirtErr)
	case err != nil:
		return "", "", fmt.Sprintf("it can't tell what its parent #%d's plan is of: %v", s.Parent, err), nil
	case !ok:
		return "", "", fmt.Sprintf("its parent #%d is no open story, or epic planned into tasks, in this repo", s.Parent), nil
	}
	branch, tip, err = d.storyBranch(ctx, story)
	switch {
	case errors.Is(err, errTwoBranches):
		return "", "", fmt.Sprintf("it can't tell which is its story #%d's branch: %v", story.Number, err), nil
	case err != nil:
		return "", "", "", fmt.Errorf("name its story branch's tip: %w", err)
	case tip == "":
		return branch, "", fmt.Sprintf("its story #%d has no branch", story.Number), nil
	case d.tipMergedOnto(branch, tip):
		return branch, tip, fmt.Sprintf("its story branch %s's tip %s had a task's PR merged onto it, so it's the branch's tip no more, and the new tip isn't checked yet", branch, tip), nil
	case !d.tipSuccess(branch, tip):
		return branch, tip, fmt.Sprintf("its story branch %s's tip %s has no %s naming it as success", branch, tip, github.TipCheck), nil
	}
	return branch, tip, "", nil
}

// prMergeOnto reports whether head, task n's PR's verified head, may be
// merged onto tip, its story branch's tip, a tip other than the one the
// head was verified against, as a sibling's merge leaves (SPEC §7.5's The
// merge's guards): only once SPEC §10.1's checks have passed on the head's
// merge with tip, as step 4 judged it (Runs.mainMoved), or after a
// restart, as the head's newest App issuedog/verify naming tip=<tip> as
// success says, which step 4 reads once, at no request here: both are
// kept in Daemon.mergeJudged, by head and tip. Until then the task waits
// at pr-open, with no request. A person's head kept there on its failure,
// as mainMoved keeps it, is never merged either way: it has no tip= on
// its move (prMoveTip), or prMerge reads that failure (prPersonFailed).
func (d *Daemon) prMergeOnto(n int, head, tip string) bool {
	return d.mergeJudged[n].holds(head, tip)
}

// prPersonFailed reports whether the App's newest issuedog/verify on head,
// task n's PR's verified head, which a person pushed (personPROpen), is a
// failure (mainChecked), as the PR's merge never takes such a head (SPEC
// §7.2). It reads the head's check runs, one conditional request, only on
// a pass that would merge, and keeps a failure (Daemon.prMergeFailed), so
// a head that waits on it costs no request again: only a new head, which
// goes through verify, gets a new check of the App's.
func (d *Daemon) prPersonFailed(ctx context.Context, n int, head string) (bool, error) {
	if d.prMergeFailed[n] == head {
		return true, nil
	}
	a := d.Step.Applier
	runs, err := a.Client.VerifyChecks(ctx, a.Owner, a.Repo, head)
	if err != nil {
		return false, fmt.Errorf("read the App's %s on %s: %w", github.VerifyCheck, head, err)
	}
	if c, ok := mainChecked(runs, a.Client.AppID()); !ok || c.Conclusion != checkFailure {
		return false, nil
	}
	if d.prMergeFailed == nil {
		d.prMergeFailed = map[int]string{}
	}
	d.prMergeFailed[n] = head
	return true, nil
}

// prMergeOnce logs, with log, why task n's PR isn't merged, unless the
// last such log for it had the same key, so a wait that lasts, or a
// refusal of one head, is logged once, not every pass, and a new cause is
// logged again. The repeats are logged at debug.
func (d *Daemon) prMergeOnce(n int, key string, log func(string, ...any) string, format string, args ...any) {
	logOnce(&d.prMergeLogged, n, key, log, format, args...)
}

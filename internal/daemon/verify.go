package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// verifyKey keys the checks' move of a task verifying → in-progress on
// findings (SPEC §5), or in its place at the round's last attempt, the verify cap's move
// to needs-human: event=v<c>, the comment holding its move to verifying,
// so the checks of each move there move it on once.
func verifyKey(c int64) state.Key {
	return state.Key{Field: "event", ID: "v" + strconv.FormatInt(c, 10)}
}

// verifyTask is step 4's checks at verifying and the verifier's run after
// them (SPEC §8.1, §10.1, §10.2), which Work runs after task work at
// in-progress and before the pick. It looks at each of ready, by priority
// then number, but issue skip, whose snapshot is from before the result
// resume just applied to it, that is an open task at verifying, by its
// stage label and its markers, of a class that gets a PR (state.AnyTask),
// so never a human task, and reports whether it ran the checks or a run on
// one. Its findings move, once written, is its latest marker, so its
// markers no longer put it at verifying. With no Commands, no task gets
// either.
//
// The head is the latest move into verifying's head=, or else the task's
// head, taskRef (verifyHead). When the latest move is step 2's on a new
// head, the task's head is moved there first, if it's behind
// (taskHeadFollow). It's checked against its target (VerifyTarget), by
// verifyChecks: for a task with no parent, main's commit as the pass's
// sync left it, rule 2's baseline and rule 7's target. A task in a story
// (storyOf) is checked against its story branch's tip, as the pass's sync
// left it, rule 7's target, with the story's baseline, the merge base of
// that tip and main, as rule 2's, only while that tip's issuedog/tip
// naming the branch is success (storyTarget, SPEC §7.5, §10.1). Until
// then it waits, uncounted, logged once. A task whose parent is no story,
// as an epic planned into stories, gets neither, uncounted. A pass writes
// nothing, and returns: it's kept in passed, by head and its target's
// commit, with what the commands gave, so the checks don't run again
// until either changes, as when the story branch moves, or the daemon
// restarts, which empties it. A failure moves the task back to
// in-progress, or on the round's last attempt, hands it off to the owner
// at needs-human (verifyFindings). On a
// person's head, the head of step 2's move on a new head (personHead), a
// failure never does: it goes back to pr-open, or for rule 2 to the owner
// at needs-human, with no attempt counted (personFindings).
//
// With a pass kept, the next pass starts the Stage at verifying, the
// verifier, on the head, the only place one starts, unless a clean pass
// marker of its own names that head (verdictOn), or none is registered.
// Its pass moves nothing: the task waits at verifying, and keeps the
// worker, its request_changes goes as a failure of the checks does, and its
// question moves the task to clarifying (Verifier.Apply). A head with a
// clean pass marker gets the PR step instead, while the checks' pass holds
// on it and its target (openPR): its head pushed to the task's branch, its
// PR to main, or for a task in a story into its story branch, opened or
// updated, its issuedog/verify check posted, and the task moved to
// pr-open, or for a head that changes nothing on that base, handed off at
// needs-human, once its follow-ups' filed record stands. On a person's
// head, the PR step gives way to personPass, which pushes nothing and
// moves the task back to pr-open, and the verifier's request_changes goes
// there too (Verifier.personChanges). It reports a run when that step read
// the issue again to write, and else looks at the next task. A run that
// fails writes nothing, and the head gets another, until the last of
// loop.max_attempts in a row hands the task off at needs-human
// (Runs.atCap). A later head has no such marker, so it gets a run of its
// own, and a later target only the checks, which move the task back on a
// failure whatever the verdict was.
//
// Naming the head and its target, and a skip for a pass kept, read only
// the bare repo, what the daemon holds of the story and its tip's check,
// and the snapshot step 1 left, so they cost no request.
//
// The commands run as agent, and after each one every agent process is
// killed (agent.KillLeftovers), so the checks must never overlap a claude
// run, which runs as agent too. Work keeps them apart: it runs one thing
// at a time, and runs the checks only once resume has applied or ended any
// run a lease names, and no task work ran in the pass (SPEC §8.2).
func (r *Runs) verifyTask(ctx context.Context, ready []Ready, skip int) (bool, error) {
	if r.Commands == nil {
		return false, nil
	}
	a := r.Daemon.Step.Applier
	for _, rd := range ready {
		s, n := rd.S, rd.S.Number
		if n == skip || s.Closed || stageOf(s) != state.Verifying {
			continue
		}
		main, err := r.Daemon.Clone.Resolve(ctx, "main")
		if err != nil {
			return false, fmt.Errorf("verify #%d: read main: %w", n, err)
		}
		t := mainTarget(main)
		if s.Parent != 0 {
			st, ok, err := r.storyTarget(ctx, s, main)
			if err != nil {
				return false, fmt.Errorf("verify #%d: %w", n, err)
			}
			if !ok {
				continue
			}
			t = st
		}
		d, err := a.Dirty(ctx, s, r.Daemon.Step.Trust)
		if err != nil {
			return false, fmt.Errorf("verify #%d: %w", n, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		if class, err := state.ClassOf(is.Item); is.Stage != state.Verifying || err != nil || class&state.AnyTask == 0 {
			logr.Debugf("%s/%s#%d gets no checks: it's no task at %s that gets a PR, by its markers", a.Owner, a.Repo, n, state.Verifying)
			continue
		}
		// A new head's move written, and the task's head not moved after
		// it, as by a crash between them, is finished first (SPEC §3.2).
		if err := r.Daemon.taskHeadFollow(ctx, n, is); err != nil {
			return false, fmt.Errorf("verify #%d: %w", n, err)
		}
		head, err := r.verifyHead(ctx, n, is)
		if err != nil {
			return false, fmt.Errorf("verify #%d: %w", n, err)
		}
		person := personHead(is)
		if r.passed[n].holds(head, t.SHA) {
			if verdictOn(is, head) {
				var wrote bool
				if person {
					wrote, err = r.personPass(ctx, rd, is, head, t)
				} else {
					wrote, err = r.openPR(ctx, rd, is, head, t)
				}
				switch {
				case err != nil:
					return wrote, fmt.Errorf("verify #%d: %w", n, err)
				case wrote:
					return true, nil
				}
				continue
			}
			st, ok := r.Stages[state.Verifying]
			if !ok {
				logr.Debugf("%s/%s#%d: its head %s passed the checks against %s already, so it waits at %s", a.Owner, a.Repo, n, head, t, state.Verifying)
				continue
			}
			ran, err := r.RunStage(ctx, rd, st)
			switch {
			case err != nil:
				return ran, fmt.Errorf("the %s on #%d: %w", st.Agent(), n, err)
			case ran:
				return true, nil
			}
			continue
		}
		findings, results, err := r.verifyChecks(ctx, s, head, t.Baseline, t.SHA)
		if err != nil {
			return true, fmt.Errorf("verify #%d: %w", n, err)
		}
		if len(findings) == 0 && commandsFailed(results) == 0 {
			if r.passed == nil {
				r.passed = map[int]checksPassed{}
			}
			r.passed[n] = checksPassed{Head: head, Target: t, Results: results}
			logr.Infof("%s/%s#%d: its head %s passes the checks against %s, at %s", a.Owner, a.Repo, n, head, t, state.Verifying)
			return true, nil
		}
		judge := r.verifyFindings
		if person {
			judge = r.personFindings
		}
		if err := judge(ctx, rd, is, head, t, findings, results); err != nil {
			return true, fmt.Errorf("verify #%d: %w", n, err)
		}
		return true, nil
	}
	return false, nil
}

// verifyHead is the head of task n, whose state is is, at verifying: a
// person's head's move's head=, looking through a hold (personMove), or
// else the head= of its latest move into verifying, which the
// implementer's move carries (SPEC §5), or else the task's head,
// taskRef(n), or "" with none, as for a task set to verifying by hand
// before any run.
func (r *Runs) verifyHead(ctx context.Context, n int, is state.Issue) (string, error) {
	if m, ok := personMove(is); ok {
		return m.Fields["head"], nil
	}
	if head := moveHead(is); head != "" {
		return head, nil
	}
	head, _, err := r.Daemon.Clone.ReadRef(ctx, taskRef(n))
	if err != nil {
		return "", fmt.Errorf("read the task's head: %w", err)
	}
	return head, nil
}

// moveHead is the head= of task is's latest move into verifying, or "" when
// it names none, as a move by hand or out of held doesn't.
func moveHead(is state.Issue) string {
	head := ""
	for _, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Verifying {
			head = m.Fields["head"]
		}
	}
	return head
}

// verifyAttempt is the attempt= of task is's latest move into verifying,
// the implementer run's number in its round, whose head the checks judge
// (verifyHead), or 0 when it names none, as a move by hand or out of held
// doesn't (SPEC §8.3).
func verifyAttempt(is state.Issue) int {
	attempt := 0
	for _, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Verifying {
			attempt = markerAttempt(m)
		}
	}
	return attempt
}

// verifyChecks runs SPEC §10.1's checks on head, in the bare repo, with
// baseline as rule 2's baseline and target as rule 7's target, for the
// task s, whose scope rule 4 reads (scopeOf): for a task with no parent,
// both are main's commit, and for a task in a story, the story's baseline
// and its branch's tip (VerifyTarget). Rules 2 to 6 read the task's own
// diff from head's merge base with target. It returns each rule's
// finding, and rule 1's commands' results. No head, or one the bare repo
// lacks, is the one finding, with no rule, and no rule runs. Rule 2 runs
// first, and its failure stops the checks, so no command runs on a diff
// that touches a protected path. A head with no merge base with target, or
// more than one, fails rule 2 too (verify.ErrMergeBases), as the task's own
// diff then has no start. Otherwise every command runs, in a fresh
// checkout of head named by a new ID, never the lease's run's, which the
// commands remove when they're done, and rules 3 to 7 all run, so the
// implementer sees every finding at once (SPEC §10.1). A failure to read
// the bare repo, or to run the commands, is an error, not a finding.
func (r *Runs) verifyChecks(ctx context.Context, s *github.Snapshot, head, baseline, target string) ([]verify.Finding, []verify.CommandResult, error) {
	if head == "" {
		return []verify.Finding{{Name: "the head", Detail: "The task has no head: its move to verifying names none, and there's no " + taskRef(s.Number) + "."}}, nil, nil
	}
	has, err := r.Daemon.Clone.HasCommit(ctx, head)
	if err != nil {
		return nil, nil, fmt.Errorf("read the head: %w", err)
	}
	if !has {
		logr.Warnf("%s/%s#%d: its head %s isn't in the bare repo", r.Daemon.Step.Owner, r.Daemon.Step.Repo, s.Number, head)
		return []verify.Finding{{Name: "the head", Detail: "The head " + head + " isn't in the bare repo."}}, nil, nil
	}
	cfg := r.Daemon.Config.Verify
	f := verify.Facts{Repo: r.Daemon.Clone, Baseline: baseline, Head: head, Target: target, Scope: scopeOf(s), Config: cfg}
	base, err := verify.BaseOf(ctx, f)
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		return []verify.Finding{{Rule: 2, Name: "protected paths", Detail: targetBasesDetail(baseline, target, err)}}, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read the head's base: %w", err)
	}
	f.Base = base
	protected, err := verify.Rule2(ctx, f)
	if err != nil {
		return nil, nil, fmt.Errorf("check rule 2: %w", err)
	}
	if protected != nil {
		return []verify.Finding{*protected}, nil, nil
	}
	results, err := r.Commands.Run(ctx, r.newID(), head, cfg.Commands)
	if err != nil {
		return nil, nil, fmt.Errorf("rule 1: %w", err)
	}
	var findings []verify.Finding
	for _, rule := range []func(context.Context, verify.Facts) (*verify.Finding, error){verify.Rule3, verify.Rule4, verify.Rule5, verify.Rule6, verify.Rule7} {
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

// targetBasesDetail is rule 2's finding on a head that doesn't have exactly
// one merge base with target, its target, whose baseline is baseline, as
// err, verify.BaseOf's, says: mergeBasesDetail's when target is the
// baseline, a main commit, as for a task with no parent, and else one
// naming target as the story branch's tip it is (VerifyTarget).
func targetBasesDetail(baseline, target string, err error) string {
	if baseline == target {
		return mergeBasesDetail(target, err)
	}
	return fmt.Sprintf("The head doesn't have exactly one merge base with its story branch's tip %s, so the task's own diff has no start: %v", target, err)
}

// VerifyTarget is what the checks at verifying judge a task's head against
// (SPEC §7.5, §10.1): SHA, rule 7's target, the head's own diff starting at
// its merge base with it, and Baseline, rule 2's baseline. For a task with
// no parent, both are main's commit as the pass's sync left it, and Branch
// is "" (mainTarget). For a task in a story, SHA is the tip of its story's
// branch, Branch, as the pass's sync left it, and Baseline is the story's,
// the merge base of that tip and main (storyTarget). A task at pr-open's
// head is checked on its merge with one the same way once it moves
// (mergeTarget).
type VerifyTarget struct {
	Branch, SHA, Baseline string
}

// mainTarget is main's commit main as the target and baseline of a task
// with no parent.
func mainTarget(main string) VerifyTarget {
	return VerifyTarget{SHA: main, Baseline: main}
}

// label names t in the App's comments and a verifier's context: "`main`
// at `<SHA>`", or "the story branch `<name>` at `<SHA>`", with the name,
// which an admin may have chosen, as a code span with its markers escaped
// (checkCode, SPEC §5.1). A SHA is git's, so it needs no escaping.
func (t VerifyTarget) label() string {
	if t.Branch == "" {
		return fmt.Sprintf("`main` at `%s`", t.SHA)
	}
	return fmt.Sprintf("the story branch %s at `%s`", checkCode(t.Branch), t.SHA)
}

// String names t in the log: "main's <SHA>", or "the story branch <name>'s
// tip <SHA>".
func (t VerifyTarget) String() string {
	if t.Branch == "" {
		return "main's " + t.SHA
	}
	return fmt.Sprintf("the story branch %s's tip %s", t.Branch, t.SHA)
}

// storyTarget is the target of task s, which has a parent, whose checks at
// verifying run with main's commit main as the pass's sync left it, and
// true; or false when they don't run, uncounted (SPEC §7.5, §10.1):
//   - its parent isn't its story (storyOf), as an epic planned into
//     stories, or one in another repo or closed: it gets no checks, as it
//     gets no run, logged at debug.
//   - its parent's plan data doesn't read back (storyOf's error), or its
//     story has two branches with its prefix (errTwoBranches), or none, or
//     that branch's tip has no issuedog/tip naming the branch as success,
//     as the daemon judged or read it (tipSuccess), or for a failure's bug
//     of the story, isn't a tip step 4 judged that passes rule 2, whatever
//     else fails there (storyTipTaken), or the tip has no one merge base
//     with main, so the story has no baseline: it waits, and that's
//     warned of once (storyWarnOnce), until that changes.
//   - the App just merged a task's PR onto that tip (tipMergedOnto), so
//     the branch is past it till the next sync: it waits, logged at info.
//
// Otherwise it's the story branch, its tip and the story's baseline, the
// merge base of that tip and main. The parent's dirty content is read only
// for an epic's plan, as storyOf reads it, and a failure to read it is an
// error. The rest reads only the snapshots the daemon holds, its judged
// tips and the bare repo, so a task that waits costs no request.
func (r *Runs) storyTarget(ctx context.Context, s *github.Snapshot, main string) (VerifyTarget, bool, error) {
	d, n := r.Daemon, s.Number
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
		return VerifyTarget{}, false, fmt.Errorf("read its parent #%d: %w", s.Parent, dirtErr)
	case err != nil:
		d.storyWarnOnce(n, "plan:"+err.Error(), "%s/%s#%d gets no checks at %s, as it can't tell what its parent #%d's plan is of: %v",
			a.Owner, a.Repo, n, state.Verifying, s.Parent, err)
		return VerifyTarget{}, false, nil
	case !ok:
		logr.Debugf("%s/%s#%d gets no checks at %s: its parent, #%d, isn't a story it's worked in", a.Owner, a.Repo, n, state.Verifying, s.Parent)
		return VerifyTarget{}, false, nil
	}
	branch, tip, err := d.storyBranch(ctx, story)
	switch {
	case errors.Is(err, errTwoBranches):
		d.storyWarnOnce(n, "branches:"+err.Error(), "%s/%s#%d waits at %s, as it can't tell which is its story #%d's branch: %v",
			a.Owner, a.Repo, n, state.Verifying, story.Number, err)
		return VerifyTarget{}, false, nil
	case err != nil:
		return VerifyTarget{}, false, fmt.Errorf("name its story branch's tip: %w", err)
	case tip == "":
		d.storyWarnOnce(n, "no-branch:"+branch, "%s/%s#%d waits at %s, as its story #%d has no branch yet",
			a.Owner, a.Repo, n, state.Verifying, story.Number)
		return VerifyTarget{}, false, nil
	case d.tipMergedOnto(branch, tip):
		logr.Infof("%s/%s#%d waits at %s, as the App just merged a task's PR onto its story branch %s's tip %s, so the branch is past it till the next sync",
			a.Owner, a.Repo, n, state.Verifying, branch, tip)
		return VerifyTarget{}, false, nil
	}
	switch taken, bug, err := d.storyTipTaken(ctx, s, story, branch, tip); {
	case err != nil:
		return VerifyTarget{}, false, fmt.Errorf("judge its story branch's tip: %w", err)
	case !taken && bug:
		d.storyWarnOnce(n, "tip:"+branch+"@"+tip, "%s/%s#%d waits at %s, as its story branch %s's tip %s %s",
			a.Owner, a.Repo, n, state.Verifying, branch, tip, failureTipWhy)
		return VerifyTarget{}, false, nil
	case !taken:
		d.storyWarnOnce(n, "tip:"+branch+"@"+tip, "%s/%s#%d waits at %s, as its story branch %s's tip %s has no %s naming it as success",
			a.Owner, a.Repo, n, state.Verifying, branch, tip, github.TipCheck)
		return VerifyTarget{}, false, nil
	}
	base, err := verify.BaseOf(ctx, verify.Facts{Repo: d.Clone, Head: tip, Target: main})
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		d.storyWarnOnce(n, "baseline:"+branch+"@"+tip, "%s/%s#%d waits at %s, as its story branch %s's tip %s has no one baseline: %v",
			a.Owner, a.Repo, n, state.Verifying, branch, tip, err)
		return VerifyTarget{}, false, nil
	case err != nil:
		return VerifyTarget{}, false, fmt.Errorf("read its story's baseline: %w", err)
	}
	return VerifyTarget{Branch: branch, SHA: tip, Baseline: base}, true, nil
}

// targetNow is t's commit as the pass's sync left it: main's, or for a
// story branch, its tip, or "" when the bare repo has no such branch. It
// reads only the bare repo, so it costs no request.
func (d *Daemon) targetNow(ctx context.Context, t VerifyTarget) (string, error) {
	if t.Branch == "" {
		main, err := d.Clone.Resolve(ctx, "main")
		if err != nil {
			return "", fmt.Errorf("read main: %w", err)
		}
		return main, nil
	}
	branches, err := d.Clone.Branches(ctx, t.Branch)
	if err != nil {
		return "", fmt.Errorf("read the story branch %s: %w", t.Branch, err)
	}
	return branches[t.Branch], nil
}

// commandsFailed is how many of results failed.
func commandsFailed(results []verify.CommandResult) int {
	failed := 0
	for _, c := range results {
		if c.Failure() != "" {
			failed++
		}
	}
	return failed
}

// verifyFindings moves rd's task, whose state was is when the checks
// started, verifying → in-progress on findings (SPEC §7.2), keyed by
// verifyKey of the comment holding its move to verifying, with head=,
// attempt=, that move's (verifyAttempt), so the round's count lives on
// the issue (SPEC §8.3), and seen=, the newest comment on step 1's
// snapshot the checks started from, as a run's move has it, so a comment
// posted during the checks is above the move's bound: at in-progress,
// where step 1 gives no input, it reaches the next run's conversation.md,
// as any trusted comment does, not its input.md. Its comment is
// renderFindings', naming the attempt of loop.max_attempts, and t, what
// the head was checked against: main, or its story branch and tip.
//
// When that attempt is loop.max_attempts or more, the round's last, the
// task never goes back to in-progress: it moves verifying → needs-human on
// attempt-cap instead, keyed as the findings move would be, so a replay or
// a restart writes it once, with seen= as that move would have it, so a
// comment posted during the checks is above the hand-off's round bound
// (SPEC §5). Its comment is the verify cap's hand-off
// (renderVerifyCapHandoff), which counts the round's attempts as that
// attempt's number, loop.max_attempts or more, so after a
// loop.max_attempts lowered since it counts no fewer than the findings
// name, then gives the findings and ends with the cc line for notify
// (SPEC §8.3, §9.3). That releases the worker (SPEC §8.1), and /done or a
// trusted 👍 on the hand-off sends the task back to ready, a new round
// (handback.go). A move to verifying that names no attempt, as one by
// hand, never reaches the cap.
//
// Before the move, it reads the issue again, and runs CheckLabels and the
// look for discussion triggers on it, as a run's apply does (SPEC §8.2).
// A task closed, gone, held, or moved since the checks started, even back
// to verifying, gets no move: then its next checks, if any, are on what's
// there.
func (r *Runs) verifyFindings(ctx context.Context, rd Ready, is state.Issue, head string, t VerifyTarget, fs []verify.Finding, cmds []verify.CommandResult) error {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error { return fmt.Errorf("move the task back on its findings: %w", err) }
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, so it gets no findings move, and is read again: %v", a.Owner, a.Repo, n, err)
		r.Daemon.reread(n, true)
		return nil
	case err != nil:
		return wrap(err)
	}
	defer r.Daemon.reread(n, false)
	d, err := a.Dirty(ctx, s, r.Daemon.Step.Trust)
	if err == nil {
		err = a.discCheckLabels(ctx, s, r.Daemon.Step.Trust, d)
	}
	if err != nil {
		return wrap(err)
	}
	if s.Closed {
		logr.Infof("%s/%s#%d was closed during the checks, so it gets no findings move", a.Owner, a.Repo, n)
		return nil
	}
	if _, err := r.Daemon.Step.discussion(ctx, s, d); err != nil {
		return wrap(err)
	}
	// Any move since the checks started, a hold's or a stage label's
	// record included, is a newer transition marker, even one back to
	// verifying, so the latest marker's comment tells.
	now := state.Derive(s, a.AppSlug, d.Has)
	if now.TransitionComment != is.TransitionComment {
		logr.Infof("%s/%s#%d moved to %s during the checks, so it gets no findings move", a.Owner, a.Repo, n, now.Stage)
		return nil
	}
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	attempt := verifyAttempt(is)
	if attempt >= r.limit() {
		m := Move{To: state.NeedsHuman, Trigger: state.TriggerAttemptCap, Key: verifyKey(is.TransitionComment),
			Text: renderVerifyCapHandoff(attempt, findingsText(head, t.label(), attempt, r.limit(), fs, cmds), n, r.Daemon.Config.Notify)}
		if seen > 0 {
			m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
		}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return fmt.Errorf("hand off the verify cap: %w", err)
		}
		logr.Warnf("%s/%s#%d: its head %s fails the checks against %s, in %d rules and %d commands, on attempt %d of %d, so it's handed off to the owner at %s",
			a.Owner, a.Repo, n, head, t, len(fs), commandsFailed(cmds), attempt, r.limit(), state.NeedsHuman)
		return nil
	}
	m := Move{To: state.InProgress, Trigger: state.TriggerFindings, Key: verifyKey(is.TransitionComment), Fields: map[string]string{},
		Text: renderFindings(head, t.label(), attempt, r.limit(), fs, cmds)}
	if head != "" {
		m.Fields["head"] = head
	}
	if attempt > 0 {
		m.Fields[fieldAttempt] = strconv.Itoa(attempt)
	}
	if seen > 0 {
		m.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: its head %s fails the checks against %s, in %d rules and %d commands, so it's back at %s",
		a.Owner, a.Repo, n, head, t, len(fs), commandsFailed(cmds), state.InProgress)
	return nil
}

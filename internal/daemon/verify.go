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

// verifyKey keys a task's move verifying → in-progress on findings (SPEC
// §5), or in its place at the round's last attempt, the verify cap's move
// to needs-human: event=v<c>, the comment holding its move to verifying,
// so the checks of each move there move it on once.
func verifyKey(c int64) state.Key {
	return state.Key{Field: "event", ID: "v" + strconv.FormatInt(c, 10)}
}

// verifyTask is step 4's checks at verifying (SPEC §8.1, §10.1), which
// Work runs after task work at in-progress and before the pick. It runs
// the checks on the first of ready, by priority then number, that is an
// open task at verifying, by its stage label and its markers, of a class
// that gets a PR (state.AnyTask), so never a human task, and reports
// whether it ran them. Its findings move, once written, is its latest
// marker, so its markers no longer put it at verifying. A task with a
// parent gets none, uncounted, as it gets no run (inStory): its baseline
// and target are its story branch's, which nothing names yet (#624). With
// no Commands, no task gets checks.
//
// The head is the latest move into verifying's head=, or else the task's
// head, taskRef (verifyHead). It's checked against main's commit as the
// pass's sync left it, rule 2's baseline and rule 7's target, by
// verifyChecks. A pass writes nothing: the task waits at verifying, and
// keeps the worker, until the verifier and the PR are built (#620, #622).
// The pass is kept in passed, by head and main's commit, so the checks
// don't run again until either changes, or the daemon restarts. A
// failure moves the task back to in-progress, or on the round's last
// attempt, hands it off to the owner at needs-human (verifyFindings).
//
// Naming the head and main, and a skip for a pass kept, read only the bare
// repo and the snapshot step 1 left, so they cost no request.
//
// The commands run as agent, and after each one every agent process is
// killed (agent.KillLeftovers), so the checks must never overlap a claude
// run, which runs as agent too. Work keeps them apart: it runs one thing
// at a time, and runs the checks only once resume has applied or ended any
// run a lease names, and no task work ran in the pass (SPEC §8.2).
func (r *Runs) verifyTask(ctx context.Context, ready []Ready) (bool, error) {
	if r.Commands == nil {
		return false, nil
	}
	a := r.Daemon.Step.Applier
	for _, rd := range ready {
		s, n := rd.S, rd.S.Number
		if s.Closed || stageOf(s) != state.Verifying {
			continue
		}
		if s.Parent != 0 {
			logr.Debugf("%s/%s#%d gets no checks at %s: it has a parent, #%d, and a task in a story gets none until its story branch's tip is named",
				a.Owner, a.Repo, n, state.Verifying, s.Parent)
			continue
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
		head, err := r.verifyHead(ctx, n, is)
		if err != nil {
			return false, fmt.Errorf("verify #%d: %w", n, err)
		}
		main, err := r.Daemon.Clone.Resolve(ctx, "main")
		if err != nil {
			return false, fmt.Errorf("verify #%d: read main: %w", n, err)
		}
		if r.passed[n] == head+":"+main {
			logr.Debugf("%s/%s#%d: its head %s passed the checks against main's %s already, so it waits at %s", a.Owner, a.Repo, n, head, main, state.Verifying)
			continue
		}
		findings, results, err := r.verifyChecks(ctx, s, head, main)
		if err != nil {
			return true, fmt.Errorf("verify #%d: %w", n, err)
		}
		if len(findings) == 0 && commandsFailed(results) == 0 {
			if r.passed == nil {
				r.passed = map[int]string{}
			}
			r.passed[n] = head + ":" + main
			logr.Infof("%s/%s#%d: its head %s passes the checks against main's %s, so it waits at %s", a.Owner, a.Repo, n, head, main, state.Verifying)
			return true, nil
		}
		if err := r.verifyFindings(ctx, rd, is, head, main, findings, results); err != nil {
			return true, fmt.Errorf("verify #%d: %w", n, err)
		}
		return true, nil
	}
	return false, nil
}

// verifyHead is the head of task n, whose state is is, at verifying: the
// head= of its latest move into verifying, which the implementer's move
// carries (SPEC §5), or else the task's head, taskRef(n), or "" with
// neither, as for a task set to verifying by hand before any run.
func (r *Runs) verifyHead(ctx context.Context, n int, is state.Issue) (string, error) {
	head := ""
	for _, m := range is.Markers {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Verifying {
			head = m.Fields["head"]
		}
	}
	if head != "" {
		return head, nil
	}
	head, _, err := r.Daemon.Clone.ReadRef(ctx, taskRef(n))
	if err != nil {
		return "", fmt.Errorf("read the task's head: %w", err)
	}
	return head, nil
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
// main's commit main as rule 2's baseline and rule 7's target, for the
// task s, whose scope rule 4 reads (scopeOf). It returns each rule's
// finding, and rule 1's commands' results. No head, or one the bare repo
// lacks, is the one finding, with no rule, and no rule runs. Rule 2 runs
// first, and its failure stops the checks, so no command runs on a diff
// that touches a protected path. A head with no merge base with main, or
// more than one, fails rule 2 too (verify.ErrMergeBases), as the task's own
// diff then has no start. Otherwise every command runs, in a fresh
// checkout of head named by a new ID, never the lease's run's, which the
// commands remove when they're done, and rules 3 to 7 all run, so the
// implementer sees every finding at once (SPEC §10.1). A failure to read
// the bare repo, or to run the commands, is an error, not a finding.
func (r *Runs) verifyChecks(ctx context.Context, s *github.Snapshot, head, main string) ([]verify.Finding, []verify.CommandResult, error) {
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
	f := verify.Facts{Repo: r.Daemon.Clone, Baseline: main, Head: head, Target: main, Scope: scopeOf(s), Config: cfg}
	base, err := verify.BaseOf(ctx, f)
	switch {
	case errors.Is(err, verify.ErrMergeBases):
		return []verify.Finding{{Rule: 2, Name: "protected paths", Detail: mergeBasesDetail(main, err)}}, nil, nil
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
// renderFindings', naming the attempt of loop.max_attempts.
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
func (r *Runs) verifyFindings(ctx context.Context, rd Ready, is state.Issue, head, main string, fs []verify.Finding, cmds []verify.CommandResult) error {
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
			Text: renderVerifyCapHandoff(attempt, findingsText(head, main, attempt, r.limit(), fs, cmds), n, r.Daemon.Config.Notify)}
		if seen > 0 {
			m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
		}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return fmt.Errorf("hand off the verify cap: %w", err)
		}
		logr.Warnf("%s/%s#%d: its head %s fails the checks against main's %s, in %d rules and %d commands, on attempt %d of %d, so it's handed off to the owner at %s",
			a.Owner, a.Repo, n, head, main, len(fs), commandsFailed(cmds), attempt, r.limit(), state.NeedsHuman)
		return nil
	}
	m := Move{To: state.InProgress, Trigger: state.TriggerFindings, Key: verifyKey(is.TransitionComment), Fields: map[string]string{},
		Text: renderFindings(head, main, attempt, r.limit(), fs, cmds)}
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
	logr.Infof("%s/%s#%d: its head %s fails the checks against main's %s, in %d rules and %d commands, so it's back at %s",
		a.Owner, a.Repo, n, head, main, len(fs), commandsFailed(cmds), state.InProgress)
	return nil
}

package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/workspace"
	"github.com/mertenvg/logr/v2"
)

// Stage is one kind of non-task stage run, such as the interviewer's (#6).
// Runs runs it for issues at the stage it's registered at.
type Stage interface {
	// Agent names the stage's settings in config.Stages, such as "interviewer".
	Agent() string
	// Pending reports whether the issue has work for the stage. It's asked
	// before the trust check of in.Inputs, and again after it.
	Pending(in Input) bool
	// Context returns the run's context files by name, prompt.md and
	// system.md among them, which the agent can read but not change.
	Context(in Input) (map[string][]byte, error)
	// Schema is the JSON schema of the stage's structured output.
	Schema() []byte
	// Apply makes the writes for a valid result, each checked first, so it
	// can run again after a crash part way (SPEC §5.2). Its moves are
	// r.Move's, keyed by run=<id>. It may run again after a restart.
	Apply(ctx context.Context, a *Applier, r Result) error
}

// Input is what a Stage gets before a run.
type Input struct {
	RunID string
	// S is the issue as step 1 read it.
	S *github.Snapshot
	// Inputs are what step 1's Run returned, less those whose author is no
	// longer trusted.
	Inputs []github.Comment
	// Attempt is the run's number, from 1, since the stage last moved or
	// got new input.
	Attempt int
}

// Result is a valid run result, for a Stage to apply.
type Result struct {
	RunID string
	// Seen is the newest comment the run was given.
	Seen int64
	// S is the issue, read again after the run and checked by CheckLabels.
	// Apply adds its writes to it.
	S      *github.Snapshot
	Dirt   Dirt
	Output json.RawMessage // valid by the Stage's Schema

	moveKey string // see moveKey
}

// Move is a move keyed by the run, with seen=r.Seen, so the next round's
// input is what came after it (state.RoundBound). If the table lacks it,
// its state error is keyed by moveKey, so the stage runs no more until the
// issue changes.
func (r Result) Move(to state.Stage, trigger state.Trigger) Move {
	m := Move{To: to, Trigger: trigger, Key: state.Key{Field: "run", ID: r.RunID}, ErrorKey: r.moveKey}
	if r.Seen > 0 {
		m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(r.Seen, 10)}
	}
	return m
}

// Lease is /state/lease.json, the run in progress (SPEC §8.2), or with
// Failed, the record of the attempts of the last run that failed. A
// non-task run doesn't move its issue when it starts, so Stage is also the
// stage a restart resets it to, which writes nothing: an issue at another
// stage was moved by the owner, which wins.
type Lease struct {
	Issue   int         `json:"issue"`
	Stage   state.Stage `json:"stage"`
	RunID   string      `json:"run_id"`
	Clone   string      `json:"clone"`
	Started time.Time   `json:"started"`
	Polled  time.Time   `json:"polled"` // when the last poll before pickup began
	// Seen is the newest comment the run was given, and Edits the last
	// edit of each of its inputs, as the run read them: zero for none.
	Seen  int64               `json:"seen"`
	Edits map[int64]time.Time `json:"edits,omitempty"`
	// AttemptKey names the attempts the run is one of (attemptKey), and
	// Attempt is its number among them, from 1.
	AttemptKey string `json:"attempt_key"`
	Attempt    int    `json:"attempt"`
	// ApplyFailures counts the failed applies of the run's result.
	ApplyFailures int `json:"apply_failures,omitempty"`
	// Failed marks a run that failed, whose files are gone: the lease is
	// kept for Attempt, so a restart doesn't reset the count.
	Failed bool `json:"failed,omitempty"`
}

// kindAttempts is the state error for a stage that failed loop.max_attempts
// times in a row.
const kindAttempts = "attempts"

// Runs is the stage-run harness. Its Work is the loop's stage work (SPEC
// §8.1, step 3), one run at a time, under a lease (SPEC §8.2).
type Runs struct {
	// Daemon gives the applier, trust and config of the moment.
	Daemon *Daemon
	// Stages are the registered Stages, by the stage they run at.
	Stages    map[state.Stage]Stage
	Runner    *agent.Runner // its StateDir holds the lease and the results
	Workspace *workspace.Workspace
	Gate      *agent.Gate
	Env       []string         // the agent's whole environment (agent.Env)
	Now       func() time.Time // nil is time.Now
	NewID     func() string    // nil makes random run IDs

	attempts map[string]int // failed runs, by countKey
	swept    bool           // leftover temp leases are removed
}

// Work is Daemon.Work. It first finishes a lease a run left, after a
// restart or a failed apply (resume). Then, unless a usage limit closed
// the gate, it runs the first issue, by priority then number, whose stage
// has a Stage with work pending.
func (r *Runs) Work(ctx context.Context, ready []Ready) error {
	if !r.swept {
		r.sweep()
		r.swept = true
	}
	skip, err := r.resume(ctx)
	if err != nil {
		return err
	}
	if now := r.now(); !r.Gate.Open(now) {
		logr.Debugf("stage work waits for a usage limit until %s", r.Gate.OpenAt().Format(time.RFC3339))
		return nil
	}
	ready = slices.Clone(ready)
	slices.SortStableFunc(ready, func(a, b Ready) int { return cmp.Compare(priority(a.S), priority(b.S)) })
	for _, rd := range ready {
		st, ok := r.Stages[stageOf(rd.S)]
		if !ok || rd.S.Number == skip {
			continue
		}
		if ran, err := r.RunStage(ctx, rd, st); err != nil || ran {
			return err
		}
	}
	return nil
}

// RunStage runs st on the issue rd if it has work pending, and reports
// whether it ran. A run that fails, but not on a usage limit or in its
// setup, counts as an attempt (failed); after loop.max_attempts in a row
// the issue gets one state error (SPEC §5.3) and no more runs until the
// stage moves or gets new input. A result asking for a move the table
// lacks is no attempt: it gets one state error (Transition), and no run
// starts until the issue changes (moveKey). A run whose result fails to
// apply keeps its lease, for the next pass to apply it (applyLease). A
// failed run logs the end of claude's stderr.
func (r *Runs) RunStage(ctx context.Context, rd Ready, st Stage) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("stage run on %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	in := Input{S: rd.S, Inputs: rd.Inputs}
	if !st.Pending(in) {
		return false, nil
	}
	d, err := a.Dirty(ctx, rd.S)
	if err != nil {
		return false, wrap(err)
	}
	is := state.Derive(rd.S, a.AppSlug, d.Has)
	errKey := attemptKey(is, rd.Inputs)
	if is.Reported(kindAttempts, errKey) {
		logr.Debugf("%s/%s#%d: no %s run: it failed loop.max_attempts times", a.Owner, a.Repo, n, st.Agent())
		return false, nil
	}
	if is.Reported("transition", moveKey(errKey, is.Item)) {
		logr.Debugf("%s/%s#%d: no %s run: the table lacks the move its last run asked for", a.Owner, a.Repo, n, st.Agent())
		return false, nil
	}
	if r.attempts[countKey(n, errKey)] >= r.limit() {
		// The cap was reached, but posting its state error failed: post it.
		return false, wrap(r.reportCap(ctx, nil, st, Lease{Issue: n, Stage: is.Stage, AttemptKey: errKey}))
	}
	fresh, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, deleted or transferred, so it gets no run and is read again: %v", a.Owner, a.Repo, n, err)
		r.Daemon.reread(n, true)
		return false, nil
	case err != nil:
		return false, wrap(err)
	case fresh.Closed || stageOf(fresh) != is.Stage:
		r.Daemon.reread(n, false)
		return false, nil
	}
	if in.Inputs, err = r.trusted(ctx, rd.Inputs); err != nil {
		return false, wrap(err)
	}
	if !st.Pending(in) {
		return false, nil
	}
	in.RunID, in.Attempt = r.newID(), r.attempts[countKey(n, errKey)]+1
	l := Lease{Issue: n, Stage: is.Stage, RunID: in.RunID, Clone: r.Workspace.Dir(in.RunID),
		Started: r.now(), Polled: r.Daemon.polled, Edits: map[int64]time.Time{}, AttemptKey: errKey, Attempt: in.Attempt}
	for _, c := range rd.S.Comments {
		l.Seen = max(l.Seen, c.ID)
	}
	for _, c := range in.Inputs {
		l.Edits[c.ID] = c.Edit.At
	}
	defer r.Daemon.reread(n, false)
	if err := r.writeLease(l); err != nil {
		return true, wrap(err)
	}
	logr.Infof("%s/%s#%d: starting %s run %s, attempt %d", a.Owner, a.Repo, n, st.Agent(), l.RunID, in.Attempt)
	stderr := agent.NewTail(stderrTail)
	logStderr := func() {
		if stderr.Len() > 0 {
			logr.Warnf("%s/%s#%d: run %s failed; the last %d of %d bytes claude wrote to stderr:\n%s",
				a.Owner, a.Repo, n, l.RunID, stderr.Len(), stderr.Total(), stderr.Text())
		}
	}
	raw, runErr := r.run(ctx, st, in, stderr)
	var setup *agent.SetupError
	switch {
	case ctx.Err() != nil && runErr == nil:
		return true, wrap(ctx.Err()) // the lease and result stay, for the restart to apply
	case ctx.Err() != nil || errors.As(runErr, &setup):
		// Not the agent's failure, so not an attempt: claude didn't start,
		// or the daemon is stopping, so the cleanup ignores ctx.
		return true, wrap(errors.Join(runErr, r.uncounted(context.WithoutCancel(ctx), l)))
	}
	if failed, text := outcome(raw); r.Gate.Observe(r.now(), failed, text) {
		logStderr()
		logr.Warnf("%s/%s#%d: run %s hit a usage limit, so stage work waits until %s; it isn't counted as an attempt",
			a.Owner, a.Repo, n, l.RunID, r.Gate.OpenAt().Format(time.RFC3339))
		return true, wrap(r.uncounted(ctx, l))
	}
	out := json.RawMessage(nil)
	if runErr == nil {
		var schema *agent.Schema
		if schema, err = agent.CompileSchema(st.Schema()); err != nil {
			return true, wrap(errors.Join(err, r.uncounted(ctx, l)))
		}
		// Validate fails whatever outcome calls failed, and invalid output.
		out, runErr = agent.Validate(raw, schema)
	}
	if runErr != nil {
		logStderr()
		// The issue may have changed during the run, so failed reads it again.
		return true, wrap(r.failed(ctx, st, l, runErr)) // logged there
	}
	return true, wrap(r.applyLease(ctx, l, st, out))
}

// stderrTail is how much of claude's stderr a failed run logs: the end.
const stderrTail = 4096

// run prepares the workspace and runs claude for st, returning its output
// and sending its stderr to stderr. A failure to set the run up is an
// *agent.SetupError, as Runner.Run's are.
func (r *Runs) run(ctx context.Context, st Stage, in Input, stderr io.Writer) ([]byte, error) {
	cfg := r.Daemon.Config.Stages
	turns, okT := pick(cfg.MaxTurns, st.Agent())
	budget, okB := pick(cfg.MaxBudgetUSD, st.Agent())
	timeout, okD := pick(cfg.Timeouts, st.Agent())
	if !okT || !okB || !okD {
		return nil, &agent.SetupError{Err: fmt.Errorf("no settings in config.stages for agent %q", st.Agent())}
	}
	files, err := st.Context(in)
	if err != nil {
		return nil, &agent.SetupError{Err: fmt.Errorf("context files: %w", err)}
	}
	if err := r.Workspace.Prepare(ctx, in.RunID, "", files); err != nil {
		return nil, &agent.SetupError{Err: err}
	}
	runner := *r.Runner
	if runner.Stderr != nil {
		stderr = io.MultiWriter(runner.Stderr, stderr)
	}
	runner.Stderr = stderr
	out, err := runner.Run(ctx, agent.Stage{RunID: in.RunID, Dir: r.Workspace.Dir(in.RunID), ContextDir: r.Workspace.Context(in.RunID),
		Schema: st.Schema(), MaxTurns: int(turns), MaxBudgetUSD: budget, Timeout: timeout, Env: r.Env})
	if err != nil {
		return out, fmt.Errorf("run %s: %w", in.RunID, err)
	}
	return out, nil
}

// outcome reads from a run's output whether it failed, and its text: it
// failed unless it parses with subtype success and no is_error (SPEC
// §3.3). This, never a Validate error, is what the gate is told.
func outcome(raw []byte) (failed bool, text string) {
	var res agent.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return true, string(raw)
	}
	return res.Subtype != "success" || res.IsError, res.Result
}

// applyLease applies l's result (apply). A failure keeps the lease,
// counted in ApplyFailures, so the next pass tries again; after
// loop.max_attempts of them, the run is a failed attempt.
func (r *Runs) applyLease(ctx context.Context, l Lease, st Stage, out json.RawMessage) error {
	err := r.apply(ctx, l, st, out)
	if err == nil {
		return nil
	}
	l.ApplyFailures++
	if l.ApplyFailures < r.limit() {
		logr.Warnf("applying run %s on #%d failed, %d of %d times, so it's tried again: %v", l.RunID, l.Issue, l.ApplyFailures, r.limit(), err)
		return errors.Join(err, r.writeLease(l))
	}
	return errors.Join(err, r.failed(ctx, st, l, fmt.Errorf("applying its result failed %d times: %w", l.ApplyFailures, err)))
}

// apply reads the issue again, runs CheckLabels on it, and applies out,
// l's valid result, through st, then removes the run's files. The result
// is dropped, writing nothing, when the issue is gone or closed, when its
// stage changed during the run (the owner's change wins, and CheckLabels
// records one by hand), or when one of its inputs was edited or deleted
// since the run read it, so the stage runs again on what's there. Edits to
// other comments, such as an untrusted user's to their own, don't count.
// A result whose move is written already is only finished. A move the
// table lacks ends the run (blocked). On an error, the lease stays.
func (r *Runs) apply(ctx context.Context, l Lease, st Stage, out json.RawMessage) error {
	a := r.Daemon.Step.Applier
	drop := func(why string, args ...any) error {
		logr.Infof("%s/%s#%d: dropping the result of run %s: "+why, append([]any{a.Owner, a.Repo, l.Issue, l.RunID}, args...)...)
		return r.uncounted(ctx, l)
	}
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, l.Issue)
	switch {
	case gone(err):
		r.Daemon.reread(l.Issue, true)
		return drop("the issue looks gone, deleted or transferred: %v", err)
	case err != nil:
		return fmt.Errorf("apply run %s: %w", l.RunID, err)
	case s.Closed:
		return drop("the issue is closed")
	}
	defer r.Daemon.reread(l.Issue, false)
	d, err := a.Dirty(ctx, s)
	if err == nil {
		err = a.CheckLabels(ctx, s, r.Daemon.Step.Trust, d.Has)
	}
	if err != nil {
		return fmt.Errorf("apply run %s: %w", l.RunID, err)
	}
	is := state.Derive(s, a.AppSlug, d.Has)
	if !is.Moved(state.Key{Field: "run", ID: l.RunID}) {
		if is.Stage != l.Stage {
			return drop("the issue moved from %s to %s during the run", l.Stage, is.Stage)
		}
		inputs := 0
		for _, c := range s.Comments {
			at, input := l.Edits[c.ID]
			if input && !c.Edit.At.Equal(at) {
				return drop("input %d was edited since the run read it, so the stage runs again", c.ID)
			}
			if input {
				inputs++
			}
		}
		if inputs < len(l.Edits) {
			return drop("an input was deleted since the run read it, so the stage runs again")
		}
	}
	key := moveKey(l.AttemptKey, is.Item)
	res := Result{RunID: l.RunID, Seen: l.Seen, S: s, Dirt: d, Output: out, moveKey: key}
	if err := st.Apply(ctx, a, res); err != nil {
		if StateError(err) {
			return r.blocked(ctx, s, d, st, l, key, err)
		}
		return fmt.Errorf("apply run %s: %w", l.RunID, err)
	}
	logr.Infof("%s/%s#%d: applied run %s", a.Owner, a.Repo, l.Issue, l.RunID)
	delete(r.attempts, countKey(l.Issue, l.AttemptKey))
	return r.finish(ctx, l)
}

// resume finishes the lease a run left, if there is one, and returns the
// issue it applied a result to, which gets no stage work this pass. A
// valid saved result is applied (applyLease). Otherwise the run counts as
// a failed attempt, its clone is deleted, and the issue, at its pre-run
// stage (see Lease), runs again when its stage has work pending. A held
// or closed issue's lease and clone are dropped by apply, or by that
// reset. A Failed lease only gives its count.
func (r *Runs) resume(ctx context.Context) (int, error) {
	l, ok, err := r.readLease(ctx)
	if err != nil || !ok {
		return 0, err
	}
	if l.Failed {
		r.count(l)
		return 0, nil
	}
	st, ok := r.Stages[l.Stage]
	if !ok {
		logr.Warnf("dropping the lease of run %s on #%d: no Stage is registered for %s", l.RunID, l.Issue, l.Stage)
		return 0, r.finish(ctx, l)
	}
	raw, err := os.ReadFile(r.resultPath(l.RunID))
	var out json.RawMessage
	if err == nil {
		var schema *agent.Schema
		if schema, err = agent.CompileSchema(st.Schema()); err != nil {
			return 0, fmt.Errorf("resume run %s: %w", l.RunID, err)
		}
		if out, err = agent.Validate(raw, schema); err != nil {
			logr.Infof("the saved result of run %s on #%d isn't valid: %v", l.RunID, l.Issue, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("resume run %s: %w", l.RunID, err)
	}
	if out == nil {
		// A crash in finish, after the result went, left a run that was
		// applied: that's no failure.
		a := r.Daemon.Step.Applier
		s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, l.Issue)
		switch {
		case gone(err):
			r.Daemon.reread(l.Issue, true)
			logr.Infof("dropping the lease of run %s: #%d looks gone, deleted or transferred: %v", l.RunID, l.Issue, err)
			return 0, r.finish(ctx, l)
		case err != nil:
			return 0, fmt.Errorf("resume run %s: %w", l.RunID, err)
		}
		d, err := a.Dirty(ctx, s)
		if err != nil {
			return 0, fmt.Errorf("resume run %s: %w", l.RunID, err)
		}
		if state.Derive(s, a.AppSlug, d.Has).Moved(state.Key{Field: "run", ID: l.RunID}) {
			logr.Infof("run %s on #%d was applied before the restart, so only its files are left to remove", l.RunID, l.Issue)
			return 0, r.finish(ctx, l)
		}
		logr.Infof("run %s on #%d left no valid result, so it's a failed attempt, its clone is deleted, and #%d stays at %s to run again",
			l.RunID, l.Issue, l.Issue, l.Stage)
		return 0, r.failed(ctx, st, l, errors.New("no valid result after a restart"))
	}
	logr.Infof("applying the saved result of run %s on #%d", l.RunID, l.Issue)
	return l.Issue, r.applyLease(ctx, l, st, out)
}

// failed records l's run as a failed attempt: it removes the run's files,
// and keeps the lease, as Failed, for the count. The last attempt allowed
// posts one state error (reportCap), after which RunStage runs no more. If
// posting it fails, RunStage posts it instead of starting a run.
func (r *Runs) failed(ctx context.Context, st Stage, l Lease, cause error) error {
	n := r.count(l)
	logr.Warnf("%s run %s on #%d failed, attempt %d of %d: %v", st.Agent(), l.RunID, l.Issue, n, r.limit(), cause)
	err := r.cleanup(ctx, l)
	if err == nil {
		l.Attempt, l.Failed, l.ApplyFailures = n, true, 0
		err = r.writeLease(l)
	}
	if err != nil {
		return fmt.Errorf("record failed run %s: %w", l.RunID, err)
	}
	if n < r.limit() {
		return nil
	}
	return r.reportCap(ctx, nil, st, l)
}

// count adds l's attempt to the count of its attempts, which it returns.
func (r *Runs) count(l Lease) int {
	if r.attempts == nil {
		r.attempts = map[string]int{}
	}
	key := countKey(l.Issue, l.AttemptKey)
	r.attempts[key] = max(r.attempts[key], l.Attempt)
	return r.attempts[key]
}

// reportCap posts the state error for l's stage failing loop.max_attempts
// times, keyed by l.AttemptKey, on s or, if nil, the issue read again. An
// issue that has it, has moved on, by a marker or a stage label not yet
// recorded, or is closed or gone gets none.
func (r *Runs) reportCap(ctx context.Context, s *github.Snapshot, st Stage, l Lease) error {
	a := r.Daemon.Step.Applier
	wrap := func(err error) error { return fmt.Errorf("report the attempt cap on #%d: %w", l.Issue, err) }
	if s == nil {
		fresh, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, l.Issue)
		if gone(err) {
			logr.Infof("#%d looks gone, so it gets no attempt cap error: %v", l.Issue, err)
			return nil
		}
		if err != nil {
			return wrap(err)
		}
		s = fresh
	}
	d, err := a.Dirty(ctx, s)
	if err != nil {
		return wrap(err)
	}
	if is := state.Derive(s, a.AppSlug, d.Has); s.Closed || is.Stage != l.Stage || otherStage(s, l.Stage) || is.Reported(kindAttempts, l.AttemptKey) {
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": kindAttempts, "key": l.AttemptKey}})
	if err != nil {
		return wrap(err)
	}
	text := fmt.Sprintf("The %s run failed %d times in a row, so I've stopped running it here. "+
		"New input, or a move to another stage, lets it run again.", st.Agent(), r.limit())
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return wrap(err)
	}
	logr.Errorf("state error: the %s run on %s/%s#%d failed %d times", st.Agent(), a.Owner, a.Repo, l.Issue, r.limit())
	return nil
}

// otherStage reports whether s has a stage label for another stage than
// st, such as one the owner set that step 1 hasn't recorded yet.
func otherStage(s *github.Snapshot, st state.Stage) bool {
	return slices.ContainsFunc(s.Labels, func(l string) bool {
		got, ok := state.ParseLabel(l)
		return ok && got != st
	})
}

// blocked ends l's run, whose result asked for a move the table lacks
// (cause, a StateError), on s. Running again can't fix that, so it's no
// attempt. It makes sure the issue has one state error keyed by key, the
// run's moveKey, so no run starts until the issue changes: Transition
// posts it for a move by Result.Move, and blocked for any other. Only an
// error that isn't cause is returned, as Transition logged cause.
func (r *Runs) blocked(ctx context.Context, s *github.Snapshot, d Dirt, st Stage, l Lease, key string, cause error) error {
	a := r.Daemon.Step.Applier
	if !state.Derive(s, a.AppSlug, d.Has).Reported("transition", key) {
		marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": "transition", "key": key}})
		if err != nil {
			return fmt.Errorf("report run %s's move: %w", l.RunID, err)
		}
		text := fmt.Sprintf("I can't apply the result of the %s run: %v. It won't run again until this issue changes.", st.Agent(), cause)
		if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
			return fmt.Errorf("report run %s's move: %w", l.RunID, err)
		}
		logr.Errorf("state error: %s/%s#%d: run %s: %v", a.Owner, a.Repo, l.Issue, l.RunID, cause)
	}
	logr.Infof("%s/%s#%d: run %s asked for a move the table lacks, so the %s stage waits for the issue to change", a.Owner, a.Repo, l.Issue, l.RunID, st.Agent())
	return r.uncounted(ctx, l)
}

// uncounted ends l's run without counting it as an attempt: a setup
// failure, a usage limit, a shutdown or a dropped result. A count of
// attempts before it stays, as a Failed lease.
func (r *Runs) uncounted(ctx context.Context, l Lease) error {
	if l.Attempt <= 1 {
		return r.finish(ctx, l)
	}
	if err := r.cleanup(ctx, l); err != nil {
		return fmt.Errorf("end run %s: %w", l.RunID, err)
	}
	l.Attempt, l.Failed, l.ApplyFailures = l.Attempt-1, true, 0
	return r.writeLease(l)
}

// moveKey keys the state error for a run's move the table lacks: the
// attempt key, and a hash of what decides the issue's class, so a move, a
// new input, or a change of type or parent lets the stage run again.
func moveKey(attemptKey string, it state.Item) string {
	return attemptKey + "-" + state.BodyHash(fmt.Sprintf("%+v", it))
}

// attemptKey names the stage's current round of attempts: its stage and the
// newest of the latest transition's bound and the inputs, so a move or a new
// input starts a new one.
func attemptKey(is state.Issue, inputs []github.Comment) string {
	bound := is.Round
	for _, c := range inputs {
		bound = max(bound, c.ID)
	}
	return fmt.Sprintf("%s-%d", is.Stage, bound)
}

// countKey is the key of issue n's attempts in Runs.attempts.
func countKey(n int, attemptKey string) string { return strconv.Itoa(n) + "/" + attemptKey }

// trusted returns the inputs whose author the current trust trusts,
// checking each author once (#84).
func (r *Runs) trusted(ctx context.Context, inputs []github.Comment) ([]github.Comment, error) {
	ok := map[string]bool{}
	var kept []github.Comment
	for _, c := range inputs {
		t, seen := ok[c.Author.Login]
		if !seen {
			var err error
			if t, err = r.Daemon.Step.Trust.Trusted(ctx, c.Author.Login); err != nil {
				return nil, fmt.Errorf("trust %s: %w", c.Author.Login, err)
			}
			ok[c.Author.Login] = t
			if !t {
				logr.Infof("dropping the inputs of %s, who is no longer trusted", c.Author.Login)
			}
		}
		if t {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

func (r *Runs) leasePath() string { return filepath.Join(r.Runner.StateDir, "lease.json") }
func (r *Runs) resultPath(id string) string {
	return filepath.Join(r.Runner.StateDir, "runs", id+".json")
}

// validID reports whether id can name a run's files.
func validID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.HasPrefix(id, ".")
}

// readLease reads the lease, if there is one. One that can't be read is
// logged and removed, as nothing can resume it, with its clone if it names
// one in the workspace.
func (r *Runs) readLease(ctx context.Context) (Lease, bool, error) {
	data, err := os.ReadFile(r.leasePath())
	if errors.Is(err, fs.ErrNotExist) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("read the lease: %w", err)
	}
	var l Lease
	err = json.Unmarshal(data, &l)
	if err == nil && !validID(l.RunID) {
		err = fmt.Errorf("bad run ID %q", l.RunID)
	}
	if err == nil {
		return l, true, nil
	}
	logr.Errorf("removing %s, which nothing can resume: %v", r.leasePath(), err)
	if id := filepath.Base(l.Clone); l.Clone != "" && filepath.Dir(l.Clone) == r.Workspace.RunsDir && validID(id) {
		if err := r.Workspace.Cleanup(ctx, id); err != nil {
			logr.Errorf("removing the clone %s of the bad lease: %v", l.Clone, err)
		}
	} else {
		logr.Warnf("the bad lease names no clone in %s, so any clone it had stays", r.Workspace.RunsDir)
	}
	if err := r.remove(r.leasePath()); err != nil {
		return Lease{}, false, fmt.Errorf("remove the lease: %w", err)
	}
	return Lease{}, false, nil
}

// writeLease writes the lease through a temp file and a rename, then syncs
// the state dir, so a restart reads the whole of one lease or another.
func (r *Runs) writeLease(l Lease) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("write the lease: %w", err)
	}
	f, err := os.CreateTemp(r.Runner.StateDir, "lease.*.tmp")
	if err != nil {
		return fmt.Errorf("write the lease: %w", err)
	}
	defer func() {
		// After the rename the temp file is gone, which is fine.
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logr.Warnf("removing %s: %v", f.Name(), err)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), r.leasePath())
	}
	if err == nil {
		err = syncDir(r.Runner.StateDir)
	}
	if err != nil {
		return fmt.Errorf("write the lease: %w", err)
	}
	return nil
}

// sweep removes the temp leases a crash in writeLease left.
func (r *Runs) sweep() {
	tmps, err := filepath.Glob(filepath.Join(r.Runner.StateDir, "lease.*.tmp"))
	if err != nil {
		logr.Errorf("finding leftover temp leases: %v", err)
	}
	for _, tmp := range tmps {
		if err := os.Remove(tmp); err != nil {
			logr.Errorf("removing the leftover temp lease %s: %v", tmp, err)
			continue
		}
		logr.Infof("removed the leftover temp lease %s", tmp)
	}
}

// cleanup removes the run's clone and context files, then its result.
func (r *Runs) cleanup(ctx context.Context, l Lease) error {
	if err := r.Workspace.Cleanup(ctx, l.RunID); err != nil {
		return fmt.Errorf("clean up run %s: %w", l.RunID, err)
	}
	return r.remove(r.resultPath(l.RunID))
}

// finish removes the run's files (cleanup), then the lease. The lease goes
// last, so a crash before it only repeats this.
func (r *Runs) finish(ctx context.Context, l Lease) error {
	err := r.cleanup(ctx, l)
	if err == nil {
		err = r.remove(r.leasePath())
	}
	if err != nil {
		return fmt.Errorf("finish run %s: %w", l.RunID, err)
	}
	return nil
}

// remove removes path, if it's there, then syncs its dir, so the removal
// survives a crash.
func (r *Runs) remove(path string) error {
	if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// syncDir flushes dir's entries to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

func (r *Runs) limit() int { return int(r.Daemon.Config.Loop.MaxAttempts) }

func (r *Runs) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runs) newID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	return strings.ToLower(rand.Text()[:8])
}

// pick is a PerStage's value for agent name.
func pick[T any](p config.PerStage[T], name string) (T, bool) {
	switch name {
	case "interviewer":
		return p.Interviewer, true
	case "planner":
		return p.Planner, true
	case "implementer":
		return p.Implementer, true
	case "verifier":
		return p.Verifier, true
	}
	var zero T
	return zero, false
}

// priority is s's priority:pN, or 2 with none (SPEC §5).
func priority(s *github.Snapshot) int {
	for _, l := range s.Labels {
		if p, ok := strings.CutPrefix(strings.ToLower(l), "priority:p"); ok && len(p) == 1 && p[0] >= '0' && p[0] <= '3' {
			return int(p[0] - '0')
		}
	}
	return 2
}

// stageOf is the stage s's first stage label names, or "".
func stageOf(s *github.Snapshot) state.Stage {
	for _, l := range s.Labels {
		if st, ok := state.ParseLabel(l); ok {
			return st
		}
	}
	return ""
}

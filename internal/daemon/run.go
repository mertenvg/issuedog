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
	"maps"
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
	// system.md among them, which the agent can read but not change. An
	// error is a setup failure: no run starts, and it isn't counted.
	Context(ctx context.Context, in Input) (map[string][]byte, error)
	// Schema is the JSON schema of the stage's structured output.
	Schema() []byte
	// Apply makes the writes for a valid result, each checked first, so it
	// can run again after a crash part way (SPEC §5.2). Its moves are
	// r.Move's, keyed by run=<id>. It may run again after a restart.
	Apply(ctx context.Context, a *Applier, r Result) error
}

// Checker is a Stage that checks a valid result before it's applied, past
// what its schema can say. Runs calls Check on the issue read again, before
// Apply, and only while the run's move isn't written, so a result applied
// part way before a restart isn't checked again. An error is a failed
// attempt (SPEC §8.3), with nothing written, never a failure to apply,
// unless it's a *checkReadError: a read of what the result is checked
// against failed, which says nothing of the result, so it's a failure to
// apply, tried again on the next pass.
type Checker interface {
	Check(ctx context.Context, r Result) error
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
	// Dirt is S's dirty content, and Issue is state.Derive of S, both read
	// before the first Pending.
	Dirt  Dirt
	Issue state.Issue
	// ContextDir is where the run's context files go, set with RunID, so
	// prompt.md can tell the agent where to read them.
	ContextDir string
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

	moveKey string            // see moveKey
	fields  map[string]string // the run's model, effort and ran (runFields)
}

// Move is a move keyed by the run, with seen=r.Seen, so the next round's
// input is what came after it (state.RoundBound), and the run's model=,
// effort= and ran= (runFields). If the table lacks it, its state error is
// keyed by moveKey, so the stage runs no more until the issue changes.
func (r Result) Move(to state.Stage, trigger state.Trigger) Move {
	m := Move{To: to, Trigger: trigger, Key: state.Key{Field: "run", ID: r.RunID}, ErrorKey: r.moveKey}
	if len(r.fields) > 0 {
		m.Fields = maps.Clone(r.fields)
	}
	if r.Seen > 0 {
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields[state.FieldSeen] = strconv.FormatInt(r.Seen, 10)
	}
	return m
}

// Lease is /state/lease.json, the run in progress (SPEC §8.2), or with
// Failed, the record of the last run that ended, failed or not counted:
// the count of failed attempts in a row it leaves, and any of its files a
// crash or a failed removal left (end). A
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
	// BodyEdit is the time of the body's last edit, as the run read it: zero
	// for never edited.
	BodyEdit time.Time `json:"body_edit,omitzero"`
	// AttemptKey names the attempts the run is one of (attemptKey), and
	// Attempt is its number among them, from 1.
	AttemptKey string `json:"attempt_key"`
	Attempt    int    `json:"attempt"`
	// Model, Effort and FallbackModels are the run's settings, resolved when
	// it started (state.RunSettings), so a restart applies its result with
	// the settings it ran with, whatever the labels say by then.
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	FallbackModels []string `json:"fallback_models,omitempty"`
	// ApplyFailures counts the failed applies of the run's result.
	ApplyFailures int `json:"apply_failures,omitempty"`
	// Failed marks a run that has ended, failed or not counted (end):
	// Attempt is then the count of failed attempts in a row it leaves, which
	// may be 0, so a restart doesn't reset the count. It's written before
	// the run's files are removed, so a crash or a failed removal may leave
	// them, and with no count the lease, for resume to remove.
	Failed bool `json:"failed,omitempty"`
}

// settings are the run settings l records.
func (l Lease) settings() state.Settings {
	return state.Settings{Model: l.Model, Effort: l.Effort, FallbackModels: l.FallbackModels}
}

// kindAttempts is the state error for a stage that failed loop.max_attempts
// times in a row.
const kindAttempts = "attempts"

// kindLabels is the state error for an issue whose model and effort labels
// give no run settings (state.ErrLabels).
const kindLabels = "labels"

// Runs is the stage-run harness. Its Work is the loop's stage work (SPEC
// §8.1, step 3), one run at a time, under a lease (SPEC §8.2).
type Runs struct {
	// Daemon gives the applier, trust and config of the moment.
	Daemon *Daemon
	// Stages are the registered Stages, by the stage they run at.
	Stages map[state.Stage]Stage
	// Unlabeled, if set, runs on open issues with no stage label and no
	// transition marker, such as triage (#6), in a pass where no Stage ran.
	// Its lease records the stage intake, which state.Derive gives such an
	// issue, so Stages has no Stage at intake while it's set.
	Unlabeled Stage
	Runner    *agent.Runner // its StateDir holds the lease and the results
	Workspace *workspace.Workspace
	Gate      *agent.Gate
	Env       []string         // the agent's whole environment (agent.Env)
	Now       func() time.Time // nil is time.Now
	NewID     func() string    // nil makes random run IDs

	attempts   map[string]int  // failed runs, by countKey
	swept      bool            // leftover temp leases are removed
	ended      string          // the ended run (a Failed lease) whose files are removed
	badSchemas map[string]bool // the agents whose Stage's schema doesn't compile, reported
}

// Work is Daemon.Work. It first finishes a lease a run left, after a
// restart or a failed apply (resume). Then, unless a usage limit closed
// the gate, it runs the first issue, by priority then number, whose stage
// has a Stage with work pending. If none ran, it runs Unlabeled, if set, on
// the newest issue, by number, with no stage label and no transition
// marker that it has work for. A lease whose issue has labels held back
// (ErrLabelsHeld) waits, and no run starts until it's applied. An issue
// with labels held back is never ready, so it starts no run either, and
// one with a stage label left out (Snapshot.LeftOut) is no Unlabeled work.
//
// Every open issue whose step 1 succeeded is ready, those with no stage
// label among them, so finding Unlabeled work costs no reads: Dirty keeps
// the histories it reads, and only the issue about to run is read again.
func (r *Runs) Work(ctx context.Context, ready []Ready) error {
	if !r.swept {
		r.sweep()
		r.swept = true
	}
	skip, err := r.resume(ctx)
	if errors.Is(err, ErrLabelsHeld) {
		// The lease waits to be applied, and no run may start before it is.
		return nil
	}
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
	if r.Unlabeled == nil {
		return nil
	}
	slices.SortStableFunc(ready, func(a, b Ready) int { return cmp.Compare(b.S.Number, a.S.Number) })
	for _, rd := range ready {
		if stageOf(rd.S) != "" || leftOutStage(rd.S) || rd.S.Number == skip {
			continue
		}
		if ran, err := r.runStage(ctx, rd, r.Unlabeled, true); err != nil || ran {
			return err
		}
	}
	return nil
}

// unlabeled reports whether s, whose state is is, is Unlabeled work: it has
// no stage label, none left out, and no transition marker.
func unlabeled(s *github.Snapshot, is state.Issue) bool {
	return stageOf(s) == "" && !leftOutStage(s) &&
		!slices.ContainsFunc(is.Markers, func(m github.Marker) bool { return m.Kind == "transition" })
}

// leftOutStage reports whether s has a stage label left out of its Labels
// (Snapshot.LeftOut).
func leftOutStage(s *github.Snapshot) bool {
	for l := range s.LeftOut {
		if _, ok := state.ParseLabel(l); ok {
			return true
		}
	}
	return false
}

// leaseStage returns the Stage l's run is of, and whether that's the
// Unlabeled stage, whose leases record intake.
func (r *Runs) leaseStage(l Lease) (st Stage, isUnlabeled, ok bool) {
	if l.Stage == state.Intake && r.Unlabeled != nil {
		return r.Unlabeled, true, true
	}
	st, ok = r.Stages[l.Stage]
	return st, false, ok
}

// CheckRegistered is the startup check of the registered Stages (#130's
// verify): it returns an error if Stages has a Stage at intake while
// Unlabeled is set. An intake lease always resolves to Unlabeled
// (leaseStage), so every result of that Stage would be dropped, silently.
func (r *Runs) CheckRegistered() error {
	if st, ok := r.Stages[state.Intake]; ok && r.Unlabeled != nil {
		return fmt.Errorf("a Stage (%T) is registered at %s while Unlabeled (%T) is set: "+
			"Unlabeled's leases record %s, so every result of the Stage at %s would be dropped",
			st, state.Intake, r.Unlabeled, state.Intake, state.Intake)
	}
	return nil
}

// RunStage runs st on the issue rd if it has work pending, and reports
// whether it ran. A run that fails, but not on a usage limit or in its
// setup, counts as an attempt (failed); after loop.max_attempts in a row
// the issue gets one state error (SPEC §5.3) and no more runs until the
// stage moves or gets new input. If posting that error failed, RunStage
// posts it instead of a run; if that fails too, it logs the error and
// moves on, so the next issue gets its turn. A Stage whose schema doesn't
// compile starts no run: it's a setup failure, reported once (schema). A
// result asking for a move the table lacks is no attempt: it gets one
// state error (Transition), and no run starts until the issue changes
// (moveKey). A run whose result fails to apply keeps its lease, for the
// next pass to apply it (applyLease), as does one whose issue has labels
// held back, uncounted. A failed run logs the end of
// claude's stderr.
//
// The run's model, effort and fallback models are resolved from the labels
// step 1 checked (state.RunSettings), for the run's agent and attempt, and
// kept in the lease, so a label changed during the run changes only the
// next one. Labels that give no settings (state.ErrLabels) start no run
// and use no attempt: the issue gets one state error for each change to
// them that stands (labelsKey), checked before the issue is read again,
// and posted only if the issue read again would have run (reportLabels).
func (r *Runs) RunStage(ctx context.Context, rd Ready, st Stage) (bool, error) {
	return r.runStage(ctx, rd, st, false)
}

// runStage is RunStage, or with isUnlabeled, the same for the Unlabeled
// stage st: it runs only on an issue that's unlabeled as step 1 read it,
// and as it's read again before the run, and its lease records intake.
func (r *Runs) runStage(ctx context.Context, rd Ready, st Stage, isUnlabeled bool) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("stage run on %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	d, err := a.Dirty(ctx, rd.S)
	if err != nil {
		return false, wrap(err)
	}
	is := state.Derive(rd.S, a.AppSlug, d.Has)
	if isUnlabeled && !unlabeled(rd.S, is) {
		return false, nil
	}
	in := Input{S: rd.S, Inputs: rd.Inputs, Dirt: d, Issue: is}
	if !st.Pending(in) {
		return false, nil
	}
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
		// A post that keeps failing mustn't hold up the other issues.
		if err := r.reportCap(ctx, nil, st, Lease{Issue: n, Stage: is.Stage, AttemptKey: errKey}); err != nil {
			logr.Errorf("%s/%s#%d: no %s run, as it failed loop.max_attempts times, but posting that failed again: %v",
				a.Owner, a.Repo, n, st.Agent(), err)
		}
		return false, nil
	}
	attempt := r.attempts[countKey(n, errKey)] + 1
	set, badLabels := state.RunSettings(state.Agent(st.Agent()), rd.S.Labels, r.Daemon.Config, attempt)
	switch {
	case errors.Is(badLabels, state.ErrLabels) && is.Reported(kindLabels, labelsKey(rd.S)):
		logr.Debugf("%s/%s#%d: no %s run: its model and effort labels give no settings, as reported: %v", a.Owner, a.Repo, n, st.Agent(), badLabels)
		return false, nil
	case badLabels != nil && !errors.Is(badLabels, state.ErrLabels):
		return false, wrap(badLabels)
	}
	schema, err := r.schema(st)
	if err != nil || schema == nil {
		return false, wrap(err)
	}
	fresh, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	if err == nil {
		a.hide(fresh) // nothing here runs CheckLabels on it
	}
	switch {
	case gone(err):
		logr.Infof("%s/%s#%d looks gone, deleted or transferred, so it gets no run and is read again: %v", a.Owner, a.Repo, n, err)
		r.Daemon.reread(n, true)
		return false, nil
	case err != nil:
		return false, wrap(err)
	case fresh.Closed || !isUnlabeled && stageOf(fresh) != is.Stage:
		r.Daemon.reread(n, false)
		return false, nil
	}
	if isUnlabeled {
		// A stage label or a transition since step 1 makes it pipeline work.
		fd, err := a.Dirty(ctx, fresh)
		if err != nil {
			return false, wrap(err)
		}
		if !unlabeled(fresh, state.Derive(fresh, a.AppSlug, fd.Has)) {
			logr.Debugf("%s/%s#%d: no %s run: it has a stage label or a transition since step 1", a.Owner, a.Repo, n, st.Agent())
			r.Daemon.reread(n, false)
			return false, nil
		}
	}
	if in.Inputs, err = r.trusted(ctx, rd.Inputs); err != nil {
		return false, wrap(err)
	}
	if !st.Pending(in) {
		return false, nil
	}
	if badLabels != nil {
		r.Daemon.reread(n, false)
		return false, wrap(r.reportLabels(ctx, rd.S, fresh, is.Stage, st, badLabels))
	}
	in.RunID, in.Attempt = r.newID(), attempt
	in.ContextDir = r.Workspace.Context(in.RunID)
	l := Lease{Issue: n, Stage: is.Stage, RunID: in.RunID, Clone: r.Workspace.Dir(in.RunID),
		Started: r.now(), Polled: r.Daemon.polled, Edits: map[int64]time.Time{}, BodyEdit: rd.S.BodyEdit.At, AttemptKey: errKey, Attempt: in.Attempt,
		Model: set.Model, Effort: set.Effort, FallbackModels: set.FallbackModels}
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
	logr.Infof("%s/%s#%d: starting %s run %s, attempt %d, with model %s and effort %s", a.Owner, a.Repo, n, st.Agent(), l.RunID, in.Attempt,
		cmp.Or(l.Model, "unset"), cmp.Or(l.Effort, "unset"))
	stderr := agent.NewTail(stderrTail)
	logStderr := func() {
		if stderr.Len() > 0 {
			logr.Warnf("%s/%s#%d: run %s failed; the last %d of %d bytes claude wrote to stderr:\n%s",
				a.Owner, a.Repo, n, l.RunID, stderr.Len(), stderr.Total(), stderr.Text())
		}
	}
	raw, runErr := r.run(ctx, st, in, l.settings(), stderr)
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
		// Validate fails whatever outcome calls failed, and invalid output.
		out, runErr = agent.Validate(raw, schema)
	}
	if runErr != nil {
		logStderr()
		// The issue may have changed during the run, so failed reads it again.
		return true, wrap(r.failed(ctx, st, l, runErr)) // logged there
	}
	if err := r.applyLease(ctx, l, st, raw, out); !errors.Is(err, ErrLabelsHeld) {
		return true, wrap(err)
	}
	return true, nil // the lease waits for the next pass to apply it
}

// stderrTail is how much of claude's stderr a failed run logs: the end.
const stderrTail = 4096

// schema compiles st's schema, before a run, as a run whose result can't
// be validated is no use. One that doesn't compile is a setup failure, so
// it starts no run and isn't counted. It's reported once: the first time,
// schema returns it as an *agent.SetupError, and after that, a nil schema
// and no error, so the stage is skipped, logged only at debug level.
func (r *Runs) schema(st Stage) (*agent.Schema, error) {
	schema, err := agent.CompileSchema(st.Schema())
	switch {
	case err == nil:
		return schema, nil
	case r.badSchemas[st.Agent()]:
		logr.Debugf("no %s run: its schema doesn't compile, as reported: %v", st.Agent(), err)
		return nil, nil
	}
	if r.badSchemas == nil {
		r.badSchemas = map[string]bool{}
	}
	r.badSchemas[st.Agent()] = true
	return nil, &agent.SetupError{Err: fmt.Errorf("no %s run starts until a restart: %w", st.Agent(), err)}
}

// run prepares the workspace and runs claude for st with the settings set,
// returning its output and sending its stderr to stderr. A failure to set
// the run up is an *agent.SetupError, as Runner.Run's are.
func (r *Runs) run(ctx context.Context, st Stage, in Input, set state.Settings, stderr io.Writer) ([]byte, error) {
	cfg := r.Daemon.Config.Stages
	turns, okT := pick(cfg.MaxTurns, st.Agent())
	budget, okB := pick(cfg.MaxBudgetUSD, st.Agent())
	timeout, okD := pick(cfg.Timeouts, st.Agent())
	if !okT || !okB || !okD {
		return nil, &agent.SetupError{Err: fmt.Errorf("no settings in config.stages for agent %q", st.Agent())}
	}
	files, err := st.Context(ctx, in)
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
		Schema: st.Schema(), MaxTurns: int(turns), MaxBudgetUSD: budget, Timeout: timeout, Env: r.Env,
		Model: set.Model, Effort: set.Effort, FallbackModels: set.FallbackModels})
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

// applyLease applies l's result (apply): raw, claude's output, whose
// structured output out is valid. A failure keeps the lease, counted in
// ApplyFailures, so the next pass tries again; after loop.max_attempts of
// them, the run is a failed attempt. A failure to end the run once the
// result is applied, dropped or blocked (an endError) isn't counted, and
// leaves the lease as end or finish left it, for the next pass. Nor is an
// issue whose labels CheckLabels holds back: the error wraps
// ErrLabelsHeld, and the lease and the result stay as they are, for a
// later pass to apply once the labels are explained, or left out after
// github.HoldBound. A result its Checker rejects (a checkError) is a failed
// attempt, not a failure to apply.
func (r *Runs) applyLease(ctx context.Context, l Lease, st Stage, raw []byte, out json.RawMessage) error {
	err := r.apply(ctx, l, st, raw, out)
	var ending *endError
	if err == nil || errors.As(err, &ending) {
		return err
	}
	var check *checkError
	if errors.As(err, &check) {
		return r.failed(ctx, st, l, check.err)
	}
	if errors.Is(err, ErrLabelsHeld) {
		logr.Infof("run %s on #%d waits to be applied, which isn't counted as a failure: %v", l.RunID, l.Issue, err)
		return err
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
// is dropped, writing nothing and with no attempt counted, when the issue
// is gone or closed, or, unless the run's move is written already:
//   - when its stage changed during the run: the owner's change wins, and
//     CheckLabels records one by hand. An Unlabeled run's result is also
//     dropped when the issue has a stage label, or a transition, at all.
//   - when one of its inputs was edited or deleted since the run read it,
//     or its body has a later edit by anyone but the App, so the stage
//     runs again on what's there. Edits to other comments, such as an
//     untrusted user's to their own, don't count, and nor does the App's
//     own edit, such as one a result applied part way made before a
//     restart.
//   - when it has new trusted input since the run's seen (newInput), which
//     the stage runs again with.
//
// Then a Checker checks the result: its error is a checkError, but for a
// checkReadError, which is a failure to apply like any other. A result
// whose move is written already is only finished, unchecked. A move the
// table lacks ends the run (blocked). On an error, the lease stays. The
// run marker's fields are l's settings and the models raw, claude's
// output, reports (runFields).
func (r *Runs) apply(ctx context.Context, l Lease, st Stage, raw []byte, out json.RawMessage) error {
	a := r.Daemon.Step.Applier
	drop := func(why string, args ...any) error {
		logr.Infof("%s/%s#%d: dropping the result of run %s, uncounted: "+why, append([]any{a.Owner, a.Repo, l.Issue, l.RunID}, args...)...)
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
	moved := is.Moved(state.Key{Field: "run", ID: l.RunID})
	if !moved {
		_, isUnlabeled, _ := r.leaseStage(l)
		switch {
		case isUnlabeled && !unlabeled(s, is):
			return drop("the issue has a stage label or a transition since the run started")
		case is.Stage != l.Stage:
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
		if e := s.BodyEdit; e.At.After(l.BodyEdit) && !state.IsApp(e.By, a.AppSlug) {
			return drop("%s edited the issue's body since the run read it, so the stage runs again", login(e.By))
		}
		id, err := r.newInput(ctx, s, d, l.Seen)
		if err != nil {
			return fmt.Errorf("apply run %s: %w", l.RunID, err)
		}
		if id != 0 {
			return drop("comment %d is new trusted input since the run started, so the stage runs again with it", id)
		}
	}
	key := moveKey(l.AttemptKey, is.Item)
	res := Result{RunID: l.RunID, Seen: l.Seen, S: s, Dirt: d, Output: out, moveKey: key, fields: runFields(l, raw)}
	if c, ok := st.(Checker); ok && !moved {
		var read *checkReadError
		switch err := c.Check(ctx, res); {
		case errors.As(err, &read):
			return fmt.Errorf("apply run %s: the %s stage's check: %w", l.RunID, st.Agent(), err)
		case err != nil:
			return &checkError{fmt.Errorf("its result failed the %s stage's check: %w", st.Agent(), err)}
		}
	}
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

// checkError is a Checker's rejection of a run's result, which applyLease
// counts as a failed attempt (SPEC §8.3), not a failure to apply.
type checkError struct{ err error }

func (e *checkError) Error() string { return e.err.Error() }
func (e *checkError) Unwrap() error { return e.err }

// checkReadError is a Checker's failure to read what it checks a result
// against, such as the timeline or trust, which applyLease counts as a
// failure to apply, kept for the next pass, not as a failed attempt: the
// result may be fine.
type checkReadError struct{ err error }

func (e *checkReadError) Error() string { return e.err.Error() }
func (e *checkReadError) Unwrap() error { return e.err }

// newInput returns the ID of the first comment on s that's new trusted
// input since seen, the newest comment a run was given, or 0 if there's
// none (plan #6, assumption 5): one above seen, by a trusted user, that
// isn't a command and isn't dirty. The App's comments, and other bots',
// aren't, as no bot is trusted. A result applied without it would lose
// it, such as an answer step 1 👍'd before a delayed apply.
func (r *Runs) newInput(ctx context.Context, s *github.Snapshot, d Dirt, seen int64) (int64, error) {
	ok := map[string]bool{}
	for _, c := range s.Comments {
		if _, cmd := state.ParseCommand(c.Body); c.ID <= seen || cmd || c.Author.Type == "Bot" || d.Has(c.ID) {
			continue
		}
		t, checked := ok[c.Author.Login]
		if !checked {
			var err error
			if t, err = r.Daemon.Step.Trust.Trusted(ctx, c.Author.Login); err != nil {
				return 0, fmt.Errorf("new input: trust %s: %w", c.Author.Login, err)
			}
			ok[c.Author.Login] = t
		}
		if t {
			return c.ID, nil
		}
	}
	return 0, nil
}

// resume finishes the lease a run left, if there is one, and returns the
// issue it applied a result to, which gets no stage work this pass. A
// valid saved result is applied (applyLease). A saved result that hit a
// usage limit closes the gate, and isn't counted, as in RunStage.
// Otherwise the run counts as a failed attempt, its clone is deleted, and
// the issue, at its pre-run stage (see Lease), runs again when its stage
// has work pending. For a held or closed issue, the clone is deleted; a
// valid saved result is dropped; without one, the run counts as a failed
// attempt unless it hit a usage limit or was applied; and the lease stays,
// marked ended, while it holds a count. A Failed lease only gives its
// count (endedLease). A saved result whose issue has labels held back
// waits, its lease kept (applyLease).
func (r *Runs) resume(ctx context.Context) (int, error) {
	l, ok, err := r.readLease(ctx)
	if err != nil || !ok {
		return 0, err
	}
	if l.Failed {
		return 0, r.endedLease(ctx, l)
	}
	st, _, ok := r.leaseStage(l)
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
		if failed, text := outcome(raw); out == nil && r.Gate.Observe(r.now(), failed, text) {
			logr.Warnf("the saved result of run %s on #%d hit a usage limit, so stage work waits until %s; it isn't counted as an attempt",
				l.RunID, l.Issue, r.Gate.OpenAt().Format(time.RFC3339))
			return 0, r.uncounted(ctx, l)
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
	return l.Issue, r.applyLease(ctx, l, st, raw, out)
}

// endedLease takes the count of l, the Failed lease of a run that ended
// (end), and removes what a crash or a failed removal in end left: the
// run's files, unless this process removed them, and the lease, if it
// holds no count. A lease with a count stays, for a restart.
func (r *Runs) endedLease(ctx context.Context, l Lease) error {
	r.count(l)
	if l.RunID != r.ended {
		if err := r.cleanup(ctx, l); err != nil {
			return fmt.Errorf("resume ended run %s: %w", l.RunID, err)
		}
		r.ended = l.RunID
	}
	if l.Attempt > 0 {
		return nil
	}
	if err := r.remove(r.leasePath()); err != nil {
		return fmt.Errorf("resume ended run %s: %w", l.RunID, err)
	}
	return nil
}

// failed records l's run as a failed attempt: it ends the run (end),
// keeping the lease, as Failed, for the count. The last attempt allowed
// posts one state error (reportCap), after which RunStage runs no more. If
// posting it fails, RunStage posts it instead of starting a run.
func (r *Runs) failed(ctx context.Context, st Stage, l Lease, cause error) error {
	n := r.count(l)
	logr.Warnf("%s run %s on #%d failed, attempt %d of %d: %v", st.Agent(), l.RunID, l.Issue, n, r.limit(), cause)
	if err := r.end(ctx, l, n); err != nil {
		return fmt.Errorf("record the failed attempt: %w", err)
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
// recorded, or is closed or gone gets none. For the Unlabeled stage, whose
// round of input never changes, the text names the ways into the pipeline
// instead of new input.
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
		a.hide(fresh)
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
	if _, isUnlabeled, _ := r.leaseStage(l); isUnlabeled {
		// No input counts at intake, so nothing runs it again: only a way
		// into the pipeline (plan for #6, assumption 1) moves the issue on.
		text = fmt.Sprintf("The %s run failed %d times in a row, so I've stopped running it here, and a comment won't start it again. "+
			"A code owner can bring this issue into the pipeline with a stage label, the `approved` label or `/track`.", st.Agent(), r.limit())
	}
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

// reportLabels posts the state error for the model and effort labels of
// the issue step 1 read as s, which give st's run no settings (cause, a
// state.ErrLabels error), keyed by labelsKey, which it doesn't have yet.
// fresh is the issue read again, open and at stage, with work
// pending: if it has another stage label, such as one the owner just set,
// or other model and effort labels than s, which may fix them, it gets
// none. cause quotes the labels, which the App didn't write, so the text
// is escaped (SPEC §5.1).
func (r *Runs) reportLabels(ctx context.Context, s, fresh *github.Snapshot, stage state.Stage, st Stage, cause error) error {
	a, n := r.Daemon.Step.Applier, s.Number
	if otherStage(fresh, stage) || !slices.Equal(overrideLabels(s.Labels), overrideLabels(fresh.Labels)) {
		logr.Debugf("%s/%s#%d: no %s run, and no state error yet: its stage or its model and effort labels changed since step 1", a.Owner, a.Repo, n, st.Agent())
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": kindLabels, "key": labelsKey(s)}})
	if err != nil {
		return fmt.Errorf("report the labels of #%d: %w", n, err)
	}
	// cause reads "<agent> run settings: override labels: <why>".
	why := cause.Error()
	if _, after, ok := strings.Cut(why, state.ErrLabels.Error()+": "); ok {
		why = after
	}
	text := fmt.Sprintf("I can't start the %s run, because of this issue's model and effort labels: %s. Nothing runs here until they're fixed: "+
		"at most one label of each kind, each naming a model in models.allowed or an effort level.", st.Agent(), why)
	if err := a.comment(ctx, fresh, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report the labels of #%d: %w", n, err)
	}
	logr.Errorf("state error: %s/%s#%d: no %s run: %v", a.Owner, a.Repo, n, st.Agent(), cause)
	return nil
}

// labelsKey keys the labels state error of s, as step 1 read it: the ID
// of the newest change to its model and effort labels that stands
// (Snapshot.SettingsEvent), as CheckLabels judged it. Each such change is a
// new occurrence (SPEC §5.3), so the same wrong labels set again after a
// fix are reported again, but an untrusted change and the App's revert of
// it don't count, so they post nothing. It costs no read: step 1's
// CheckLabels set it from the timeline when it last checked s, which may
// have been in an earlier pass, as step 1 runs only on changed issues.
func labelsKey(s *github.Snapshot) string { return strconv.FormatInt(s.SettingsEvent, 10) }

// overrideLabels returns the model and effort labels of all four kinds in
// labels, in lower case and sorted.
func overrideLabels(labels []string) []string {
	var set []string
	for _, l := range labels {
		if github.OverrideLabel(l) {
			set = append(set, strings.ToLower(l))
		}
	}
	slices.Sort(set)
	return set
}

// runFields are the run marker's fields for l's run: model= and effort=,
// as it started with them, and ran=, the models raw, claude's output,
// reports in modelUsage, comma-separated. One left unset, so claude used
// its default, or with no models reported, is left out. A value a marker
// can't hold, such as a modelUsage key with a space, a '>' or a comma, is
// left out and logged, so it can't fail the apply.
func runFields(l Lease, raw []byte) map[string]string {
	fields := map[string]string{}
	for _, f := range []struct{ name, value string }{{"model", l.Model}, {"effort", l.Effort}} {
		switch {
		case f.value == "":
		case !markerValue(f.value):
			logr.Warnf("run %s on #%d: its marker leaves out %s %q, which a marker can't hold", l.RunID, l.Issue, f.name, f.value)
		default:
			fields[f.name] = f.value
		}
	}
	models, err := agent.ModelsRan(raw)
	if err != nil {
		logr.Warnf("run %s on #%d: its marker has no ran=, as its output names no models: %v", l.RunID, l.Issue, err)
	}
	var ran []string
	for _, m := range models {
		if strings.Contains(m, ",") || !markerValue(m) {
			logr.Warnf("run %s on #%d: its marker's ran= leaves out the model %q claude reported, which a marker can't hold", l.RunID, l.Issue, m)
			continue
		}
		ran = append(ran, m)
	}
	if len(ran) > 0 {
		fields["ran"] = strings.Join(ran, ",")
	}
	return fields
}

// markerValue reports whether v is a value FormatMarker writes.
func markerValue(v string) bool {
	_, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: map[string]string{"model": v}})
	return err == nil
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

// uncounted ends l's run without counting it as an attempt (end): a setup
// failure, a usage limit, a shutdown, a dropped result, or a move the
// table lacks. A count of attempts before it stays, as a Failed lease.
func (r *Runs) uncounted(ctx context.Context, l Lease) error {
	return r.end(ctx, l, max(l.Attempt-1, 0))
}

// end ends l's run, leaving count failed attempts in a row. It writes the
// lease as Failed, holding count, before it removes anything, so a crash
// or a failed removal after that leaves a lease that resume only takes the
// count from (endedLease), never one it counts as another failed attempt.
// Then it removes the run's files (cleanup), and the lease if count is 0.
// Its error is an endError.
//
// A gap remains: when the ended lease can't be written, the lease stays
// the run's, so resume treats the run as one a restart cut short, and so
// it does after a crash in the short window before that write. A usage
// limit is then observed twice, once more on resume, if claude exited 0,
// as the runner saved its result; with a non-zero exit there's no saved
// result, so it's counted as a failed attempt, as are a setup failure and
// a shutdown, which leave none either.
func (r *Runs) end(ctx context.Context, l Lease, count int) error {
	l.Attempt, l.Failed, l.ApplyFailures = count, true, 0
	err := r.writeLease(l)
	if err == nil {
		err = r.cleanup(ctx, l)
	}
	if err == nil {
		r.ended = l.RunID
		if count == 0 {
			err = r.remove(r.leasePath())
		}
	}
	if err != nil {
		return &endError{fmt.Errorf("end run %s: %w", l.RunID, err)}
	}
	return nil
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
// last, so a crash before it only repeats this. Its error is an endError.
func (r *Runs) finish(ctx context.Context, l Lease) error {
	err := r.cleanup(ctx, l)
	if err == nil {
		err = r.remove(r.leasePath())
	}
	if err != nil {
		return &endError{fmt.Errorf("finish run %s: %w", l.RunID, err)}
	}
	return nil
}

// endError is a failure to end a run (end, finish) whose result, if any,
// is dealt with: applied, dropped or blocked. That's no failure to apply
// it, so applyLease doesn't count it.
type endError struct{ err error }

func (e *endError) Error() string { return e.err.Error() }
func (e *endError) Unwrap() error { return e.err }

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

// pick is a PerStage's value for agent name. The triage agent has the
// interviewer's (plan #6, assumption 2), so config needn't change.
func pick[T any](p config.PerStage[T], name string) (T, bool) {
	switch name {
	case "interviewer", "triage":
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

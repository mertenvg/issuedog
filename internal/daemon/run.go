package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
}

// Move is a move keyed by the run, with seen=r.Seen, so the next round's
// input is what came after it (state.RoundBound).
func (r Result) Move(to state.Stage, trigger state.Trigger) Move {
	m := Move{To: to, Trigger: trigger, Key: state.Key{Field: "run", ID: r.RunID}}
	if r.Seen > 0 {
		m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(r.Seen, 10)}
	}
	return m
}

// Lease is /state/lease.json, the run in progress (SPEC §8.2). A non-task
// run doesn't move its issue when it starts, so Stage is also the stage a
// restart resets it to, which writes nothing: an issue at another stage
// was moved by the owner, which wins.
type Lease struct {
	Issue   int         `json:"issue"`
	Stage   state.Stage `json:"stage"`
	RunID   string      `json:"run_id"`
	Clone   string      `json:"clone"`
	Started time.Time   `json:"started"`
	Polled  time.Time   `json:"polled"` // when the last poll before pickup began
	// Seen is the newest comment the run was given, and Edits the last
	// edit of each edited comment up to it, as the run read them.
	Seen  int64               `json:"seen"`
	Edits map[int64]time.Time `json:"edits,omitempty"`
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

	attempts map[string]int // failed runs, by issue and attemptKey
}

// Work is Daemon.Work. It first finishes a lease a run left, after a
// restart or a failed apply (resume). Then, unless a usage limit closed
// the gate, it runs the first issue, by priority then number, whose stage
// has a Stage with work pending.
func (r *Runs) Work(ctx context.Context, ready []Ready) error {
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
// whether it ran. A run that fails, but not on a usage limit, counts as an
// attempt; after loop.max_attempts in a row the issue gets one state error
// (SPEC §5.3) and no more runs until the stage moves or gets new input.
// The run's lease and files stay only if applying its result fails, for
// the next pass to try again.
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
	key := strconv.Itoa(n) + "/" + errKey
	if is.Reported(kindAttempts, errKey) {
		logr.Debugf("%s/%s#%d: no %s run: it failed loop.max_attempts times", a.Owner, a.Repo, n, st.Agent())
		return false, nil
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
	in.RunID, in.Attempt = r.newID(), r.attempts[key]+1
	l := Lease{Issue: n, Stage: is.Stage, RunID: in.RunID, Clone: r.Workspace.Dir(in.RunID),
		Started: r.now(), Polled: r.Daemon.polled, Edits: map[int64]time.Time{}}
	for _, c := range rd.S.Comments {
		l.Seen = max(l.Seen, c.ID)
		if !c.Edit.At.IsZero() {
			l.Edits[c.ID] = c.Edit.At
		}
	}
	defer r.Daemon.reread(n, false)
	if err := r.writeLease(l); err != nil {
		return true, wrap(err)
	}
	logr.Infof("%s/%s#%d: starting %s run %s, attempt %d", a.Owner, a.Repo, n, st.Agent(), l.RunID, in.Attempt)
	raw, runErr := r.run(ctx, st, in)
	var prep prepError
	if errors.As(runErr, &prep) || ctx.Err() != nil {
		// Not the agent's failure. A lease left by a shutdown is reset on restart.
		return true, wrap(errors.Join(runErr, r.finishUnless(ctx, l)))
	}
	if failed, text := outcome(raw); r.Gate.Observe(r.now(), failed, text) {
		logr.Warnf("%s/%s#%d: run %s hit a usage limit, so stage work waits until %s; it isn't counted as an attempt",
			a.Owner, a.Repo, n, l.RunID, r.Gate.OpenAt().Format(time.RFC3339))
		return true, wrap(r.finish(ctx, l))
	}
	out := json.RawMessage(nil)
	if runErr == nil {
		var schema *agent.Schema
		if schema, err = agent.CompileSchema(st.Schema()); err != nil {
			return true, wrap(errors.Join(err, r.finish(ctx, l)))
		}
		out, runErr = agent.Validate(raw, schema)
	}
	if runErr != nil {
		return true, wrap(errors.Join(r.failed(ctx, rd.S, key, errKey, st, l, runErr), r.finish(ctx, l)))
	}
	switch err := r.apply(ctx, l, st, out); {
	case err == nil:
		delete(r.attempts, key)
		return true, nil
	case StateError(err):
		// Transition reported it, and apply finished the run: a failed attempt.
		return true, wrap(r.failed(ctx, rd.S, key, errKey, st, l, err))
	default:
		return true, wrap(err) // the lease stays, so the next pass applies it
	}
}

// prepError is a failure to set a run up, which isn't the agent's.
type prepError struct{ error }

func (e prepError) Unwrap() error { return e.error }

// run prepares the workspace and runs claude for st, returning its output.
func (r *Runs) run(ctx context.Context, st Stage, in Input) ([]byte, error) {
	cfg := r.Daemon.Config.Stages
	turns, okT := pick(cfg.MaxTurns, st.Agent())
	budget, okB := pick(cfg.MaxBudgetUSD, st.Agent())
	timeout, okD := pick(cfg.Timeouts, st.Agent())
	if !okT || !okB || !okD {
		return nil, prepError{fmt.Errorf("no settings in config.stages for agent %q", st.Agent())}
	}
	files, err := st.Context(in)
	if err != nil {
		return nil, prepError{fmt.Errorf("context files: %w", err)}
	}
	if err := r.Workspace.Prepare(ctx, in.RunID, "", files); err != nil {
		return nil, prepError{err}
	}
	return r.Runner.Run(ctx, agent.Stage{RunID: in.RunID, Dir: r.Workspace.Dir(in.RunID), ContextDir: r.Workspace.Context(in.RunID),
		Schema: st.Schema(), MaxTurns: int(turns), MaxBudgetUSD: budget, Timeout: timeout, Env: r.Env})
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

// apply reads the issue again, runs CheckLabels on it, and applies out,
// l's valid result, through st, then removes the run's files. The result
// is dropped, writing nothing, when the issue is gone or closed, when its
// stage changed during the run (the owner's change wins, and CheckLabels
// records one by hand), or when a comment up to l.Seen was edited since
// the run read it, so the stage runs again on the edit. A result whose
// move is written already is only finished. On an error that isn't a
// state error, the lease stays for the next pass.
func (r *Runs) apply(ctx context.Context, l Lease, st Stage, out json.RawMessage) error {
	a := r.Daemon.Step.Applier
	drop := func(why string, args ...any) error {
		logr.Infof("%s/%s#%d: dropping the result of run %s: "+why, append([]any{a.Owner, a.Repo, l.Issue, l.RunID}, args...)...)
		return r.finish(ctx, l)
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
	if is := state.Derive(s, a.AppSlug, d.Has); !is.Moved(state.Key{Field: "run", ID: l.RunID}) {
		if is.Stage != l.Stage {
			return drop("the issue moved from %s to %s during the run", l.Stage, is.Stage)
		}
		for _, c := range s.Comments {
			if c.ID <= l.Seen && !c.Edit.At.Equal(l.Edits[c.ID]) {
				return drop("comment %d was edited since the run read it, so the stage runs again", c.ID)
			}
		}
	}
	if err := st.Apply(ctx, a, Result{RunID: l.RunID, Seen: l.Seen, S: s, Dirt: d, Output: out}); err != nil {
		if StateError(err) {
			return errors.Join(err, r.finish(ctx, l))
		}
		return fmt.Errorf("apply run %s: %w", l.RunID, err)
	}
	logr.Infof("%s/%s#%d: applied run %s", a.Owner, a.Repo, l.Issue, l.RunID)
	return r.finish(ctx, l)
}

// resume finishes the lease a run left, if there is one, and returns the
// issue it applied a result to, which gets no stage work this pass. A
// valid saved result is applied (apply). Otherwise the clone is deleted,
// and the issue, at its pre-run stage (see Lease), runs again when its
// stage has work pending. A held or closed issue's lease and clone are
// dropped by apply, or by that reset.
func (r *Runs) resume(ctx context.Context) (int, error) {
	l, ok, err := r.readLease()
	if err != nil || !ok {
		return 0, err
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
		logr.Infof("run %s on #%d left no valid result, so its clone is deleted and #%d stays at %s to run again",
			l.RunID, l.Issue, l.Issue, l.Stage)
		return 0, r.finish(ctx, l)
	}
	logr.Infof("applying the saved result of run %s on #%d", l.RunID, l.Issue)
	return l.Issue, r.apply(ctx, l, st, out)
}

// failed counts a failed attempt at key. The last one allowed posts one
// state error on s, keyed by errKey, after which RunStage runs no more. If
// posting it fails, the next failed run posts it.
func (r *Runs) failed(ctx context.Context, s *github.Snapshot, key, errKey string, st Stage, l Lease, cause error) error {
	if r.attempts == nil {
		r.attempts = map[string]int{}
	}
	r.attempts[key]++
	limit := int(r.Daemon.Config.Loop.MaxAttempts)
	a := r.Daemon.Step.Applier
	logr.Warnf("%s/%s#%d: %s run %s failed, attempt %d of %d: %v", a.Owner, a.Repo, s.Number, st.Agent(), l.RunID, r.attempts[key], limit, cause)
	if r.attempts[key] < limit {
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": kindAttempts, "key": errKey}})
	if err != nil {
		return fmt.Errorf("report the attempt cap: %w", err)
	}
	text := fmt.Sprintf("The %s run failed %d times in a row, so I've stopped running it here. "+
		"New input, or a move to another stage, lets it run again.", st.Agent(), limit)
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report the attempt cap: %w", err)
	}
	logr.Errorf("state error: %s run on %s/%s#%d failed %d times, the last: %v", st.Agent(), a.Owner, a.Repo, s.Number, limit, cause)
	return nil
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

// readLease reads the lease, if there is one. One that can't be read is
// logged and removed, as nothing can resume it.
func (r *Runs) readLease() (Lease, bool, error) {
	data, err := os.ReadFile(r.leasePath())
	if errors.Is(err, fs.ErrNotExist) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("read the lease: %w", err)
	}
	var l Lease
	err = json.Unmarshal(data, &l)
	if err == nil && (l.RunID == "" || filepath.Base(l.RunID) != l.RunID || strings.HasPrefix(l.RunID, ".")) {
		err = fmt.Errorf("bad run ID %q", l.RunID)
	}
	if err != nil {
		logr.Errorf("removing %s, which nothing can resume: %v", r.leasePath(), err)
		if rerr := os.Remove(r.leasePath()); rerr != nil {
			return Lease{}, false, fmt.Errorf("remove the lease: %w", rerr)
		}
		return Lease{}, false, nil
	}
	return l, true, nil
}

// writeLease writes the lease through a temp file and a rename, so a
// restart never reads half of one.
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
	if err != nil {
		return fmt.Errorf("write the lease: %w", err)
	}
	return nil
}

// finish removes the run's clone and context files, its result, then the
// lease. The lease goes last, so a crash before it only repeats this.
func (r *Runs) finish(ctx context.Context, l Lease) error {
	err := r.Workspace.Cleanup(ctx, l.RunID)
	if rerr := os.Remove(r.resultPath(l.RunID)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	if err == nil {
		if err = os.Remove(r.leasePath()); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("finish run %s: %w", l.RunID, err)
	}
	return nil
}

// finishUnless finishes l unless the daemon is stopping, which leaves the
// lease for the restart to reset.
func (r *Runs) finishUnless(ctx context.Context, l Lease) error {
	if ctx.Err() != nil {
		return nil
	}
	return r.finish(ctx, l)
}

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

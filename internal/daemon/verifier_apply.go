package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// verifyKind is the kind of the marker on a verifier's verdict (SPEC §5).
const verifyKind = "verify"

// The verifier's verdicts, as its schema names them (SPEC §10.2): nothing
// at the blocking bar, a defect at it, and an ambiguous criterion.
const (
	verdictPass           = "pass"
	verdictRequestChanges = "request_changes"
	verdictQuestion       = "question"
)

// verifierOutput is the verifier's output (schemas/verifier.json), but for
// NoVerdict, which no run's output can set: the schema has no such field.
// It's the daemon's cause when a run gave no readable verdict (noVerdict),
// so it's request_changes with no findings.
type verifierOutput struct {
	Verdict   string              `json:"verdict"`
	Notes     string              `json:"notes"`
	Findings  []verifierFinding   `json:"findings"`
	FollowUps []verifierFollowUp  `json:"follow_ups"`
	Questions []interviewQuestion `json:"questions"`
	NoVerdict string              `json:"no_verdict"`
}

// verifierFinding is one defect at the blocking bar (SPEC §10.2).
type verifierFinding struct {
	Criterion string `json:"criterion"`
	Class     string `json:"class"`
	Detail    string `json:"detail"`
}

// verifierFollowUp is one candidate rarer than the blocking bar (SPEC §10.2).
type verifierFollowUp struct {
	Title      string `json:"title"`
	Scope      string `json:"scope"`
	Scenario   string `json:"scenario"`
	Likelihood string `json:"likelihood"`
	Impact     string `json:"impact"`
}

// decodeVerifier decodes a run's output. A field the types don't have is
// an error (decodeCause).
func decodeVerifier(raw json.RawMessage) (verifierOutput, error) {
	var out verifierOutput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return verifierOutput{}, decodeCause("the verifier's", err)
	}
	return out, nil
}

// noVerdictFixed is why a run gave no readable verdict, when nothing of the
// cause is the daemon's to quote (publicCause).
const noVerdictFixed = "its output can't be read"

// noVerdict is the output that stands in for a run with no readable verdict
// (SPEC §10.2): request_changes with no findings, and in NoVerdict what a
// public comment may quote of cause, on one line, or else noVerdictFixed. So
// it holds the daemon's words alone, never the run's output.
func noVerdict(cause error) verifierOutput {
	why := cmp.Or(publicCause(cause), noVerdictFixed)
	return verifierOutput{Verdict: verdictRequestChanges, NoVerdict: strings.ReplaceAll(why, "\n", "; ")}
}

// NoOutput makes the Verifier FailClosed: a run that finished with no
// readable output counts as request_changes (SPEC §10.2), by noVerdict. Its
// fields are strings, so marshalling them can't fail. If it did, the error
// is logged, and noVerdictFixed stands for the cause.
func (*Verifier) NoOutput(cause error) json.RawMessage {
	logr.Warnf("a verifier run gave no readable verdict, which counts as %s: %v", verdictRequestChanges, cause)
	raw, err := json.Marshal(map[string]string{"verdict": verdictRequestChanges, "notes": "", "no_verdict": noVerdict(cause).NoVerdict})
	if err != nil {
		logr.Errorf("the verifier's stand-in output doesn't marshal: %v", err)
		return json.RawMessage(`{"verdict":"` + verdictRequestChanges + `","notes":"","no_verdict":"` + noVerdictFixed + `"}`)
	}
	return raw
}

// verifierFacts is what checkVerifier checks an output against, and what
// its comment says.
type verifierFacts struct {
	// Head is the head the run judged (Result.Start), Attempt the attempt=
	// of the move into verifying, 0 for none (verifyAttempt), and Limit
	// loop.max_attempts.
	Head           string
	Attempt, Limit int
	// Issue is the task's number and Notify the config's notify, for the
	// hand-off's comment and its cc line.
	Issue  int
	Notify []string
}

// facts are what checkVerifier holds r's output to, on an issue in state is.
func (v *Verifier) facts(r Result, is state.Issue) verifierFacts {
	c := v.Daemon.Config
	return verifierFacts{Head: r.Start, Attempt: verifyAttempt(is), Limit: int(c.Loop.MaxAttempts), Issue: r.S.Number, Notify: c.Notify}
}

// atCap reports whether the head judged is the round's last attempt's, or
// one past a loop.max_attempts lowered since (SPEC §8.3). A move into
// verifying that names no attempt never is, as verifyFindings has it.
func (f verifierFacts) atCap() bool { return f.Attempt >= f.Limit }

// changesComment is the comment request_changes posts: the findings
// (renderVerdict), or on the round's last attempt, the verify cap's hand-off
// with them (renderVerifyCapHandoff).
func changesComment(out verifierOutput, f verifierFacts) string {
	if f.atCap() {
		return renderVerifyCapHandoff(f.Attempt, verdictText(f.Head, f.Attempt, f.Limit, out), f.Issue, f.Notify)
	}
	return renderVerdict(f.Head, f.Attempt, f.Limit, out)
}

// checkVerifier checks out against f after the schema. Its error joins a
// *ruleError for every rule broken, and a verdict that fails counts as no
// readable verdict (SPEC §10.2), so each names only the daemon's words,
// never the run's text: it reaches a public comment. Each verdict is paired
// with its fields here, as the schema has no conditional at its top level.
// A field the verdict leaves empty is as good as left out, so a pass with
// "findings": [] is a pass (SPEC §12):
//   - pass has no findings or questions, and its comment (renderPass) is
//     within maxText less textRoom.
//   - request_changes has findings and no questions, each finding's
//     criterion is on one line, and the comment it posts (changesComment),
//     the findings or at the cap the hand-off, is within maxText less
//     textRoom.
//   - question has questions and no findings.
//
// Follow-up candidates aren't looked at: a failing verdict drops its own.
// A stand-in for a run with no readable verdict has nothing to check.
func checkVerifier(out verifierOutput, f verifierFacts) error {
	if out.NoVerdict != "" {
		return nil
	}
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &ruleError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
	}
	switch out.Verdict {
	case verdictPass:
		if len(out.Findings) > 0 {
			fail(ruleOutcome, "pass has no findings")
		}
		if len(out.Questions) > 0 {
			fail(ruleOutcome, "pass has no questions")
		}
		if n := textLen(github.Escape(renderPass(f.Head, out.Notes))); n > maxText-textRoom {
			fail(ruleLength, "the comment is %d characters, over %d", n, maxText-textRoom)
		}
	case verdictRequestChanges:
		if len(out.Findings) == 0 {
			fail(ruleOutcome, "request_changes has findings")
		}
		if len(out.Questions) > 0 {
			fail(ruleOutcome, "request_changes has no questions")
		}
		for i, finding := range out.Findings {
			if !oneLine(finding.Criterion) {
				fail(ruleOneLine, "the criterion of finding %d", i+1)
			}
		}
		if n := textLen(changesComment(out, f)); n > maxText-textRoom {
			fail(ruleLength, "the comment is %d characters, over %d", n, maxText-textRoom)
		}
	case verdictQuestion:
		if len(out.Questions) == 0 {
			fail(ruleOutcome, "question has questions")
		}
		if len(out.Findings) > 0 {
			fail(ruleOutcome, "question has no findings")
		}
	default:
		fail(ruleOutcome, "the verdict isn't one the schema names")
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("check verifier output: %w", errors.Join(errs...))
}

// verdictOn reports whether is has a clean pass of the verifier's on head:
// a verify marker of the App's, in clean content, with verdict=pass and
// that head=. No head has none. It reads only is, so it costs no request.
func verdictOn(is state.Issue, head string) bool {
	for _, m := range is.Markers {
		if m.Kind == verifyKind && m.Fields["verdict"] == verdictPass && head != "" && m.Fields["head"] == head {
			return true
		}
	}
	return false
}

// verdictRun reports whether is has a verify marker of run runID's: its
// verdict is written (SPEC §8.2).
func verdictRun(is state.Issue, runID string) bool {
	for _, m := range is.Markers {
		if m.Kind == verifyKind && runID != "" && m.Fields["run"] == runID {
			return true
		}
	}
	return false
}

// Apply writes a verdict (SPEC §10.2). A verdict already written is left as
// it is, as its marker is the record (verdictRun), and a move already written
// only has its labels finished (runMoved), so a replay or a restart, at any
// write, posts nothing more. Output that doesn't decode, or fails
// checkVerifier, counts as no readable verdict (noVerdict), as a run's that
// never had any does (NoOutput).
//
// A pass is one comment naming the head the run judged, r.Start, and its
// notes, escaped, carrying the verify marker: verdict=, run=, head=, the
// attempt= of the move into verifying, if it names one, and the run's
// model=, effort= and ran=. It moves nothing, whatever the checks say next:
// the task waits at verifying, holding the worker, until the PR is built
// (#622), and they run again when main moves or the daemon restarts.
//
// A request_changes is the implementer attempt that made the head failing
// verify, so it's one of the verify loop's moves (verifyLoopMove), and the
// round goes on (SPEC §8.3). See applyChanges. Its candidates are dropped.
//
// A question isn't applied yet (#726). It's a state error (StateError), so
// the run ends uncounted, with one kind=transition error on the issue, keyed
// by the run's moveKey, and the task stays where it is, with no comment that
// reads as a verdict. No run starts on the task until its round changes
// (runStage), after a restart too, as the error is the record.
func (v *Verifier) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply verifier run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	if verdictRun(is, r.RunID) {
		logr.Debugf("%s/%s#%d: run %s's verdict is written already", a.Owner, a.Repo, r.S.Number, r.RunID)
		return nil
	}
	if m, moved := runMoved(is, r, verifierTriggers); moved {
		logr.Debugf("%s/%s#%d: run %s's move is written already, so only its labels are left", a.Owner, a.Repo, r.S.Number, r.RunID)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return wrap(err)
		}
		return nil
	}
	f := v.facts(r, is)
	out, err := decodeVerifier(r.Output)
	if err == nil {
		err = checkVerifier(out, f)
	}
	if err != nil {
		logr.Warnf("%s/%s#%d: run %s gave no readable verdict, which counts as %s: %v", a.Owner, a.Repo, r.S.Number, r.RunID, verdictRequestChanges, err)
		out = noVerdict(err)
	}
	if out.Verdict == verdictRequestChanges {
		if err := applyChanges(ctx, a, r, out, f); err != nil {
			return wrap(err)
		}
		return nil
	}
	if out.Verdict != verdictPass {
		return fmt.Errorf("%w: its `%s` verdict can't be applied yet, so the task stays at %s", state.ErrNoTransition, out.Verdict, state.Verifying)
	}
	fields := map[string]string{"verdict": verdictPass, "run": r.RunID, "head": r.Start}
	maps.Copy(fields, r.fields)
	if f.Attempt > 0 {
		fields[fieldAttempt] = strconv.Itoa(f.Attempt)
	}
	marker, err := github.FormatMarker(github.Marker{Kind: verifyKind, Fields: fields})
	if err != nil {
		return wrap(err)
	}
	if err := a.comment(ctx, r.S, join(github.Escape(renderPass(r.Start, out.Notes)), marker)); err != nil {
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: run %s passes its head %s, which waits at %s", a.Owner, a.Repo, r.S.Number, r.RunID, r.Start, state.Verifying)
	return nil
}

// applyChanges writes a request_changes, as r.Move's move, keyed by the run,
// with seen=, the lease's, and the run's model=, effort= and ran=. The move
// is the checks' own (verifyFindings) in all but its key and its comment:
//   - below the round's last attempt, verifying → in-progress on findings,
//     with head=, the head the run judged, and attempt=, that of the move
//     into verifying, if it names one, so the round's count lives on the
//     issue (SPEC §8.3). Its comment is renderVerdict's.
//   - on that attempt or past it, verifying → needs-human on attempt-cap,
//     and its comment the verify cap's hand-off, with the findings in it,
//     which ends in the cc line for notify. That releases the worker
//     (SPEC §8.1), and /done or a trusted 👍 on the hand-off sends the task
//     back to ready, a new round (handback.go).
func applyChanges(ctx context.Context, a *Applier, r Result, out verifierOutput, f verifierFacts) error {
	if f.atCap() {
		m := r.Move(state.NeedsHuman, state.TriggerAttemptCap)
		m.Text = changesComment(out, f)
		if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
			return fmt.Errorf("hand off the verify cap: %w", err)
		}
		logr.Warnf("%s/%s#%d: run %s fails its head %s on attempt %d of %d, so it's handed off to the owner at %s",
			a.Owner, a.Repo, r.S.Number, r.RunID, f.Head, f.Attempt, f.Limit, state.NeedsHuman)
		return nil
	}
	m := r.Move(state.InProgress, state.TriggerFindings)
	if m.Fields == nil {
		m.Fields = map[string]string{}
	}
	m.Fields["head"] = f.Head
	if f.Attempt > 0 {
		m.Fields[fieldAttempt] = strconv.Itoa(f.Attempt)
	}
	m.Text = changesComment(out, f)
	if err := a.Transition(ctx, r.S, r.Dirt.Has, m); err != nil {
		return fmt.Errorf("move the task back on the verifier's findings: %w", err)
	}
	logr.Infof("%s/%s#%d: run %s fails its head %s, in %d findings, so it's back at %s", a.Owner, a.Repo, r.S.Number, r.RunID, f.Head, len(out.Findings), state.InProgress)
	return nil
}

// renderPass renders the comment of a pass: the head the run judged, which
// is git's, so it's the App's text, then its notes, the agent's text (block).
func renderPass(head, notes string) string {
	return "The verifier passed `" + head + "`.\n\n" + block(notes)
}

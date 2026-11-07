package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// verifyKind is the kind of the marker on a verifier's verdict (SPEC §5).
const verifyKind = "verify"

// verdictPass is the verdict of a run that found nothing at the blocking
// bar (SPEC §10.2).
const verdictPass = "pass"

// verifierOutput is the verifier's output (schemas/verifier.json), but for
// NoVerdict, which no run's output can set: the schema has no such field.
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

// Apply writes a valid verdict (SPEC §10.2). A verdict already written is
// left as it is, as the marker is its record (verdictRun), so a replay or a
// restart posts nothing more.
//
// A pass is one comment naming the head the run judged, r.Start, and its
// notes, escaped, carrying the verify marker: verdict=, run=, head=, the
// attempt= of the move into verifying, if it names one, and the run's
// model=, effort= and ran=. It moves nothing, whatever the checks say next:
// the task waits at verifying, holding the worker, until the PR is built
// (#622), and they run again when main moves or the daemon restarts.
//
// Any other verdict isn't applied yet (#724, #726). It's a state error
// (StateError), so the run ends uncounted, with one kind=transition error
// on the issue, keyed by the run's moveKey, and the task stays where it is,
// with no comment that reads as a verdict. No run starts on the task until
// its round changes (runStage), after a restart too, as the error is the
// record.
func (v *Verifier) Apply(ctx context.Context, a *Applier, r Result) error {
	wrap := func(err error) error {
		return fmt.Errorf("apply verifier run %s on %s/%s#%d: %w", r.RunID, a.Owner, a.Repo, r.S.Number, err)
	}
	is := state.Derive(r.S, a.AppSlug, r.Dirt.Has)
	if verdictRun(is, r.RunID) {
		logr.Debugf("%s/%s#%d: run %s's verdict is written already", a.Owner, a.Repo, r.S.Number, r.RunID)
		return nil
	}
	out, err := decodeVerifier(r.Output)
	if err != nil {
		return wrap(err)
	}
	if out.Verdict != verdictPass {
		return fmt.Errorf("%w: its `%s` verdict can't be applied yet, so the task stays at %s", state.ErrNoTransition, out.Verdict, state.Verifying)
	}
	fields := map[string]string{"verdict": verdictPass, "run": r.RunID, "head": r.Start}
	maps.Copy(fields, r.fields)
	if attempt := verifyAttempt(is); attempt > 0 {
		fields[fieldAttempt] = strconv.Itoa(attempt)
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

// renderPass renders the comment of a pass: the head the run judged, which
// is git's, so it's the App's text, then its notes, the agent's text (block).
func renderPass(head, notes string) string {
	return "The verifier passed `" + head + "`.\n\n" + block(notes)
}

package daemon

import (
	"errors"

	"github.com/mertenvg/issuedog/internal/agent"
)

// What a state error's public comment quotes of a failure's cause (SPEC
// §5.3, #320). It quotes only what the daemon wrote itself, chosen by type:
// a check's broken rules, each a *ruleError with its rule and detail; a
// run's own failure, such as claude's subtype or the stage's timeout, an
// *agent.PublicCauseError; and the daemon's other failures, each a
// *daemonCause. Their text may quote the agent's output, so it's fenced
// and escaped. Any other cause, such as the runner's error, which may
// name a path under /work or /state, a failed apply's, or GitHub's error
// text, isn't quoted: the comment says the daemon's log has it, as the
// full cause is logged once where the failure happens, by failed for an
// attempt, by closerErrReport, as it posts, for a closer question, and by
// the title rule (digestTake) for an edit history read.

// publicCauseLogged is Lease.PublicFailure for a failed attempt whose cause
// has nothing publicCause quotes: the attempt cap's error then says only
// that the daemon's log has it.
const publicCauseLogged = "(in the daemon's log)"

// daemonCause is a failed attempt's cause in the daemon's own words, past
// a check's broken rules: a run that left no valid result after a restart,
// an issue its stage no longer runs on, a move that doesn't fit, or output
// that doesn't decode. Its text names only the issue's number and type,
// the agent's output, the transition table's words and, for a decode, the
// decoder's, such as encoding/json's, which name a field, a Go type or an
// offset, never a path, a run ID or GitHub's text, so a public comment may
// quote it (SPEC §5.3). Only an error of that text may be wrapped in one.
type daemonCause struct{ err error }

func (e *daemonCause) Error() string { return e.err.Error() }
func (e *daemonCause) Unwrap() error { return e.err }

// publicCause returns the part of cause a public comment may quote: every
// *ruleError, *agent.PublicCauseError and *daemonCause in it, in order,
// one per line, cut as lastFailure cuts a cause, or "" if it has none.
func publicCause(cause error) string {
	parts := publicCauseParts(cause)
	if len(parts) == 0 {
		return ""
	}
	return lastFailure(errors.Join(parts...))
}

// publicCauseParts walks err's tree as errors.As does, but returns every
// error in it of a type publicCause quotes, not only the first, as a check
// joins one *ruleError per rule broken.
func publicCauseParts(err error) []error {
	switch e := err.(type) {
	case *ruleError, *agent.PublicCauseError, *daemonCause:
		return []error{e}
	case interface{ Unwrap() []error }:
		var parts []error
		for _, x := range e.Unwrap() {
			parts = append(parts, publicCauseParts(x)...)
		}
		return parts
	case interface{ Unwrap() error }:
		return publicCauseParts(e.Unwrap())
	}
	return nil
}

// publicCauseText is the paragraph a state error ends with, on why the
// last of its tries, what, such as "attempt", failed: quote, publicCause's
// text, fenced and escaped (ivFenced), as it may quote the agent's output,
// or, for "", that the daemon's log says why.
func publicCauseText(what, quote string) string {
	if quote == "" {
		return "\n\nThe last " + what + " failed; see the daemon's log for why."
	}
	return "\n\nThe last " + what + " failed:\n\n" + ivFenced(quote)
}

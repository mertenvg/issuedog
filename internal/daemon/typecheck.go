package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A type that doesn't fit the issue's stage is a state error, kind=type
// (SPEC §4, §5.3; plan #7, assumption 9). The owner may change the type at
// any time; the daemon then leaves the issue where it is and says so, once
// per type change. Step 1 checks the type (typeCheck), and Transition
// reports it when the type is why the table lacks a move. openErrors keeps
// it open until the type fits the stage, or a newer type change stands.

// typeErrKind is the kind of the state error for a type that doesn't fit
// the issue's stage.
const typeErrKind = "type"

// typeErrKey keys the type error of s: the ID of the type change that
// stands (Snapshot.TypeEvent), as CheckLabels judged it. So each type
// change is a new occurrence, and the same type set again after a fix is
// reported again. With none, it's 0, and there's no type error
// (typeErrStands).
func typeErrKey(s *github.Snapshot) string { return strconv.FormatInt(s.TypeEvent, 10) }

// typeErrCause reports whether an issue of type typ can't be at stage, so
// its type is the cause of a type error there: an unknown type, which the
// pipeline never takes past intake, anywhere; any other type, the untyped
// included, where no issue of it fits (state.Fits), whatever its parent,
// origin marker or past.
//
// So only the type counts, not the rest of what Fits reads: an owner's
// story at spec-review that gains a parent no longer fits, but no type
// change is behind that, and the table's "any open" rows move it on
// (plan #7, #153's note). A move the table lacks there is a transition
// error, as before.
func typeErrCause(stage state.Stage, typ string) bool {
	if _, err := state.ClassOf(state.Item{Type: typ}); err != nil {
		return true // ClassOf's only error is an unknown type
	}
	// Every combination of the facts other than the type.
	for i := range 16 {
		it := state.Item{Type: typ, HasParent: i&1 != 0, FromPlan: i&2 != 0, Human: i&4 != 0, ReachedReady: i&8 != 0}
		if state.Fits(stage, it) {
			return false
		}
	}
	return true
}

// typeErrStands reports whether s, in state is, has a type error's cause:
// a type change stands (Snapshot.TypeEvent), which keys the error, and its
// type can't be at its stage (typeErrCause). With no type change, as for
// an issue filed with no type and never typed, no type change is behind
// where it is, and nothing keys the error: a move the table lacks is a
// transition error, keyed by what triggered it, as before.
func typeErrStands(s *github.Snapshot, is state.Issue) bool {
	return s.TypeEvent != 0 && typeErrCause(is.Stage, is.Item.Type)
}

// typeCheck is step 1's check of the type (plan #7, assumption 9): on an
// open issue with a transition marker, whose type, set by a type change
// that stands, can't be at its stage (typeErrStands), it posts the type
// error, once per key (typeErrReport).
// The issue stays where it is. An issue with no transition marker is at
// intake, which every type the pipeline knows fits, and an unknown type
// there is reported by the move out of it (Transition).
func (st *Step) typeCheck(ctx context.Context, s *github.Snapshot, d Dirt) error {
	if s.Closed {
		return nil
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	if !typeErrMoved(is) || !typeErrStands(s, is) {
		return nil
	}
	if err := st.typeErrReport(ctx, s, is); err != nil {
		return fmt.Errorf("check the type: %w", err)
	}
	return nil
}

// typeErrMoved reports whether is has a transition marker that sets its
// stage (state.RoundBound), in a comment or the body.
func typeErrMoved(is state.Issue) bool {
	for _, m := range is.Markers {
		if _, ok := state.RoundBound(m, state.BodyID); ok {
			return true
		}
	}
	return false
}

// typeErrReport posts s's type error, keyed by typeErrKey, unless one was
// posted or acknowledged (state.Issue.Reported). It is logged as an error
// when posted, and at debug level after. is is s's state.
func (a *Applier) typeErrReport(ctx context.Context, s *github.Snapshot, is state.Issue) error {
	key := typeErrKey(s)
	desc := fmt.Sprintf("%s/%s#%d: type %q doesn't fit stage %s, keyed %s", a.Owner, a.Repo, s.Number, is.Item.Type, is.Stage, key)
	if is.Reported(typeErrKind, key) {
		logr.Debugf("state error, reported already: %s", desc)
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": typeErrKind, "key": key}})
	if err != nil {
		return fmt.Errorf("report the type: %w", err)
	}
	if err := a.comment(ctx, s, join(github.Escape(typeErrText(is)), marker)); err != nil {
		return fmt.Errorf("report the type: %w", err)
	}
	logr.Errorf("state error: %s", desc)
	return nil
}

// typeErrText says why is's type doesn't fit its stage. The type isn't the
// App's text, so it's defused here, and the caller escapes it (SPEC §5.1).
func typeErrText(is state.Issue) string {
	what := "This issue has no type, which"
	if is.Item.Type != "" {
		what = fmt.Sprintf("This issue's type, `%s`,", defuse(is.Item.Type))
	}
	why := fmt.Sprintf("doesn't fit its stage, `%s`", is.Stage)
	if _, err := state.ClassOf(is.Item); errors.Is(err, state.ErrUnknownType) {
		why = "isn't one the pipeline knows"
	}
	return fmt.Sprintf("%s %s (SPEC §4). It stays at `%s` until its type fits it, or someone moves it by hand. "+
		"A 👍 on this comment acknowledges it.", what, why, is.Stage)
}

package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/state"
)

// doneChecked is the check DoneCommand's Check last found passing, for the
// /done in comment id: what it found, for Apply's text, so Apply, which
// Step calls right after, reads nothing again.
type doneChecked struct {
	id    int64
	found string
}

// DoneCommand is the /done handler (SPEC §9.2, §9.3, plan #250, T5).
// state.Command.Check lets /done apply only at needs-human, and Waits makes
// it wait while the issue is held.
//
//   - Check, which only reads, rejects /done on an issue that isn't a human
//     task, as a task blocked on the owner, whose hand-off is phase 3's. On
//     a human task it runs the task's check (humanCheck) on its origin
//     marker's Issue.Origin.Check: the zero Check of a plain-text "Done
//     when" passes with nothing read. A failed check is a Rejection whose
//     reason quotes what it found, so Step 👎s the comment and replies once,
//     and the issue stays open; a later /done checks again. A read that
//     fails is an error, not a failed check: the pass fails with no reply,
//     and the next pass checks again.
//   - Apply closes the issue as completed (Transition, needs-human → closed
//     on human-task-done), keyed cmd=<comment id>, with seen=<comment id>:
//     the marker, the 👍, then the close. Its text says who finished the
//     task and what the check found, escaped. The close judge sets
//     stage:completed on the next pass.
func DoneCommand() Handler {
	return Handler{
		Check: func(ctx context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is closed, so Step
			// finishes the command before it asks Check.
			is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
			if class, err := state.ClassOf(is.Item); err != nil || class != state.ClassHumanTask {
				return Rejection{Reason: "`/done` on a blocked task isn't handled yet (build phase 3)", Final: true}
			}
			pass, found, err := a.humanCheck(ctx, is.Origin.Check)
			if err != nil {
				return fmt.Errorf("done: %w", err)
			}
			if !pass {
				return Rejection{Reason: `its "Done when" check didn't pass. ` + strings.TrimSuffix(found, ".")}
			}
			a.doneChecked = doneChecked{id: c.Comment.ID, found: found}
			return nil
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			found, err := a.doneFound(ctx, c)
			if err != nil {
				return fmt.Errorf("done: %w", err)
			}
			m := Move{To: state.Closed, StateReason: "completed", Trigger: state.TriggerHumanTaskDone, Key: state.CmdKey(c.Comment.ID),
				Fields: map[string]string{state.FieldSeen: strconv.FormatInt(c.Comment.ID, 10)},
				Text:   fmt.Sprintf("%s said this task is done. %s So I've closed it as completed.", c.Comment.Author.Login, found)}
			if err := a.Transition(ctx, c.S, c.Dirt.Has, m); err != nil {
				return fmt.Errorf("done: %w", err)
			}
			return nil
		},
	}
}

// doneFound is what the check DoneCommand's Check passed for c found. Step
// calls Apply right after Check, so it's kept (doneChecked); were it not, the
// check runs again, and one that no longer passes is an error, so the next
// pass's Check rejects the command.
func (a *Applier) doneFound(ctx context.Context, c Cmd) (string, error) {
	if a.doneChecked.id == c.Comment.ID {
		return a.doneChecked.found, nil
	}
	pass, found, err := a.humanCheck(ctx, state.Derive(c.S, a.AppSlug, c.Dirt.Has).Origin.Check)
	if err != nil {
		return "", err
	}
	if !pass {
		return "", fmt.Errorf("the check no longer passes: %s", found)
	}
	return found, nil
}

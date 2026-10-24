package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// doneChecked is the check DoneCommand's Check last found passing, for the
// /done in comment id: what it found, for Apply's text, so Apply, which
// Step calls right after, reads nothing again.
type doneChecked struct {
	id    int64
	found string
}

// DoneCommand is the /done handler (SPEC §9.2, §9.3, #285).
// state.Command.Check lets /done apply only at needs-human, and Waits makes
// it wait while the issue is held.
//
//   - Check, which only reads, rejects /done on an issue that isn't a human
//     task, as a task blocked on the owner, whose hand-off is phase 3's. On
//     a human task it runs the task's check (humanCheck) on its origin
//     marker's Issue.Origin.Check: the zero Check of a plain-text "Done
//     when" passes with nothing read, and check fields that couldn't be
//     read (state.CheckUnreadable) fail with nothing read. A failed check
//     is a Rejection whose reason quotes what it found, so Step 👎s the
//     comment and replies once, and the issue stays open; a later /done
//     checks again. A read that fails is an error, not a failed check: the
//     pass fails with no reply, and the next pass checks again.
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
		return "", fmt.Errorf("re-run the check: %w", err)
	}
	if !pass {
		return "", fmt.Errorf("the check no longer passes: %s", found)
	}
	return found, nil
}

// DoneByReaction returns the reaction trigger for Step.Reactions that does
// what /done does on a trusted 👍 on a human task itself (SPEC §9.2, §9.3,
// #286). It acts only on an open human task at needs-human, so a
// 👍 given while the task is held waits until the hold ends, and an issue
// that isn't a human task costs no read. It reads the issue's own
// reactions (IssueReactions), never a comment's, with a conditional
// request, so a pass where nothing changed costs a 304. It takes the
// newest 👍 that no marker names (state.ReactionKey) by a user trust
// trusts, not a bot, so neither the App nor any other bot counts, and runs
// the task's check on it (humanCheck), as DoneCommand does:
//
//   - A pass, or a plain-text "Done when", closes the issue as completed
//     (Transition, needs-human → closed on human-task-done), keyed
//     cmd=r<reaction id>, with seen the newest comment read (newestRead).
//     The close judge sets stage:completed on the next pass.
//   - A failure, including a check that couldn't be read
//     (state.CheckUnreadable), gets one reply, quoting the 👍 and its
//     user, and saying what the check found and how to check again, with a
//     reply marker keyed cmd=r<reaction id>, reason=rejected. The issue
//     stays open, and the marker names the 👍, so no later pass checks it
//     again.
//
// So each 👍 counts once: removing one and adding it again makes a new
// reaction, which checks again. A read that fails, of the reactions or for
// the check, is an error, not a failed check: the pass fails with no
// reply, and the next pass tries again.
func DoneByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("done by 👍 on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		if s.Closed || is.Stage != state.NeedsHuman {
			return nil
		}
		if class, err := state.ClassOf(is.Item); err != nil || class != state.ClassHumanTask {
			return nil
		}
		reactions, err := a.Client.IssueReactions(ctx, a.Owner, a.Repo, s.Number)
		if err != nil {
			return wrap(err)
		}
		// Newest first, so the first trusted one is the 👍 to take.
		slices.SortFunc(reactions, func(x, y github.Reaction) int { return cmp.Compare(y.ID, x.ID) })
		var thumb *github.Reaction
		for _, r := range reactions {
			if r.Content != "+1" || r.User.Type == "Bot" || is.Keys[state.ReactionKey(r.ID)] {
				continue
			}
			t, err := trust.Trusted(ctx, r.User.Login)
			if err != nil {
				return wrap(fmt.Errorf("trust %s: %w", r.User.Login, err))
			}
			if t {
				thumb = &r
				break
			}
		}
		if thumb == nil {
			return nil
		}
		key := state.ReactionKey(thumb.ID)
		pass, found, err := a.humanCheck(ctx, is.Origin.Check)
		if err != nil {
			return wrap(err)
		}
		// The login isn't the App's text: it's escaped, and defused.
		who := "`" + defuse(login(thumb.User)) + "`"
		if !pass {
			marker, err := github.FormatMarker(github.Marker{Kind: "reply", Fields: map[string]string{"cmd": key.ID, "reason": "rejected"}})
			if err != nil {
				return wrap(err)
			}
			text := fmt.Sprintf("> 👍 by %s\n\nI didn't close this task: its \"Done when\" check didn't pass. %s "+
				"To check again, remove the 👍 from this issue and add it again, or post `/done`.", who, found)
			if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
				return wrap(err)
			}
			logr.Infof("%s/%s#%d: %s's 👍 didn't close the human task, as its check didn't pass: %s", a.Owner, a.Repo, s.Number, login(thumb.User), found)
			return nil
		}
		seen := newestRead(s, a.AppSlug, lastTransition(s, a.AppSlug, d.Has))
		m := Move{To: state.Closed, StateReason: "completed", Trigger: state.TriggerHumanTaskDone, Key: key,
			Fields: map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)},
			Text:   fmt.Sprintf("%s's 👍 says this task is done. %s So I've closed it as completed.", who, found)}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return wrap(err)
		}
		logr.Infof("%s/%s#%d: %s's 👍 closed the human task", a.Owner, a.Repo, s.Number, login(thumb.User))
		return nil
	}
}

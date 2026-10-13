package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Handler handles one valid command for Step. Until the command is
// processed, each pass calls Check, then Apply, even after a crash part way
// through Apply.
//   - Check, if set, returns a Rejection when the command doesn't apply. It
//     must not write, as Step also calls it to word a lost reply again, and
//     must not reject a command Apply has begun: a cmd=<comment id> marker
//     Apply wrote, here or on another issue, means that part is applied.
//   - Apply makes the writes, each checked first. A cmd=<comment id> marker
//     on this issue ends the command, and so does the App's 👍 or 👎 on it, so
//     Apply writes that marker, then the 👍, last. Transition with a comment
//     key adds the 👍, so a move on another issue, such as a descendant,
//     mustn't go through it as it is.
type Handler struct {
	Check, Apply func(ctx context.Context, a *Applier, c Cmd) error
}

// Cmd is a command for a Handler.
type Cmd struct {
	state.Command
	Comment github.Comment
	// S is the issue. The handler adds its writes to it.
	S    *github.Snapshot
	Dirt Dirt
}

// Rejection is a Handler's error for a command that doesn't apply. Step
// replies with its reason (SPEC §9.2). It isn't a state error (SPEC §5.3).
type Rejection struct {
	Reason string
	// Final is set when a new comment would be rejected the same way, so
	// the reply doesn't suggest one.
	Final bool
}

func (r Rejection) Error() string { return r.Reason }

// command applies cmd, from comment c, through its handler, or rejects it.
func (st *Step) command(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) error {
	r, h, err := st.why(ctx, s, d, c, cmd, stage)
	switch {
	case err != nil:
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	case r.Reason != "":
		if err := st.react(ctx, c.ID, "-1"); err != nil {
			return fmt.Errorf("reject: %w", err)
		}
		_, parsed := state.ParseCommand(c.Body)
		return st.rejected(ctx, s, c, r, !parsed)
	case h.Apply == nil:
		logr.Debugf("%s/%s#%d: /%s in comment %d waits: nothing handles it yet", st.Owner, st.Repo, s.Number, cmd.Name, c.ID)
		return nil
	}
	if err := h.Apply(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d}); err != nil {
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	}
	return nil
}

// why returns why cmd, from comment c, doesn't apply at stage, or a
// Rejection with no Reason and its handler. It doesn't write.
func (st *Step) why(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) (Rejection, Handler, error) {
	if err := cmd.Check(stage); err != nil {
		return Rejection{Reason: err.Error()}, Handler{}, nil
	}
	h, ok := st.Commands[cmd.Name]
	switch {
	case !ok && cmd.Name == "done":
		return Rejection{Reason: "`/done` isn't handled yet"}, h, nil
	case h.Check == nil:
		return Rejection{}, h, nil
	}
	err := h.Check(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d})
	if r := (Rejection{}); errors.As(err, &r) {
		return r, h, nil
	}
	if err != nil {
		return Rejection{}, h, fmt.Errorf("check /%s: %w", cmd.Name, err)
	}
	return Rejection{}, h, nil
}

// lost is a lost reply's reason when it can't be worded again.
const lost = "my reply saying why was lost"

// rejected posts the reply to c, a command the App 👎'd first (plan #5,
// amended assumption 5), saying why it doesn't apply, worded for a revision
// request if request is set.
func (st *Step) rejected(ctx context.Context, s *github.Snapshot, c github.Comment, r Rejection, request bool) error {
	logr.Infof("%s/%s#%d: rejected comment %d: %s", st.Owner, st.Repo, s.Number, c.ID, r.Reason)
	text := "I didn't apply this command: " + r.Reason + "."
	if request {
		text = "I didn't send the spec back for revision: " + r.Reason + "."
	}
	if !r.Final {
		text += " Post a new comment to try again."
	}
	if err := st.reply(ctx, s, c, "rejected", text); err != nil {
		return fmt.Errorf("reject: %w", err)
	}
	return nil
}

// reply posts text, escaped (SPEC §5.1), below a quote of comment c's first
// line, with a reply marker naming c, which counts it as processed.
func (st *Step) reply(ctx context.Context, s *github.Snapshot, c github.Comment, reason, text string) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "reply", Fields: map[string]string{"cmd": state.CmdKey(c.ID).ID, "reason": reason}})
	if err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(c.Body), "\n")
	if err := st.comment(ctx, s, join(github.Escape("> "+strings.TrimSpace(line)+"\n\n"+text), marker)); err != nil {
		return fmt.Errorf("reply %s: %w", reason, err)
	}
	return nil
}

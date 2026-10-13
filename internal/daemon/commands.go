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

// Handler handles one valid command for Step.
//   - Check, if set, returns a Rejection when the command doesn't apply now.
//     It must not write: Step also calls it to word again a rejection whose
//     reply was lost.
//   - Apply makes the writes and acks the command. It must be replay-safe,
//     checking each write first. Step stops calling it once a marker on the
//     issue names cmd=<comment id>, so it writes that marker last; such a
//     marker already on another issue, such as a descendant, means that part
//     is applied.
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
type Rejection struct{ Reason string }

func (r Rejection) Error() string { return r.Reason }

// command applies cmd, from comment c, through its handler, or rejects it.
func (st *Step) command(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) error {
	reason, h, err := st.why(ctx, s, d, c, cmd, stage)
	switch {
	case err != nil:
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	case reason != "":
		if err := st.react(ctx, c.ID, "-1"); err != nil {
			return fmt.Errorf("reject: %w", err)
		}
		return st.rejected(ctx, s, c, reason)
	case h.Apply == nil:
		logr.Debugf("%s/%s#%d: /%s in comment %d waits: nothing handles it yet", st.Owner, st.Repo, s.Number, cmd.Name, c.ID)
		return nil
	}
	if err := h.Apply(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d}); err != nil {
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	}
	return nil
}

// why returns why cmd, from comment c, doesn't apply at stage, or "" and its
// handler. It doesn't write.
func (st *Step) why(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) (string, Handler, error) {
	if err := cmd.Check(stage); err != nil {
		return err.Error(), Handler{}, nil
	}
	h, ok := st.Commands[cmd.Name]
	switch {
	case !ok && cmd.Name == "done":
		return "`/done` isn't handled yet", h, nil
	case h.Check == nil:
		return "", h, nil
	}
	err := h.Check(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d})
	if r := (Rejection{}); errors.As(err, &r) {
		return r.Reason, h, nil
	}
	if err != nil {
		return "", h, fmt.Errorf("check /%s: %w", cmd.Name, err)
	}
	return "", h, nil
}

// lost is the reason a reply gives when the one it replaces can't be worded
// again: the command would apply now, or is no longer a command.
const lost = "my reply saying why was lost"

// rejected posts the reply to command comment c, which the App has 👎'd,
// saying why it doesn't apply. The 👎 comes first: only the App can remove
// it, so it keeps c processed if the reply is lost, and Step then posts the
// reply again (plan #5, amended assumption 5).
func (st *Step) rejected(ctx context.Context, s *github.Snapshot, c github.Comment, reason string) error {
	logr.Infof("%s/%s#%d: rejected comment %d: %s", st.Owner, st.Repo, s.Number, c.ID, reason)
	if err := st.reply(ctx, s, c, "rejected", "I didn't apply this command: "+reason+". Post a new comment to try again."); err != nil {
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

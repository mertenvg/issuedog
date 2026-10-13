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

// Handler applies one valid command for Step. It acks the command once it
// is applied, as Transition does for a cmd key, or returns a Rejection.
// Like Transition, it checks each write first, so it can run again after a
// crash.
type Handler func(ctx context.Context, a *Applier, c Cmd) error

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

// command checks cmd, from comment c, at stage, then applies it through its
// handler, or rejects it.
func (st *Step) command(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) error {
	if err := cmd.Check(stage); err != nil {
		return st.reject(ctx, s, c, err.Error())
	}
	h := st.Commands[cmd.Name]
	switch {
	case h == nil && cmd.Name == "done":
		return st.reject(ctx, s, c, "`/done` isn't handled yet")
	case h == nil:
		logr.Debugf("%s/%s#%d: /%s in comment %d waits: nothing handles it yet", st.Owner, st.Repo, s.Number, cmd.Name, c.ID)
		return nil
	}
	err := h(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d})
	if r := (Rejection{}); errors.As(err, &r) {
		return st.reject(ctx, s, c, r.Reason)
	}
	if err != nil {
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	}
	return nil
}

// reject replies to command comment c with why it doesn't apply.
func (st *Step) reject(ctx context.Context, s *github.Snapshot, c github.Comment, reason string) error {
	logr.Infof("%s/%s#%d: rejected comment %d: %s", st.Owner, st.Repo, s.Number, c.ID, reason)
	return st.reply(ctx, s, c, "rejected", "I didn't apply this command: "+reason+". Post a new comment to try again.")
}

// reply posts text below a quote of comment c's first line, with a reply
// marker naming c, which counts it as processed (plan #5, assumption 5). Both
// are escaped (SPEC §5.1). Run doesn't reach it once c is processed, so it's
// posted once.
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

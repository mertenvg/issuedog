package daemon

import (
	"context"
	"fmt"
	"maps"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
)

// NewStep is step 1 as the daemon runs it, on a with the trust tr: the
// command and reaction handlers merged so far, /track and /ignore among
// them, and the reaction triggers: first the end of a discussion hold
// once a 👍 or a counter comment resolves it, on a held issue; then a 👍 on
// a revision and a 👍 on a flag, each at its own stages, spec-review or
// plan-review for a spec or plan revision, and intake for a flag, so at
// most one of them acts on an issue; then a 👍 on a human task itself, at
// needs-human, which a hold the first ended may have returned it to; then
// a 👍 on a state error, at any stage. The loop's reloads replace its Trust
// and the daemon's Config, so the handlers and the triggers ask for the
// current trust, and models for the current models.allowed, rather than
// keeping what they had at the start. main's startDaemon and the whole-run
// tests both build step 1 with it, so the two can't drift apart (#218).
func NewStep(a *Applier, tr github.Trust, models func() config.Models) *Step {
	st := &Step{Applier: a, Trust: tr, Models: models}
	st.Commands = Commands(st.CurrentTrust())
	st.Commands["approve"] = ApproveCommand()
	st.Commands["revise"] = ReviseCommand()
	st.Commands["done"] = DoneCommand()
	maps.Copy(st.Commands, SettingCommands(st.CurrentTrust(), models))
	maps.Copy(st.Commands, FlagCommands())
	approve, answer := ApproveByReaction(st.CurrentTrust()), FlagByReaction(st.CurrentTrust())
	errAck, resolved := ErrorByReaction(st.CurrentTrust()), ResolveByReaction(st.CurrentTrust())
	done := DoneByReaction(st.CurrentTrust())
	st.Reactions = func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		if err := resolved(ctx, a, s, d); err != nil {
			return fmt.Errorf("discussion hold resolved: %w", err)
		}
		if err := approve(ctx, a, s, d); err != nil {
			return fmt.Errorf("approval by 👍: %w", err)
		}
		if err := answer(ctx, a, s, d); err != nil {
			return fmt.Errorf("flag answer by 👍: %w", err)
		}
		if err := done(ctx, a, s, d); err != nil {
			return fmt.Errorf("human task done by 👍: %w", err)
		}
		// Last, so an error a move above ended gets no read.
		if err := errAck(ctx, a, s, d); err != nil {
			return fmt.Errorf("state error ack by 👍: %w", err)
		}
		return nil
	}
	return st
}

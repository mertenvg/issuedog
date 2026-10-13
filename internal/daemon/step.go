package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Step is loop step 1 (SPEC §8.1) for one issue at a time: it acts on label
// events, commands, answers and reactions.
type Step struct {
	*Applier
	Trust github.Trust
	// Commands are the handlers, by command name. A valid command with none
	// stays unprocessed for the task that adds it, except /done, which is
	// rejected as not handled yet. Each must be replay-safe (see Handler).
	Commands map[string]Handler
	// Reactions, if set, runs after the comments: the reaction triggers.
	Reactions func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error
}

// Run runs step 1 on s, in this order: the label rules (CheckLabels), dirty
// content (MarkDirty), trusted comments oldest first, then Reactions. Dirty
// is read before CheckLabels, which needs it. It returns s's Inputs. Writes
// are added to s, and each is checked first, so a pass that crashed part way
// can run again (SPEC §5.2).
func (st *Step) Run(ctx context.Context, s *github.Snapshot) ([]github.Comment, error) {
	wrap := func(err error) error { return fmt.Errorf("step 1 on %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s)
	if err != nil {
		return nil, wrap(err)
	}
	if err := st.CheckLabels(ctx, s, st.Trust, d.Has); err != nil {
		return nil, wrap(err)
	}
	if err := st.MarkDirty(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	counts, err := st.counted(ctx, s, d)
	if err != nil {
		return nil, wrap(err)
	}
	// Replies are added to s.Comments, so range over the comments read.
	for _, c := range slices.Clone(s.Comments) {
		if !counts[c.ID] {
			continue
		}
		if err := st.handle(ctx, s, d, c); err != nil {
			return nil, wrap(fmt.Errorf("comment %d: %w", c.ID, err))
		}
	}
	if st.Reactions != nil {
		if err := st.Reactions(ctx, st.Applier, s, d); err != nil {
			return nil, wrap(err)
		}
	}
	return state.Derive(s, st.AppSlug, d.Has).Inputs(s, func(c github.Comment) bool { return counts[c.ID] }), nil
}

// counted returns which of s's comments count as trusted input (SPEC §6.2,
// §6.4): those by a trusted user that aren't dirty. No bot is trusted.
func (st *Step) counted(ctx context.Context, s *github.Snapshot, d Dirt) (map[int64]bool, error) {
	counts, trusted := map[int64]bool{}, map[string]bool{}
	for _, c := range s.Comments {
		if c.Author.Type == "Bot" || d.Has(c.ID) {
			continue
		}
		t, seen := trusted[c.Author.Login]
		if !seen {
			var err error
			if t, err = st.Trust.Trusted(ctx, c.Author.Login); err != nil {
				return nil, fmt.Errorf("trust %s: %w", c.Author.Login, err)
			}
			trusted[c.Author.Login] = t
		}
		counts[c.ID] = t
	}
	return counts, nil
}

// handle processes c, a comment that counts as trusted input. It is
// processed once a marker names it in cmd, a reply marker included, or the
// App has 👍'd it.
//   - A comment edited since it was acted on gets one edited reply, and isn't
//     applied again. That is since the first App comment whose marker names
//     it, or, for one only 👍'd, such as /priority, since the 👍. An answer
//     no marker has used yet is the exception: its latest text is the input.
//   - One a marker names, but the App hasn't 👍'd, only gets its 👍.
//   - At clarifying, one that isn't a command is an answer, and gets a 👍.
//   - A command is checked, then applied by its handler or rejected.
func (st *Step) handle(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment) error {
	is := state.Derive(s, st.AppSlug, d.Has)
	named := is.Keys[state.CmdKey(c.ID)]
	cmd, isCmd := state.ParseCommand(c.Body)
	answer := !isCmd && !named && is.Stage == state.Clarifying
	if replied(is, c.ID) || (!isCmd && !answer && !named && c.Edit.At.IsZero()) {
		return nil
	}
	if at, ok := st.usedAt(s, d, c.ID); ok && editedSince(c, at) {
		return st.reply(ctx, s, c, "edited", editedText)
	}
	ack, err := st.appAck(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("read the App's 👍 on comment %d: %w", c.ID, err)
	}
	switch {
	case ack != nil && !named && !answer && editedSince(c, ack.CreatedAt):
		return st.reply(ctx, s, c, "edited", editedText)
	case ack != nil:
		return nil
	case named || answer:
		return st.addAck(ctx, c.ID)
	case !isCmd:
		return nil
	}
	return st.command(ctx, s, d, c, cmd, is.Stage)
}

const editedText = "This was edited after I acted on it, so I've ignored the edit. Post a new comment instead."

// editedSince reports whether c was edited at or after t. GitHub's times are
// to the second, so an edit in t's second counts as after it.
func editedSince(c github.Comment, t time.Time) bool {
	return !c.Edit.At.IsZero() && !c.Edit.At.Before(t)
}

// usedAt returns when the first App comment whose marker names comment id
// in cmd was posted. Like state.Derive, it skips dirty comments.
func (st *Step) usedAt(s *github.Snapshot, d Dirt, id int64) (time.Time, bool) {
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, st.AppSlug) {
			if m.Fields["cmd"] == state.CmdKey(id).ID {
				return c.CreatedAt, true
			}
		}
	}
	return time.Time{}, false
}

// replied reports whether a reply marker names comment id.
func replied(is state.Issue, id int64) bool {
	return slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
		return m.Kind == "reply" && m.Fields["cmd"] == state.CmdKey(id).ID
	})
}

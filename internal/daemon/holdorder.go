package daemon

import (
	"context"
	"fmt"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// holdOrderWaits reports whether c, a /hold or /resume on s's issue that
// step 1 is about to handle, waits for a /hold or /resume posted before it
// on an open ancestor in this repo, which isn't processed yet (SPEC §6.4,
// #243). Step 1 visits the issues in number order, so one on an ancestor
// numbered above s's issue would otherwise be handled after c, although c
// came later: a child's /resume, handled before its ancestor's /hold, which
// then holds the child again, though the /resume ends that /hold. c waits
// for the ancestor's visit, in this pass or a later one, and s's issue is
// Touched, so it's read again next pass, and c is handled then, in the
// order they were posted. The other way round needs no wait: a /resume
// doesn't end a /hold on its path posted after it (resolveEndsHold, #205),
// so a /hold above it handled first holds as it would have, and a /hold
// below it, handled first, ends only by a /resume there or below. Other
// commands never wait for these. The walk up (madeAbove) reads nothing for
// an issue with no parent, and an unprocessed /hold or /resume reaches this
// only on a pass that has it to handle, so an unchanged pass costs no
// request.
func (st *Step) holdOrderWaits(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command) (bool, error) {
	if !holdOrderCommand(cmd) || s.Parent == 0 {
		return false, nil
	}
	trust := st.trustOnceTrust()
	above, err := st.madeAbove(ctx, Cmd{Command: cmd, Comment: c, S: s, Dirt: d, Trust: trust})
	if err != nil {
		return false, fmt.Errorf("the commands above comment %d: %w", c.ID, err)
	}
	for _, n := range above.nodes[1:] {
		if n.s.Closed { // never visited, so nothing on it is handled
			continue
		}
		id, err := st.holdOrderPending(ctx, n, c.ID, trust)
		if err != nil {
			return false, fmt.Errorf("the commands above comment %d: %w", c.ID, err)
		}
		if id != 0 {
			logr.Debugf("%s/%s#%d: /%s in comment %d waits for the /hold or /resume posted before it on #%d, in comment %d",
				st.Owner, st.Repo, s.Number, cmd.Name, c.ID, n.s.Number, id)
			if st.Touched != nil {
				st.Touched(s.Number)
			}
			return true, nil
		}
	}
	return false, nil
}

// holdOrderPending returns the first /hold or /resume on n, an ancestor,
// posted before comment before, that counts as trusted input (counted) and
// isn't processed yet (SPEC §9.2): no cmd or reply marker on n names it,
// and the App hasn't 👍'd or 👎'd it. Or 0, if there's none. The cheap
// checks come first, so only such a command costs requests: its author's
// trust, and its reactions.
func (st *Step) holdOrderPending(ctx context.Context, n node, before int64, trust github.Trust) (int64, error) {
	is := state.Derive(n.s, st.AppSlug, n.d.Has)
	for _, cm := range n.s.Comments {
		cmd, ok := state.ParseCommand(cm.Body)
		if cm.ID >= before || !ok || !holdOrderCommand(cmd) || cm.Author.Type == "Bot" || n.d.Has(cm.ID) || is.Keys[state.CmdKey(cm.ID)] {
			continue
		}
		trusted, err := trust.Trusted(ctx, cm.Author.Login)
		if err != nil {
			return 0, fmt.Errorf("trust %s: %w", cm.Author.Login, err)
		}
		if !trusted {
			continue
		}
		mine, err := st.appReactions(ctx, cm.ID)
		if err != nil {
			return 0, fmt.Errorf("read the App's reactions to comment %d on #%d: %w", cm.ID, n.s.Number, err)
		}
		_, up := mine["+1"]
		_, down := mine["-1"]
		if !up && !down {
			return cm.ID, nil
		}
	}
	return 0, nil
}

// holdOrderCommand reports whether cmd is one that holdOrderWaits keeps in
// the order posted across a family: /hold or /resume.
func holdOrderCommand(cmd state.Command) bool {
	return cmd.Name == "hold" || cmd.Name == "resume"
}

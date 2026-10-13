package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
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
//     sets Move.NoAck.
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

// lost is a lost reply's reason when it can't be worded again.
const lost = "my reply saying why was lost"

// rejected posts the reply to c, a command the App 👎'd first (plan #5,
// amended assumption 5), saying why it doesn't apply.
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

// Commands returns the handlers of /hold, /resume and /priority, for
// Step.Commands. trust is Step's: a descendant's labels are checked
// (CheckLabels) before it's moved, as Transition needs.
func Commands(trust github.Trust) map[string]Handler {
	return map[string]Handler{
		"hold": {Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			return a.moveFamily(ctx, c, trust, hold)
		}},
		"resume": {Check: resumeCheck, Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			return a.moveFamily(ctx, c, trust, resume)
		}},
		"priority": {Apply: setPriority},
	}
}

// planner returns a command's move for issue s, in state is, if any. self
// is set for the command's own issue, and by names the command.
type planner func(s *github.Snapshot, is state.Issue, self bool, by string) (Move, bool)

// hold is /hold's move for an issue, by the command by (SPEC §6.4). Holds
// stack, so an issue that's held already keeps its first at, and isn't moved.
func hold(_ *github.Snapshot, is state.Issue, _ bool, by string) (Move, bool) {
	if is.Stage == state.Held {
		return Move{}, false
	}
	return Move{To: state.Held, Trigger: state.TriggerHold, Fields: map[string]string{"at": string(is.Stage), "reason": "owner"},
		Text: fmt.Sprintf("On hold by %s, from `%s`.", by, is.Stage)}, true
}

// resume is /resume's move for an issue on an owner hold that the command
// ends (see ends), by the command by: back to its at. Any other hold, such
// as for discussion, stays.
func resume(s *github.Snapshot, is state.Issue, self bool, by string) (Move, bool) {
	at, ok := ends(s, is, self)
	return Move{To: at, Trigger: state.TriggerResume, Text: fmt.Sprintf("Resumed by %s, back to `%s`.", by, at)}, ok
}

// ownerHold returns where an issue on an owner hold goes back to, and the
// marker that held it: its at, if it has reason=owner.
func ownerHold(is state.Issue) (state.Stage, github.Marker, bool) {
	if is.Stage != state.Held {
		return "", github.Marker{}, false
	}
	for _, m := range slices.Backward(is.Markers) {
		if m.Kind == "transition" && state.Stage(m.Fields["to"]) == state.Held {
			at := state.Stage(m.Fields["at"])
			return at, m, m.Fields["reason"] == "owner" && at.Open() && at != state.Held
		}
	}
	return "", github.Marker{}, false
}

// ends returns where /resume on issue s, if self, or on an ancestor of s
// ends s's owner hold, if it does. On s itself it ends any. On an ancestor it
// ends only one an ancestor's /hold made: a marker whose cmd names a comment
// that isn't s's. A /hold on s names s's own comment, and a hand-set hold has
// no cmd (by= and event=), so those stay until /resume on s. Holds stack, so
// a /hold on s after an ancestor's writes no marker, and the ancestor's
// /resume still ends it; #87 may record stacked holds.
func ends(s *github.Snapshot, is state.Issue, self bool) (state.Stage, bool) {
	at, m, ok := ownerHold(is)
	if !ok || self {
		return at, ok
	}
	id, ok := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
	return at, ok && !slices.ContainsFunc(s.Comments, func(c github.Comment) bool { return c.ID == id })
}

// resumeCheck rejects a /resume unless Apply began, which a marker of c's on
// c's issue or any descendant, closed ones included, would show. A child
// unlinked since Apply moved it isn't found, so in that narrow window a
// replay can reject the command it began. Otherwise it rejects one that ends
// no hold (see ends) on c's issue or an open descendant, saying which
// descendants have holds of their own: an owner hold it doesn't end, or a
// stage:held label no marker explains, which Apply's CheckLabels would
// record as one of their own. And it rejects one whose issue's own owner
// hold has no row back to its at, which Apply would only report as a state
// error.
func resumeCheck(ctx context.Context, a *Applier, c Cmd) error {
	family, err := a.family(ctx, c)
	if err != nil {
		return fmt.Errorf("check /resume: %w", err)
	}
	held := false
	var own []string
	for _, n := range family {
		is := state.Derive(n.s, a.AppSlug, n.d.Has)
		if is.Moved(state.CmdKey(c.Comment.ID)) {
			return nil
		}
		if !n.open {
			continue
		}
		_, ended := ends(n.s, is, n.s == c.S)
		_, _, owner := ownerHold(is)
		unrecorded := is.Stage != state.Held && slices.ContainsFunc(n.s.Labels, func(l string) bool { return strings.EqualFold(l, "stage:held") })
		held = held || ended
		if !ended && (owner || unrecorded) {
			own = append(own, fmt.Sprintf("#%d", n.s.Number))
		}
	}
	is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
	if at, _, ok := ownerHold(is); ok {
		why, err := noRow(is, Move{To: at, Trigger: state.TriggerResume})
		if StateError(err) {
			return Rejection{Reason: fmt.Sprintf("this issue can't go back to `%s`: %s", at, why)}
		}
		if err != nil {
			return fmt.Errorf("check /resume: %w", err)
		}
	}
	switch {
	case held:
		return nil
	case len(own) > 0:
		return Rejection{Reason: fmt.Sprintf("the only holds below are the sub-issues' own (%s), which only `/resume` on each of them ends", strings.Join(own, ", "))}
	}
	return Rejection{Reason: "nothing here or in its open sub-issues is on an owner hold"}
}

// moveFamily makes the move plan returns, if any, on each open descendant of
// c's issue, depth first, then on the issue itself, then 👍s c. Each move is
// keyed by c, and one already written only has its labels finished. A
// descendant's labels are checked first, and its move has NoAck: the 👍
// comes only after the issue's own marker (see Handler). A move that's a
// state error, which Transition reports once, is logged and skipped, so one
// issue can't stall the rest.
func (a *Applier) moveFamily(ctx context.Context, c Cmd, trust github.Trust, plan planner) error {
	family, err := a.family(ctx, c)
	if err != nil {
		return fmt.Errorf("move: %w", err)
	}
	key := state.CmdKey(c.Comment.ID)
	for _, n := range family {
		if !n.open {
			continue
		}
		by := "`/" + c.Name + "`"
		if n.s != c.S {
			if err := a.CheckLabels(ctx, n.s, trust, n.d.Has); err != nil {
				return fmt.Errorf("move: %w", err)
			}
			by += fmt.Sprintf(" on #%d", c.S.Number)
		}
		is := state.Derive(n.s, a.AppSlug, n.d.Has)
		m, ok := plan(n.s, is, n.s == c.S, by)
		if is.Moved(key) {
			m.To, ok = is.Stage, true
		}
		if !ok {
			continue
		}
		m.Key, m.NoAck = key, true
		err := a.Transition(ctx, n.s, n.d.Has, m)
		if StateError(err) {
			logr.Warnf("%s/%s#%d: /%s in comment %d left #%d as it is: %v", a.Owner, a.Repo, c.S.Number, c.Name, c.Comment.ID, n.s.Number, err)
		} else if err != nil {
			return fmt.Errorf("move: %w", err)
		}
	}
	if err := a.Ack(ctx, c.Comment.ID); err != nil {
		return fmt.Errorf("move: %w", err)
	}
	return nil
}

// node is an issue a command moves, with its dirty content.
type node struct {
	s    *github.Snapshot
	d    Dirt
	open bool
}

// family returns the descendants of c's issue (sub-issues) in this repo,
// open and closed, depth first, each read fresh, then the issue itself,
// last. A sub-issue in another repo is skipped, with all below it.
func (a *Applier) family(ctx context.Context, c Cmd) ([]node, error) {
	var nodes []node
	seen := map[int]bool{c.S.Number: true}
	here := strings.ToLower("/repos/" + a.Owner + "/" + a.Repo)
	var walk func(n int) error
	walk = func(n int) error {
		opts := &gogithub.ListOptions{PerPage: 100}
		for sub, err := range a.Client.SubIssue.ListByIssueIter(ctx, a.Owner, a.Repo, int64(n), opts) {
			if err != nil {
				return fmt.Errorf("sub-issues of %s/%s#%d: %w", a.Owner, a.Repo, n, err)
			}
			is := (*gogithub.Issue)(sub)
			if !strings.HasSuffix(strings.ToLower(is.GetRepositoryURL()), here) {
				logr.Infof("%s/%s#%d: skipping its sub-issue #%d of %s, which isn't in this repo",
					a.Owner, a.Repo, n, is.GetNumber(), is.GetRepositoryURL())
				continue
			}
			if seen[is.GetNumber()] {
				continue
			}
			seen[is.GetNumber()] = true
			s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, is.GetNumber())
			if err != nil {
				return fmt.Errorf("descendant: %w", err)
			}
			d, err := a.Dirty(ctx, s)
			if err != nil {
				return fmt.Errorf("descendant: %w", err)
			}
			nodes = append(nodes, node{s, d, is.GetState() == "open"})
			if err := walk(is.GetNumber()); err != nil {
				return fmt.Errorf("below #%d: %w", n, err)
			}
		}
		return nil
	}
	if err := walk(c.S.Number); err != nil {
		return nil, fmt.Errorf("family of %s/%s#%d: %w", a.Owner, a.Repo, c.S.Number, err)
	}
	return append(nodes, node{c.S, c.Dirt, true}), nil
}

// setPriority sets c's priority:pN label, removes the other priority:*
// labels, then 👍s c (SPEC §9.2). It writes no marker, so the 👍 ends it.
// Command.Check has rejected anything but p0 to p3.
func setPriority(ctx context.Context, a *Applier, c Cmd) error {
	want := "priority:" + c.Arg
	if err := a.addLabel(ctx, c.S, want); err != nil {
		return fmt.Errorf("priority: %w", err)
	}
	for _, l := range slices.Clone(c.S.Labels) {
		if strings.HasPrefix(strings.ToLower(l), "priority:") && !strings.EqualFold(l, want) {
			if err := a.removeLabel(ctx, c.S, l); err != nil {
				return fmt.Errorf("priority: %w", err)
			}
		}
	}
	if err := a.Ack(ctx, c.Comment.ID); err != nil {
		return fmt.Errorf("priority: %w", err)
	}
	return nil
}

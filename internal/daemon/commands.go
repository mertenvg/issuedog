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
// Rejection with no Reason and its handler. It doesn't write. A revision
// request (requested) at the wrong stage was posted at spec-review, and the
// stage has moved on since, so its reason says that, not which stages
// /revise applies at. It's Final: a new comment now wouldn't send it back.
func (st *Step) why(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) (Rejection, Handler, error) {
	if err := cmd.Check(stage); err != nil {
		if _, parsed := state.ParseCommand(c.Body); !parsed && errors.Is(err, state.ErrWrongStage) {
			return Rejection{Reason: fmt.Sprintf("this issue is at `%s` now, and a comment sends the spec back only at `%s`", stage, state.SpecReview), Final: true}, Handler{}, nil
		}
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

// Commands returns the handlers of /hold, /resume and /priority, for
// Step.Commands. trust is Step's: a descendant's labels are checked
// (CheckLabels) before it's moved, as Transition needs.
func Commands(trust github.Trust) map[string]Handler {
	return map[string]Handler{
		"hold": {Check: holdCheck, Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			return a.moveFamily(ctx, c, trust, hold)
		}},
		"resume": {Check: resumeCheck, Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			above, err := a.madeAbove(ctx, c)
			if err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			return a.moveFamily(ctx, c, trust, func(is state.Issue, self bool, by string) (Move, bool) {
				return resume(is, self, above, by)
			})
		}},
		"priority": {Apply: setPriority},
	}
}

// planner returns a command's move for an issue in state is, if any. self
// is set for the command's own issue, and by names the command.
type planner func(is state.Issue, self bool, by string) (Move, bool)

// hold is /hold's move for an issue, by the command by (SPEC §6.4). Holds
// stack, so an issue that's held already keeps its first at, and isn't moved.
func hold(is state.Issue, _ bool, by string) (Move, bool) {
	if is.Stage == state.Held {
		return Move{}, false
	}
	return Move{To: state.Held, Trigger: state.TriggerHold, Fields: map[string]string{"at": string(is.Stage), "reason": "owner"},
		Text: fmt.Sprintf("On hold by %s, from `%s`.", by, is.Stage)}, true
}

// resume is /resume's move for an issue on an owner hold that the command
// ends (see ends), by the command by: back to its at. Any other hold, such
// as for discussion, stays.
func resume(is state.Issue, self bool, above map[int64]bool, by string) (Move, bool) {
	at, ok := ends(is, self, above)
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

// ends returns where /resume on an issue ends the owner hold of an issue in
// its family, in state is, if it does. On the issue itself, if self, it ends
// any. On a descendant it ends only one that a /hold on the issue or an
// ancestor of it made: a marker whose cmd names a comment in above (see
// madeAbove). A /hold on a descendant names a comment below, a hand-set hold
// has no cmd (by= and event=), and a deleted /hold comment is found nowhere,
// so those stay until /resume there. Holds stack, so a /hold below after one
// above writes no marker, and /resume above still ends it; #87 may record
// stacked holds.
func ends(is state.Issue, self bool, above map[int64]bool) (state.Stage, bool) {
	at, m, ok := ownerHold(is)
	if !ok || self {
		return at, ok
	}
	id, ok := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
	return at, ok && above[id]
}

// madeAbove returns the IDs of the comments on c's issue and its ancestors,
// read up through each parent in this repo. It stops at a parent in another
// repo, as family skips those going down.
func (a *Applier) madeAbove(ctx context.Context, c Cmd) (map[int64]bool, error) {
	above, seen := map[int64]bool{}, map[int]bool{}
	for s := c.S; ; {
		seen[s.Number] = true
		for _, cm := range s.Comments {
			above[cm.ID] = true
		}
		if s.Parent == 0 || seen[s.Parent] {
			return above, nil
		}
		parent, _, err := a.Client.SubIssue.GetParentIssue(ctx, a.Owner, a.Repo, int64(s.Number))
		if err != nil {
			return nil, fmt.Errorf("parent of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		if !a.inRepo(parent.GetRepositoryURL()) {
			logr.Infof("%s/%s#%d: its parent #%d of %s isn't in this repo, so it's where the walk up stops",
				a.Owner, a.Repo, s.Number, parent.GetNumber(), parent.GetRepositoryURL())
			return above, nil
		}
		if s, err = a.Client.Snapshot(ctx, a.Owner, a.Repo, parent.GetNumber()); err != nil {
			return nil, fmt.Errorf("ancestor of %s/%s#%d: %w", a.Owner, a.Repo, c.S.Number, err)
		}
	}
}

// inRepo reports whether an issue's repository_url, ".../repos/{owner}/{repo}",
// names this repo. GitHub ignores case in both.
func (a *Applier) inRepo(url string) bool {
	return strings.HasSuffix(strings.ToLower(url), strings.ToLower("/repos/"+a.Owner+"/"+a.Repo))
}

// madeBelow maps the comments on the descendants in family, all but its
// last node, to their issue numbers, for naming where a hold was made.
func madeBelow(family []node) map[int64]int {
	below := map[int64]int{}
	for _, n := range family[:len(family)-1] {
		for _, c := range n.s.Comments {
			below[c.ID] = n.s.Number
		}
	}
	return below
}

// begun reports whether a marker of c's is on an issue in family, closed
// ones included, which means Apply began. A child unlinked since Apply
// moved it isn't found, so in that narrow window a replay can reject the
// command it began.
func (a *Applier) begun(family []node, c Cmd) bool {
	return slices.ContainsFunc(family, func(n node) bool {
		return state.Derive(n.s, a.AppSlug, n.d.Has).Moved(state.CmdKey(c.Comment.ID))
	})
}

// holdCheck rejects a /hold, unless Apply began (see begun), when its
// issue's own move to held has no row, as for an unknown type.
func holdCheck(ctx context.Context, a *Applier, c Cmd) error {
	is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
	if is.Stage == state.Held {
		return nil
	}
	why, err := noRow(is, Move{To: state.Held, Trigger: state.TriggerHold})
	if !StateError(err) {
		if err != nil {
			return fmt.Errorf("check /hold: %w", err)
		}
		return nil
	}
	family, err := a.family(ctx, c)
	if err != nil {
		return fmt.Errorf("check /hold: %w", err)
	}
	if a.begun(family, c) {
		return nil
	}
	return Rejection{Reason: "this issue can't go on hold: " + why}
}

// resumeCheck rejects a /resume, unless Apply began (see begun), that ends
// no hold (see ends) on c's issue or an open descendant. It names the issues
// whose /resume would end the holds left: those that made them, by /hold or
// by hand, including a stage:held label no marker explains, which Apply's
// CheckLabels would record as a hold by hand. And it rejects one whose
// issue's own owner hold has no row back to its at, which Apply would only
// report as a state error.
func resumeCheck(ctx context.Context, a *Applier, c Cmd) error {
	family, err := a.family(ctx, c)
	if err != nil {
		return fmt.Errorf("check /resume: %w", err)
	}
	if a.begun(family, c) {
		return nil
	}
	above, err := a.madeAbove(ctx, c)
	if err != nil {
		return fmt.Errorf("check /resume: %w", err)
	}
	below := madeBelow(family)
	held := false
	var makers []string
	for _, n := range family {
		if !n.open {
			continue
		}
		is := state.Derive(n.s, a.AppSlug, n.d.Has)
		_, ended := ends(is, n.s == c.S, above)
		_, m, owner := ownerHold(is)
		unrecorded := is.Stage != state.Held && slices.ContainsFunc(n.s.Labels, func(l string) bool { return strings.EqualFold(l, "stage:held") })
		held = held || ended
		if owner || unrecorded {
			id, _ := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
			by, ok := below[id]
			if !ok {
				by = n.s.Number
			}
			if s := fmt.Sprintf("#%d", by); !slices.Contains(makers, s) {
				makers = append(makers, s)
			}
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
	case len(makers) > 0:
		return Rejection{Reason: fmt.Sprintf("the holds below were made on %s, by `/hold` or by hand, so `/resume` there ends them", strings.Join(makers, ", "))}
	}
	return Rejection{Reason: "nothing here or in its open sub-issues is on an owner hold"}
}

// moveFamily makes the move plan returns, if any, on each open descendant of
// c's issue, depth first, then on the issue itself, then 👍s c. Each move is
// keyed by c, so none is written twice. A descendant's labels are checked
// first, and its move has NoAck: the 👍 comes only after the issue's own
// marker (see Handler). A move that's a state error, which Transition
// reports once, is logged and skipped, so one issue can't stall the rest.
func (a *Applier) moveFamily(ctx context.Context, c Cmd, trust github.Trust, plan planner) error {
	family, err := a.family(ctx, c)
	if err != nil {
		return fmt.Errorf("move: %w", err)
	}
	for _, n := range family {
		if !n.open {
			continue
		}
		by := "`/" + c.Name + "`"
		if n.s != c.S {
			if a.Touched != nil {
				a.Touched(n.s.Number)
			}
			if err := a.CheckLabels(ctx, n.s, trust, n.d.Has); err != nil {
				return fmt.Errorf("move: %w", err)
			}
			by += fmt.Sprintf(" on #%d", c.S.Number)
		}
		is := state.Derive(n.s, a.AppSlug, n.d.Has)
		m, ok := plan(is, n.s == c.S, by)
		if !ok {
			continue
		}
		m.Key, m.NoAck = state.CmdKey(c.Comment.ID), true
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
	var walk func(n int) error
	walk = func(n int) error {
		opts := &gogithub.ListOptions{PerPage: 100}
		for sub, err := range a.Client.SubIssue.ListByIssueIter(ctx, a.Owner, a.Repo, int64(n), opts) {
			if err != nil {
				return fmt.Errorf("sub-issues of %s/%s#%d: %w", a.Owner, a.Repo, n, err)
			}
			is := (*gogithub.Issue)(sub)
			if !a.inRepo(is.GetRepositoryURL()) {
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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/config"
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
	// Trust is Step's, which Dirt was found with, for the dirty content
	// of the other issues a command reads, such as a /hold's descendants.
	Trust github.Trust
}

// Rejection is a Handler's error for a command that doesn't apply. Step
// replies with its reason (SPEC §9.2). It isn't a state error (SPEC §5.3).
type Rejection struct {
	Reason string
	// Final is set when a new comment would be rejected the same way, or
	// when Reason already says what to do next, so the reply doesn't
	// suggest one.
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
	if err := h.Apply(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d, Trust: st.Trust}); err != nil {
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	}
	return nil
}

// why returns why cmd, from comment c, doesn't apply at stage, or a
// Rejection with no Reason and its handler. It doesn't write. A revision
// request (requested) at any stage but spec-review was posted there, and
// the stage has moved on since, so its reason says that, whatever /revise
// would say at the stage now. It's Final: a new comment now wouldn't send
// the spec back.
func (st *Step) why(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, stage state.Stage) (Rejection, Handler, error) {
	if _, parsed := state.ParseCommand(c.Body); !parsed && stage != state.SpecReview {
		return Rejection{Reason: fmt.Sprintf("this issue is at `%s` now, and a comment sends the spec back only at `%s`", stage, state.SpecReview), Final: true}, Handler{}, nil
	}
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
	err := h.Check(ctx, st.Applier, Cmd{Command: cmd, Comment: c, S: s, Dirt: d, Trust: st.Trust})
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
			return a.moveFamily(ctx, c, trust, nil, hold)
		}},
		"resume": {Check: resumeCheck, Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			above, err := a.madeAbove(ctx, c)
			if err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			// A descendant whose owner hold it ends while a discussion stays
			// gets a record of that (resolveResumed).
			record := func(ctx context.Context, n node, is state.Issue, after bool) error {
				return a.resolveResumed(ctx, c, n, is, above, after)
			}
			// The descendants it ends a hold on that stay held: by hand, or
			// for discussion (resolveHeld), which /resume on an ancestor
			// doesn't end.
			var rest, discussed []string
			return a.moveFamily(ctx, c, trust, record, func(is state.Issue, self bool, n int, by string, after bool) (Move, bool) {
				m, ok := resume(is, self, above, by, after)
				if _, ended, left := heldStackResume(is, self, above); ended > 0 && len(left) > 0 {
					if slices.ContainsFunc(left, func(m github.Marker) bool { return m.Fields["reason"] == "discussion" }) {
						discussed = append(discussed, fmt.Sprintf("#%d", n))
					} else {
						rest = append(rest, fmt.Sprintf("#%d", n))
					}
				}
				if self && len(rest) > 0 {
					m.Text += fmt.Sprintf(" Still on hold: %s, also held by hand, so `/resume` there ends that.", list(rest))
				}
				if self && len(discussed) > 0 {
					m.Text += fmt.Sprintf(" Still on hold for discussion: %s.", list(discussed))
				}
				return m, ok
			})
		}},
		"priority": {Apply: setPriority},
	}
}

// planner returns a command's move for an issue in state is, if any. self
// is set for the command's own issue, n is the issue's number, by names
// the command, and after is resolveHeld's, for /resume.
type planner func(is state.Issue, self bool, n int, by string, after bool) (Move, bool)

// hold is /hold's move for an issue, by the command by (SPEC §6.4). Holds
// stack, and the first hold wins (plan #87 rev 2, Q2b), so an issue that's
// held already keeps its first at, and isn't moved. The move carries the
// round's bound, which the hold keeps (state.HeldStack.Seen).
func hold(is state.Issue, _ bool, _ int, by string, _ bool) (Move, bool) {
	if is.Stage == state.Held {
		return Move{}, false
	}
	fields := map[string]string{"at": string(is.Stage), "reason": "owner"}
	if is.AfterHold() {
		// Every marker at the clarifying after a hold carries its origin
		// forward, a later hold of either kind included (SPEC §6.4).
		fields["origin"] = string(discOrigin(is, is.Stage))
	}
	return Move{To: state.Held, Trigger: state.TriggerHold, Fields: state.HeldStackFields(fields, is.Round),
		Text: fmt.Sprintf("On hold by %s, from `%s`.", by, is.Stage)}, true
}

// resume is /resume's move for an issue in its family, in state is, by the
// command by: back to the first hold's at, once the command ends every hold
// on it (heldStackResume). The move carries the round bound from before the
// hold (state.HeldStack.Seen), so the round the hold came in goes on. With
// after, as when a discussion hold ended on the issue while an owner hold
// stayed (resolveHeld), it goes to clarifying instead (SPEC §6.4), with at
// and origin too (resolveFields).
func resume(is state.Issue, self bool, above map[int64]bool, by string, after bool) (Move, bool) {
	at, ended, left := heldStackResume(is, self, above)
	fields := map[string]string{}
	if o, ok := resolveHeldAfterHold(is); ok {
		// Back to the clarifying after a hold, which carries its origin
		// forward (SPEC §6.4), so the commands there keep waiting.
		fields["origin"] = string(o)
	}
	m := Move{To: at, Trigger: state.TriggerResume, Fields: state.HeldStackFields(fields, is.HeldStack.Seen),
		Text: fmt.Sprintf("Resumed by %s, back to `%s`.", by, at)}
	if after {
		m = Move{To: state.Clarifying, Trigger: state.TriggerResumeAfterDiscussion, Fields: resolveFields(is.HeldStack, time.Time{}),
			Text: fmt.Sprintf("Resumed by %s. A discussion on this issue was resolved during its hold from `%s`, so it goes to `clarifying`. %s", by, at, resolveWait)}
	}
	return m, ended > 0 && len(left) == 0
}

// heldStackResume returns what /resume on an issue does to an issue in its
// family, in state is (SPEC §6.4): at is where the issue goes back to, the
// first hold's at; ended counts the holds on it the command ends
// (heldStackEnds); and left are the holds that stay. The issue leaves held
// only when none is left. An issue that isn't held, or whose at isn't a
// stage to go back to, has no hold the command ends or leaves.
func heldStackResume(is state.Issue, self bool, above map[int64]bool) (at state.Stage, ended int, left []github.Marker) {
	h := is.HeldStack
	if is.Stage != state.Held || !h.At.Open() || h.At == state.Held {
		return h.At, 0, nil
	}
	for _, m := range h.Holds {
		if heldStackEnds(m, self, above) {
			ended++
		} else {
			left = append(left, m)
		}
	}
	return h.At, ended, left
}

// heldStackEnds reports whether /resume on an issue ends the hold that
// marker m records on an issue in its family. On the issue itself, if
// self, it ends any owner or discussion hold: a discussion hold's
// triggers, all of them, are accepted by its move out of held (SPEC §6.4,
// T4). On a descendant it ends only an owner hold that a /hold on the
// issue or an ancestor of it made: a marker whose cmd names a comment in
// above (see madeAbove). A /hold on a descendant names a comment below, a
// hold set by hand has no cmd (by= and event=), and a deleted /hold
// comment is found nowhere, so those stay until /resume there, as does a
// discussion hold. The first hold wins (plan #87 rev 2, Q2b), so a /hold
// below after one above writes no marker, and /resume above still ends it.
// Nor does /resume on the issue end a discussion hold whose trigger came
// after it (resolveStays).
func heldStackEnds(m github.Marker, self bool, above map[int64]bool) bool {
	if m.Fields["reason"] == "discussion" {
		return self && m.Fields[resolveStays] == ""
	}
	if m.Fields["reason"] != "owner" {
		return false
	}
	id, ok := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
	return self || ok && above[id]
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
// ones included, which means Apply began: a move it keys, or a record of
// an owner hold it ended (resolveResumed), but not a reply, as a
// rejection's is (resolveApplied). A child unlinked since Apply moved it
// isn't found, so in that narrow window a replay can reject the command
// it began.
func (a *Applier) begun(family []node, c Cmd) bool {
	return slices.ContainsFunc(family, func(n node) bool {
		return resolveApplied(state.Derive(n.s, a.AppSlug, n.d.Has), c.Comment.ID)
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

// resumeCheck rejects a /resume, unless Apply began (see begun), that takes
// neither c's issue nor an open descendant out of held (heldStackResume),
// nor ends the owner hold of a descendant that stays held for discussion
// only, which Apply records (resolveResumed). It names the issues whose
// /resume would end the holds left: those held for discussion, and those
// that made the owner holds, by /hold or by hand, including a stage:held
// label no marker explains, which Apply's CheckLabels would record as a
// hold by hand. And it rejects one whose issue's own holds would all end
// but have no row back to their at, or to clarifying after a discussion
// (resolveHeld), which Apply would only report as a state error.
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
	var makers, discussed []string
	for _, n := range family {
		if !n.open {
			continue
		}
		// Not a label the label rules left out, or would (Applier.hidden).
		s := a.hidden(n.s)
		var extra []github.Marker
		if n.s != c.S {
			if extra, err = a.resolveHeldBy(ctx, family, c, n, c.Trust); err != nil {
				return fmt.Errorf("check /resume: %w", err)
			}
		}
		is, _, err := a.resolveHeld(ctx, s, n.d, c.Trust, state.Derive(s, a.AppSlug, n.d.Has), c.Comment, n.s == c.S, extra)
		if err != nil {
			return fmt.Errorf("check /resume: %w", err)
		}
		_, ended, left := heldStackResume(is, n.s == c.S, above)
		// It leaves held, or stays held for discussion only, which Apply
		// records (resolveResumed).
		held = held || ended > 0 && !resolveOwnerHeld(left)
		// A descendant held for discussion only, which this doesn't end; if
		// it ends an owner hold there too, the /resume applies (held).
		if len(left) > 0 && !resolveOwnerHeld(left) {
			discussed = append(discussed, fmt.Sprintf("#%d", n.s.Number))
		}
		var made []int64 // the comments that made the owner holds left, or 0
		for _, m := range left {
			if m.Fields["reason"] == "owner" {
				id, _ := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
				made = append(made, id)
			}
		}
		if is.Stage != state.Held && slices.ContainsFunc(s.Labels, func(l string) bool { return strings.EqualFold(l, "stage:held") }) {
			made = append(made, 0) // unrecorded: a hold by hand
		}
		for _, id := range made {
			by, ok := below[id]
			if !ok {
				by = n.s.Number
			}
			if s := fmt.Sprintf("#%d", by); !slices.Contains(makers, s) {
				makers = append(makers, s)
			}
		}
	}
	// The issue's own move: back to its at, or to clarifying after a
	// discussion (resolveHeld).
	is, after, err := a.resolveHeld(ctx, c.S, c.Dirt, c.Trust, state.Derive(c.S, a.AppSlug, c.Dirt.Has), c.Comment, true, nil)
	if err != nil {
		return fmt.Errorf("check /resume: %w", err)
	}
	if at, ended, left := heldStackResume(is, true, above); ended > 0 && len(left) == 0 {
		to, trigger := at, state.TriggerResume
		if after {
			to, trigger = state.Clarifying, state.TriggerResumeAfterDiscussion
		}
		why, err := noRow(is, Move{To: to, Trigger: trigger})
		if StateError(err) {
			return Rejection{Reason: fmt.Sprintf("this issue can't go back to `%s`: %s", to, why)}
		}
		if err != nil {
			return fmt.Errorf("check /resume: %w", err)
		}
	}
	switch {
	case held:
		return nil
	case len(discussed) > 0:
		// /resume on an ancestor doesn't end a discussion hold (SPEC §6.4).
		return Rejection{Reason: fmt.Sprintf("%s on hold for discussion, which only a code owner's response there, "+
			"or `/resume` there, ends", strings.Join(discussed, ", ")), Final: true}
	case len(makers) > 0:
		return Rejection{Reason: fmt.Sprintf("the holds below were made on %s, by `/hold` or by hand, so `/resume` there ends them", strings.Join(makers, ", ")), Final: true}
	}
	return Rejection{Reason: "nothing here or in its open sub-issues is on an owner hold"}
}

// moveFamily makes the move plan returns, if any, on each open descendant of
// c's issue, depth first, then on the issue itself, then 👍s c. Each move is
// keyed by c, so none is written twice. A descendant's labels are checked
// first, and its move has NoAck: the 👍 comes only after the issue's own
// marker (see Handler). A move that's a state error, which Transition
// reports once, is logged and skipped, so one issue can't stall the rest.
// With record, as for /resume, plan gets the open discussion triggers of a
// held issue among its holds, its owner holds no marker records, and its
// after (resolveHeld), and record runs first on each issue, for its writes.
func (a *Applier) moveFamily(ctx context.Context, c Cmd, trust github.Trust, record func(ctx context.Context, n node, is state.Issue, after bool) error, plan planner) error {
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
			if err := a.discCheckLabels(ctx, n.s, trust, n.d); err != nil {
				return fmt.Errorf("move: %w", err)
			}
			by += fmt.Sprintf(" on #%d", c.S.Number)
		}
		is, after := state.Derive(n.s, a.AppSlug, n.d.Has), false
		if record != nil {
			var extra []github.Marker
			if n.s != c.S {
				if extra, err = a.resolveHeldBy(ctx, family, c, n, trust); err != nil {
					return fmt.Errorf("move: %w", err)
				}
			}
			if is, after, err = a.resolveHeld(ctx, n.s, n.d, trust, is, c.Comment, n.s == c.S, extra); err != nil {
				return fmt.Errorf("move: %w", err)
			}
			if err := record(ctx, n, is, after); err != nil {
				return fmt.Errorf("move: %w", err)
			}
		}
		m, ok := plan(is, n.s == c.S, n.s.Number, by, after)
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
			d, err := a.Dirty(ctx, s, c.Trust)
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
// labels, then 👍s c (SPEC §9.2). Command.Check has rejected anything but p0
// to p3.
func setPriority(ctx context.Context, a *Applier, c Cmd) error {
	if err := a.setLabel(ctx, c, "priority:"+c.Arg); err != nil {
		return fmt.Errorf("priority: %w", err)
	}
	return nil
}

// setLabel adds want to c's issue, removes the other labels of its kind, the
// part up to its first colon, ignoring case, then 👍s c. It writes no marker,
// so the 👍 ends c, and each write is checked first, so a replay redoes only
// what's missing. A label kept in another case stays as it is.
func (a *Applier) setLabel(ctx context.Context, c Cmd, want string) error {
	kind, _, _ := strings.Cut(want, ":")
	if err := a.addLabel(ctx, c.S, want); err != nil {
		return fmt.Errorf("set %s: %w", want, err)
	}
	for _, l := range slices.Clone(c.S.Labels) {
		if strings.HasPrefix(strings.ToLower(l), strings.ToLower(kind)+":") && !strings.EqualFold(l, want) {
			if err := a.removeLabel(ctx, c.S, l); err != nil {
				return fmt.Errorf("set %s: %w", want, err)
			}
		}
	}
	if err := a.Ack(ctx, c.Comment.ID); err != nil {
		return fmt.Errorf("set %s: %w", want, err)
	}
	return nil
}

// SettingCommands returns the handlers of /model and /effort (plan #93,
// T5), for Step.Commands. Each sets one label of its kind, as /priority
// does: model:*, effort:*, or for the verifier's runs, verifier-model:* or
// verifier-effort:*. models returns models.allowed as of the call, such as
// Daemon.CurrentModels, so a config reload that changes it takes effect.
// trust is Step's, such as Step.CurrentTrust, for telling the label rules'
// reverts apart (see addedSince).
func SettingCommands(trust github.Trust, models func() config.Models) map[string]Handler {
	return map[string]Handler{
		"model": setting(func(ctx context.Context, a *Applier, c Cmd) (string, error) {
			return modelLabel(ctx, a, trust, models(), c)
		}),
		"effort": setting(func(_ context.Context, _ *Applier, c Cmd) (string, error) { return effortLabel(c) }),
	}
}

// setting is the handler of a command that sets the label that label
// returns for it, or is rejected with the Rejection that label returns.
// label must not write, as Check calls it too.
func setting(label func(ctx context.Context, a *Applier, c Cmd) (string, error)) Handler {
	return Handler{
		Check: func(ctx context.Context, a *Applier, c Cmd) error {
			if _, err := label(ctx, a, c); err != nil {
				return fmt.Errorf("label: %w", err)
			}
			return nil
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			want, err := label(ctx, a, c)
			if err != nil {
				return fmt.Errorf("label: %w", err)
			}
			if err := a.setLabel(ctx, c, want); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
			return nil
		},
	}
}

// settingPrefix returns the value c, a /model or /effort command, sets, and
// the prefix of the label it sets it in: github.ModelPrefix or
// github.EffortPrefix, or for the verifier's runs github.VerifierModelPrefix
// or github.VerifierEffortPrefix.
func settingPrefix(c Cmd) (value, prefix string) {
	value, verifier := c.Setting()
	switch {
	case c.Name == "model" && verifier:
		return value, github.VerifierModelPrefix
	case c.Name == "model":
		return value, github.ModelPrefix
	case verifier:
		return value, github.VerifierEffortPrefix
	}
	return value, github.EffortPrefix
}

// modelLabel returns the label c, a /model command, sets: its prefix and the
// label suffix of the model on models.allowed that it names, by full name
// or by suffix, either ignoring case. Otherwise it returns a Rejection,
// which quotes c's text; reply escapes it (SPEC §5.1).
//
// A reload can drop a model from models.allowed after Apply began, and
// Check mustn't then reject c (see Handler). So when c names no allowed
// model, the labels with c's prefix on c's issue that name no allowed model
// either, its stray labels, are looked up on the timeline (see addedSince),
// and c names a stray label if either:
//   - a begun Apply of c added it, and it is the only stray label that one
//     added. c then removes the others.
//   - it is the only stray label, and the App removed another label with
//     c's prefix since c: a begun Apply whose add found its label there
//     already, which then removed the others.
//
// Otherwise c is rejected, and its labels stay. Only this path reads the
// timeline, and it doesn't write.
//
// Known limits:
//   - Two begun Applies still go unseen, so c is rejected and its labels
//     stay as they are. One crashed before its first write, so it changed
//     nothing. In the other, the add found its label there already, and
//     another stray label is still on the issue, so the App's removals
//     don't show which one c named.
//   - The App's label writes since c are taken as c's Apply's, but an
//     earlier command's Apply can make them after c is posted. Say the
//     owner posts /model opus, then /model gpt, and a pass applies the
//     first, adding model:opus after the second, then crashes before
//     handling the second, and a reload drops opus. The next pass 👍s
//     /model gpt as if it named opus, and its Apply removes the other
//     labels of its kind, such as a model:haiku a trusted user added in
//     between. This needs only the owner's own commands, a crash and a
//     reload, and an untrusted user can't cause it.
func modelLabel(ctx context.Context, a *Applier, trust github.Trust, models config.Models, c Cmd) (string, error) {
	arg, prefix := settingPrefix(c)
	name, ok := models.Resolve(arg)
	var same []string // the full names arg matches ignoring case, quoted
	for _, m := range models.Allowed {
		if !ok && strings.EqualFold(m.Name, arg) {
			name = m.Name
			same = append(same, "`"+m.Name+"`")
		}
	}
	if ok || len(same) == 1 {
		// name is on models.allowed, so it has a suffix: ok is always true.
		suffix, _ := models.LabelSuffix(name)
		return prefix + suffix, nil
	}
	var stray []string
	for _, l := range c.S.Labels {
		if !strings.HasPrefix(strings.ToLower(l), prefix) {
			continue
		}
		if _, allowed := models.ModelName(l[len(prefix):]); !allowed {
			stray = append(stray, l)
		}
	}
	if len(stray) > 0 {
		kind := "" // removals count only with one stray label
		if len(stray) == 1 {
			kind = prefix
		}
		begun, removed, err := a.addedSince(ctx, c, trust, stray, kind)
		if err != nil {
			return "", fmt.Errorf("model: %w", err)
		}
		switch {
		case len(begun) == 1:
			return begun[0], nil
		case len(begun) == 0 && removed:
			return stray[0], nil
		}
	}
	if len(same) > 1 {
		return "", Rejection{Reason: fmt.Sprintf("`%s` matches %s on `models.allowed`, ignoring case, so give the name in its exact case", arg, list(same))}
	}
	var suffixes []string
	for _, m := range models.Allowed {
		suffixes = append(suffixes, "`"+m.Suffix()+"`")
	}
	if len(suffixes) == 0 {
		return "", Rejection{Reason: fmt.Sprintf("`%s` isn't on `models.allowed`, and the config allows no model", arg)}
	}
	return "", Rejection{Reason: fmt.Sprintf("`%s` isn't on `models.allowed`, which has %s", arg, list(suffixes))}
}

// addedSince returns those of labels, on c's issue, that a begun Apply of c
// added: each one whose latest labeled or unlabeled event on the timeline,
// after c's own commented event, is the App's labeled event, and isn't the
// label rules' revert (see appRevert). If kind, a label prefix, isn't
// empty, it also reports whether the App, after c's commented event,
// removed a label with that prefix that isn't one of labels, in an
// unlabeled event that isn't a revert either: Apply removes the other
// labels of its kind after its add. As the App, only /model commands and
// the label rules change those labels, and Step handles commands in order.
// A label rules' change that isn't keyed by the event before it, such as a
// removal of a pipeline label that no trusted or App event explains (#118,
// keyed name@id), is still a revert by appRevert's walk back, not by its
// marker: the label rules remove a label only when the last accepted change
// left it off.
func (a *Applier) addedSince(ctx context.Context, c Cmd, trust github.Trust, labels []string, kind string) ([]string, bool, error) {
	wrap := func(err error) error { return fmt.Errorf("labels added since comment %d: %w", c.Comment.ID, err) }
	events, err := a.Client.TimelineSince(ctx, a.Owner, a.Repo, c.S.Number, time.Time{})
	if err != nil {
		return nil, false, wrap(err)
	}
	after := slices.IndexFunc(events, func(e *gogithub.Timeline) bool {
		return e.GetEvent() == "commented" && e.GetID() == c.Comment.ID
	})
	if after < 0 {
		return nil, false, wrap(fmt.Errorf("the comment isn't on the timeline of %s/%s#%d", a.Owner, a.Repo, c.S.Number))
	}
	h := labelHistory{a: a, trust: trust, events: events, reverted: map[string]bool{}, verdicts: map[string]bool{}}
	byLabel := map[string][]int{} // each label's events' positions, by lower-case name
	var names []string            // byLabel's keys, in timeline order
	for i, e := range events {
		switch e.GetEvent() {
		case "labeled", "unlabeled":
			l := strings.ToLower(e.GetLabel().GetName())
			if byLabel[l] == nil {
				names = append(names, l)
			}
			byLabel[l] = append(byLabel[l], i)
		case "commented":
			if c.Dirt.Has(e.GetID()) {
				continue
			}
			for _, m := range github.TrustedMarkers(author(e.GetUser()), e.GetBody(), a.AppSlug) {
				if m.Kind == "revert" {
					h.reverted[m.Fields["key"]] = true
				}
			}
		}
	}
	// since reports whether the k-th of a label's events, at the positions
	// ats, is the App's event of kind ev after c's, and not a revert.
	since := func(ats []int, k int, ev string) (bool, error) {
		e := events[ats[k]]
		if ats[k] <= after || e.GetEvent() != ev || !state.IsApp(author(e.GetActor()), a.AppSlug) {
			return false, nil
		}
		revert, err := h.appRevert(ctx, ats, k)
		if err != nil {
			return false, fmt.Errorf("%s %s: %w", ev, e.GetLabel().GetName(), err)
		}
		return !revert, nil
	}
	var added []string
	for _, l := range labels {
		ats := byLabel[strings.ToLower(l)]
		if len(ats) == 0 {
			continue
		}
		ok, err := since(ats, len(ats)-1, "labeled")
		if err != nil {
			return nil, false, wrap(err)
		}
		if ok {
			added = append(added, l)
		}
	}
	removed := false
	for _, l := range names {
		if removed || kind == "" || !strings.HasPrefix(l, kind) || slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			continue
		}
		for k := range byLabel[l] {
			ok, err := since(byLabel[l], k, "unlabeled")
			if err != nil {
				return nil, false, wrap(err)
			}
			if ok {
				removed = true
				break
			}
		}
	}
	return added, removed, nil
}

// labelHistory is an issue's timeline, oldest first, for telling the label
// rules' reverts apart from a begun Apply's writes.
type labelHistory struct {
	a        *Applier
	trust    github.Trust
	events   []*gogithub.Timeline
	reverted map[string]bool // the keys of trusted revert markers in clean comments
	verdicts map[string]bool // trust by lower-case login, asked once
}

// appRevert reports whether the App's label event at events[ats[k]], the
// k-th of its label's events, at the positions ats, is the label rules'
// revert, which puts back a change an untrusted user made. It is one if
// either:
//   - a trusted revert marker names the event before it: its key is that
//     event's ID (github.CheckLabels). Like the label rules, it skips dirty
//     comments.
//   - it restores the last accepted state: the label's latest event before
//     it that the App or a trusted user, by trust, made left the label as
//     it does, or there is none and it removes the label. This is the label
//     rules' own test (labelRules.restores, package github's own, so done
//     again here). An untrusted user can delete or edit a revert's comment,
//     but not the events. CheckLabels runs before commands in each pass, so
//     it puts back an untrusted change before Apply could write the label,
//     and untrusted changes that leave nothing to put back, such as an add
//     and then a removal, don't make Apply's later add a revert.
func (h *labelHistory) appRevert(ctx context.Context, ats []int, k int) (bool, error) {
	if k > 0 && h.reverted[strconv.FormatInt(h.events[ats[k-1]].GetID(), 10)] {
		return true, nil
	}
	before := false
	for j := k - 1; j >= 0; j-- {
		ok, err := h.accepted(ctx, h.events[ats[j]])
		if err != nil {
			return false, fmt.Errorf("revert: %w", err)
		}
		if ok {
			before = h.events[ats[j]].GetEvent() == "labeled"
			break
		}
	}
	return before == (h.events[ats[k]].GetEvent() == "labeled"), nil
}

// accepted reports whether the App or a trusted user made e. trust isn't
// asked about the App.
func (h *labelHistory) accepted(ctx context.Context, e *gogithub.Timeline) (bool, error) {
	by := e.GetActor()
	if state.IsApp(author(by), h.a.AppSlug) {
		return true, nil
	}
	login := strings.ToLower(by.GetLogin())
	if v, ok := h.verdicts[login]; ok {
		return v, nil
	}
	v, err := h.trust.Trusted(ctx, by.GetLogin())
	if err != nil {
		return false, fmt.Errorf("trust %s: %w", by.GetLogin(), err)
	}
	h.verdicts[login] = v
	return v, nil
}

// author is u as a github.Author.
func author(u *gogithub.User) github.Author {
	return github.Author{Login: u.GetLogin(), Type: u.GetType()}
}

// effortLabel returns the label c, a /effort command, sets: its prefix and
// the effort level it names, ignoring case. Otherwise it returns a
// Rejection, which quotes c's text; reply escapes it (SPEC §5.1). The levels
// are fixed in code, so no reload changes them.
func effortLabel(c Cmd) (string, error) {
	arg, prefix := settingPrefix(c)
	if level := strings.ToLower(arg); config.ValidEffort(level) {
		return prefix + level, nil
	}
	levels := config.EffortLevels()
	for i, l := range levels {
		levels[i] = "`" + l + "`"
	}
	return "", Rejection{Reason: fmt.Sprintf("`%s` isn't an effort level: the levels are %s", arg, list(levels))}
}

// list joins items as "a, b and c".
func list(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

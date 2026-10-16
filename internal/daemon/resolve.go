package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Resolving a discussion hold, and leaving it (SPEC §6.4, plan #87 rev 2,
// T4, #188). Every discussion trigger (discussion.go) needs a trusted
// user's response (#87's assumption 6):
//   - a comment trigger is resolved by a trusted user's 👍 on it, which
//     approves it, or by any later clean comment of a trusted user, which
//     counters every comment before it (resolver.comment);
//   - any other trigger is resolved by a trusted user's 👍 on its clean
//     notice, which accepts it (resolver.notice), the hold's own comment
//     included, which carries the first trigger's notice. Not one that
//     edited or deleted the App's content, though (resolveHitsApp): only
//     /resume on the issue accepts that, as it may have hidden another
//     trigger's record (#87's assumption 4);
//   - every open trigger is accepted by /resume on the issue itself
//     (heldStackEnds), which takes the issue out of held.
//
// A resolution is known from what resolved it, on GitHub, so a restart
// reads it again, and a later change of the trigger's content doesn't
// undo it: a 👍 stays on its comment or notice, and a counter comment
// stays where it is, whatever the content shows later. A resolved trigger
// is left out of discFind's result, judged before anyone is asked whether
// its maker is a writer. What closes it for good is the App's record that
// it's resolved, bounding the triggers from then on (discFloor), which a
// later act can't undo: a move out of held, or, while the issue stays
// held, an "ended" record (resolveLeave). Until then, removing the 👍, deleting
// the counter comment or the comment a 👍 approved, making either dirty,
// or the trusted user's loss of trust, opens the trigger again, failing
// closed. Records in edited or deleted App content aren't read (SPEC
// §5.1), so editing or deleting a record is a discussion trigger of its
// own, which only /resume accepts, and opens the triggers it closed.
//
// Once every trigger is resolved and no owner hold is left, the issue goes
// held → clarifying (resolveLeave, #87's assumption 9), with at and origin,
// and the round's bound from before the hold in seen (HeldStack.Seen).
// While an owner hold stays, the issue stays held, and its last /resume
// sends it to clarifying (resolveHeld). An issue held from intake isn't in
// the pipeline yet, so it stays held, and /resume takes it back to intake
// (resolveIntake). Until T5 (#189) runs the
// interviewer there, every class waits at that clarifying until a code
// owner sets its stage by hand (Interviewer.Pending), and the commands
// that would move it wait too (state.Command.Waits).

// resolveKind is the kind of the marker that records the end of one hold
// while another stays (SPEC §6.4), so the issue stays held:
//   - "ended reason=discussion resolved=<time>": every discussion trigger
//     on the issue is resolved, while an owner hold stays, or on an issue
//     held from intake (resolveLeave). It bounds the triggers as a move out
//     of held does (discFloor), and makes the issue's last /resume send it
//     to clarifying (resolveHeld), but from intake.
//   - "ended reason=owner key=<the /hold's comment id> cmd=<the /resume's
//     comment id>": a /resume on an ancestor ended the owner hold its /hold
//     made, while the issue stays held for discussion (resolveResumed), so
//     that hold counts no more (resolveLeft).
const resolveKind = "ended"

// resolveField is the marker field resolved=<Unix seconds>, on a move out
// of held for a resolved discussion, and on an "ended reason=discussion"
// record: when the act that resolved the last trigger happened. The record
// bounds the triggers from then, not from when the App wrote it, so an act
// in between still counts (discFloor).
const resolveField = "resolved"

// How a trigger was resolved (resolveAct.How).
const (
	resolveApproved  = "approved"  // a trusted 👍 on the comment
	resolveCountered = "countered" // a later clean comment by a trusted user
	resolveAccepted  = "accepted"  // a trusted 👍 on the trigger's notice
)

// resolveAct is what resolved a discussion trigger.
type resolveAct struct {
	Key string    // the trigger's key
	By  string    // who resolved it
	At  time.Time // when: the 👍's time, or the counter comment's
	How string    // resolveApproved, resolveCountered or resolveAccepted
	// Of is who made the trigger, and Recorded is set when a marker
	// records it: an unrecorded comment's maker isn't asked whether
	// they're a writer before it's found resolved (resolver.verified).
	Of       string
	Recorded bool
}

// resolver finds which open discussion triggers on s are resolved (SPEC
// §6.4), for discFind, once each: it reads each comment's reactions once,
// and only a candidate trigger's, conditionally, so an unchanged pass costs
// no rate limit.
type resolver struct {
	a         *Applier
	s         *github.Snapshot
	d         Dirt
	who       discWho
	records   map[string]int64            // discRecords
	notices   map[string]int64            // the clean App comment carrying each key's notice marker
	reactions map[int64][]github.Reaction // read, by comment
	done      map[string]bool             // judged, by key: whether resolved
	acts      []resolveAct                // what resolved those that are, in the order judged
}

// resolverOf is a resolver for s, its triggers' records, and who.
func (a *Applier) resolverOf(s *github.Snapshot, d Dirt, who discWho, records map[string]int64) *resolver {
	r := &resolver{a: a, s: s, d: d, who: who, records: records, notices: map[string]int64{},
		reactions: map[int64][]github.Reaction{}, done: map[string]bool{}}
	discMarkers(s, a.AppSlug, d.Has, func(id int64, _ time.Time, m github.Marker) {
		if key := m.Fields["key"]; m.Kind == "notice" && key != "" {
			if _, seen := r.notices[key]; !seen {
				r.notices[key] = id
			}
		}
	})
	return r
}

// early reports whether discFind skips tr, an open comment trigger, before
// it asks whether its author is a writer: if no marker records it
// (recorded), when its author is surely no writer who isn't trusted, being
// nobody (discWho.nobody) or trusted; and when it's resolved.
func (r *resolver) early(ctx context.Context, tr discTrigger, recorded bool, author github.Author) (bool, error) {
	if !recorded {
		if r.who.nobody(author) {
			return true, nil
		}
		t, err := r.who.trusted(ctx, author)
		if err != nil || t {
			return t, err
		}
	}
	return r.resolved(ctx, tr)
}

// drop returns found without the triggers that are resolved.
func (r *resolver) drop(ctx context.Context, found []discTrigger) ([]discTrigger, error) {
	var open []discTrigger
	for _, tr := range found {
		ok, err := r.resolved(ctx, tr)
		if err != nil {
			return nil, err
		}
		if !ok {
			open = append(open, tr)
		}
	}
	return open, nil
}

// resolved reports whether tr is resolved, and if so notes what resolved
// it in r.acts: a comment by resolver.comment, a trigger a notice accepts
// (resolveByNotice) by resolver.notice. Nothing else is resolved but by a
// move out of held.
func (r *resolver) resolved(ctx context.Context, tr discTrigger) (bool, error) {
	if v, ok := r.done[tr.Key]; ok {
		return v, nil
	}
	var act *resolveAct
	var err error
	switch {
	case tr.Kind == discComment:
		act, err = r.comment(ctx, tr)
	case resolveByNotice(tr, r.s, r.a.AppSlug):
		act, err = r.notice(ctx, tr)
	}
	if err != nil {
		return false, fmt.Errorf("resolution of %s: %w", tr.Key, err)
	}
	r.done[tr.Key] = act != nil
	if act != nil {
		_, recorded := r.records[tr.Key]
		act.Key, act.Of, act.Recorded = tr.Key, tr.By, recorded
		r.acts = append(r.acts, *act)
	}
	return act != nil, nil
}

// comment returns what resolved tr, a comment trigger, or nil: the first
// later clean comment by a trusted user, no bot, which counters it, else
// the earliest trusted 👍 on it, while it's still there and clean, which
// approves it. A comment gone is known by its key alone (FromRecord), so
// only a counter comment resolves it. The counter is found from the
// snapshot alone, so a countered comment's reactions aren't read.
func (r *resolver) comment(ctx context.Context, tr discTrigger) (*resolveAct, error) {
	for _, c := range r.s.Comments {
		if c.ID <= tr.ID || r.d.Has(c.ID) {
			continue
		}
		t, err := r.who.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("counter comment %d: %w", c.ID, err)
		}
		if t {
			return &resolveAct{By: c.Author.Login, At: c.CreatedAt, How: resolveCountered}, nil
		}
	}
	if tr.FromRecord || r.d.Has(tr.ID) {
		return nil, nil
	}
	return r.thumbsUp(ctx, tr.ID, resolveApproved)
}

// notice returns what accepted tr, a trigger resolveByNotice allows, or
// nil: the earliest trusted 👍 on the clean App comment carrying its
// notice marker. Each notice accepts only the key it names: a 👍 on the
// notice of a newer edit of some content doesn't accept an older edit's,
// which is a trigger of its own (#187's verify note), as the owner saw
// only the newer one named. A notice made dirty or deleted isn't read
// (discMarkers), so a 👍 on it accepts nothing, and no notice is posted
// for its trigger again (discLost): only /resume accepts that.
func (r *resolver) notice(ctx context.Context, tr discTrigger) (*resolveAct, error) {
	id, ok := r.notices[tr.Key]
	if !ok {
		return nil, nil
	}
	return r.thumbsUp(ctx, id, resolveAccepted)
}

// thumbsUp returns the earliest 👍 on comment id by a trusted user, no
// bot and not the App (discWho.trusted), as an act of kind how, or nil.
// It reads the comment's reactions once.
func (r *resolver) thumbsUp(ctx context.Context, id int64, how string) (*resolveAct, error) {
	reactions, read := r.reactions[id]
	if !read {
		var err error
		if reactions, err = r.a.Client.Reactions(ctx, r.a.Owner, r.a.Repo, id); err != nil {
			return nil, fmt.Errorf("👍s on comment %d: %w", id, err)
		}
		r.reactions[id] = reactions
	}
	var first *github.Reaction
	for _, x := range reactions {
		if x.Content != "+1" || first != nil && first.ID < x.ID {
			continue
		}
		t, err := r.who.trusted(ctx, x.User)
		if err != nil {
			return nil, fmt.Errorf("👍s on comment %d: %w", id, err)
		}
		if t {
			first = &x
		}
	}
	if first == nil {
		return nil, nil
	}
	return &resolveAct{By: first.User.Login, At: first.CreatedAt, How: how}, nil
}

// verified reports whether act resolved a trigger for sure: one a marker
// records, or a notice's, or else a comment by a write collaborator who
// isn't trusted, which resolver.early didn't ask about.
func (r *resolver) verified(ctx context.Context, act resolveAct) (bool, error) {
	if act.Recorded || act.How == resolveAccepted {
		return true, nil
	}
	w, err := r.who.writer(ctx, github.Author{Login: act.Of})
	if err != nil {
		return false, fmt.Errorf("verify %s: %w", act.Key, err)
	}
	return w, nil
}

// resolveByNotice reports whether a 👍 on tr's notice may accept it: a
// trigger that isn't a comment, of a kind discFind knows, and that didn't
// edit or delete the App's content (resolveHitsApp).
func resolveByNotice(tr discTrigger, s *github.Snapshot, appSlug string) bool {
	switch tr.Kind {
	case discEdit, discOwnEdit, discDeletion, discRename:
		return !resolveHitsApp(tr, s, appSlug)
	}
	return false
}

// resolveHitsApp reports whether tr edited or deleted content that may
// have been the App's, which only /resume accepts (#87's assumption 4,
// #187's verify note): that content may have held a record of another
// trigger, which the edit or deletion hid (SPEC §5.1), so accepting it by
// a 👍 could release a trigger whose record is gone. It's so when
// discHitsApp says, and, failing closed, for an edit known from its record
// alone (FromRecord) of the body, which the App may have rewritten, or of
// a comment gone or the App's.
func resolveHitsApp(tr discTrigger, s *github.Snapshot, appSlug string) bool {
	if discHitsApp(tr, appSlug) {
		return true
	}
	if !tr.FromRecord || tr.Kind != discEdit {
		return false
	}
	i := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return c.ID == tr.ID })
	return tr.ID == state.BodyID || i < 0 || state.IsApp(s.Comments[i].Author, appSlug)
}

// resolveActAt returns when the act that a move out of held, m, records
// happened, for its bound (discFloor): its resolved field, or the time of
// the comment its cmd names, such as a /resume on s. A /resume on another
// issue, as an ancestor's, isn't known here.
func resolveActAt(s *github.Snapshot, m github.Marker) (time.Time, bool) {
	if v, err := strconv.ParseInt(m.Fields[resolveField], 10, 64); err == nil && v > 0 {
		return time.Unix(v, 0).UTC(), true
	}
	if id, ok := (state.Key{Field: "cmd", ID: m.Fields["cmd"]}).Comment(); ok {
		if i := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return c.ID == id }); i >= 0 {
			return s.Comments[i].CreatedAt, true
		}
	}
	return time.Time{}, false
}

// resolveEndedAt returns when the last trigger was resolved, if m is an
// "ended reason=discussion" record with its resolved field (resolveKind).
func resolveEndedAt(m github.Marker) (time.Time, bool) {
	if m.Kind != resolveKind || m.Fields["reason"] != "discussion" {
		return time.Time{}, false
	}
	v, err := strconv.ParseInt(m.Fields[resolveField], 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}, false
	}
	return time.Unix(v, 0).UTC(), true
}

// resolvePeriod is the hold an issue is in, as its markers say, read as
// state.Derive reads them (discMarkers): from the move into held that began
// it, as state.HeldStack begins.
type resolvePeriod struct {
	start int64     // the comment holding the move that began it; 0 if not held
	at    time.Time // when that comment was posted
	// ended is set once an "ended reason=discussion" record is in it, and
	// endedAt is the latest such record's resolved time.
	ended   bool
	endedAt time.Time
	// owners are the cmd values of the owner holds that an ancestor's
	// /resume ended while a discussion stayed, "ended reason=owner key=",
	// each with the cmd of that /resume.
	owners map[string]string
}

// resolvePeriodOf returns the hold s is in, or the zero resolvePeriod if
// it isn't held.
func resolvePeriodOf(s *github.Snapshot, appSlug string, dirty func(int64) bool) resolvePeriod {
	var p resolvePeriod
	held := false
	discMarkers(s, appSlug, dirty, func(id int64, at time.Time, m github.Marker) {
		if _, bounds := state.RoundBound(m, id); bounds {
			switch {
			case state.Stage(m.Fields["to"]) != state.Held:
				p, held = resolvePeriod{}, false
			case !held || state.Stage(m.Fields["from"]) != state.Held:
				p, held = resolvePeriod{start: id, at: at, owners: map[string]string{}}, true
			}
			return
		}
		if m.Kind != resolveKind || !held {
			return
		}
		switch m.Fields["reason"] {
		case "discussion":
			p.ended = true
			if t, ok := resolveEndedAt(m); ok && t.After(p.endedAt) {
				p.endedAt = t
			}
		case "owner":
			p.owners[m.Fields["key"]] = m.Fields["cmd"]
		}
	})
	return p
}

// resolveLeft returns holds without the owner holds that p says an
// ancestor's /resume ended (resolveResumed), but for those that the
// /resume in comment except ended, which a replay of it still counts as
// its own to end.
func resolveLeft(holds []github.Marker, p resolvePeriod, except int64) []github.Marker {
	return slices.DeleteFunc(slices.Clone(holds), func(m github.Marker) bool {
		by, ended := p.owners[m.Fields["cmd"]]
		return m.Fields["reason"] == "owner" && m.Fields["cmd"] != "" && ended && by != state.CmdKey(except).ID
	})
}

// resolveOwnerHeld reports whether any of holds isn't a discussion hold:
// an owner hold, by /hold or by hand, or any other.
func resolveOwnerHeld(holds []github.Marker) bool {
	return slices.ContainsFunc(holds, func(m github.Marker) bool { return m.Fields["reason"] != "discussion" })
}

// resolveHeld is discHeldStack for /resume (commands.go): is, the state of
// s, with its open discussion triggers among its holds, and without the
// owner holds an ancestor's /resume ended while a discussion stayed
// (resolveLeft), but for those the /resume in comment c ended itself. after
// reports whether the issue goes to clarifying once c ends every hold on
// it: a discussion ended
// on it while an owner hold stayed (SPEC §6.4). That's so once the App
// recorded it (resolveLeave), or else when, during the hold and before
// the /resume, a writer's trigger was resolved while an owner hold stays,
// as when the /resume came before the App's pass that would record it. A
// /resume's own comment, which counters every comment before it, doesn't
// count, as /resume accepts those itself, and restores at. Nor does an
// issue held from intake go to clarifying, which would bring it into the
// pipeline without the approval SPEC §6.2 asks for (resolveIntake).
func (a *Applier) resolveHeld(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue, c github.Comment) (state.Issue, bool, error) {
	is, rs, err := a.discHeldStack(ctx, s, d, trust, is)
	if err != nil {
		return is, false, fmt.Errorf("resolved: %w", err)
	}
	if is.Stage != state.Held {
		return is, false, nil
	}
	p := resolvePeriodOf(s, a.AppSlug, d.Has)
	is.HeldStack.Holds = resolveLeft(is.HeldStack.Holds, p, c.ID)
	if resolveIntake(is.HeldStack) || p.ended || !resolveOwnerHeld(is.HeldStack.Holds) {
		return is, p.ended && !resolveIntake(is.HeldStack), nil
	}
	for _, act := range rs.acts {
		if act.At.Before(p.at) || !act.At.Before(c.CreatedAt) {
			continue
		}
		ok, err := rs.verified(ctx, act)
		if err != nil {
			return is, false, fmt.Errorf("resolved: %w", err)
		}
		if ok {
			return is, true, nil
		}
	}
	return is, false, nil
}

// resolveFields are the fields of a move out of held to clarifying after a
// discussion (#87's assumption 9): at, where the first hold was, and
// origin, where the issue was before its first discussion hold (SPEC
// §6.4), the first hold's, or else its at, each only if it's an open stage
// but held; the round's bound from before the hold in seen
// (state.HeldStackFields); and resolved, when the last trigger was
// resolved, if known (resolveField).
func resolveFields(h state.HeldStack, resolved time.Time) map[string]string {
	fields := map[string]string{}
	stage := func(s state.Stage) bool { return s.Open() && s != state.Held }
	if stage(h.At) {
		fields["at"] = string(h.At)
	}
	origin := h.Origin
	if !stage(origin) {
		origin = h.At
	}
	if stage(origin) {
		fields["origin"] = string(origin)
	}
	if !resolved.IsZero() {
		fields[resolveField] = strconv.FormatInt(resolved.Unix(), 10)
	}
	return state.HeldStackFields(fields, h.Seen)
}

// resolveIntake reports whether h, the holds on an issue, began at intake:
// such an issue isn't in the pipeline yet, so the end of a discussion on
// it never sends it to clarifying, which would admit it without the
// approval SPEC §6.2 asks for. It stays held until /resume restores intake.
func resolveIntake(h state.HeldStack) bool { return h.At == state.Intake }

// resolveWait is what the move to clarifying after a hold says of what
// comes next: until T5 (#189), every class waits there (Q3b).
const resolveWait = "It waits at `clarifying` until a code owner sets its stage by hand."

// ResolveByReaction returns the reaction trigger for Step.Reactions that
// ends a discussion hold once it's resolved (resolveLeave). It runs after
// the commands, so a /resume, which accepts every trigger and restores at,
// applies first.
func ResolveByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		if err := a.resolveLeave(ctx, s, d, trust); err != nil {
			return fmt.Errorf("discussion resolved on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		return nil
	}
}

// resolveLeave acts on s, if it's held, once no discussion trigger on it
// is open (discFind), as each is resolved:
//   - With no owner hold left (resolveLeft, resolveOwnerHeld), it goes
//     held → clarifying (#87's assumption 9), keyed event=h<the comment of
//     the move that began the hold>, unique to the hold, with
//     resolveFields: resolved is the latest act's time, which bounds the
//     triggers from then on (discFloor). The comment ends in cc @<notify>,
//     as the issue waits on a code owner (resolveWait).
//   - While an owner hold stays, or when the hold began at intake
//     (resolveIntake), it records, once for each resolution, that the
//     discussion ended: an App comment with an "ended reason=discussion
//     resolved=<time>" marker (resolveKind), when a writer's trigger was
//     resolved during the hold since the latest such record. That record
//     bounds the triggers as a move out of held does, so the next pass
//     finds no resolution after it, and writes nothing.
//
// A pure owner hold, with no trigger resolved, writes nothing. A move the
// table lacks, as for a type the pipeline doesn't know, is reported once by
// Transition, then only logged.
func (a *Applier) resolveLeave(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust) error {
	is := state.Derive(s, a.AppSlug, d.Has)
	if is.Stage != state.Held || len(is.HeldStack.Holds) == 0 {
		return nil
	}
	var rs *resolver
	found, err := a.discFindResolved(ctx, s, d, trust, &rs)
	if err != nil || len(found) > 0 {
		if err != nil {
			return fmt.Errorf("leave: %w", err)
		}
		return nil
	}
	p := resolvePeriodOf(s, a.AppSlug, d.Has)
	if resolveIntake(is.HeldStack) || resolveOwnerHeld(resolveLeft(is.HeldStack.Holds, p, 0)) {
		return a.resolveEnded(ctx, s, rs, p, resolveIntake(is.HeldStack))
	}
	var last time.Time
	var by []string
	for _, act := range rs.acts {
		if act.At.After(last) {
			last = act.At
		}
		if l := discLogin(act.By); !slices.Contains(by, l) {
			by = append(by, l)
		}
	}
	text := "The discussion is resolved"
	if len(by) > 0 {
		text += ", by " + list(by)
	}
	h := is.HeldStack
	text += fmt.Sprintf(", so this issue leaves its hold from `%s` for `clarifying`. %s", h.At, resolveWait)
	m := Move{To: state.Clarifying, Trigger: state.TriggerDiscussionResolved, Key: state.Key{Field: "event", ID: "h" + strconv.FormatInt(p.start, 10)},
		Fields: resolveFields(h, last), Text: discCC(text, a.Notify)}
	err = a.Transition(ctx, s, d.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: the discussion is resolved, but the issue stays held: %v", a.Owner, a.Repo, s.Number, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("leave: %w", err)
	}
	logr.Infof("%s/%s#%d: the discussion is resolved, so it goes to clarifying, from a hold at %s", a.Owner, a.Repo, s.Number, h.At)
	return nil
}

// resolveEnded writes the "ended reason=discussion" record on s, held by
// an owner hold that stays, or from intake, if a writer's trigger was
// resolved during the hold, p, since the latest such record
// (resolveLeave). Its time is the latest of those resolutions.
func (a *Applier) resolveEnded(ctx context.Context, s *github.Snapshot, rs *resolver, p resolvePeriod, intake bool) error {
	var last time.Time
	var by []string
	for _, act := range rs.acts {
		if act.At.Before(p.at) || p.ended && !act.At.After(p.endedAt) {
			continue
		}
		ok, err := rs.verified(ctx, act)
		if err != nil {
			return fmt.Errorf("record the end: %w", err)
		}
		if !ok {
			continue
		}
		if act.At.After(last) {
			last = act.At
		}
		if l := discLogin(act.By); !slices.Contains(by, l) {
			by = append(by, l)
		}
	}
	if last.IsZero() {
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: resolveKind, Fields: map[string]string{"reason": "discussion",
		resolveField: strconv.FormatInt(last.Unix(), 10)}})
	if err != nil {
		return fmt.Errorf("record the end: %w", err)
	}
	text := fmt.Sprintf("The discussion is resolved, by %s, but this issue stays on hold: an owner hold on it hasn't ended. "+
		"`/resume` here ends that, and then it goes to `clarifying`.", list(by))
	if intake {
		text = fmt.Sprintf("The discussion is resolved, by %s, but this issue stays on hold: it was at `intake` before the hold, "+
			"so `clarifying` would bring it into the pipeline. `/resume` here takes it back to `intake`.", list(by))
	}
	if err := a.comment(ctx, s, join(github.Escape(discCC(text, a.Notify)), marker)); err != nil {
		return fmt.Errorf("record the end: %w", err)
	}
	logr.Infof("%s/%s#%d: the discussion is resolved, but an owner hold stays", a.Owner, a.Repo, s.Number)
	return nil
}

// resolveResumed records on n, a descendant of c's issue in state is
// (resolveHeld), that c, a /resume, ended the owner holds that a /hold on
// c's issue or above made on it, above as for heldStackEnds, while it
// stays held, for discussion only: an App comment with one "ended
// reason=owner key=<the hold's cmd> cmd=<c's id>" marker for each such
// hold (resolveKind). So once the discussion is resolved, no owner hold is
// left (resolveLeft), and the issue goes to clarifying (resolveLeave),
// with no other /resume (#187's verify note). Its cmd makes it c's marker
// on n, so it's written once, and a replay of c counts it as begun.
func (a *Applier) resolveResumed(ctx context.Context, c Cmd, n node, is state.Issue, above map[int64]bool) error {
	_, ended, left := heldStackResume(is, false, above)
	if ended == 0 || len(left) == 0 || resolveOwnerHeld(left) || state.Derive(n.s, a.AppSlug, n.d.Has).Keys[state.CmdKey(c.Comment.ID)] {
		return nil
	}
	var markers []string
	for _, m := range is.HeldStack.Holds {
		if !heldStackEnds(m, false, above) {
			continue
		}
		f, err := github.FormatMarker(github.Marker{Kind: resolveKind, Fields: map[string]string{"reason": "owner", "key": m.Fields["cmd"],
			"cmd": state.CmdKey(c.Comment.ID).ID}})
		if err != nil {
			return fmt.Errorf("record the end of a hold on #%d: %w", n.s.Number, err)
		}
		markers = append(markers, f)
	}
	text := fmt.Sprintf("`/resume` on #%d ended the owner hold on this issue, but it stays on hold for discussion "+
		"until a code owner resolves that, or posts `/resume` here.", c.S.Number)
	if err := a.comment(ctx, n.s, join(github.Escape(text), strings.Join(markers, "\n"))); err != nil {
		return fmt.Errorf("record the end of a hold on #%d: %w", n.s.Number, err)
	}
	logr.Infof("%s/%s#%d: /resume in comment %d ended the owner hold on #%d, which stays held for discussion", a.Owner, a.Repo, c.S.Number, c.Comment.ID, n.s.Number)
	return nil
}

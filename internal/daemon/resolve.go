package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Resolving a discussion hold, and leaving it (SPEC §6.4, #188). Every
// discussion trigger (discussion.go) needs a trusted user's response (SPEC
// §6.4, Resolving):
//   - a comment trigger is resolved by a trusted user's 👍 on it, which
//     approves it, or by any later clean comment of a trusted user, which
//     counters every comment before it, but for a /hold or a /resume
//     (resolver.comment);
//   - any other trigger is resolved by a trusted user's 👍 on its clean
//     notice, which accepts it (resolver.notice), the hold's own comment
//     included, which carries the first trigger's notice. Not one that
//     edited or deleted the App's content, though (resolveHitsApp): only
//     /resume on the issue accepts that, as it may have hidden another
//     trigger's record (SPEC §6.4, Resolving);
//   - every open trigger made before it is accepted by /resume on the
//     issue itself (heldStackEnds), which takes the issue out of held. A
//     trigger made at or after the /resume, as one between it and the
//     App's pass, keeps the issue held (resolveStays): the /resume then
//     records that it ended the holds before it (resolveResumed), and the
//     issue leaves once the later trigger is resolved too.
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
// held → clarifying (resolveLeave, SPEC §6.4, Leaving the hold), with at
// and origin, and the round's bound from before the hold in seen
// (HeldStack.Seen). While an owner hold stays, the issue stays held, and
// its last /resume sends it to clarifying (resolveHeld). An owner hold is
// a hold marker, or, as a /hold on an issue held already writes none (SPEC
// §6.4), the /hold's own comment, on the issue (resolveOwnHolds) or an
// ancestor (resolveAbove, resolveHeldBy), until a /resume ends it by SPEC
// §6.4's one rule (resolveEndsHold): one posted after it, on the held
// issue, on the issue where the /hold was posted, or between, as
// heldStackEnds and resolveUnended both judge. A /resume back to the
// clarifying after a hold, and a hold from it of either kind, carry its
// origin forward (SPEC §6.4), so the commands that wait there keep waiting
// (resume, hold). An issue held from intake isn't in the pipeline yet, so
// it stays held, and /resume takes it back to intake (resolveIntake). The
// interviewer reads the discussion at that clarifying on an epic, a story
// with no parent or an untyped issue (#189); every other class waits there
// until a code owner sets its stage by hand (SPEC §6.4, Leaving the hold;
// Interviewer.Pending, ivHoldWait). The commands that would move the issue
// wait there too (state.Command.Waits).

// resolveKind is the kind of the marker that records the end of one hold
// while another stays (SPEC §6.4), so the issue stays held:
//   - "ended reason=discussion resolved=<time>": every discussion trigger
//     on the issue is resolved, while an owner hold stays, or on an issue
//     held from intake (resolveLeave). It bounds the triggers as a move out
//     of held does (discFloor), and makes the issue's last /resume send it
//     to clarifying (resolveHeld), but from intake. With held=<comment
//     ids>, the /holds on ancestors that keep the issue held (resolveAbove).
//     With cmd=<the /resume's comment id>, a /resume on the issue accepted
//     the triggers before it, while a later one keeps it held
//     (resolveResumed).
//   - "ended reason=owner key=<resolveHoldKey> cmd=<the /resume's comment
//     id>": a /resume ended that owner hold, while the issue stays held for
//     discussion (resolveResumed), so that hold counts no more
//     (resolveLeft).
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
	open      []discTrigger               // the triggers discFind found open, once it's done
	heldAt    time.Time                   // when the hold s is in began, or zero (resolvePeriod.at)
}

// resolverOf is a resolver for s, its triggers' records, and who.
func (a *Applier) resolverOf(s *github.Snapshot, d Dirt, who discWho, records map[string]int64) *resolver {
	r := &resolver{a: a, s: s, d: d, who: who, records: records, notices: map[string]int64{},
		reactions: map[int64][]github.Reaction{}, done: map[string]bool{}, heldAt: resolvePeriodOf(s, a.AppSlug, d.Has).at}
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
	case resolveByNotice(tr, r.s, r.a.AppSlug, r.heldAt):
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
//
// Any trusted comment counters, a command too, whose command applies as
// usual (SPEC §6.4): an /approve is a response to the discussion, and
// waits (state.Command.Waits). But not a /hold, which holds the issue
// more, nor a /resume, which accepts the triggers by its own rule, only
// those made before it (resolveHeld) (resolveHoldCommand).
func (r *resolver) comment(ctx context.Context, tr discTrigger) (*resolveAct, error) {
	for _, c := range r.s.Comments {
		if c.ID <= tr.ID || r.d.Has(c.ID) || resolveHoldCommand(c.Body) {
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
// records, as every trigger a notice accepts is, its notice marker being
// one of its records (discRecords), or else a comment by a write
// collaborator who isn't trusted, which resolver.early didn't ask about.
func (r *resolver) verified(ctx context.Context, act resolveAct) (bool, error) {
	if act.Recorded {
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
// edit or delete the App's content, nor delete a comment during the hold
// that began at heldAt (resolveHitsApp). A close (closing.go) hides no App
// content, so its notice, the hold's comment or one of its own, accepts it
// (SPEC §6.4, Notices; #190's verify note).
func resolveByNotice(tr discTrigger, s *github.Snapshot, appSlug string, heldAt time.Time) bool {
	switch tr.Kind {
	case discEdit, discOwnEdit, discDeletion, discRename, discClose:
		return !resolveHitsApp(tr, s, appSlug, heldAt)
	}
	return false
}

// resolveHitsApp reports whether tr edited or deleted content that may
// have been the App's, which only /resume accepts (SPEC §6.4, Resolving;
// #187's verify note): that content may have held a record of another
// trigger, which the edit or deletion hid (SPEC §5.1), so accepting it by
// a 👍 could release a trigger whose record is gone. It's so when
// discHitsApp says, and, failing closed, for an edit known from its record
// alone (FromRecord) of the body, which the App may have rewritten, or of
// a comment gone or the App's.
//
// So is a deletion during the hold, at or after heldAt, when it began, if
// it did: the comment may have been a code owner's /hold, applied with no
// marker as the issue was held already (SPEC §6.4), which GitHub's
// deletion event doesn't name, so it fails closed. /resume on the issue,
// which ends that /hold too, accepts it. A deletion that held the issue
// came before the hold began, so it hid no /hold applied during it.
func resolveHitsApp(tr discTrigger, s *github.Snapshot, appSlug string, heldAt time.Time) bool {
	if discHitsApp(tr, appSlug) || tr.Kind == discDeletion && !heldAt.IsZero() && !tr.At.Before(heldAt) {
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
	// ended is set once an "ended reason=discussion" record that says a
	// discussion ended is in it (resolvePeriodOf), and endedAt is the
	// latest such record's resolved time.
	ended   bool
	endedAt time.Time
	// owners are the keys (resolveHoldKey) of the owner holds that a
	// /resume ended while a discussion stayed, "ended reason=owner key=",
	// each with the cmd of that /resume.
	owners map[string]string
	// held are the /hold comments on ancestors that kept the issue held
	// when its discussion ended, "ended reason=discussion held=" (resolveAbove),
	// and heldOn the issue each counted as posted on then, by its comment,
	// from the same record's heldon= (resolveHeldOn).
	held   []int64
	heldOn map[int64]int
	// gone are the comments deleted on ancestors during the hold that may
	// have been /holds that kept the issue held when its discussion ended,
	// "ended reason=discussion gone=", each <issue>-<Unix seconds of the
	// deletion> (resolveGone).
	gone []string
	// prev is the comment holding the transition before the hold, or 0:
	// a /hold posted since, that no marker on the issue names, is one more
	// owner hold on it (resolveOwnHolds).
	prev int64
}

// resolvePeriodOf returns the hold s is in, or the zero resolvePeriod if
// it isn't held.
func resolvePeriodOf(s *github.Snapshot, appSlug string, dirty func(int64) bool) resolvePeriod {
	var p resolvePeriod
	held, prev := false, int64(0)
	discMarkers(s, appSlug, dirty, func(id int64, at time.Time, m github.Marker) {
		if _, bounds := state.RoundBound(m, id); bounds {
			switch {
			case state.Stage(m.Fields["to"]) != state.Held:
				p, held, prev = resolvePeriod{}, false, id
			case !held || state.Stage(m.Fields["from"]) != state.Held:
				p, held = resolvePeriod{start: id, at: at, owners: map[string]string{}, prev: prev}, true
			}
			return
		}
		if m.Kind != resolveKind || !held {
			return
		}
		switch m.Fields["reason"] {
		case "discussion":
			// A discussion ended in it if a trigger's resolution did it,
			// with a resolved time, and not a /resume's own record (cmd)
			// that kept the issue held for a later trigger, unless that
			// /resume would itself have gone to clarifying (after=1).
			// One that only names ancestors' /holds (held=) ended none.
			t, timed := resolveEndedAt(m)
			if timed && (m.Fields["cmd"] == "" || m.Fields["after"] == "1") {
				p.ended = true
				if t.After(p.endedAt) {
					p.endedAt = t
				}
			}
			ons := strings.Split(m.Fields[resolveHeldOn], ",")
			for i, v := range strings.Split(m.Fields["held"], ",") {
				id, err := strconv.ParseInt(v, 10, 64)
				if err != nil || id <= 0 {
					continue
				}
				if !slices.Contains(p.held, id) {
					p.held = append(p.held, id)
				}
				if i >= len(ons) {
					continue
				}
				if on, err := strconv.Atoi(ons[i]); err == nil && on > 0 {
					if p.heldOn == nil {
						p.heldOn = map[int64]int{}
					}
					p.heldOn[id] = on
				}
			}
			for _, v := range strings.Split(m.Fields[resolveGone], ",") {
				if _, _, ok := resolveGoneParse(v); ok && !slices.Contains(p.gone, v) {
					p.gone = append(p.gone, v)
				}
			}
		case "owner":
			p.owners[m.Fields["key"]] = m.Fields["cmd"]
		}
	})
	return p
}

// resolveLeft returns holds without the owner holds that p says a /resume
// ended while the issue stayed held (resolveResumed), but for those that
// the /resume in comment except ended, which a replay of it still counts
// as its own to end. It's the one rule for which records end owner holds, so
// every judgement of them agrees: the holds markers record and the
// /holds no marker records on the issue (resolveHoldsLeft, resolveHeld),
// the /holds on its ancestors (resolveUnended, so resolveAbove and
// resolveHeldBy), and the hold item's (holdItemOf). A record that says a
// /resume ended an ancestor's /hold, which no marker on the issue names,
// ends it here too, whether or not that /resume applied on its own issue,
// as when that issue was out of held by then (SPEC §6.4).
func resolveLeft(holds []github.Marker, p resolvePeriod, except int64) []github.Marker {
	return slices.DeleteFunc(slices.Clone(holds), func(m github.Marker) bool {
		by, ended := p.owners[resolveHoldKey(m)]
		return m.Fields["reason"] == "owner" && resolveHoldKey(m) != "" && ended && by != state.CmdKey(except).ID
	})
}

// resolveHoldKey names the owner hold that marker m records, for an
// "ended reason=owner" record: c<comment id> for a /hold, by its cmd, or
// e<event> for a hold set by hand, or one discRestore reposted, by its
// event. It's "" for any other marker.
func resolveHoldKey(m github.Marker) string {
	switch {
	case m.Fields["cmd"] != "":
		return "c" + m.Fields["cmd"]
	case m.Fields["event"] != "":
		return "e" + m.Fields["event"]
	case m.Fields[resolveGone] != "":
		return "g" + m.Fields[resolveGone]
	}
	return ""
}

// resolveGone is the field of an owner hold, in memory, that a comment
// deleted on an ancestor may have made (resolveUnended), and of the
// record of the discussion's end that names such holds (resolveEnded):
// <issue>-<Unix seconds>, the ancestor's number and when the comment was
// deleted. GitHub's deletion event doesn't say which comment it was, so a
// code owner's /hold that wrote no marker, as the issues were held already
// (SPEC §6.4), may be gone with it, failing closed. Only /resume on that
// ancestor, on an issue between, or on the issue itself, posted after the
// deletion, ends it, as /resume on the ancestor itself is the only thing
// that accepts the deletion there (resolveHitsApp). Its key is g<value>
// (resolveHoldKey); the App writes it only in that record, and in "ended
// reason=owner" records of it.
const resolveGone = "gone"

// resolveGoneHold is the owner hold marker, in memory, that a comment
// deleted on issue number at time at may have made (resolveGone).
func resolveGoneHold(number int, at time.Time) github.Marker {
	return github.Marker{Kind: "transition", Fields: map[string]string{"from": string(state.Held), "to": string(state.Held),
		"reason": "owner", resolveGone: fmt.Sprintf("%d-%d", number, at.Unix())}}
}

// resolveGoneParse returns the issue number and the deletion time that v,
// a resolveGone value, names, and whether it's well formed.
func resolveGoneParse(v string) (int, time.Time, bool) {
	n, secs, ok := strings.Cut(v, "-")
	number, err := strconv.Atoi(n)
	if !ok || err != nil || number <= 0 {
		return 0, time.Time{}, false
	}
	t, err := strconv.ParseInt(secs, 10, 64)
	if err != nil || t <= 0 {
		return 0, time.Time{}, false
	}
	return number, time.Unix(t, 0).UTC(), true
}

// resolveStays is a field resolveHeld sets, in memory only, on a
// discussion hold whose trigger was made at or after the /resume it
// judges for: that /resume doesn't end it (heldStackEnds). The App never
// writes it.
const resolveStays = "stays"

// resolveHoldCommand reports whether body is a /hold or a /resume, which
// counters no comment (resolver.comment).
func resolveHoldCommand(body string) bool {
	c, ok := state.ParseCommand(body)
	return ok && (c.Name == "hold" || c.Name == "resume")
}

// resolveApplied reports whether the App applied the command in comment
// id on the issue in state is, as a marker on it says: one that names it
// in its cmd field, but a reply, which a rejection is (Step.rejected), as
// an "edited" reply to an applied one is too. A /resume that the App
// rejected, or hasn't handled yet, has none, and ends nothing (SPEC §9.2,
// §6.4). One applied on an issue held for it writes one on that issue: a
// move out of held, or the record that it ended the holds before it while
// a later trigger keeps the issue held (resolveResumed). So one that
// ended no hold on its own issue, as one applied for its descendants
// alone, has none there either.
func resolveApplied(is state.Issue, id int64) bool {
	return slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
		return m.Kind != "reply" && m.Fields["cmd"] == state.CmdKey(id).ID
	})
}

// resolveRejected reports whether the App rejected the command in comment
// id on the issue in state is: its reply says so (Step.rejected). One
// 👎'd whose reply is lost isn't known here, and counts until reword
// posts it again, failing closed.
func resolveRejected(is state.Issue, id int64) bool {
	return replied(is, id) == "rejected"
}

// resolveCommands returns the IDs of the clean comments on s, above after
// and, unless before is 0, below before, that are a valid command named
// name, by a user trust trusts, no bot. Whether the App applied each is
// for the caller to judge (resolveApplied, resolveRejected).
func (a *Applier) resolveCommands(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, name string, after, before int64) ([]int64, error) {
	who := a.discWhoOf(s, trust)
	var ids []int64
	for _, c := range s.Comments {
		cmd, ok := state.ParseCommand(c.Body)
		if c.ID <= after || before != 0 && c.ID >= before || d.Has(c.ID) || !ok || cmd.Name != name || cmd.Check(state.Held) != nil {
			continue
		}
		t, err := who.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("/%s in comment %d: %w", name, c.ID, err)
		}
		if t {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// resolveOwnHolds returns the owner holds on s, in state is, held, p,
// that no transition marker records (SPEC §6.4), as owner hold markers:
// each /hold posted on s since the transition before the hold that no
// marker names, as one posted while s was held, its comment the record of
// that hold, keyed by its cmd; and each /hold on an ancestor that the
// record of the discussion's end names (resolveAbove). For a /resume,
// before is its comment: a /hold posted after it counts only once the App
// applied it (resolveHoldApplied), as when s is a descendant whose pass
// came first, and the /resume doesn't end it; one not handled yet holds
// the issue itself once handled (hold). A /hold on s that the App rejected
// holds nothing (resolveRejected), as one posted before the hold began,
// while s couldn't go on hold; one not handled yet counts, as it holds s
// once handled, since a /hold on an issue held already is never rejected
// (holdCheck).
//
// A /hold on s counts by the App's record that it applied it, whatever its
// comment shows now (SPEC §6.4): one a write collaborator edited since, so
// dirty, counts when it's a trusted user's that the App 👍'd during the
// hold (resolveHoldWitnessed). A comment deleted is known by no ID, so a
// deletion during the hold, which may have hidden an applied /hold, is
// accepted only by /resume (resolveHitsApp), which ends the /hold too.
// Last come the comments deleted on ancestors that the same record names,
// as holds that may have been /holds (resolveGone). A /resume on an
// ancestor never ends a /hold on s, which was posted below it, nor one
// between, only one on its own issue or above, or a comment deleted there
// before it (heldStackEnds, resolveEndsHold).
func (a *Applier) resolveOwnHolds(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue, p resolvePeriod, before int64) ([]github.Marker, error) {
	all, err := a.resolveCommands(ctx, s, d, trust, "hold", p.prev, 0)
	if err != nil {
		return nil, fmt.Errorf("owner holds: %w", err)
	}
	var ids []int64
	for _, id := range all {
		ok := !resolveRejected(is, id)
		if ok && before != 0 && id >= before {
			if ok, err = a.resolveHoldApplied(ctx, is, id); err != nil {
				return nil, fmt.Errorf("owner holds: %w", err)
			}
		}
		if ok {
			ids = append(ids, id)
		}
	}
	who := a.discWhoOf(s, trust)
	for _, c := range s.Comments {
		if !d.Has(c.ID) || c.ID <= p.prev || is.Moved(state.CmdKey(c.ID)) {
			continue
		}
		t, err := who.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("owner holds: comment %d: %w", c.ID, err)
		}
		if !t {
			continue
		}
		witnessed, err := a.resolveHoldWitnessed(ctx, is, c.ID, p.at)
		if err != nil {
			return nil, fmt.Errorf("owner holds: %w", err)
		}
		if witnessed {
			ids = append(ids, c.ID)
		}
	}
	var holds []github.Marker
	for _, id := range ids {
		if !is.Moved(state.CmdKey(id)) {
			holds = append(holds, resolveOwnerHold(id))
		}
	}
	for _, id := range p.held {
		if !is.Moved(state.CmdKey(id)) {
			m := resolveOwnerHold(id)
			if on, ok := p.heldOn[id]; ok {
				m.Fields[resolveOn] = strconv.Itoa(on)
			}
			holds = append(holds, m)
		}
	}
	for _, v := range p.gone {
		number, at, _ := resolveGoneParse(v) // well formed: resolvePeriodOf kept only those
		holds = append(holds, resolveGoneHold(number, at))
	}
	return holds, nil
}

// resolveHoldWitnessed reports whether the App 👍'd the comment id, made
// dirty since, on an issue in state is, at or after since, the start of
// the hold it would keep an issue in: the App's record that it applied a
// command there, which an edit doesn't undo, so the comment counts as the
// /hold it was, failing closed. Not one the App rejected, nor one a marker
// names (resolveApplied), whose marker says what it was, as a /resume's
// record does; nor one 👍'd before the hold, as an answer at clarifying
// is. An edited command the App 👍'd during the hold that held nothing,
// such as /priority, so counts too, until /resume ends it.
func (a *Applier) resolveHoldWitnessed(ctx context.Context, is state.Issue, id int64, since time.Time) (bool, error) {
	if resolveRejected(is, id) || resolveApplied(is, id) {
		return false, nil
	}
	mine, err := a.appReactions(ctx, id)
	if err != nil {
		return false, fmt.Errorf("comment %d: %w", id, err)
	}
	up, ok := mine["+1"]
	return ok && !up.CreatedAt.Before(since), nil
}

// resolveHoldMarked returns the comment that marker m names as the /hold
// that moved an issue into held, an owner hold's cmd=<comment id>: the
// App's own record that it applied that /hold, whatever the comment shows
// now.
func resolveHoldMarked(m github.Marker) (int64, bool) {
	if m.Kind != "transition" || state.Stage(m.Fields["to"]) != state.Held || m.Fields["reason"] != "owner" {
		return 0, false
	}
	return state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
}

// resolveOn is the field of an owner hold marker, in memory, that names
// the issue its /hold counted as posted on when the record of the
// discussion's end named it (resolveHeldOn): a /resume there, or below,
// ends it, though its comment be deleted since (madeAboveSet.made).
const resolveOn = "on"

// resolveHeldOn is the field of an "ended reason=discussion" record that
// names, for each /hold in its held field, in order, the issue that /hold
// counted as posted on (resolveUnended): so the record's "`/resume` there"
// stays true once the /hold's comment is deleted, when the issue it was
// posted on may have no marker of it (SPEC §6.4). A record written before
// it had none, and its /holds count as before.
const resolveHeldOn = "heldon"

// resolveOwnerHold is the owner hold marker that the /hold in comment id
// stands for, in memory.
func resolveOwnerHold(id int64) github.Marker {
	return github.Marker{Kind: "transition", Fields: map[string]string{"from": string(state.Held), "to": string(state.Held),
		"reason": "owner", "cmd": state.CmdKey(id).ID}}
}

// resolveHeldBy returns the holds that keep n, a descendant of c's issue
// in family, held, as owner hold markers, made on the issues above it,
// those between it and c's issue and then c's issue and its ancestors as
// madeAbove read them, above (resolveUnended), with c judged as a /resume
// on its issue, for resolveResumed. It reads every issue above n, as the
// leave path does (resolveAbove), so both count the same holds: those on
// c's issue or above, a /hold no marker on n records, or a comment deleted
// there during n's hold that may have been one, c ends when they came
// before it (heldStackEnds, resolveEndsHold), and resolveResumed records
// that, so the leave path doesn't count them again (resolveLeft); those
// on the path, or on n, c doesn't end, whether a /hold wrote a marker or
// none (SPEC §6.4), nor those posted after c that the App applied first,
// as on c's issue or above in the same pass (#205).
//
// For c's issue itself, held, which c ends every hold on that came before
// it (heldStackEnds), it returns only the /holds on its ancestors posted
// after c that the App applied first, with no marker on it, as it was held
// already (SPEC §6.4): an ancestor whose pass came first in the same pass,
// as commands run in issue-number order (#205). c doesn't end those, so
// its issue stays held by them, as it would had c been handled first. And
// so the comments deleted there at or after c that may have been such
// /holds (resolveGone, #244), read from what madeAbove read already.
func (a *Applier) resolveHeldBy(ctx context.Context, family []node, c Cmd, n node, trust github.Trust, above madeAboveSet) ([]github.Marker, error) {
	byNumber := map[int]node{}
	for _, x := range family {
		byNumber[x.s.Number] = x
	}
	is := state.Derive(n.s, a.AppSlug, n.d.Has)
	p := resolvePeriodOf(n.s, a.AppSlug, n.d.Has)
	if n.s == c.S {
		if is.Stage != state.Held || len(above.nodes) == 0 {
			return nil, nil
		}
		return a.resolveUnended(ctx, n, above.nodes[1:], is, p, trust, &resolveJudged{c: c.Comment, self: true})
	}
	var path []node // nearest first
	for x, ok := byNumber[n.s.Parent]; ok && x.s != c.S && len(path) < len(family); x, ok = byNumber[x.s.Parent] {
		path = append(path, x)
	}
	return a.resolveUnended(ctx, n, append(path, above.nodes...), is, p, trust, &resolveJudged{c: c.Comment})
}

// resolveJudged is a /resume that resolveUnended judges for: its comment,
// and self, set when it was posted on the held issue itself, which it
// ends every hold on that came before it (heldStackEnds).
type resolveJudged struct {
	c    github.Comment
	self bool
}

// resolveUnended returns, as owner hold markers on self, an issue in
// state is, held, p, the holds made on the issues in path, its ancestors
// nearest first, that keep self held, judged for j, a /resume posted on
// an issue on the path, or on self if j.self, if it isn't nil. The holds
// are:
//   - each valid trusted /hold posted on an issue on the path since the
//     transition before self's hold, named by no marker on self, that the
//     App applied (resolveHoldApplied); with j.self, only those posted
//     after j, as j ends every hold on self that came before it;
//   - each comment that a write collaborator who isn't trusted deleted on
//     an issue on the path during self's hold, written by a trusted user
//     or by someone GitHub doesn't name: it may have been such a /hold,
//     one that wrote no marker as the issues were held already (SPEC
//     §6.4), and GitHub doesn't say which comment it was (resolveGone).
//     With j.self, only those deleted at or after j was posted: j ends
//     each before it on self, so those aren't read, but one after it may
//     have been a /hold posted after j, which j doesn't end
//     (heldStackEnds, heldDeletionLater, #244).
//
// Each ends by SPEC §6.4's one rule (resolveEndsHold), which heldStackEnds
// judges by too: by a /resume the App applied, posted after the /hold, or
// after the deletion, on the issue where it was posted, on one nearer
// self, or on self, which ends every hold on it, as when it kept self held
// for a trigger made after it (resolveResumed); and by no other. So a
// /resume never ends a /hold posted below its issue, whether that /hold
// wrote a marker or, as the issue it was posted on was held already, none
// (SPEC §6.4). Last, it leaves out those a record on self says a /resume
// ended, but for j (resolveLeft), as every other judgement of the owner
// holds left does.
//
// Judged for j, the holds j ends, on j's issue or above it, are kept, so
// that heldStackEnds ends them, and resolveResumed records that, which
// the leave path, where j may not count as applied, reads too
// (resolveLeft). A /hold posted after j that the App applied before j,
// on the path below j's issue, or on j's issue or above it, as when an
// ancestor's pass comes first in the same pass (#205), holds self too,
// and j doesn't end it (resolveEndsHold), so heldStackEnds leaves it.
//
// A /hold counts by the App's record that it applied it, whatever its
// comment shows now (SPEC §6.4): an owner hold marker on an issue on the
// path that names it (resolveHoldMarked), its comment clean, edited by a
// write collaborator since, or deleted; else, for one with no marker, the
// App's 👍, on a clean valid one (resolveHoldApplied) or, on one edited
// since, during self's hold (resolveHoldWitnessed). One whose comment is
// gone is taken as posted on the farthest issue on the path with its
// marker, failing closed.
//
// Each /resume counts only once the App applied it on its own issue, as a
// marker there names it (resolveApplied): a rejected one ends nothing
// (SPEC §9.2), as one posted before a write collaborator's comment that
// held the issue first, and so does one not handled yet. So does one
// applied for its descendants alone, which ended no hold on its own issue:
// a /hold posted before it, handled after it in the same pass, as on an
// ancestor visited later, holds the issue it was posted on, so it holds
// self too. A /resume that took self out of held is before p.prev, in an
// earlier hold. It's bounded by p.prev, not by the hold's start: one
// applied in the pass that began the hold, as one that accepted the
// triggers before it while a later one kept the issue held, is below the
// hold's marker.
func (a *Applier) resolveUnended(ctx context.Context, self node, path []node, is state.Issue, p resolvePeriod, trust github.Trust, j *resolveJudged) ([]github.Marker, error) {
	var except int64    // j's comment, if any
	onSelf := false     // j was posted on self, so only /holds and deletions after it are read
	var since time.Time // when j was posted, if any
	if j != nil {
		except, onSelf, since = j.c.ID, j.self, j.c.CreatedAt
	}
	// in reports whether the /hold in comment id is one to read: since the
	// transition before the hold, and, with j on self, after j.
	in := func(id int64) bool { return id > p.prev && (!onSelf || id > except) }
	posted := map[int64]int{}   // the level of the issue each comment on self and the path is on, -1 for self
	at := map[int64]time.Time{} // when each of those comments was posted
	for _, c := range self.s.Comments {
		posted[c.ID], at[c.ID] = -1, c.CreatedAt
	}
	var resumes []resolveResume // each applied /resume
	own, err := a.resolveCommands(ctx, self.s, self.d, trust, "resume", p.prev, 0)
	if err != nil {
		return nil, fmt.Errorf("held from above: %w", err)
	}
	for _, id := range own {
		if resolveApplied(is, id) {
			resumes = append(resumes, resolveResume{id: id, level: -1, at: at[id]})
		}
	}
	states := make([]state.Issue, len(path)) // each issue's on the path
	marked := map[int64]int{}                // each /hold an owner hold marker on the path names, by the farthest level with one
	for level, x := range path {
		states[level] = state.Derive(x.s, a.AppSlug, x.d.Has)
		for _, c := range x.s.Comments {
			posted[c.ID], at[c.ID] = level, c.CreatedAt
		}
		r, err := a.resolveCommands(ctx, x.s, x.d, trust, "resume", p.prev, 0)
		if err != nil {
			return nil, fmt.Errorf("held from above: %w", err)
		}
		for _, id := range r {
			if resolveApplied(states[level], id) {
				resumes = append(resumes, resolveResume{id: id, level: level, at: at[id]})
			}
		}
		for _, m := range states[level].Markers {
			if id, ok := resolveHoldMarked(m); ok {
				marked[id] = max(marked[id], level)
			}
		}
	}
	var holds []resolveMade
	for level, x := range path {
		h, err := a.resolveCommands(ctx, x.s, x.d, trust, "hold", p.prev, 0)
		if err != nil {
			return nil, fmt.Errorf("held from above: %w", err)
		}
		for _, id := range h {
			if !in(id) {
				continue
			}
			_, applied := marked[id]
			if !applied {
				if applied, err = a.resolveHoldApplied(ctx, states[level], id); err != nil {
					return nil, fmt.Errorf("held from above: %w", err)
				}
			}
			if applied {
				holds = append(holds, resolveMade{id: id, level: level})
			}
		}
		// A /hold a write collaborator edited since counts by the App's
		// record that it applied it (resolveHoldMarked,
		// resolveHoldWitnessed), whatever it shows now.
		who := a.discWhoOf(x.s, trust)
		for _, c := range x.s.Comments {
			if !x.d.Has(c.ID) || !in(c.ID) {
				continue
			}
			_, applied := marked[c.ID]
			if !applied {
				t, err := who.trusted(ctx, c.Author)
				if err != nil {
					return nil, fmt.Errorf("held from above: comment %d: %w", c.ID, err)
				}
				if t {
					if applied, err = a.resolveHoldWitnessed(ctx, states[level], c.ID, p.at); err != nil {
						return nil, fmt.Errorf("held from above: %w", err)
					}
				}
			}
			if applied {
				holds = append(holds, resolveMade{id: c.ID, level: level})
			}
		}
	}
	// A /hold whose comment is deleted is known by its markers alone: it's
	// taken as posted on the farthest issue on the path that has its
	// marker, failing closed, so /resume there, or nearer, or on self ends
	// it (SPEC §6.4).
	for _, id := range slices.Sorted(maps.Keys(marked)) {
		if _, ok := posted[id]; !ok && in(id) {
			holds = append(holds, resolveMade{id: id, level: marked[id]})
		}
	}
	ended := func(h resolveMade) bool {
		return slices.ContainsFunc(resumes, func(r resolveResume) bool { return resolveEndsHold(h, r) })
	}
	var left []github.Marker
	for _, h := range holds {
		// One the record of the discussion's end names is judged by that
		// record, as resolveOwnHolds gives it (resolveOn).
		if is.Moved(state.CmdKey(h.id)) || slices.Contains(p.held, h.id) || ended(h) {
			continue
		}
		m := resolveOwnerHold(h.id)
		m.Fields[resolveOn] = strconv.Itoa(path[h.level].s.Number)
		left = append(left, m)
	}
	for level, x := range path {
		who := a.discWhoOf(x.s, trust)
		for _, e := range x.s.Deleted {
			if onSelf && e.At.Before(since) {
				continue // j ends it on self (heldStackEnds)
			}
			may, err := a.resolveGoneMay(ctx, who, e, p.at)
			if err != nil {
				return nil, fmt.Errorf("held from above: comment deleted on #%d: %w", x.s.Number, err)
			}
			m := resolveGoneHold(x.s.Number, e.At)
			if may && !ended(resolveMade{at: e.At, level: level}) && !slices.ContainsFunc(left, func(l github.Marker) bool { return resolveHoldKey(l) == resolveHoldKey(m) }) {
				left = append(left, m)
			}
		}
	}
	return resolveLeft(left, p, except), nil
}

// resolveMade is an owner hold that resolveEndsHold judges: the /hold in
// comment id, or, with id 0, a comment deleted at at that may have been
// one (resolveGone); and the level of the issue it was posted or deleted
// on. Levels count up from the held issue, -1 in resolveUnended, so each
// issue's level is one more than its child's; only how two levels compare
// matters, so madeAboveSet.made gives a /resume's own level as 0, and the
// held issue's as below it.
type resolveMade struct {
	id    int64
	at    time.Time
	level int
}

// resolveResume is a /resume that resolveEndsHold judges by: its comment,
// the level of the issue it was posted on, as resolveMade's, and when it
// was posted.
type resolveResume struct {
	id    int64
	level int
	at    time.Time
}

// resolveEndsHold is SPEC §6.4's one rule for whether the /resume r ends
// the owner hold h on an issue held below the /resume, or on its own
// issue: r was posted on the issue where h's /hold was posted, on an
// issue between that one and the held issue, or on the held issue
// itself, so at h's level or below it, and after the /hold, or, for a
// comment deleted that may have been one, after the deletion. So a
// /resume posted above a /hold never ends it, whichever holds the held
// issue was in when that /hold was posted (#188's decision, round 3): a
// child's own /hold, one that wrote no marker as the child was held
// already (SPEC §6.4) included, ends only by /resume on the child. Both the
// /resume's judgement (heldStackEnds) and the leave path's
// (resolveUnended) judge by it, so they agree.
func resolveEndsHold(h resolveMade, r resolveResume) bool {
	if r.level > h.level {
		return false
	}
	if h.id != 0 {
		return h.id < r.id
	}
	return h.at.Before(r.at)
}

// resolveGoneMay reports whether e, a comment deleted on an ancestor, by
// who's issue, may have been a /hold that keeps an issue held since
// heldAt, when its hold began (resolveGone): deleted during that hold, or
// at any time if that isn't known, by a write collaborator who isn't
// trusted, of a comment by someone else who is trusted, or whom GitHub
// doesn't name, failing closed. Not the App's, which is no /hold, nor the
// deleter's own. The cheap checks come first, so a deletion before the
// hold costs no request.
func (a *Applier) resolveGoneMay(ctx context.Context, who discWho, e github.DeletedComment, heldAt time.Time) (bool, error) {
	if e.At.Before(heldAt) || strings.EqualFold(e.By.Login, e.Author.Login) || state.IsApp(e.Author, a.AppSlug) {
		return false, nil
	}
	w, err := who.writer(ctx, e.By)
	if err != nil || !w {
		return false, err
	}
	if e.Author.Login == "" {
		return true, nil
	}
	return who.trusted(ctx, e.Author)
}

// resolveHoldApplied reports whether the App applied the /hold in comment
// id on an ancestor in state is: a marker on it names it (resolveApplied),
// as when it moved that issue into held, or else the App 👍'd it, as it
// does once it has moved the family (moveFamily), which it also does on
// an issue held already, where it writes no marker (SPEC §6.4). A rejected
// one, 👎'd, holds nothing, nor does one not handled yet, as on an ancestor
// visited after self in the pass: once handled, it holds self by a marker
// if self has left held by then, and else is read here again. Its
// reactions are read only for a /hold with no marker, and only when self
// is about to leave held, or a /resume is judged, so an unchanged pass
// reads none.
func (a *Applier) resolveHoldApplied(ctx context.Context, is state.Issue, id int64) (bool, error) {
	if resolveApplied(is, id) {
		return true, nil
	}
	if resolveRejected(is, id) {
		return false, nil
	}
	mine, err := a.appReactions(ctx, id)
	if err != nil {
		return false, fmt.Errorf("/hold in comment %d: %w", id, err)
	}
	_, acked := mine["+1"]
	return acked, nil
}

// resolveAbove returns the /holds on s's ancestors in this repo that keep
// s, in state is, held, p, (resolveUnended), read up through each parent
// as madeAbove reads them. It reads only when s's discussion is resolved
// with no other hold left, and the record of the discussion's end names
// what it finds, so no other pass reads them again (resolveLeave).
func (a *Applier) resolveAbove(ctx context.Context, s *github.Snapshot, sd Dirt, trust github.Trust, is state.Issue, p resolvePeriod) ([]github.Marker, error) {
	var path []node
	seen := map[int]bool{s.Number: true}
	for x := s; x.Parent != 0 && !seen[x.Parent]; {
		seen[x.Parent] = true
		parent, _, err := a.Client.SubIssue.GetParentIssue(ctx, a.Owner, a.Repo, int64(x.Number))
		if err != nil {
			return nil, fmt.Errorf("parent of %s/%s#%d: %w", a.Owner, a.Repo, x.Number, err)
		}
		if !a.inRepo(parent.GetRepositoryURL()) {
			break
		}
		if x, err = a.Client.Snapshot(ctx, a.Owner, a.Repo, parent.GetNumber()); err != nil {
			return nil, fmt.Errorf("ancestor of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		d, err := a.Dirty(ctx, x, trust)
		if err != nil {
			return nil, fmt.Errorf("ancestor of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		path = append(path, node{s: x, d: d, open: true})
	}
	above, err := a.resolveUnended(ctx, node{s: s, d: sd, open: true}, path, is, p, trust, nil)
	if err != nil {
		return nil, fmt.Errorf("held from above: %w", err)
	}
	return above, nil
}

// resolveHoldsLeft returns the holds on s, in state is, held, that
// resolveLeave judges by, and the hold period p it's in: those its markers
// record, and the owner holds no marker records (resolveOwnHolds), less
// the owner holds a /resume ended while the issue stayed held
// (resolveLeft). The hold item says from it whether an owner hold stays
// (Step.holdItemFind).
func (a *Applier) resolveHoldsLeft(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue) ([]github.Marker, resolvePeriod, error) {
	p := resolvePeriodOf(s, a.AppSlug, d.Has)
	own, err := a.resolveOwnHolds(ctx, s, d, trust, is, p, 0)
	if err != nil {
		return nil, p, fmt.Errorf("holds left: %w", err)
	}
	return resolveLeft(append(slices.Clone(is.HeldStack.Holds), own...), p, 0), p, nil
}

// resolveOwnerHeld reports whether any of holds isn't a discussion hold:
// an owner hold, by /hold or by hand, or any other.
func resolveOwnerHeld(holds []github.Marker) bool {
	return slices.ContainsFunc(holds, func(m github.Marker) bool { return m.Fields["reason"] != "discussion" })
}

// resolveHeld is discHeldStack for /resume (commands.go): is, the state of
// s, with its open discussion triggers among its holds, and the owner
// holds no marker records, its own (resolveOwnHolds) and extra, those from
// the issues between it and c's (resolveHeldBy), which heldStackEnds
// judges by SPEC §6.4's one rule (resolveEndsHold), so on a descendant,
// not self, c never ends a /hold posted on it or between; without the
// owner holds a /resume ended while the issue stayed held (resolveLeft),
// but for those the /resume in comment c ended itself; on a descendant,
// without the discussion holds whose triggers are resolved. A discussion
// hold whose trigger was made at or after c stays (resolveStays): c
// accepts only the
// triggers made before it, so a later one keeps the issue held, or holds
// it again once out. after reports whether the issue goes to clarifying
// once c ends every hold on it: a discussion ended
// on it while an owner hold stayed (SPEC §6.4). That's so once the App
// recorded it (resolveLeave), or else when, during the hold and before
// the /resume, a writer's trigger was resolved, as when the /resume came
// before the App's pass that would record it, or while another trigger
// was still open, whatever holds the issue besides (#188's note for #189):
// an owner hold that shows on it, a /hold on an ancestor that writes no
// marker here (SPEC §6.4), or none. So the interviewer reads what a code
// owner approved or countered (#189). An act at or after the /resume
// doesn't count: the triggers still open then, it accepts itself, and with
// no act before it restores at. Nor does an issue held from intake go to
// clarifying, which would bring it into the pipeline without the approval
// SPEC §6.2 asks for (resolveIntake).
func (a *Applier) resolveHeld(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue, c github.Comment, self bool,
	extra []github.Marker) (state.Issue, bool, error) {
	is, rs, err := a.discHeldStack(ctx, s, d, trust, is)
	if err != nil {
		return is, false, fmt.Errorf("resolved: %w", err)
	}
	if is.Stage != state.Held {
		return is, false, nil
	}
	p := resolvePeriodOf(s, a.AppSlug, d.Has)
	own, err := a.resolveOwnHolds(ctx, s, d, trust, is, p, c.ID)
	if err != nil {
		return is, false, fmt.Errorf("resolved: %w", err)
	}
	after := map[string]bool{} // the open triggers, true for one made at or after c
	for _, tr := range rs.open {
		after[tr.Key] = after[tr.Key] || !tr.At.Before(c.CreatedAt)
	}
	// On a descendant, a discussion hold whose trigger is resolved holds
	// nothing any more, so an ancestor's /resume that ends the rest takes
	// the issue out, to clarifying (after). On c's own issue it's one more
	// hold c ends, back to at, as /resume restores at.
	holds := slices.DeleteFunc(slices.Clone(is.HeldStack.Holds), func(m github.Marker) bool {
		_, open := after[m.Fields["event"]]
		return !self && m.Fields["reason"] == "discussion" && !open
	})
	holds = append(append(holds, own...), extra...)
	for i, m := range holds {
		if m.Fields["reason"] == "discussion" && after[m.Fields["event"]] {
			f := maps.Clone(m.Fields)
			f[resolveStays] = "1"
			holds[i] = github.Marker{Kind: m.Kind, Fields: f}
		}
	}
	is.HeldStack.Holds = resolveLeft(holds, p, c.ID)
	// Whether an owner hold shows on the issue doesn't matter (#188's note
	// for #189): a /hold on an ancestor, which writes no marker here (SPEC
	// §6.4), looks the same as none, so both go to clarifying once a
	// trigger was resolved during the hold, and the interviewer reads what
	// it approved.
	if resolveIntake(is.HeldStack) || p.ended {
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
// discussion (SPEC §6.4, Leaving the hold): at, where the first hold was,
// and origin, where the issue was before its first discussion hold (SPEC
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

// resolveHeldAfterHold returns the origin of the clarifying after a hold
// that is, held, was at when its hold began, if it was at one: the origin
// the transition before the move into held carries (state.Issue.AfterHold).
// A /resume back there carries it forward (resume). A held → held marker
// with no hold before it, as discRestore's when an edit or a deletion hid
// the move into held, is that move.
func resolveHeldAfterHold(is state.Issue) (state.Stage, bool) {
	var before, last github.Marker
	held := false
	for _, m := range is.Markers {
		if _, bounds := state.RoundBound(m, 0); !bounds {
			continue
		}
		switch {
		case state.Stage(m.Fields["to"]) != state.Held:
			last, held = m, false
		case !held || state.Stage(m.Fields["from"]) != state.Held:
			before, held = last, true
		}
	}
	o := state.Stage(before.Fields["origin"])
	return o, state.Stage(before.Fields["to"]) == state.Clarifying && o.Open() && o != state.Held
}

// resolveIntake reports whether h, the holds on an issue, began at intake:
// such an issue isn't in the pipeline yet, so the end of a discussion on
// it never sends it to clarifying, which would admit it without the
// approval SPEC §6.2 asks for. It stays held until /resume restores intake.
func resolveIntake(h state.HeldStack) bool { return h.At == state.Intake }

// resolveWait is what the move to clarifying after a hold says of what
// comes next on an issue the interviewer doesn't run on yet: it waits
// there (SPEC §6.4, Leaving the hold; ivHoldWait).
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
//     held → clarifying (SPEC §6.4), keyed event=h<the comment of
//     the move that began the hold>, unique to the hold, with
//     resolveFields: resolved is the latest act's time, which bounds the
//     triggers from then on (discFloor). The comment says what comes next
//     (ivHoldWait), and ends in cc @<notify>.
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
// Transition, then only logged, and the ancestors aren't read for it.
//
// When the last trigger goes with no act to resolve it, as a comment its
// author deleted that no marker records, the issue still goes to
// clarifying here, saying the discussion is resolved; but where an
// ancestor's /hold keeps it held, its record says no discussion ended, and
// that ancestor's /resume sends it back to at (resolveEnded,
// resolvePeriodOf). SPEC §6.4 states both ("With no act").
func (a *Applier) resolveLeave(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust) error {
	is := state.Derive(s, a.AppSlug, d.Has)
	if is.Stage != state.Held {
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
	holds, p, err := a.resolveHoldsLeft(ctx, s, d, trust, is)
	if err != nil {
		return fmt.Errorf("leave: %w", err)
	}
	if resolveIntake(is.HeldStack) || resolveOwnerHeld(holds) {
		return a.resolveEnded(ctx, s, rs, p, resolveIntake(is.HeldStack), nil)
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
	fields := resolveFields(h, last)
	text += fmt.Sprintf(", so this issue leaves its hold from `%s` for `clarifying`. %s", h.At, ivHoldWait(is.Item, state.Stage(fields["origin"])))
	m := Move{To: state.Clarifying, Trigger: state.TriggerDiscussionResolved, Key: state.Key{Field: "event", ID: "h" + strconv.FormatInt(p.start, 10)},
		Fields: fields, Text: discCC(text, a.Notify)}
	// About to leave: a /hold on an ancestor posted while it was held, which
	// no marker here records, keeps it held (SPEC §6.4). The ancestors
	// are read only when the move can be made: one the table lacks, as for a
	// type the pipeline doesn't know, writes nothing that would stop the
	// next pass reading them again, so Transition only reports it, once,
	// and returns any other error.
	if _, err := noRow(is, m); err == nil {
		above, err := a.resolveAbove(ctx, s, d, trust, is, p)
		if err != nil {
			return fmt.Errorf("leave: %w", err)
		}
		if len(above) > 0 {
			return a.resolveEnded(ctx, s, rs, p, false, above)
		}
	}
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
// an owner hold that stays, or from intake, or by above, /holds on
// ancestors and comments deleted on them that may have been /holds
// (resolveAbove, resolveGone), if a writer's trigger was resolved during
// the hold, p, since the latest such record (resolveLeave). Its time is
// the latest of those resolutions, and its held field names the
// ancestors' /holds, and its gone field the deleted comments, so no later
// pass reads them again: their /resume ends them (resolveHeld). An
// ancestor's /hold is read only once the discussion is resolved with no
// other hold left. A record for a hold no record names yet is written
// even with no new resolution, as when the last trigger went with no act
// to resolve it, a comment its author deleted: then it has no resolved
// time, and says no discussion ended (resolvePeriodOf). If the /hold it
// names is deleted, its heldon= entry still names the issue it counted as
// posted on, so /resume there, or between, ends it (madeAboveSet.made); a
// record from before heldon=, with none, leaves it to where a marker on
// that ancestor, or above it, names it (madeAbove), else to /resume here. Nor does
// "`/resume` there" reach the issue once it's unlinked from that ancestor
// since: the record's words are as they stood when it was written, and
// /resume here still ends the /hold.
func (a *Applier) resolveEnded(ctx context.Context, s *github.Snapshot, rs *resolver, p resolvePeriod, intake bool, above []github.Marker) error {
	var held, heldOn, gone, on []string
	news := false // above names a hold no record names yet
	for _, m := range above {
		if v := m.Fields[resolveGone]; v != "" {
			news = news || !slices.Contains(p.gone, v)
			gone = append(gone, v)
			if number, _, ok := resolveGoneParse(v); ok && !slices.Contains(on, fmt.Sprintf("#%d", number)) {
				on = append(on, fmt.Sprintf("#%d", number))
			}
			continue
		}
		id, _ := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment()
		news = news || !slices.Contains(p.held, id)
		held = append(held, m.Fields["cmd"])
		heldOn = append(heldOn, m.Fields[resolveOn])
	}
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
	if last.IsZero() && !news {
		return nil
	}
	fields := map[string]string{"reason": "discussion"}
	if !last.IsZero() {
		fields[resolveField] = strconv.FormatInt(last.Unix(), 10)
	}
	if len(held) > 0 {
		fields["held"] = strings.Join(held, ",")
		if !slices.Contains(heldOn, "") {
			fields[resolveHeldOn] = strings.Join(heldOn, ",")
		}
	}
	if len(gone) > 0 {
		fields[resolveGone] = strings.Join(gone, ",")
	}
	marker, err := github.FormatMarker(github.Marker{Kind: resolveKind, Fields: fields})
	if err != nil {
		return fmt.Errorf("record the end: %w", err)
	}
	text := fmt.Sprintf("The discussion is resolved, by %s, but this issue stays on hold: an owner hold on it hasn't ended. "+
		"`/resume` here ends that, and then it goes to `clarifying`.", list(by))
	if len(held)+len(gone) > 0 {
		// What holds it from above: /holds, and comments deleted during
		// the hold that may have been /holds (resolveGone).
		var what []string
		if len(held) > 0 {
			what = append(what, fmt.Sprintf("a `/hold` on an issue above it, in comment %s, hasn't ended", heldWordingHolds(held, heldOn)))
		}
		if len(gone) > 0 {
			what = append(what, fmt.Sprintf("a write collaborator deleted a comment on %s during this hold, which may have been a `/hold`, "+
				"so it counts as one that hasn't ended", list(on)))
		}
		text = fmt.Sprintf("The discussion is resolved, but this issue stays on hold: %s. "+
			"`/resume` there, or here, ends that, and then it goes to `clarifying`.", strings.Join(what, "; and "))
		if last.IsZero() {
			text = fmt.Sprintf("No discussion trigger is open on this issue, but it stays on hold: %s. "+
				"`/resume` there, or here, ends that.", strings.Join(what, "; and "))
		}
	}
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

// heldWordingHolds names the /holds that the held= record's text names
// (resolveEnded): each by its comment, in held, and the issue it counted
// as posted on, in heldOn, in the same order, as "11 on #2", so that
// "`/resume` there" names an issue a reader can find once the comment is
// deleted (#222). One whose issue isn't known, or isn't a number, is named
// by its comment alone, as before heldon=.
func heldWordingHolds(held, heldOn []string) string {
	names := make([]string, len(held))
	for i, id := range held {
		names[i] = id
		if i >= len(heldOn) {
			continue
		}
		if on, err := strconv.Atoi(heldOn[i]); err == nil && on > 0 {
			names[i] = fmt.Sprintf("%s on #%d", id, on)
		}
	}
	return list(names)
}

// resolveResumed records on n, an issue in c's family in state is
// (resolveHeld), self if it's c's own, that c, a /resume, ended holds on it
// while it stays held, for discussion, or, on a descendant, by an owner
// hold c doesn't end, or, on any of them, by a /hold posted after c that
// the App applied first, as on an ancestor whose pass came first (#205),
// so that no later pass counts them (resolveLeft): an App comment with
// one "ended reason=owner key=<resolveHoldKey> cmd=<c's id>" marker
// (resolveKind) for each owner hold c ends, as heldStackEnds says, once
// each. An ended hold stays ended so, whatever comes after (SPEC §6.4):
// once its /hold's comment is deleted, it would count as posted on the
// farthest issue with its marker, which may be below c's issue, and a
// held= record naming it would say "`/resume` there" of an issue whose
// /resume no longer ends it. On c's
// own issue, it also writes an "ended
// reason=discussion resolved=<c's time> cmd=<c's id>" marker, as c
// accepts every trigger made before it, which it bounds (discFloor). It
// says a discussion ended, for the next /resume (resolveHeld), only with
// after=1, if c itself would have gone to clarifying (after). So on
// a descendant, once the discussion is resolved, no owner hold is left,
// and the issue goes to clarifying (resolveLeave), with no other /resume
// (#187's verify note). On c's own issue, it stays held for the triggers
// made at or after c (resolveStays), and only for those. Its cmd makes it
// c's marker on n, so it's written once, and a replay of c counts it as
// begun.
func (a *Applier) resolveResumed(ctx context.Context, c Cmd, n node, is state.Issue, above madeAboveSet, after bool) error {
	self := n.s == c.S
	_, ended, left := heldStackResume(is, self, above)
	if ended == 0 || len(left) == 0 || resolveApplied(state.Derive(n.s, a.AppSlug, n.d.Has), c.Comment.ID) {
		return nil
	}
	cmd := state.CmdKey(c.Comment.ID).ID
	var records []github.Marker
	var keys []string // each recorded once, though two judgements found it
	for _, m := range is.HeldStack.Holds {
		if key := resolveHoldKey(m); m.Fields["reason"] == "owner" && key != "" && !slices.Contains(keys, key) && heldStackEnds(m, self, above) {
			keys = append(keys, key)
			records = append(records, github.Marker{Kind: resolveKind, Fields: map[string]string{"reason": "owner", "key": key, "cmd": cmd}})
		}
	}
	text := fmt.Sprintf("`/resume` on #%d ended the owner hold on this issue, but it stays on hold for discussion "+
		"until a code owner resolves that, or posts `/resume` here.", c.S.Number)
	discussed := slices.ContainsFunc(left, func(m github.Marker) bool { return m.Fields["reason"] == "discussion" })
	if resolveOwnerHeld(left) {
		// A descendant's: c ends every owner hold on its own issue that
		// came before it.
		kinds := "one set by hand, a `/hold` below #%d, or a deleted comment that was, or may have been, a `/hold`"
		if heldStackLater(left, above) {
			// One the App applied before c, as on an ancestor whose pass
			// came first (#205).
			kinds = "one set by hand, a `/hold` below #%d, a `/hold` posted after this `/resume`, or a deleted comment that was, or may have been, a `/hold`"
		}
		text = fmt.Sprintf("`/resume` on #%d ended an owner hold on this issue, but it stays on hold by one that `/resume` doesn't end: "+
			kinds+". `/resume` here ends it.", c.S.Number, c.S.Number)
		if discussed {
			text += " It's on hold for discussion too."
		}
	}
	if self {
		fields := map[string]string{"reason": "discussion", resolveField: strconv.FormatInt(c.Comment.CreatedAt.Unix(), 10), "cmd": cmd}
		if after {
			fields["after"] = "1"
		}
		records = append(records, github.Marker{Kind: resolveKind, Fields: fields})
		text = "This `/resume` ended the holds on this issue from before it, but it stays on hold for discussion: " +
			"a write collaborator acted after the `/resume` was posted. A code owner's response to that, or another `/resume` here, ends it."
		if resolveOwnerHeld(left) {
			// Only a /hold posted after c stays (heldStackEnds), one the
			// App applied first, as on an ancestor whose pass came first
			// (#205), or a comment deleted above after c that may have
			// been one (heldDeletionLater, #244).
			later := slices.ContainsFunc(left, func(m github.Marker) bool { return m.Fields["cmd"] != "" })
			gone := slices.ContainsFunc(left, func(m github.Marker) bool { return heldDeletionLater(m, above.resume) })
			switch {
			case gone && later:
				text = "This `/resume` ended the holds on this issue from before it, but it stays on hold by a `/hold` posted after it, " +
					"which it doesn't end, and a comment deleted after it, on an issue above, may have been another. " +
					"`/resume` where each was posted or deleted, or here, ends that."
			case gone:
				text = "This `/resume` ended the holds on this issue from before it, but it stays on hold: a comment deleted after it, " +
					"on an issue above, may have been a `/hold`, which it doesn't end. `/resume` where that comment was deleted, or here, ends that."
			default:
				text = "This `/resume` ended the holds on this issue from before it, but it stays on hold by a `/hold` posted after it, " +
					"which it doesn't end. `/resume` where that `/hold` was posted, or here, ends that."
			}
			if discussed {
				text += " It's on hold for discussion too: a write collaborator acted after the `/resume` was posted."
			}
		}
	}
	var markers []string
	for _, m := range records {
		f, err := github.FormatMarker(m)
		if err != nil {
			return fmt.Errorf("record the end of a hold on #%d: %w", n.s.Number, err)
		}
		markers = append(markers, f)
	}
	if err := a.comment(ctx, n.s, join(github.Escape(text), strings.Join(markers, "\n"))); err != nil {
		return fmt.Errorf("record the end of a hold on #%d: %w", n.s.Number, err)
	}
	logr.Infof("%s/%s#%d: /resume in comment %d ended holds before it on #%d, which stays held", a.Owner, a.Repo, c.S.Number, c.Comment.ID, n.s.Number)
	return nil
}

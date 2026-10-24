package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// PostRevision posts the next revision comment for s's spec, its body (SPEC
// §7.4): one more than the latest spec revision (state.LatestRevision), with
// the body's hash, how to approve it, and a revision marker. Plan revisions
// number on their own, so they don't count. m.Text, escaped, goes first,
// and the cc line for notify, if any, last, after the revision, as in the
// App's other comments (SPEC §9). dirty is as for state.Derive.
//   - With m.To set, the revision comes with that move, in one comment
//     through Transition, whose marker also gets rev and body. It is posted
//     whatever the hash: a question answered is a new revision even if
//     nothing changed (SPEC §7.4), and the earlier revision's 👍s mustn't
//     approve it. Once the move's marker exists, Transition only finishes it.
//   - With no m.To, it posts the revision on its own, unless the latest
//     revision has the body's hash already and isn't Hidden. Its marker
//     gets seen (newestRead), so a comment posted after s was read can
//     still be a revision request on it (requested). With a move, the
//     transition's seen, if the move has one, does that.
//
// A dirty body (#91) gets no revision, since nothing may approve it. A move
// still happens, saying why there's no revision, then the cc line, so the
// issue doesn't wait on a stage run that can't succeed.
func (a *Applier) PostRevision(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move, notify []string) error {
	if m.To != "" && state.Derive(s, a.AppSlug, dirty).Moved(m.Key) {
		return a.Transition(ctx, s, dirty, m)
	}
	if dirty(state.BodyID) {
		logr.Debugf("%s/%s#%d: no revision: the body has an unsanctioned edit", a.Owner, a.Repo, s.Number)
		if m.To == "" {
			return nil
		}
		m.Text = discCC(join(m.Text, "There's no revision to approve: the issue description was edited without approval (see the note on it)."), notify)
		return a.Transition(ctx, s, dirty, m)
	}
	latest, _ := state.LatestRevision(s, a.AppSlug, state.SpecRevision, dirty)
	hash := state.BodyHash(s.Body)
	if m.To == "" && latest.Rev > 0 && latest.Hash == hash && !latest.Hidden {
		return nil
	}
	rev := strconv.Itoa(latest.Rev + 1)
	marker := github.Marker{Kind: "revision", Fields: map[string]string{"rev": rev, "body": hash}}
	text := discCC(join(m.Text, fmt.Sprintf("**Revision %s** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.", rev, hash)), notify)
	logr.Infof("%s/%s#%d: posting revision %s, %s", a.Owner, a.Repo, s.Number, rev, hash)
	if m.To != "" {
		m.Fields = maps.Clone(m.Fields)
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields["rev"], m.Fields["body"] = rev, hash
		m.Text, m.Markers = text, append(m.Markers[:len(m.Markers):len(m.Markers)], marker)
		return a.Transition(ctx, s, dirty, m)
	}
	floor := max(latest.Comment, state.Derive(s, a.AppSlug, dirty).Round)
	if seen := newestRead(s, a.AppSlug, floor); seen > 0 {
		marker.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	f, err := github.FormatMarker(marker)
	if err != nil {
		return fmt.Errorf("post revision %s on %s/%s#%d: %w", rev, a.Owner, a.Repo, s.Number, err)
	}
	if err := a.comment(ctx, s, join(github.Escape(text), f)); err != nil {
		return fmt.Errorf("post revision %s: %w", rev, err)
	}
	return nil
}

// ApproveCommand is the /approve handler (SPEC §9.2). At spec-review, a
// trusted /approve posted after the latest revision comment approves that
// revision while the body still has its hash and isn't dirty (SPEC §7.4):
// Apply moves the issue to planning with cmd=<comment id>, rev, body and
// seen=<comment id>, so a comment posted after the /approve is planning's
// input (state.Inputs), then the 👍. At plan-review, one posted after the
// latest plan revision comment approves that plan revision while
// state.PlanApproval gives no reason it can't be (SPEC §7.4, A plan that
// nothing approves, and The body is edited at plan-review): Apply moves
// the issue to planned the same way, with the plan's hash as body. Check
// rejects it otherwise, with the reason, and when the table has no such
// move for the issue's class (SPEC §7.2). At spec-review, a rejection for
// a dirty body or a stale revision says whether a revision follows, and
// what the owner can do (ivWordingStale, #229), so none of them asks for a
// new comment.
func ApproveCommand() Handler {
	return Handler{
		Check: func(ctx context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is planning or planned,
			// so Step rejects or finishes the command before it asks Check.
			is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
			if is.Stage == state.PlanReview {
				latest, why := state.PlanApproval(c.S, a.AppSlug, c.Dirt.Has)
				switch {
				case len(why) > 0:
					return planRejection(latest, why[0], c.Dirt)
				case c.Comment.ID < latest.Comment:
					return Rejection{Reason: fmt.Sprintf("it was posted before plan revision %d, the latest. "+
						"To approve that, react 👍 to it, or post `/approve` again", latest.Rev), Final: true}
				}
				return inTable(is, state.Planned, state.TriggerPlanApproved, "on approval")
			}
			latest, ok := state.LatestRevision(c.S, a.AppSlug, state.SpecRevision, c.Dirt.Has)
			if c.Dirt.Has(state.BodyID) {
				return Rejection{Reason: fmt.Sprintf("the issue description has an edit by %s that wasn't approved, so I can't approve it, "+
					"and I won't post a revision of it. %s", c.Dirt[state.BodyID], ivWordingRespec), Final: true}
			}
			// ReviewVisit posts the next revision of a stale spec after this,
			// unless the body's last editor isn't trusted (SPEC §7.4), so a
			// stale spec's reason says which (#229).
			stale := func() error {
				by, follows, err := ivWordingBodyEditor(ctx, c.S, a.AppSlug, c.Trust)
				if err != nil {
					return fmt.Errorf("approve: %w", err)
				}
				return ivWordingStale(latest, ok, by, follows)
			}
			// A stale revision can't be approved, so the reply says so
			// before it would say to approve that revision (#399).
			switch {
			case !ok, latest.Hidden, state.BodyHash(c.S.Body) != latest.Hash:
				return stale()
			case c.Comment.ID < latest.Comment:
				return Rejection{Reason: fmt.Sprintf("it was posted before revision %d, the latest. To approve that, react 👍 to it, or post `/approve` again", latest.Rev), Final: true}
			}
			return inTable(is, state.Planning, state.TriggerSpecApproved, "on approval")
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			stage := state.Derive(c.S, a.AppSlug, c.Dirt.Has).Stage
			return a.approve(ctx, c.S, c.Dirt, stage, state.CmdKey(c.Comment.ID), c.Comment.Author.Login+"'s `/approve`")
		},
	}
}

// ivWordingRespec is how to get a spec that replaces an edit no revision
// follows (#229): a revision request sends the spec back (SPEC §7.4), and
// the interviewer's next spec rewrites the body whole.
const ivWordingRespec = "To get a new spec, which replaces the edit, post `/revise <notes>`, or any other comment"

// ivWordingStale is /approve's Rejection at spec-review when the latest
// spec revision, latest, is stale: missing (ok false), Hidden, or no
// longer matching the body's hash. by last wrote the body, and follows
// says whether ReviewVisit posts the next revision (ivWordingBodyEditor).
// It says so either way (#229), and is Final: a new /approve now would be
// rejected the same way. Where a revision follows, the owner approves
// that; where none does, the owner edits the description, which posts
// one, or sends the spec back.
func ivWordingStale(latest state.Revision, ok bool, by github.Author, follows bool) Rejection {
	var why string
	switch {
	case !ok && follows:
		why = "there's no revision to approve yet"
	case !ok:
		why = "there's no revision to approve"
	case latest.Hidden:
		why = fmt.Sprintf("a comment of mine after revision %d was edited without approval or deleted, so it may hide a newer revision", latest.Rev)
		if follows {
			return Rejection{Reason: why + ". Approve the next revision I post instead", Final: true}
		}
	default:
		why = fmt.Sprintf("the issue description changed after revision %d, so it no longer matches that revision's hash, `%s`", latest.Rev, latest.Hash)
	}
	if follows {
		return Rejection{Reason: why + ". I'll post a new revision next. To approve it, react 👍 to it, or post `/approve` after it", Final: true}
	}
	return Rejection{Reason: fmt.Sprintf("%s. The description was last edited by %s, who isn't trusted, so I won't post a revision of it. "+
		"To get one, edit the description yourself, or post `/revise <notes>`, or any other comment, for a new spec", why, login(by)), Final: true}
}

// planRejection is /approve's Rejection at plan-review for why, a reason
// state.PlanApproval gives that latest can't be approved, worded like the
// spec's. The daemon posts no plan revision of its own (SPEC §7.4), so
// where only a new plan helps, the reason says how to get one, and is Final.
// PlanNoRevision may mean the only plan comment was made dirty or deleted,
// not that no plan was ever posted, so it doesn't say "yet". A dirty body
// stays dirty through a new plan, so its reason is Final too, and says
// that a new plan won't help (#229).
func planRejection(latest state.Revision, why state.PlanReason, d Dirt) Rejection {
	const replan = "To get a new plan, post `/revise`, or any other comment"
	switch why {
	case state.PlanNoRevision:
		return Rejection{Reason: "there's no clean plan revision to approve. " + replan, Final: true}
	case state.PlanHidden:
		return Rejection{Reason: fmt.Sprintf("a comment of mine after plan revision %d was edited without approval or deleted, "+
			"so it may hide a newer plan revision. %s", latest.Rev, replan), Final: true}
	case state.PlanChanged:
		return Rejection{Reason: fmt.Sprintf("the comment of plan revision %d was edited after I posted it, "+
			"so it no longer matches that revision's hash, `%s`. %s", latest.Rev, latest.Hash, replan), Final: true}
	case state.PlanBodyDirty:
		// Nothing at planning or plan-review rewrites the body (SPEC §7.4),
		// so a new comment, or a new plan, wouldn't help (#229).
		return Rejection{Reason: fmt.Sprintf("the issue description has an edit by %s that wasn't approved, so I can't approve the plan. "+
			"Nothing at `planning` or `plan-review` rewrites the description, so sending the plan back won't clear the edit. "+
			"To move on, set the stage by hand", d[state.BodyID]), Final: true}
	}
	return Rejection{Reason: fmt.Sprintf("plan revision %d can't be approved (reason %d)", latest.Rev, why)}
}

// ApproveByReaction returns the reaction trigger for Step.Reactions that
// approves a spec or a plan by 👍 (SPEC §7.4). At spec-review, the earliest
// 👍 on the latest spec revision comment from a user trust trusts, not a
// bot, approves it while the body still has its hash: the issue moves to
// planning with cmd=r<reaction id>, rev and body. At plan-review, the same
// on the latest plan revision comment approves it while state.PlanApproval
// gives no reason it can't be: the issue moves to planned, with the plan's
// hash as body. Until then, its reactions aren't read. Other 👍s, the App's,
// those on an earlier revision and those on a revision of the other kind
// included, are ignored. At any other stage it reads nothing, so removing
// the 👍 once the approval is applied changes nothing. A 👍 the table can't
// apply is reported once by Transition (SPEC §5.3), then only logged. An
// issue of a type ClassOf doesn't know is only logged: the §4 type error
// will report it.
func ApproveByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		is := state.Derive(s, a.AppSlug, d.Has)
		var latest state.Revision
		switch is.Stage {
		case state.SpecReview:
			var ok bool
			if latest, ok = state.LatestRevision(s, a.AppSlug, state.SpecRevision, d.Has); !ok {
				return nil
			}
		case state.PlanReview:
			var why []state.PlanReason
			if latest, why = state.PlanApproval(s, a.AppSlug, d.Has); len(why) > 0 {
				logr.Debugf("%s/%s#%d: no approval of the plan by 👍: %s", a.Owner, a.Repo, s.Number, planRejection(latest, why[0], d).Reason)
				return nil
			}
		default:
			return nil
		}
		if _, err := state.ClassOf(is.Item); err != nil {
			logr.Debugf("%s/%s#%d: no approval by 👍: %v", a.Owner, a.Repo, s.Number, err)
			return nil
		}
		reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, latest.Comment)
		if err != nil {
			return fmt.Errorf("approvals on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		var first *github.Reaction
		for _, r := range reactions {
			if r.Content != "+1" || r.User.Type == "Bot" || (first != nil && first.ID < r.ID) {
				continue
			}
			t, err := trust.Trusted(ctx, r.User.Login)
			if err != nil {
				return fmt.Errorf("approvals on %s/%s#%d: trust %s: %w", a.Owner, a.Repo, s.Number, r.User.Login, err)
			}
			if t {
				first = &r
			}
		}
		if first == nil {
			return nil
		}
		err = a.approve(ctx, s, d, is.Stage, state.ReactionKey(first.ID), first.User.Login+"'s 👍")
		if errors.Is(err, state.ErrNoTransition) {
			logr.Debugf("%s/%s#%d: ignoring the 👍 on %s revision %d: %v", a.Owner, a.Repo, s.Number, is.Stage, latest.Rev, err)
			return nil
		}
		return err
	}
}

// approve moves s on key from stage, spec-review or plan-review, approving
// the latest revision of the stage's kind. by says whose approval it is.
//   - From spec-review, to planning, if the body isn't dirty and still has
//     the revision's hash, and the revision isn't Hidden.
//   - From plan-review, to planned, if state.PlanApproval gives no reason
//     it can't be.
//
// The move's rev and body are the revision's number and hash, which
// state.ApprovedPlan reads back for a plan. Its seen is a comment key's ID,
// else, for a 👍, the newest comment read (newestRead), so a comment posted
// after s was read is the next stage's input. That swallows no revision
// request: Step handled every comment in s first, and one it rejected for
// the table or the type would make the approval fail the same way.
func (a *Applier) approve(ctx context.Context, s *github.Snapshot, d Dirt, stage state.Stage, key state.Key, by string) error {
	var latest state.Revision
	var m Move
	if stage == state.PlanReview {
		var why []state.PlanReason
		if latest, why = state.PlanApproval(s, a.AppSlug, d.Has); len(why) > 0 {
			logr.Debugf("%s/%s#%d: not approving the plan by %s: %s", a.Owner, a.Repo, s.Number, by, planRejection(latest, why[0], d).Reason)
			return nil
		}
		m = Move{To: state.Planned, Trigger: state.TriggerPlanApproved, Key: key,
			Fields: map[string]string{"rev": strconv.Itoa(latest.Rev), "body": latest.Hash},
			Text:   fmt.Sprintf("Revision %d of the plan is approved, by %s.", latest.Rev, by)}
	} else {
		if d.Has(state.BodyID) {
			logr.Debugf("%s/%s#%d: not approving by %s: the body has an edit by %s that wasn't approved", a.Owner, a.Repo, s.Number, by, d[state.BodyID])
			return nil
		}
		latest, _ = state.LatestRevision(s, a.AppSlug, state.SpecRevision, d.Has)
		if latest.Hidden {
			logr.Debugf("%s/%s#%d: not approving revision %d by %s: a dirty or deleted comment of the App's after it may hide a newer one", a.Owner, a.Repo, s.Number, latest.Rev, by)
			return nil
		}
		if hash := state.BodyHash(s.Body); hash != latest.Hash {
			logr.Debugf("%s/%s#%d: not approving revision %d by %s: the body's hash is %s, not %s", a.Owner, a.Repo, s.Number, latest.Rev, by, hash, latest.Hash)
			return nil
		}
		m = Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: key,
			Fields: map[string]string{"rev": strconv.Itoa(latest.Rev), "body": latest.Hash},
			Text:   fmt.Sprintf("Revision %d of the spec is approved, by %s.", latest.Rev, by)}
	}
	m.Fields[state.FieldSeen] = key.ID
	if _, ok := key.Comment(); !ok {
		m.Fields[state.FieldSeen] = strconv.FormatInt(newestRead(s, a.AppSlug, latest.Comment), 10)
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	return nil
}

// classWords words the classes of a §7.2 row for inTable's reason, in this
// order. A set is worded as one when the row has all of it.
var classWords = []struct {
	c    state.Class
	word string
}{
	{state.ClassEpic, "an epic"},
	{state.AnyStory, "a story"},
	{state.ClassStory, "a story with a parent"},
	{state.ClassOwnerStory, "a story with no parent"},
	{state.ClassTask, "a task from a plan"},
	{state.ClassOwnerTask, "a task filed by the owner"},
	{state.ClassHumanTask, "a human task"},
	{state.ClassBug, "a bug"},
	{state.ClassUntyped, "an issue with no type"},
	{state.ClassNewBug, "a bug before `ready`"},
	{state.ClassNewOwnerTask, "a task filed by the owner before `ready`"},
}

// rowClasses words the classes the table moves from from to to on trigger
// (SPEC §7.2), as the subject of inTable's reason: "only an epic", "only an
// epic, or a story with no parent,", or "no issue" if it moves none.
func rowClasses(from, to state.Stage, trigger state.Trigger) string {
	var allowed state.Class
	for c := state.Class(1); c&state.All != 0; c <<= 1 {
		if _, err := state.Lookup(from, to, c, trigger); err == nil {
			allowed |= c
		}
	}
	var words []string
	for _, w := range classWords {
		if allowed&w.c == w.c {
			words, allowed = append(words, w.word), allowed&^w.c
		}
	}
	switch len(words) {
	case 0:
		return "no issue"
	case 1:
		return "only " + words[0]
	}
	return "only " + strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1] + ","
}

// inTable returns a Final Rejection unless the table moves an issue like is
// to stage to on trigger (SPEC §7.2). on says when, for the reason, which
// names the classes the table does move so (rowClasses).
func inTable(is state.Issue, to state.Stage, trigger state.Trigger, on string) error {
	class, err := state.ClassOf(is.Item)
	if err != nil {
		return Rejection{Reason: fmt.Sprintf("I can't tell what kind of issue this is: %v", err), Final: true}
	}
	if _, err := state.Lookup(is.Stage, to, class, trigger); errors.Is(err, state.ErrNoTransition) {
		return Rejection{Reason: fmt.Sprintf("%s goes from `%s` to `%s` %s, and this is a %s issue",
			rowClasses(is.Stage, to, trigger), is.Stage, to, on, class), Final: true}
	} else if err != nil {
		return fmt.Errorf("move to %s %s: %w", to, on, err)
	}
	return nil
}

// ReviseCommand is the /revise handler (SPEC §9.2), and Step sends it each
// revision request too (requested). Apply moves the issue with
// cmd=<comment id> and seen=<comment id>, then the 👍:
//   - At spec-review, it sends the spec back, to clarifying. The comment is
//     the notes in state.Inputs, and a comment posted after it is the next
//     round's answer.
//   - At plan-review, it sends the plan back, to planning (SPEC §7.4).
//     The comment is the next planner run's notes in state.Inputs, and a
//     comment posted after it is planning's input too.
//
// Check rejects it when the table has no such move for the issue's class
// (SPEC §7.2).
func ReviseCommand() Handler {
	return Handler{
		Check: func(_ context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is clarifying or
			// planning, so Step rejects or finishes the command before it
			// asks Check.
			is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
			if is.Stage != state.PlanReview {
				return inTable(is, state.Clarifying, state.TriggerSpecRevise, "on a revision request")
			}
			return inTable(is, state.Planning, state.TriggerPlanRevise, "on a revision request")
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			// Apply runs only until its marker exists, so the stage is
			// still the review stage Check passed.
			m := Move{To: state.Clarifying, Trigger: state.TriggerSpecRevise, Key: state.CmdKey(c.Comment.ID),
				Fields: map[string]string{state.FieldSeen: strconv.FormatInt(c.Comment.ID, 10)},
				Text:   fmt.Sprintf("The spec is sent back for revision, with %s's comment as the notes.", c.Comment.Author.Login)}
			if state.Derive(c.S, a.AppSlug, c.Dirt.Has).Stage == state.PlanReview {
				m.To, m.Trigger = state.Planning, state.TriggerPlanRevise
				m.Text = fmt.Sprintf("The plan is sent back for revision, with %s's comment as the notes.", c.Comment.Author.Login)
			}
			if err := a.Transition(ctx, c.S, c.Dirt.Has, m); err != nil {
				return fmt.Errorf("revise: %w", err)
			}
			return nil
		},
	}
}

// requested returns the /revise that comment c, trusted input, stands for
// if it is a revision request (SPEC §7.4), and the review stage it was
// posted at: a comment that isn't a command and that no marker names,
// posted at spec-review after the latest spec revision, or at plan-review
// after the latest plan revision, whatever the stage is now. The stage c
// was posted at is its round's (postedAt), so a remark left at another
// stage never counts. After the revision is after its Seen, if it has one,
// as a comment posted after the App read the issue may come before the
// revision comment; else after its comment. A plan revision's Seen is that
// of the move in its comment.
//
// So a request rejected at a review stage whose reply was lost still gets
// it after the stage moves on (Step.handle). The latest revision is then
// still the one c was posted after: only the App posts one, and only after
// a pass whose Run succeeded, so had the reply. This reads only the markers
// in s, and Step reads the App's reactions only to a request, not to every
// plain comment. At a review stage it handles a request in the pass that
// finds it, after which a reply or a move names it. Once the stage has
// moved on, the requests left are those posted after the latest revision
// that the App never handled, such as one by a user trusted only since: the
// leftovers of the last review visit, which the next revision ends.
func requested(s *github.Snapshot, appSlug string, d Dirt, is state.Issue, c github.Comment) (state.Command, state.Stage, bool) {
	if _, cmd := state.ParseCommand(c.Body); cmd || is.Keys[state.CmdKey(c.ID)] {
		return state.Command{}, "", false
	}
	posted, kind := postedAt(s, appSlug, d, c.ID), state.SpecRevision
	switch posted {
	case state.SpecReview:
	case state.PlanReview:
		kind = state.PlanRevision
	default:
		return state.Command{}, "", false
	}
	latest, _ := state.LatestRevision(s, appSlug, kind, d.Has)
	if c.ID <= cmp.Or(latest.Seen, latest.Comment) {
		return state.Command{}, "", false
	}
	text := strings.TrimSpace(c.Body)
	line, _, _ := strings.Cut(text, "\n")
	return state.Command{Name: "revise", Arg: strings.TrimSpace(line), Text: text}, posted, true
}

// postedAt returns the stage of the round comment id is in (state.RoundBound),
// at which it was posted: the to= of the last transition whose bound is below
// id, or Intake if there is none. A move to held posted after id doesn't
// count, though its bound, the one from before the hold (state.HeldStack),
// is below id: id was posted before the hold. Like state.Derive, it skips
// dirty comments.
func postedAt(s *github.Snapshot, appSlug string, d Dirt, id int64) state.Stage {
	posted := state.Intake
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if b, bounds := state.RoundBound(m, c.ID); bounds && b < id && (c.ID < id || state.Stage(m.Fields["to"]) != state.Held) {
				posted = state.Stage(m.Fields["to"])
			}
		}
	}
	return posted
}

// newestRead returns the newest ID of s's comments by anyone but the App,
// or floor if it is newer: a seen for what a pass read. The App's own
// comments in s may be this pass's writes, posted after a comment s lacks.
func newestRead(s *github.Snapshot, appSlug string, floor int64) int64 {
	for _, c := range s.Comments {
		if !state.IsApp(c.Author, appSlug) && c.ID > floor {
			floor = c.ID
		}
	}
	return floor
}

// ReviewVisit ends a visit to an issue at spec-review: the loop calls it
// after Run succeeds on the issue, and at any other stage it does nothing.
// While the latest spec revision is stale, Hidden or missing (SPEC §7.4), it
// posts the next one (PostRevision), as after the owner edits the body,
// which voids any pending approval: that binds to the latest revision. It
// ends in the cc line for Notify, as every spec does (SPEC §9), so the
// owner hears of a revision after another trusted user's edit too (#452).
// A dirty body, or one last edited by a user Trust doesn't trust, gets
// none, so the mismatch keeps approval blocked; the second is logged here.
func (st *Step) ReviewVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("revision of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	if state.Derive(s, st.AppSlug, d.Has).Stage != state.SpecReview {
		return nil
	}
	latest, ok := state.LatestRevision(s, st.AppSlug, state.SpecRevision, d.Has)
	if stale := !ok || latest.Hidden || latest.Hash != state.BodyHash(s.Body); stale && !d.Has(state.BodyID) {
		by, t, err := ivWordingBodyEditor(ctx, s, st.AppSlug, st.Trust)
		if err != nil {
			return wrap(err)
		}
		if !t {
			logr.Debugf("%s/%s#%d: no revision: the body was last edited by %s, who isn't trusted", st.Owner, st.Repo, s.Number, login(by))
			return nil
		}
	}
	if err := st.PostRevision(ctx, s, d.Has, Move{}, st.Notify); err != nil {
		return wrap(err)
	}
	return nil
}

// ivWordingBodyEditor returns who last wrote s's body, its last editor or
// else its author, and whether ReviewVisit posts a revision for a stale
// spec on it, a body that isn't dirty: only when that's the App, or a user
// trust trusts who isn't a bot (SPEC §7.4). /approve's reply says which
// (#229), so it never promises a revision that won't come.
func ivWordingBodyEditor(ctx context.Context, s *github.Snapshot, appSlug string, trust github.Trust) (github.Author, bool, error) {
	by := s.Author
	if !s.BodyEdit.At.IsZero() {
		by = s.BodyEdit.By
	}
	if state.IsApp(by, appSlug) {
		return by, true, nil
	}
	if by.Type == "Bot" || by.Login == "" {
		return by, false, nil
	}
	t, err := trust.Trusted(ctx, by.Login)
	if err != nil {
		return by, false, fmt.Errorf("trust %s: %w", by.Login, err)
	}
	return by, t, nil
}

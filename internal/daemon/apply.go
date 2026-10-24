// Package daemon runs the pipeline. Every GitHub write that changes an
// issue's state goes through its Applier.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Applier makes the writes that change an issue's state. Each is checked
// before it's made, so a pass that crashed part way can run again (SPEC §5.2).
type Applier struct {
	Client      *github.Client
	Owner, Repo string
	// AppSlug is the App's slug. Its comments are by "<slug>[bot]".
	AppSlug string
	// Touched, if set, is told of each issue that a command on another
	// issue may write to, such as a descendant of a /hold, before it does,
	// and of each issue whose /hold or /resume waits for one on an
	// ancestor (holdOrderWaits), so it's read again next pass.
	Touched func(number int)
	// Unexplained, if set, is the memory of unexplained labels CheckLabels
	// keeps between passes. If nil, the Applier keeps its own.
	Unexplained *github.Unexplained
	// IssueTypes is set on a repo that uses issue types (types:
	// issue-types), so CheckLabels takes the type change that stands from
	// the issue-type events, not the type:* labels (LabelCheck.TypeEvent).
	// Daemon.reload keeps it up to date with the config.
	IssueTypes bool
	// Notify are config's notify users, for the cc line of a discussion
	// hold and its notices (discussion.go). Daemon.reload keeps it up to
	// date with the config.
	Notify []string
	// ReadMain reads a file on main from the daemon's bare clone, as
	// repo.Repo.ReadFile does, for a human task's file check (humanCheck).
	// A missing file is an error matching repo.ErrNotFound, and a
	// directory one matching repo.ErrNotFile.
	ReadMain func(ctx context.Context, path string) ([]byte, error)

	// madeAboveDigest, if set, reports whether issue number is a digest,
	// which the walk up (madeAbove) takes no snapshot of, and step 1 never
	// visits (Daemon.digest), so nothing on it is read, and no /hold or
	// /resume waits for one on it (holdOrderWaits).
	madeAboveDigest func(number int) bool

	histories     histories                // see Dirty
	originNone    map[int]string           // the body, by issue, in which originRepost found no origin to repost
	discNoRecords map[string]*discNoRecord // logins with no permission record, in lower case (discNoRecord)
	unexplained   github.Unexplained
	typeRenames   *github.IssueTypeNames // the org's issue types' names for this pass's reverts, or nil (typeRenamePass)

	doneChecked doneChecked // the /done check last found passing (DoneCommand)

	closingBy    map[string]github.ClosingCloser // how each close was made, by its event's node ID (closingCloser)
	closingAgain map[int]closingListed           // issues the App closed again, after an untrusted reopen, as it left them (closingCloseAgain)
	closerErr    closerErrState                  // closer questions that failed, and the pass (closererr.go)
}

// Move is one transition to apply.
type Move struct {
	To      state.Stage
	Trigger state.Trigger
	// Key is what caused the move: a cmd, run or event. It makes the move
	// idempotent, and a command comment's key also gets the App's 👍.
	Key state.Key
	// Fields are further marker fields, such as rev and body. They can't
	// be from, to or a key's field, but for cmd=<comment id> beside a run
	// key: a command the run handled.
	Fields map[string]string
	// Text goes before the marker. It is escaped (SPEC §5.1), so it may
	// quote agent output or comments.
	Text string
	// NoAck leaves out the 👍 for a command comment's key: for a move a
	// command makes before its own marker, such as on a descendant.
	NoAck bool
	// Markers go after the transition marker, in the same comment, such as
	// a revision marker.
	Markers []github.Marker
	// PlanRev, if set, makes the move's comment plan revision PlanRev, its
	// plan comment (SPEC §7.4, §11.4). The transition marker gets
	// rev, but not body, and the comment's last line, after Markers, is the
	// plan revision marker, revision kind=plan rev=<PlanRev> body=<hash>,
	// with state.PlanHash of the comment: the hash of exactly the text above
	// that line. Fields can't then have rev or body, nor Markers a revision.
	PlanRev int
	// ErrorKey, if set, keys the state error for a move the table lacks,
	// instead of Key's ID.
	ErrorKey string
	// DiscFrom, if set, is the stage the move is from, for its marker and
	// the table, in place of the latest marker's: for a discussion hold
	// that restores the stage a write collaborator's edit or deletion of
	// the App's marker hid (discRestore).
	DiscFrom state.Stage
	// StateReason is the state_reason a move to closed closes the issue
	// with, completed or not_planned, and only such a move has one. Its
	// marker records it as reason=, so a close cut short is finished with
	// it (closeFinish).
	StateReason string
}

// closeReasons are the state reasons a move to closed can close with.
var closeReasons = []string{"completed", "not_planned"}

// errorKey keys the state error for m, if the table lacks it.
func (m Move) errorKey() string {
	if m.ErrorKey != "" {
		return m.ErrorKey
	}
	return m.Key.ID
}

// keyFields are the marker fields a Key names.
var keyFields = []string{"cmd", "run", "event"}

func (m Move) check() error {
	if !slices.Contains(keyFields, m.Key.Field) || m.Key.ID == "" {
		return fmt.Errorf("bad key %+v", m.Key)
	}
	for f, v := range m.Fields {
		// A run's move may also name a command it handled, by cmd=<comment
		// id>, as the interviewer does for a /revise that waited through a
		// hold (SPEC §6.4, Commands that wait): the command is then processed.
		if f == "cmd" && m.Key.Field == "run" {
			if _, ok := (state.Key{Field: "cmd", ID: v}).Comment(); !ok {
				return fmt.Errorf("field cmd=%s names no comment", v)
			}
			continue
		}
		if f == "from" || f == "to" || slices.Contains(keyFields, f) {
			return fmt.Errorf("field %s is set by the move", f)
		}
	}
	switch {
	case m.To == state.Closed:
		if !slices.Contains(closeReasons, m.StateReason) {
			return fmt.Errorf("can't close with state reason %q: it's completed or not_planned", m.StateReason)
		}
		if _, ok := m.Fields["reason"]; ok {
			return errors.New("field reason is set by the move to closed")
		}
	case !m.To.Open():
		return fmt.Errorf("can't move to %s: it's no stage", m.To)
	case m.StateReason != "":
		return fmt.Errorf("state reason %s on a move to %s, which closes nothing", m.StateReason, m.To)
	}
	return m.checkPlanRev()
}

// checkPlanRev refuses a PlanRev that isn't a revision number, or that
// comes with a rev or body field or a revision marker of its own: the plan
// revision sets rev, its marker carries the hash, and a comment is one
// revision.
func (m Move) checkPlanRev() error {
	if m.PlanRev < 0 {
		return fmt.Errorf("bad plan revision %d", m.PlanRev)
	}
	if m.PlanRev == 0 {
		return nil
	}
	for _, f := range []string{"rev", "body"} {
		if _, ok := m.Fields[f]; ok {
			return fmt.Errorf("field %s is set by plan revision %d", f, m.PlanRev)
		}
	}
	for _, x := range m.Markers {
		if x.Kind == "revision" {
			return fmt.Errorf("a revision marker beside plan revision %d", m.PlanRev)
		}
	}
	return nil
}

// Transition moves the issue s to m.To through the §7.2 table. It writes in
// the §5.2 order, skipping each step already done: the marker
// comment, the new stage label, removing the old ones, then the 👍, unless
// m.NoAck. Its writes are added to s. dirty is as for state.Derive.
//
// A move to closed is the only way the App closes an issue through the
// table: a human task on /done (DoneCommand), an epic planned into
// stories once its children have closed (epicCloseOne, #251), and, on
// re-planning (SPEC §6.4), a dropped child, as not planned. Its marker
// records m.StateReason as reason=; then, in place of the label writes,
// comes the 👍, and last the close with that state_reason, unless the issue
// is closed already. The 👍 goes before the close, as step 1 never visits a
// closed issue again to finish it. It writes no stage label: the close
// judge sets the closed stage on a later pass (closingJudge). A crash
// after the marker is finished by the label rules, which close the issue
// as the marker says (closeFinish), and by step 1, which 👍s the command
// its marker names.
//
// The stage labels are set to match the latest marker, so a stage label a
// trusted user set by hand would be overwritten. That's safe only because
// Applier.CheckLabels runs first and records such a change as an override
// (SPEC §5.1): callers run it on a fresh snapshot just before Transition
// (the stage-run harness does). A change made after that check, while no
// other marker lands, is still caught: the next CheckLabels records it as
// an override, as it isn't in the marker this writes. A snapshot older
// than the latest marker isn't safe, so a run's result must re-read the
// issue before its Transition.
//
// A move the table lacks, including one for an issue of unknown type,
// changes no label. It is reported once with an error comment keyed by
// m.ErrorKey or m.Key's ID (SPEC §5.3), logged as an error when that
// comment is posted and at debug level after, and returned as an error
// StateError reports. But when the type is the cause (typeErrStands), as
// for an unknown type or one that can't be at the issue's stage, the
// type error, kind=type keyed by the type change that stands, is posted
// instead, once (typeErrReport), and the move's error is only logged, at
// info level with that post and at debug level after: so is it while that
// type error is reported or acknowledged.
func (a *Applier) Transition(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move) error {
	wrap := func(err error) error {
		return fmt.Errorf("move %s/%s#%d to %s on %s: %w", a.Owner, a.Repo, s.Number, m.To, m.Trigger, err)
	}
	if err := m.check(); err != nil {
		return wrap(err)
	}
	is := state.Derive(s, a.AppSlug, dirty)
	if !is.Moved(m.Key) {
		if m.DiscFrom != "" {
			is.Stage = m.DiscFrom
		}
		if why, err := noRow(is, m); err != nil {
			if !StateError(err) {
				return wrap(err)
			}
			if typeErrStands(s, is) {
				// The type is the cause (SPEC §4): its type error, posted once
				// per type change, says so, and this one isn't posted. It's
				// logged at info level with the type error's post, and at
				// debug level after, as a reported error is (#156's verify).
				log := logr.Infof
				if is.Reported(typeErrKind, typeErrKey(s)) {
					log = logr.Debugf
				}
				if rerr := a.typeErrReport(ctx, s, is); rerr != nil {
					return wrap(rerr)
				}
				log("state error held back, as the type error keyed %s has the same cause: %v", typeErrKey(s), wrap(err))
				return wrap(err)
			}
			if is.Reported("transition", m.errorKey()) {
				logr.Debugf("state error, reported already: %v", wrap(err))
				return wrap(err)
			}
			if rerr := a.report(ctx, s, is, m, why); rerr != nil {
				return wrap(rerr)
			}
			logr.Errorf("state error: %v", wrap(err))
			return wrap(err)
		}
		fields := maps.Clone(m.Fields)
		if fields == nil {
			fields = map[string]string{}
		}
		fields["from"], fields["to"], fields[m.Key.Field] = string(is.Stage), string(m.To), m.Key.ID
		if m.To == state.Closed {
			fields["reason"] = m.StateReason
		}
		if m.PlanRev > 0 {
			fields["rev"] = strconv.Itoa(m.PlanRev)
		}
		marker, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: fields})
		if err != nil {
			return wrap(err)
		}
		for _, x := range m.Markers {
			more, err := github.FormatMarker(x)
			if err != nil {
				return wrap(err)
			}
			marker += "\n" + more
		}
		body := join(github.Escape(m.Text), marker)
		if m.PlanRev > 0 {
			if body, err = planRevision(body, m.PlanRev); err != nil {
				return wrap(err)
			}
		}
		if err := a.comment(ctx, s, body); err != nil {
			return wrap(err)
		}
		is = state.Derive(s, a.AppSlug, dirty)
	}
	// The latest marker's stage: a move written before, and since followed
	// by another, isn't redone.
	if is.Stage != state.Closed {
		if err := a.setStage(ctx, s, is.Stage); err != nil {
			return wrap(err)
		}
	}
	if id, ok := m.Key.Comment(); ok && !m.NoAck {
		if err := a.Ack(ctx, id); err != nil {
			return wrap(err)
		}
	}
	if is.Stage == state.Closed {
		if err := a.closeFinish(ctx, s, is); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// latestTransition returns the transition marker of is that sets its Stage,
// the latest whose to is a stage or closed, as state.Derive reads it, or
// false if there's none.
func latestTransition(is state.Issue) (github.Marker, bool) {
	for i := len(is.Markers) - 1; i >= 0; i-- {
		m := is.Markers[i]
		if to := state.Stage(m.Fields["to"]); m.Kind == "transition" && (to.Open() || to == state.Closed) {
			return m, true
		}
	}
	return github.Marker{}, false
}

// closeFinish closes s, whose latest transition marker in is is a move to
// closed, with the state reason that marker records (Move.StateReason),
// unless s is closed already, and marks s closed. A marker with no valid
// reason, which the App never writes, closes with GitHub's default,
// completed. The closed stage is the close judge's (closingJudge).
func (a *Applier) closeFinish(ctx context.Context, s *github.Snapshot, is state.Issue) error {
	if s.Closed {
		return nil
	}
	req := gogithub.UpdateIssueRequest{State: new("closed")}
	m, _ := latestTransition(is)
	reason := m.Fields["reason"]
	if slices.Contains(closeReasons, reason) {
		req.StateReason = &reason
	} else {
		reason = "completed, GitHub's default"
	}
	if _, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.Number, req); err != nil {
		return fmt.Errorf("close %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	s.Closed = true
	logr.Infof("%s/%s#%d: closed it, as %s, as its latest marker says", a.Owner, a.Repo, s.Number, reason)
	return nil
}

// planRevision appends plan revision rev's marker to body, the plan comment
// as written, as its last line, with the hash of exactly that body, so
// state.PlanHash of the result is the marker's body.
func planRevision(body string, rev int) (string, error) {
	marker, err := github.FormatMarker(github.Marker{Kind: "revision", Fields: map[string]string{
		"kind": string(state.PlanRevision), "rev": strconv.Itoa(rev), "body": state.BodyHash(body)}})
	if err != nil {
		return "", fmt.Errorf("plan revision %d: %w", rev, err)
	}
	return body + "\n" + marker, nil
}

// Ack adds the App's 👍 to a comment, unless it has one already.
func (a *Applier) Ack(ctx context.Context, commentID int64) error {
	mine, err := a.appReactions(ctx, commentID)
	if err != nil {
		return fmt.Errorf("ack comment %d: %w", commentID, err)
	}
	if _, ok := mine["+1"]; ok {
		return nil
	}
	return a.react(ctx, commentID, "+1")
}

// appReactions returns the App's reactions to a comment, by content. Only
// the App's Bot counts.
func (a *Applier) appReactions(ctx context.Context, commentID int64) (map[string]github.Reaction, error) {
	reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, commentID)
	if err != nil {
		return nil, fmt.Errorf("the App's reactions: %w", err)
	}
	mine := map[string]github.Reaction{}
	for _, r := range reactions {
		if state.IsApp(r.User, a.AppSlug) {
			mine[r.Content] = r
		}
	}
	return mine, nil
}

// react adds the App's reaction, such as "+1", to a comment, with no check:
// see Ack.
func (a *Applier) react(ctx context.Context, commentID int64, content string) error {
	if _, _, err := a.Client.Client.Reactions.CreateIssueCommentReaction(ctx, a.Owner, a.Repo, commentID, content); err != nil {
		return fmt.Errorf("react %s to comment %d in %s/%s: %w", content, commentID, a.Owner, a.Repo, err)
	}
	return nil
}

// StateError reports whether err is a state error (SPEC §5.3) from
// Transition: a move the table lacks, or an issue of unknown type.
func StateError(err error) bool {
	return errors.Is(err, state.ErrNoTransition) || errors.Is(err, state.ErrUnknownType)
}

// noRow returns why the table has no row for m on an issue in state is,
// with a StateError, or "" and nil if it has one. Other errors come back
// with no reason.
func noRow(is state.Issue, m Move) (string, error) {
	class, err := state.ClassOf(is.Item)
	if errors.Is(err, state.ErrUnknownType) {
		return fmt.Sprintf("its type `%s` isn't one the pipeline knows (SPEC §4)", is.Item.Type), err
	}
	if err != nil {
		return "", err
	}
	if _, err := state.Lookup(is.Stage, m.To, class, m.Trigger); err != nil {
		if errors.Is(err, state.ErrNoTransition) {
			return fmt.Sprintf("the transition table (SPEC §7.2) has no such row for a %s issue", class), err
		}
		return "", err
	}
	return "", nil
}

// report posts the state error for a move the table lacks, saying why. The
// type in why isn't the App's text, so it's escaped (SPEC §5.1).
func (a *Applier) report(ctx context.Context, s *github.Snapshot, is state.Issue, m Move, why string) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": "transition", "key": m.errorKey()}})
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	text := fmt.Sprintf("I can't move this issue from `%s` to `%s` on %s: %s. It stays at `%s`.", is.Stage, m.To, m.Trigger, why, is.Stage)
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}

// comment posts body on s as the App and adds it to s.
func (a *Applier) comment(ctx context.Context, s *github.Snapshot, body string) error {
	c, _, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, s.Number, gogithub.IssueCommentRequest{Body: body})
	if err != nil {
		return fmt.Errorf("comment on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	s.Comments = append(s.Comments, github.Comment{ID: c.GetID(), Body: c.GetBody(),
		Author:    github.Author{Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()},
		CreatedAt: c.GetCreatedAt().Time, UpdatedAt: c.GetUpdatedAt().Time})
	return nil
}

// setStage makes stage's label the only stage:* label on s: it adds it, then
// removes the others, even one that names no stage. Only those s had before
// the add go: one that the add's answer shows for the first time was set
// since s was read, perhaps by hand, so it's left for the next CheckLabels
// to record as an override or repair. A stage with no label changes nothing.
func (a *Applier) setStage(ctx context.Context, s *github.Snapshot, stage state.Stage) error {
	want, ok := stage.Label()
	if !ok {
		return nil
	}
	known := slices.Clone(s.Labels)
	if err := a.addLabel(ctx, s, want); err != nil {
		return fmt.Errorf("set stage %s: %w", stage, err)
	}
	for _, l := range known {
		if !isStageLabel(l) || strings.EqualFold(l, want) {
			continue
		}
		if err := a.removeLabel(ctx, s, l); err != nil {
			return fmt.Errorf("set stage %s: %w", stage, err)
		}
	}
	return nil
}

// addLabel adds l to s, unless s has it. Like GitHub, it ignores case. The
// labels GitHub answers with replace s's, but for those left out
// (Snapshot.LeftOut). A label that's on the issue but left out, unexplained,
// is first removed, so the add makes the App's own event, which explains it:
// otherwise GitHub would make none, and the label would be removed later as
// unexplained. A crash between the two leaves the label off, which a replay
// adds, as for any label not yet added.
func (a *Applier) addLabel(ctx context.Context, s *github.Snapshot, l string) error {
	if slices.ContainsFunc(s.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
		return nil
	}
	if s.LeftOut[strings.ToLower(l)] {
		resp, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, s.Number, l)
		if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
			return fmt.Errorf("remove %s, unexplained, from %s/%s#%d to add it: %w", l, a.Owner, a.Repo, s.Number, err)
		}
		delete(s.LeftOut, strings.ToLower(l))
		a.seen().ForgetLabel(a.Owner, a.Repo, s.Number, l)
	}
	labels, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, s.Number, []string{l})
	if err != nil {
		return fmt.Errorf("add %s to %s/%s#%d: %w", l, a.Owner, a.Repo, s.Number, err)
	}
	var names []string
	for _, x := range labels {
		names = append(names, x.GetName())
	}
	s.SetLabels(names)
	return nil
}

// removeLabel removes l from s, if s has it. Like GitHub, it ignores case.
// A label gone meanwhile is no error.
func (a *Applier) removeLabel(ctx context.Context, s *github.Snapshot, l string) error {
	is := func(x string) bool { return strings.EqualFold(x, l) }
	if !slices.ContainsFunc(s.Labels, is) {
		return nil
	}
	resp, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, s.Number, l)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("remove %s from %s/%s#%d: %w", l, a.Owner, a.Repo, s.Number, err)
	}
	s.SetLabels(slices.DeleteFunc(slices.Clone(s.Labels), is))
	return nil
}

// join puts text, if any, before a marker.
func join(text, marker string) string {
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}

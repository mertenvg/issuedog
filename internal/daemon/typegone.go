package daemon

import (
	"context"
	"fmt"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A revert of the issue type that would set back a type the org no longer
// lists, as once it deleted it, can't be made (github.ErrIssueTypeGone),
// so CheckLabels writes nothing and leaves the untrusted type out for the
// type that stands (github.IssueTypeLeftOut.Gone), as for a type no event
// explains (#409). That's a state error, kind=typegone, reported once per
// revert, by its key (SPEC §5.3): the daemon reports it instead of the
// revert it can't make, and it ends when its cause is gone, as a type
// error does, when no such revert stands any more, as once a trusted user
// changes the type. So it's open while the cause stands, the digest lists
// it, and the owner is assigned for it.

// typeGoneErrKind is the kind of the state error for a revert of the issue
// type that the org's deleted type blocks.
const typeGoneErrKind = "typegone"

// typeGoneKey keys the typegone error of s: the key of the revert that the
// org's deleted type blocks, as CheckLabels last found it on s, or "" with
// none, so no such error stands (openErrors). It's the revert's own key
// (github.IssueTypeRevert.Key), the untrusted change's event, or the type
// no event explains and the newest event, so a new change is a new
// occurrence.
func typeGoneKey(s *github.Snapshot) string {
	if lo := s.TypeLeftOut; lo != nil && lo.Gone != nil {
		return lo.Gone.Key
	}
	return ""
}

// typeGoneReport posts s's typegone error, if CheckLabels just left its
// type out for a type the org no longer lists, keyed by typeGoneKey, unless
// one was posted or acknowledged (state.Issue.Reported), or s is closed,
// where no state error is open. It is logged as an error when posted, and
// at debug level after, so an unchanged pass writes nothing. dirty is as
// for state.Derive.
func (a *Applier) typeGoneReport(ctx context.Context, s *github.Snapshot, dirty func(int64) bool) error {
	key := typeGoneKey(s)
	if key == "" || s.Closed {
		return nil
	}
	lo := s.TypeLeftOut
	desc := fmt.Sprintf("%s/%s#%d: the issue type %q can't be reverted to %q, which the org no longer lists, keyed %s", a.Owner, a.Repo, s.Number, lo.Name, lo.To, key)
	if state.Derive(s, a.AppSlug, dirty).Reported(typeGoneErrKind, key) {
		logr.Debugf("state error, reported already: %s", desc)
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": typeGoneErrKind, "key": key}})
	if err != nil {
		return fmt.Errorf("report the issue type the org deleted: %w", err)
	}
	if err := a.comment(ctx, s, join(github.Escape(typeGoneText(*lo)), marker)); err != nil {
		return fmt.Errorf("report the issue type the org deleted: %w", err)
	}
	logr.Errorf("state error: %s", desc)
	return nil
}

// typeGoneText says that lo's type, the issue's own, isn't reverted, as the
// org no longer lists the type that stands, lo.To, which the pass acts on
// instead, and that a trusted user's change of the type ends it. The types'
// names and the login aren't the App's text, so each is a code span, which
// reads as it is and mentions no one (checkCode).
func typeGoneText(lo github.IssueTypeLeftOut) string {
	now := "no type"
	if lo.Name != "" {
		now = "the issue type " + checkCode(lo.Name)
	}
	why := fmt.Sprintf("%s changed it, and only trusted users can change the issue type", checkCode(lo.Gone.By))
	if lo.Gone.By == "" {
		why = "no change by a trusted user or the App explains it, and only trusted users can change the issue type"
	}
	return fmt.Sprintf("This issue has %s, but %s. I can't set back the type before it, %s: the org no longer has that type, as once it's deleted. "+
		"So I've left the type as it is, and act on %s as if the issue had it, until a trusted user changes the type. "+
		"A 👍 on this comment acknowledges it.", now, why, checkCode(lo.To), checkCode(lo.To))
}

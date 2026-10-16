package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// IssueTypeRevert is the revert of an untrusted change of the issue type
// (SPEC §5.1), as CheckLabels finds it (LabelCheck.IssueTypeRevert).
type IssueTypeRevert struct {
	// Key is the ID of the change to revert, the newest issue-type event:
	// the revert marker's key.
	Key int64
	// By is the untrusted login that made it.
	By string
	// From is the type it left, and To the type that stands, which the
	// revert sets, each as GitHub names it, or "" for none.
	From, To string
	// Posted is set when the revert comment keyed Key is on the timeline
	// already, as after a pass that crashed before the revert.
	Posted bool
}

// ErrIssueTypeRevertDropped is wrapped by RevertIssueType's error when
// GitHub answers the write with another type than the one it set. GitHub
// drops a type change from anyone without push access, without an error.
var ErrIssueTypeRevertDropped = errors.New("GitHub kept another issue type")

// issueTypeRevertLeft is the type an issue-type event left, as GitHub names
// it: the issue_type of issue_type_added or issue_type_changed, or "" for
// issue_type_removed, or for an event whose type GitHub no longer names,
// such as one the org has since deleted.
func issueTypeRevertLeft(e *issueTypeRevertEvent) string {
	if e.GetEvent() == "issue_type_removed" {
		return ""
	}
	return e.IssueType.GetName()
}

// issueTypeRevertCheck judges the issue type's history l, oldest first, as
// settle judges a pipeline label's. It returns the ID of the newest
// issue-type event that stands (standing), or 0, and, if the newest event
// left another type than the one that stands, the revert to make. The type
// that stands is the one the newest standing event left, or no type if
// none stands. Unlike settle, it needs no fallback to the App's last word:
// with none standing, each of the App's events is a restore (standing), so
// of the type before it, and so all of them restore no type. reverted
// holds the revert keys posted already. It writes nothing.
func (r *labelRules) issueTypeRevertCheck(ctx context.Context, l *labelState, reverted map[string]bool) (int64, *IssueTypeRevert, error) {
	if len(l.events) == 0 {
		return 0, nil, nil
	}
	acc, accAt, err := r.standing(ctx, l, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("check the issue type: %w", err)
	}
	var id int64
	to := ""
	if acc != nil {
		id, to = acc.GetID(), l.types[slices.Index(l.ats, accAt)]
	}
	// The newest event stands, or left the type that stands already.
	last := len(l.events) - 1
	if strings.EqualFold(to, l.types[last]) {
		return id, nil, nil
	}
	latest := l.events[last]
	key := strconv.FormatInt(latest.GetID(), 10)
	return id, &IssueTypeRevert{Key: latest.GetID(), By: latest.GetActor().GetLogin(), From: l.types[last], To: to, Posted: reverted[key]}, nil
}

// RevertIssueType makes rv on owner/repo#number: a comment carrying a
// revert marker keyed rv.Key, unless rv.Posted, then the type set back to
// rv.To, or removed, through REST's update of an issue, PATCH
// /repos/{owner}/{repo}/issues/{issue_number} with type set to the type's
// name, or null to remove it. The comment goes first and only once, so a
// crash between the two writes replays cleanly, as settle's does. GitHub
// answers with the issue, and a type other than rv.To in it is an error
// wrapping ErrIssueTypeRevertDropped. The App's own write then stands on
// the timeline as the App's restore of the type before it, which the next
// CheckLabels doesn't take for a change.
func (c *Client) RevertIssueType(ctx context.Context, owner, repo string, number int, rv IssueTypeRevert) error {
	wrap := func(err error) error {
		return fmt.Errorf("revert %s's change of the issue type on %s/%s#%d: %w", rv.By, owner, repo, number, err)
	}
	if !rv.Posted {
		text := fmt.Sprintf("I restored the issue type `%s`", rv.To)
		if rv.To == "" {
			text = fmt.Sprintf("I removed the issue type `%s`", rv.From)
		}
		marker, err := FormatMarker(Marker{Kind: "revert", Fields: map[string]string{"key": strconv.FormatInt(rv.Key, 10)}})
		if err != nil {
			return wrap(err)
		}
		// The type's name and the login are someone else's text, so a marker
		// in them is escaped (§5.1).
		body := Escape(fmt.Sprintf("%s: only trusted users can change the issue type, and %s isn't one.", text, rv.By)) + "\n\n" + marker
		if _, _, err := c.Issues.CreateComment(ctx, owner, repo, number, gogithub.IssueCommentRequest{Body: body}); err != nil {
			return wrap(fmt.Errorf("comment: %w", err))
		}
	}
	// go-github's UpdateIssueRequest drops a nil type, so null goes as
	// RemoveMilestone sends its milestone.
	var set struct {
		Type *string `json:"type"`
	}
	if rv.To != "" {
		set.Type = &rv.To
	}
	req, err := c.NewRequest(ctx, http.MethodPatch, fmt.Sprintf("repos/%v/%v/issues/%v", owner, repo, number), &set)
	if err != nil {
		return wrap(err)
	}
	var issue gogithub.Issue
	if _, err := c.Do(req, &issue); err != nil {
		return wrap(fmt.Errorf("set the type: %w", err))
	}
	if got := issue.GetType().GetName(); !strings.EqualFold(got, rv.To) {
		return wrap(fmt.Errorf("%w, %q, not %q: the App may lack push access", ErrIssueTypeRevertDropped, got, rv.To))
	}
	logr.Infof("reverted %s's change of the issue type to %q on %s/%s#%d, back to %q", rv.By, rv.From, owner, repo, number, rv.To)
	return nil
}

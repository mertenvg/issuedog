package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// IssueTypeGuard asks CheckLabels to guard the issue type (SPEC §5.1), as
// on a repo that uses issue types (types: issue-types). With none, as on a
// repo that uses labels, CheckLabels leaves issue types alone and asks no
// one's trust for them.
type IssueTypeGuard struct {
	// Type is the issue's type as the caller read it, with the labels it
	// passes: the issue type's name, in any case, or "" for none.
	Type string
}

// IssueTypeRevert is a revert of the issue type that CheckLabels made
// (LabelCheck.IssueTypeRevert), or one for RevertIssueType to make.
type IssueTypeRevert struct {
	// Key is the revert marker's key: the ID of the untrusted change, the
	// newest issue-type event, or for a type no event explains,
	// "issue-type:" and the type's lowercase name, URL-escaped, then "@"
	// and the ID of the newest issue-type event, or 0.
	Key string
	// By is the untrusted login that made the change, or "" for a type no
	// event explains.
	By string
	// From is the type on the issue, and To the type that stands, which the
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

// issueTypeRevertStanding judges the issue type's history l, oldest first,
// as settle judges a pipeline label's. It returns the ID of the newest
// issue-type event that stands (standing), or 0, and the type it left, or
// "" if none stands. Unlike settle, it needs no fallback to the App's last
// word: with none standing, each of the App's events is a restore
// (standing), so of the type before it, and so all of them restore no type.
func (r *labelRules) issueTypeRevertStanding(ctx context.Context, l *labelState) (int64, string, error) {
	acc, accAt, err := r.standing(ctx, l, nil)
	if err != nil {
		return 0, "", fmt.Errorf("the issue type that stands: %w", err)
	}
	if acc == nil {
		return 0, "", nil
	}
	return acc.GetID(), l.types[slices.Index(l.ats, accAt)], nil
}

// issueTypeRevertGuard applies the label rules to the issue type, l its
// history, when guard asks for it. It returns the ID of the issue-type
// event that stands, the revert it made, if any, and a description of the
// type it holds back, if any: then the caller acts on nothing of the issue,
// as for a held label. guard.Type is the type on the issue, which:
//
//   - is the type that stands: nothing to do, even if the newest event is an
//     untrusted change, since a type set back, by the App or anyone, may
//     have no event yet;
//   - is the type the newest event left, an untrusted change: it's
//     reverted, keyed by that event, with a comment posted once (reverted
//     holds the keys posted already);
//   - else, no event explains it yet. GitHub writes issue-type events a
//     second or more late, as it does label events, so it's held back,
//     from when it was first found so, by seen's memory, as the type it is
//     with the events the timeline had then, for HoldBound. If no event
//     explains it by then, it's reverted all the same, keyed by the type
//     and the newest event.
//
// Times are by GitHub's clock, as the client last saw it (Client.Now). A
// nil seen remembers nothing, so such a type is held back for good.
func (r *labelRules) issueTypeRevertGuard(ctx context.Context, l *labelState, reverted map[string]bool, guard *IssueTypeGuard,
	seen *Unexplained) (int64, *IssueTypeRevert, string, error) {
	if guard == nil {
		return 0, nil, "", nil
	}
	if seen == nil {
		seen = &Unexplained{}
	}
	issueKey := unexplainedKey(r.owner, r.repo, r.number)
	id, to, err := r.issueTypeRevertStanding(ctx, l)
	if err != nil {
		return 0, nil, "", fmt.Errorf("check the issue type: %w", err)
	}
	if strings.EqualFold(guard.Type, to) {
		seen.issueTypeRevertForget(issueKey)
		return id, nil, "", nil
	}
	rv := IssueTypeRevert{From: guard.Type, To: to}
	last := len(l.events) - 1
	if last >= 0 && strings.EqualFold(guard.Type, l.types[last]) {
		latest := l.events[last]
		rv.Key, rv.By, rv.From = strconv.FormatInt(latest.GetID(), 10), latest.GetActor().GetLogin(), l.types[last]
	} else {
		now := r.c.Now()
		first := seen.issueTypeRevertFirst(issueKey, strings.ToLower(guard.Type), len(l.events), now)
		if held := now.Sub(first); held < HoldBound {
			logr.Debugf("the issue type %q on %s/%s#%d is unexplained, so it's held back, for %v so far", guard.Type, r.owner, r.repo, r.number, held)
			return id, nil, fmt.Sprintf("the issue type %q", guard.Type), nil
		}
		var latest int64
		if last >= 0 {
			latest = l.events[last].GetID()
		}
		rv.Key = "issue-type:" + url.QueryEscape(strings.ToLower(guard.Type)) + "@" + strconv.FormatInt(latest, 10)
	}
	rv.Posted = reverted[rv.Key]
	if err := r.c.RevertIssueType(ctx, r.owner, r.repo, r.number, rv); err != nil {
		return 0, nil, "", fmt.Errorf("check the issue type: %w", err)
	}
	seen.issueTypeRevertForget(issueKey)
	return id, &rv, "", nil
}

// issueTypeRevertSeen is what Unexplained keeps of an issue whose type no
// event explains: since when, continuously, and the type and the count of
// issue-type events then.
type issueTypeRevertSeen struct {
	at     time.Time
	typ    string // lowercase
	events int
}

// issueTypeRevertFirst returns when issue was first found with type typ,
// lowercase, unexplained, while its timeline had events issue-type events,
// remembering now if it wasn't, or if the type or the events changed since.
func (u *Unexplained) issueTypeRevertFirst(issue, typ string, events int, now time.Time) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.types[issue]; ok && s.typ == typ && s.events == events {
		return s.at
	}
	if u.types == nil {
		u.types = map[string]issueTypeRevertSeen{}
	}
	u.types[issue] = issueTypeRevertSeen{at: now, typ: typ, events: events}
	return now
}

// issueTypeRevertForget forgets issue's unexplained type.
func (u *Unexplained) issueTypeRevertForget(issue string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.types, issue)
}

// issueTypeRevertAtEntityRE is an HTML entity for "@", which GitHub may
// mention on.
var issueTypeRevertAtEntityRE = regexp.MustCompile(`(?i)&(?:#0*64|#x0*40|commat);`)

// issueTypeRevertDefuse puts a zero-width space after each "@" in text, an
// entity for one included, that starts a mention, so text someone else
// wrote mentions no one, as the daemon's defuse does for a type error.
func issueTypeRevertDefuse(text string) string {
	parts := strings.Split(issueTypeRevertAtEntityRE.ReplaceAllString(text, "@"), "@")
	for i := 1; i < len(parts); i++ {
		if r, _ := utf8.DecodeRuneInString(parts[i]); parts[i] != "" && !unicode.IsSpace(r) && r != '\u200b' {
			parts[i] = "\u200b" + parts[i]
		}
	}
	return strings.Join(parts, "@")
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
	who := rv.By
	if who == "" {
		who = "an unexplained"
	} else {
		who += "'s"
	}
	wrap := func(err error) error {
		return fmt.Errorf("revert %s change of the issue type on %s/%s#%d: %w", who, owner, repo, number, err)
	}
	if !rv.Posted {
		// The types' names and the login are someone else's text, so they're
		// defused, and a marker in them is escaped (§5.1).
		did := fmt.Sprintf("I restored the issue type `%s`", issueTypeRevertDefuse(rv.To))
		if rv.To == "" {
			did = fmt.Sprintf("I removed the issue type `%s`", issueTypeRevertDefuse(rv.From))
		}
		why := fmt.Sprintf("only trusted users can change the issue type, and %s isn't one.", issueTypeRevertDefuse(rv.By))
		if rv.By == "" {
			why = fmt.Sprintf("no change by a trusted user or the App explains `%s`, and only trusted users can change the issue type.",
				issueTypeRevertDefuse(rv.From))
		}
		marker, err := FormatMarker(Marker{Kind: "revert", Fields: map[string]string{"key": rv.Key}})
		if err != nil {
			return wrap(err)
		}
		body := Escape(did+": "+why) + "\n\n" + marker
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
	logr.Infof("reverted %s change of the issue type to %q on %s/%s#%d, back to %q", who, rv.From, owner, repo, number, rv.To)
	return nil
}

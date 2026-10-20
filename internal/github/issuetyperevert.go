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
	// passes: the issue type's name, as GitHub shows it, or "" for none.
	// It is compared ignoring case, where ID can't be.
	Type string
	// ID is the issue type's ID, as GitHub gives it with the issue, or 0
	// for none, or if it isn't known (SameIssueType).
	ID int64
	// Names, if set, is what the org's issue types are named now, which a
	// revert reads once for the pass it's kept for (IssueTypeNames). If
	// nil, each revert that needs a name reads the org's list.
	Names *IssueTypeNames
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
	// revert sets, each as GitHub names it, or "" for none. CheckLabels
	// gives To as the org names the type now, by ToID, which may not be
	// the name on the timeline, if the org renamed it (typeRenameCurrent).
	From, To string
	// ToID is To's ID, as the issue-type event that stands gives it, or 0
	// for none, or if it doesn't give one.
	ToID int64
	// Posted is set when the revert comment keyed Key is on the timeline
	// already, as after a pass that crashed before the revert.
	Posted bool
}

// IssueTypeLeftOut is an issue type on the issue that no issue-type event
// explains, which CheckLabels leaves out (LabelCheck.IssueTypeLeftOut), as
// it leaves out a pipeline label no event explains: once it has held the
// issue back for HoldBound, until the type is explained, or reverted once
// it has stayed unexplained for UnexplainedWait. The caller acts on the
// type that stands instead, as if the issue had it.
type IssueTypeLeftOut struct {
	// Name and ID are the type left out, the issue's own, as the caller
	// passed it in IssueTypeGuard: "" and 0 for none.
	Name string
	ID   int64
	// To and ToID are the type that stands, which the caller reads instead,
	// as IssueTypeRevert's are: "" and 0 for none.
	To   string
	ToID int64
}

// SameIssueType reports whether two issue types, each a name and an ID,
// are the same: by ID where both have one, as a type the org renamed keeps
// its ID while the timeline keeps the old name, or else by name, ignoring
// case. "" and 0 is no type.
func SameIssueType(aName string, aID int64, bName string, bID int64) bool {
	if aID != 0 && bID != 0 {
		return aID == bID
	}
	return strings.EqualFold(aName, bName)
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

// issueTypeRevertLeftID is the ID of the type an issue-type event left
// (issueTypeRevertLeft), or 0 for none, or if GitHub gives none.
func issueTypeRevertLeftID(e *issueTypeRevertEvent) int64 {
	if e.GetEvent() == "issue_type_removed" {
		return 0
	}
	return e.IssueType.GetID()
}

// issueTypeRevertStanding judges the issue type's history l, oldest first,
// as settle judges a pipeline label's. It returns the ID of the newest
// issue-type event that stands (standing), or 0, and the type it left and
// that type's ID, or "" and 0 if none stands. Unlike settle, it needs no
// fallback to the App's last word: with none standing, each of the App's
// events is a restore (standing), so of the type before it, and so all of
// them restore no type.
func (r *labelRules) issueTypeRevertStanding(ctx context.Context, l *labelState) (int64, string, int64, error) {
	acc, accAt, err := r.standing(ctx, l, nil)
	if err != nil {
		return 0, "", 0, fmt.Errorf("the issue type that stands: %w", err)
	}
	if acc == nil {
		return 0, "", 0, nil
	}
	i := slices.Index(l.ats, accAt)
	return acc.GetID(), l.types[i], l.ids[i], nil
}

// issueTypeRevertGuard applies the label rules to the issue type, l its
// history, when guard asks for it. It returns the ID of the issue-type
// event that stands, the revert it made, if any, the type it leaves out,
// if any, and a description of the type it holds back, if any: then the
// caller acts on nothing of the issue, as for a held label. guard.Type and
// guard.ID are the type on the issue, which, compared as SameIssueType
// compares types:
//
//   - is the type that stands: nothing to do, even if the newest event is an
//     untrusted change, since a type set back, by the App or anyone, may
//     have no event yet;
//   - is the type the newest event left, an untrusted change: it's
//     reverted, keyed by that event, with a comment posted once (reverted
//     holds the keys posted already), to the type that stands by the name
//     the org gives it now (typeRenameCurrent);
//   - else, no event explains it yet. GitHub writes issue-type events a
//     second or more late, as it does label events, so it's dealt with as
//     an unexplained label is, from when it was first found so, by seen's
//     memory, as the type it is with the events the timeline had then. For
//     HoldBound, it's held back. After that, it's left out: the caller acts
//     on the type that stands instead, and reads the issue again once it
//     falls due (Unexplained.Due). Once it has stayed unexplained for
//     UnexplainedWait, it's reverted all the same, keyed by the type and the
//     newest event. A trusted change whose event lands before then is
//     judged as any change is, and stands.
//
// Times are by GitHub's clock, as the client last saw it (Client.Now). A
// nil seen remembers nothing, so such a type is held back for good.
func (r *labelRules) issueTypeRevertGuard(ctx context.Context, l *labelState, reverted map[string]bool, guard *IssueTypeGuard,
	seen *Unexplained) (int64, *IssueTypeRevert, *IssueTypeLeftOut, string, error) {
	if guard == nil {
		return 0, nil, nil, "", nil
	}
	if seen == nil {
		seen = &Unexplained{}
	}
	issueKey := unexplainedKey(r.owner, r.repo, r.number)
	id, to, toID, err := r.issueTypeRevertStanding(ctx, l)
	if err != nil {
		return 0, nil, nil, "", fmt.Errorf("check the issue type: %w", err)
	}
	if SameIssueType(guard.Type, guard.ID, to, toID) {
		seen.issueTypeRevertForget(issueKey)
		return id, nil, nil, "", nil
	}
	rv := IssueTypeRevert{From: guard.Type, To: to, ToID: toID}
	last := len(l.events) - 1
	if last >= 0 && SameIssueType(guard.Type, guard.ID, l.types[last], l.ids[last]) {
		latest := l.events[last]
		rv.Key, rv.By, rv.From = strconv.FormatInt(latest.GetID(), 10), latest.GetActor().GetLogin(), l.types[last]
	} else {
		now := r.c.Now()
		first := seen.issueTypeRevertFirst(issueKey, strings.ToLower(guard.Type), len(l.events), now)
		held := now.Sub(first)
		if held < HoldBound {
			logr.Debugf("the issue type %q on %s/%s#%d is unexplained, so it's held back, for %v so far", guard.Type, r.owner, r.repo, r.number, held)
			return id, nil, nil, fmt.Sprintf("the issue type %q", guard.Type), nil
		}
		if held < UnexplainedWait {
			lo := IssueTypeLeftOut{Name: guard.Type, ID: guard.ID, To: to, ToID: toID}
			seen.issueTypeRevertLeaveOut(issueKey, lo)
			logr.Infof("the issue type %q on %s/%s#%d is still unexplained after %v, so it's left out for the type that stands, %q, until it's explained or reverted",
				guard.Type, r.owner, r.repo, r.number, held, to)
			return id, nil, &lo, "", nil
		}
		var latest int64
		if last >= 0 {
			latest = l.events[last].GetID()
		}
		rv.Key = "issue-type:" + url.QueryEscape(strings.ToLower(guard.Type)) + "@" + strconv.FormatInt(latest, 10)
	}
	// The type that stands is set back by the name the org gives it now,
	// as GitHub refuses one the org renamed away (#245).
	if err := r.typeRenameCurrent(ctx, &rv, guard.Names); err != nil {
		return 0, nil, nil, "", fmt.Errorf("check the issue type: %w", err)
	}
	rv.Posted = reverted[rv.Key]
	if err := r.c.RevertIssueType(ctx, r.owner, r.repo, r.number, rv); err != nil {
		return 0, nil, nil, "", fmt.Errorf("check the issue type: %w", err)
	}
	seen.issueTypeRevertForget(issueKey)
	return id, &rv, nil, "", nil
}

// issueTypeRevertSeen is what Unexplained keeps of an issue whose type no
// event explains: since when, continuously, and the type and the count of
// issue-type events then, and once it's left out, what for.
type issueTypeRevertSeen struct {
	at      time.Time
	typ     string // lowercase
	events  int
	leftOut *IssueTypeLeftOut
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

// issueTypeRevertLeaveOut remembers that issue's unexplained type, which
// issueTypeRevertFirst keeps, is left out as lo says.
func (u *Unexplained) issueTypeRevertLeaveOut(issue string, lo IssueTypeLeftOut) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.types[issue]; ok {
		s.leftOut = &lo
		u.types[issue] = s
	}
}

// IssueTypeLeftOut returns the issue type of owner/repo#number that
// CheckLabels last left out, with the type that stands for it, and false
// if it left none out, or has since found it explained, or reverted it. A
// caller that reads the issue again without CheckLabels leaves it out the
// same way, if the issue still has it.
func (u *Unexplained) IssueTypeLeftOut(owner, repo string, number int) (IssueTypeLeftOut, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.types[unexplainedKey(owner, repo, number)]
	if !ok || s.leftOut == nil {
		return IssueTypeLeftOut{}, false
	}
	return *s.leftOut, true
}

// issueTypeRevertDue is when issue's unexplained type falls due for its
// revert, if it stays so with no change to its events: UnexplainedWait
// after it was first found so. u is locked.
func (u *Unexplained) issueTypeRevertDue(issue string) (time.Time, bool) {
	s, ok := u.types[issue]
	if !ok {
		return time.Time{}, false
	}
	return s.at.Add(UnexplainedWait), true
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
		switch {
		case rv.By != "":
		case rv.From == "":
			// A removal no event explains: no type, not empty backticks.
			why = "no change by a trusted user or the App explains the issue having no type, and only trusted users can change the issue type."
		default:
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
	if got := issue.GetType(); !SameIssueType(got.GetName(), got.GetID(), rv.To, rv.ToID) {
		return wrap(fmt.Errorf("%w, %q, not %q: the App may lack push access", ErrIssueTypeRevertDropped, got.GetName(), rv.To))
	}
	logr.Infof("reverted %s change of the issue type to %q on %s/%s#%d, back to %q", who, rv.From, owner, repo, number, rv.To)
	return nil
}

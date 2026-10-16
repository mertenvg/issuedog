package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// SetTypeBy sets an issue's type as login, as on org repos, with the
// issue-type event GitHub writes on its timeline, and returns the event's
// ID: issue_type_added for an issue with no type, issue_type_removed for an
// empty name, which removes it, or else issue_type_changed. The event
// carries the types as GitHub's REST timeline does: issue_type, the type
// set, and prev_issue_type, the type replaced. SetType sets a type with no
// event.
func (f *Fake) SetTypeBy(number int, login, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueTypeRevertSet(f.issues[number-1], login, name)
}

// DropTypeChanges makes the fake drop every later change of an issue's type
// through the API, answering with the issue as it was, as GitHub does for
// anyone without push access. Changes made with SetTypeBy still apply.
func (f *Fake) DropTypeChanges() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropTypes = true
}

// issueTypeRevertChange is what an issue-type event changed: the type it
// set and the one it replaced, each nil for none.
type issueTypeRevertChange struct {
	to, from *gogithub.IssueType
}

// issueTypeRevertEvent is an issue-type event as the timeline serves it,
// with its types.
type issueTypeRevertEvent struct {
	*gogithub.Timeline
	IssueType     *gogithub.IssueType `json:"issue_type,omitempty"`
	PrevIssueType *gogithub.IssueType `json:"prev_issue_type,omitempty"`
}

// issueTypeRevertSet sets is's type to name as login, or removes it for "",
// with its issue-type event, and returns the event's ID. f is locked.
func (f *Fake) issueTypeRevertSet(is *issue, login, name string) int64 {
	event := "issue_type_changed"
	ch := issueTypeRevertChange{from: is.Type}
	switch {
	case name == "":
		event, is.Type = "issue_type_removed", nil
	case is.Type == nil:
		event, is.Type = "issue_type_added", &gogithub.IssueType{Name: &name}
	default:
		is.Type = &gogithub.IssueType{Name: &name}
	}
	ch.to = is.Type
	is.UpdatedAt = f.tick()
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: &event, Actor: user(login),
		CreatedAt: &gogithub.Timestamp{Time: is.UpdatedAt.Add(f.eventLag)}})
	if is.typeEvents == nil {
		is.typeEvents = map[int64]issueTypeRevertChange{}
	}
	is.typeEvents[id] = ch
	return id
}

// issueTypeRevertServe is e as the timeline serves it: with its types, for
// an issue-type event of is.
func (is *issue) issueTypeRevertServe(e *gogithub.Timeline) any {
	ch, ok := is.typeEvents[e.GetID()]
	if !ok {
		return e
	}
	return issueTypeRevertEvent{Timeline: e, IssueType: ch.to, PrevIssueType: ch.from}
}

// issueTypeRevertEdit sets the issue's type as the App, as the update of
// an issue does for its type field: a name sets that type, and null removes
// it. A type the issue has already, matched ignoring case, or none for an
// issue with none, is no change and writes no event. Under DropTypeChanges,
// nothing changes. f is locked.
func (f *Fake) issueTypeRevertEdit(is *issue, raw json.RawMessage) (int, any) {
	var name *string
	if err := json.Unmarshal(raw, &name); err != nil {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: type must be a name or null: %v", err))
	}
	if name != nil && *name == "" {
		return http.StatusUnprocessableEntity, message("githubtest: type must be a name or null, not empty")
	}
	switch {
	case f.dropTypes:
	case name == nil:
		if is.Type != nil {
			f.issueTypeRevertSet(is, f.slug+"[bot]", "")
		}
	case !strings.EqualFold(is.GetType().GetName(), *name):
		f.issueTypeRevertSet(is, f.slug+"[bot]", *name)
	}
	return 0, nil
}

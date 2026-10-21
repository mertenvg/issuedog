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
// set, and prev_issue_type, the type replaced, each with its ID in the org
// (IssueTypeID). SetType sets a type with no event.
func (f *Fake) SetTypeBy(number int, login, name string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueTypeRevertSet(f.fakeNumberIssue(number), login, name)
}

// firstIssueType is one less than the first ID an org issue type gets: the
// org's types have IDs of their own, apart from comments' and events'.
const firstIssueType = 500

// issueTypeNamed is the org's issue type named name, ignoring case, as a
// new value with the type's ID and its name as the org shows it, as GitHub
// serves a type set as "task" as the org's Task. A name the org hasn't
// seen yet gets its ID now, with name as the org shows it from then on
// (orgIssueTypes). "" gets no ID. f is locked.
func (f *Fake) issueTypeNamed(name string) *gogithub.IssueType {
	t := &gogithub.IssueType{Name: &name}
	if name == "" {
		return t
	}
	key := strings.ToLower(name)
	id, ok := f.orgTypes[key]
	if !ok {
		f.lastIssueType++
		id = firstIssueType + f.lastIssueType
		if f.orgTypes == nil {
			f.orgTypes = map[string]int64{}
		}
		f.orgTypes[key] = id
		if f.orgTypeNames == nil {
			f.orgTypeNames = map[int64]string{}
		}
		f.orgTypeNames[id] = name
		delete(f.goneTypes, key)
	}
	t.ID, t.Name = &id, new(f.orgTypeNames[id])
	return t
}

// IssueTypeID is the ID of the org's issue type named name, ignoring case,
// which it gets now if the org hasn't seen it yet, as SetType and SetTypeBy
// give it.
func (f *Fake) IssueTypeID(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueTypeNamed(name).GetID()
}

// RenameOrgIssueType renames the org's issue type from to to, ignoring
// case, keeping its ID, as an org owner may. Every issue of that type shows
// the new name, and so does the org's list of its types (orgIssueTypes),
// while the events on their timelines keep the name they were written
// with. It changes no issue's updated_at, as GitHub isn't known to. The old
// name is no type any more, so setting it through the API is refused with
// a 422.
func (f *Fake) RenameOrgIssueType(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.issueTypeNamed(from).GetID()
	delete(f.orgTypes, strings.ToLower(from))
	f.orgTypes[strings.ToLower(to)] = id
	if f.goneTypes == nil {
		f.goneTypes = map[string]bool{}
	}
	f.goneTypes[strings.ToLower(from)] = true
	delete(f.goneTypes, strings.ToLower(to))
	f.orgTypeNames[id] = to
	for i, t := range f.checks.issueTypes {
		if strings.EqualFold(t.Name, from) {
			f.checks.issueTypes[i].Name = to
		}
	}
	for _, is := range f.issues {
		if is.GetType().GetID() == id {
			// A new value: the events' types keep the old name.
			is.Type = &gogithub.IssueType{ID: &id, Name: &to}
		}
	}
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
		event, is.Type = "issue_type_added", f.issueTypeNamed(name)
	default:
		is.Type = f.issueTypeNamed(name)
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
// an issue does for its type field: a name sets that type, by the name the
// org shows (issueTypeNamed), and null removes
// it. A type the issue has already, matched ignoring case, or none for an
// issue with none, is no change and writes no event. A name the org renamed
// away (RenameOrgIssueType) is refused with a 422, and so is one
// fakeTypeRefusal refuses. Under DropTypeChanges, nothing changes. f is
// locked.
func (f *Fake) issueTypeRevertEdit(is *issue, raw json.RawMessage) (int, any) {
	var name *string
	if err := json.Unmarshal(raw, &name); err != nil {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: type must be a name or null: %v", err))
	}
	if name != nil && *name == "" {
		return http.StatusUnprocessableEntity, message("githubtest: type must be a name or null, not empty")
	}
	if name != nil && f.goneTypes[strings.ToLower(*name)] {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: the org has no issue type %q", *name))
	}
	if name != nil {
		if status, msg := f.fakeTypeRefusal(*name); msg != nil {
			return status, msg
		}
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

package githubtest

import (
	gogithub "github.com/google/go-github/v92/github"
)

// SetTypeBy sets an issue's type as login, as on org repos, with the
// issue-type event GitHub writes on its timeline, and returns the event's
// ID: issue_type_added for an issue with no type, issue_type_removed for an
// empty name, which removes it, or else issue_type_changed. SetType sets a
// type with no event.
func (f *Fake) SetTypeBy(number int, login, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	event := "issue_type_changed"
	switch {
	case name == "":
		event, is.Type = "issue_type_removed", nil
	case is.Type == nil:
		event, is.Type = "issue_type_added", &gogithub.IssueType{Name: &name}
	default:
		is.Type = &gogithub.IssueType{Name: &name}
	}
	is.UpdatedAt = f.tick()
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: &event, Actor: user(login),
		CreatedAt: &gogithub.Timestamp{Time: is.UpdatedAt.Add(f.eventLag)}})
	return id
}

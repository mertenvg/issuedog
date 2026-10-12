package githubtest

import (
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// firstEvent is one less than the first timeline event's ID. Events have IDs
// of their own on GitHub; these stay clear of the comment IDs.
const firstEvent = 9000

// AddLabel adds a label to an issue as login, and returns the labeled
// event's ID, or 0 if the issue had the label already and nothing changed.
func (f *Fake) AddLabel(number int, login, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	if !is.addLabel(name) {
		return 0
	}
	is.UpdatedAt = f.tick()
	return f.labelEvent(is, "labeled", login, name, is.UpdatedAt)
}

// RemoveLabel removes a label from an issue as login, and returns the
// unlabeled event's ID, or 0 if the issue didn't have it.
func (f *Fake) RemoveLabel(number int, login, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	i := slices.IndexFunc(is.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), name) })
	if i < 0 {
		return 0
	}
	name = is.Labels[i].GetName()
	is.Labels, is.UpdatedAt = slices.Delete(is.Labels, i, i+1), f.tick()
	return f.labelEvent(is, "unlabeled", login, name, is.UpdatedAt)
}

// labelEvent adds a labeled or unlabeled event to an issue's timeline, and
// returns its ID.
func (f *Fake) labelEvent(is *issue, event, login, name string, at *gogithub.Timestamp) int64 {
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: &event, Actor: user(login),
		Label: &gogithub.Label{Name: name}, CreatedAt: at})
	return id
}

// timeline serves an issue's labeled, unlabeled and commented events, oldest
// first. As on GitHub, a commented event has its comment's ID, user and
// current body, and a deleted comment's is gone.
func (f *Fake) timeline(c *call) (int, any) {
	events := slices.Clone(c.issue.labelEvents)
	for _, cm := range f.comments {
		if cm.issue == c.issue.GetNumber() {
			events = append(events, &gogithub.Timeline{ID: cm.ID, Event: new("commented"), Actor: cm.User, User: cm.User,
				Body: cm.Body, CreatedAt: cm.CreatedAt})
		}
	}
	// Every change ticks the clock, so only one call's label events share a time.
	slices.SortStableFunc(events, func(a, b *gogithub.Timeline) int { return a.GetCreatedAt().Compare(b.GetCreatedAt().Time) })
	return page(c, events)
}

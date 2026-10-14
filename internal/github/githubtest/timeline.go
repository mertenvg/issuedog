package githubtest

import (
	"slices"
	"strings"
	"time"

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

// RenameLabel renames a label, matched ignoring case, as the owner can in
// the repo's settings: in the repo and on every issue that has it. No event
// says so, and past labeled and unlabeled events keep the name they had, as
// GitHub is believed to (see github.CheckLabels).
func (f *Fake) RenameLabel(from, to string) { f.rename(from, to, false) }

// RenameLabelInEvents is RenameLabel, but past labeled and unlabeled events
// take the new name too: the other way GitHub might report a rename.
func (f *Fake) RenameLabelInEvents(from, to string) { f.rename(from, to, true) }

func (f *Fake) rename(from, to string, events bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.labels {
		if strings.EqualFold(l.GetName(), from) {
			l.Name = to
		}
	}
	for _, is := range f.issues {
		for i, l := range is.Labels {
			if strings.EqualFold(l.GetName(), from) {
				is.Labels[i] = &gogithub.Label{Name: to}
			}
		}
		slices.SortFunc(is.Labels, func(a, b *gogithub.Label) int { return strings.Compare(a.GetName(), b.GetName()) })
		for _, e := range is.labelEvents {
			if events && strings.EqualFold(e.GetLabel().GetName(), from) {
				e.Label = &gogithub.Label{Name: to}
			}
		}
	}
}

// LagEvents makes the label events of every later change, the App's
// included, be written d after the change, as GitHub was seen to write an
// issue's labeled events a second or two after the issue was filed with its
// labels. Until the clock reaches it (see Advance), an event is missing from
// the timeline. 0 writes them at once again.
func (f *Fake) LagEvents(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventLag = d
}

// Advance moves the fake clock on by d, as time passing with no change.
// Answers are dated by the clock.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// labelEvent adds a labeled or unlabeled event to an issue's timeline, made
// at at and written after the lag, and returns its ID.
func (f *Fake) labelEvent(is *issue, event, login, name string, at *gogithub.Timestamp) int64 {
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: &event, Actor: user(login),
		Label: &gogithub.Label{Name: name}, CreatedAt: &gogithub.Timestamp{Time: at.Add(f.eventLag)}})
	return id
}

// timeline serves an issue's labeled, unlabeled and commented events, oldest
// first, but for label events not written yet (LagEvents). As on GitHub, a
// commented event has its comment's ID, user and current body, and a deleted
// comment's is gone.
func (f *Fake) timeline(c *call) (int, any) {
	events := slices.DeleteFunc(slices.Clone(c.issue.labelEvents), func(e *gogithub.Timeline) bool { return e.GetCreatedAt().After(f.now) })
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

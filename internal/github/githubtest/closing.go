package githubtest

import (
	gogithub "github.com/google/go-github/v92/github"
)

// CloseBy closes an issue as login, as completed, and returns the closed
// event's ID, or 0 if the issue was closed already and nothing changed. An
// empty login means GitHub names no actor.
func (f *Fake) CloseBy(number int, login string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.issues[number-1], "closed", "", login)
}

// ReopenBy reopens an issue as login, and returns the reopened event's ID,
// or 0 if the issue was open already and nothing changed. An empty login
// means GitHub names no actor.
func (f *Fake) ReopenBy(number int, login string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.issues[number-1], "open", "", login)
}

// setState opens or closes an issue as login, as GitHub is believed to: a
// close sets closed_at and state_reason (reason, or "completed"), a reopen
// clears closed_at and sets state_reason to "reopened", either one moves
// updated_at, and either adds a closed or reopened event to the timeline,
// written after the lag (LagEvents), whose ID it returns. A state the issue
// has already changes nothing, and returns 0.
func (f *Fake) setState(is *issue, state, reason, login string) int64 {
	if is.GetState() == state {
		return 0
	}
	now := f.tick()
	event := "closed"
	if state == "open" {
		event, reason, is.ClosedAt = "reopened", "reopened", nil
	} else {
		if reason == "" {
			reason = "completed"
		}
		is.ClosedAt = now
	}
	is.State, is.StateReason, is.UpdatedAt = &state, &reason, now
	var actor *gogithub.User
	if login != "" {
		actor = user(login)
	}
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: &event, Actor: actor,
		CreatedAt: &gogithub.Timestamp{Time: now.Add(f.eventLag)}})
	return id
}

package githubtest

import (
	"net/http"

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

// CloseAs is CloseBy with a state_reason, such as "not_planned" or
// "duplicate", which the issue and its closed event carry (T6).
func (f *Fake) CloseAs(number int, login, reason string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.issues[number-1], "closed", reason, login)
}

// CloseByCommit closes an issue as login, as completed, with a closed
// event that names commit sha, as GitHub writes one when a pull request
// that says "Closes #N" merges, its merger the actor, or when a commit
// that says so is pushed. With merged, sha is a merged pull request's, as
// the commit's pull requests list it (closingPulls); without, it's a
// commit no pull request has. It returns the event's ID, or 0 if the issue
// was closed already.
func (f *Fake) CloseByCommit(number int, login, sha string, merged bool) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	id := f.setState(is, "closed", "", login)
	if id != 0 {
		is.labelEvents[len(is.labelEvents)-1].CommitID = &sha
	}
	if merged {
		if f.closingMerged == nil {
			f.closingMerged = map[string]bool{}
		}
		f.closingMerged[sha] = true
	}
	return id
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
// written after the lag (LagEvents), whose ID it returns. A closed event
// carries the close's state_reason (closingServe). A state the issue has
// already changes nothing, and returns 0.
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
	if event == "closed" {
		if is.closingReasons == nil {
			is.closingReasons = map[int64]string{}
		}
		is.closingReasons[id] = reason
	}
	return id
}

// closingEvent is a closed event as the timeline serves it, with its
// state_reason, which go-github's Timeline leaves out.
type closingEvent struct {
	*gogithub.Timeline
	StateReason string `json:"state_reason"`
}

// closingServe is e as the timeline serves it: a closed event of is with
// its state_reason, and any other as issueTypeRevertServe serves it.
func (is *issue) closingServe(e *gogithub.Timeline) any {
	if reason, ok := is.closingReasons[e.GetID()]; ok {
		return closingEvent{Timeline: e, StateReason: reason}
	}
	return is.issueTypeRevertServe(e)
}

// closingPulls serves the pull requests of the commit {sha}, as GitHub's
// list of a commit's pull requests does: one merged pull request for a
// commit CloseByCommit merged, and none for any other.
func (f *Fake) closingPulls(c *call) (int, any) {
	sha := c.r.PathValue("sha")
	prs := []*gogithub.PullRequest{}
	if f.closingMerged[sha] {
		prs = append(prs, &gogithub.PullRequest{Number: new(9999), State: new("closed"), MergeCommitSHA: &sha,
			MergedAt: &gogithub.Timestamp{Time: f.now}})
	}
	return http.StatusOK, prs
}

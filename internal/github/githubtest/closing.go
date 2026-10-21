package githubtest

import (
	"fmt"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// Closes and reopens. As on GitHub (checked live, #190's verify), a closed
// event on the REST timeline names its actor, the merger for a pull
// request's merge, and has a null state_reason, and a null commit_id but
// for a commit's close; how the issue was closed is GraphQL's ClosedEvent:
// its stateReason and its closer, a PullRequest, a Commit or null
// (closingAnswer). Every closed and reopened event carries its node_id.

// closingCloser is how a closed event was made, for GraphQL's ClosedEvent:
// its state reason, and its closer's type, "" for none, with a pull
// request's merge and base, and the closer's repo.
type closingCloser struct {
	reason, typename string
	merged           bool
	base, repo       string
	forbidden        bool // see ForbidCloser
	// fail is the error GraphQL gives under node, with a null node, for
	// each question of this event (FailCloser); nil for none.
	fail map[string]any
}

// ClosingPR is a pull request that closes an issue as it merges
// (CloseByPullRequest): whether it merged, its base branch ("" for main,
// the fake's default branch) and its base repo ("" for the fake's).
type ClosingPR struct {
	Merged     bool
	Base, Repo string
}

// CloseBy closes an issue as login, as completed, and returns the closed
// event's ID, or 0 if the issue was closed already and nothing changed. An
// empty login means GitHub names no actor.
func (f *Fake) CloseBy(number int, login string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.issues[number-1], "closed", "", login)
}

// CloseAs is CloseBy with a state_reason, such as "not_planned" or
// "duplicate", which the issue and its ClosedEvent carry (T6).
func (f *Fake) CloseAs(number int, login, reason string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.issues[number-1], "closed", reason, login)
}

// CloseByPullRequest closes an issue as login, as completed, the way a pull
// request that says "Closes #N" does as login merges it: the closed event
// names login and no commit, and its ClosedEvent's closer is the pull
// request pr. It returns the event's ID, or 0 if the issue was closed
// already.
func (f *Fake) CloseByPullRequest(number int, login string, pr ClosingPR) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	id := f.setState(is, "closed", "", login)
	if id != 0 {
		base, repo := pr.Base, pr.Repo
		if base == "" {
			base = "main"
		}
		if repo == "" {
			repo = f.repo
		}
		is.closingClosers[id] = closingCloser{reason: "completed", typename: "PullRequest", merged: pr.Merged, base: base, repo: repo}
	}
	return id
}

// CloseByCommit closes an issue as login, as completed, the way a commit
// that says "Fixes #N" does once login pushes it to the default branch of
// repo ("" for the fake's), as one pushed to another repo that names this
// one's issue does too: the closed event names the commit, and its
// ClosedEvent's closer is the commit. It returns the event's ID, or 0 if
// the issue was closed already.
func (f *Fake) CloseByCommit(number int, login, sha, repo string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo == "" {
		repo = f.repo
	}
	is := f.issues[number-1]
	id := f.setState(is, "closed", "", login)
	if id != 0 {
		e := is.labelEvents[len(is.labelEvents)-1]
		e.CommitID, e.CommitURL = &sha, new(fmt.Sprintf("%s/repos/%s/commits/%s", f.srv.URL, repo, sha))
		is.closingClosers[id] = closingCloser{reason: "completed", typename: "Commit", repo: repo}
	}
	return id
}

// CloseByProject closes an issue as login, as completed, the way a
// project's workflow does, as GitHub's project automation bot: its
// ClosedEvent's closer is a ProjectV2. It returns the event's ID, or 0 if
// the issue was closed already.
func (f *Fake) CloseByProject(number int, login string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[number-1]
	id := f.setState(is, "closed", "", login)
	if id != 0 {
		is.closingClosers[id] = closingCloser{reason: "completed", typename: "ProjectV2"}
	}
	return id
}

// ForbidCloser makes GraphQL refuse the App closed event id's closer, as
// GitHub does for one the App may not read, such as a project's on a
// personal account or a commit in a private repo it can't see: the closer
// is null, with a FORBIDDEN error at ["node", "closer"].
func (f *Fake) ForbidCloser(number int, id int64) {
	f.edit(number, func(is *issue) {
		cl := is.closingClosers[id]
		cl.forbidden = true
		is.closingClosers[id] = cl
	})
}

// FailCloser makes GraphQL fail every question of closed event id's
// closer, until MendCloser: the node is null, with an error at ["node"]
// with message, of type typ, or with no type for typ "", as a passing
// fault GitHub doesn't name might show (#204).
func (f *Fake) FailCloser(number int, id int64, typ, message string) {
	f.edit(number, func(is *issue) {
		cl := is.closingClosers[id]
		cl.fail = map[string]any{"path": []any{"node"}, "message": message}
		if typ != "" {
			cl.fail["type"] = typ
		}
		is.closingClosers[id] = cl
	})
}

// MendCloser ends FailCloser's errors for closed event id: GraphQL answers
// how it was made again.
func (f *Fake) MendCloser(number int, id int64) {
	f.edit(number, func(is *issue) {
		cl := is.closingClosers[id]
		cl.fail = nil
		is.closingClosers[id] = cl
	})
}

// FailGraphQLAt makes the next GraphQL answer list an error of type typ,
// with message, at path, such as ["repository", "defaultBranchRef"],
// beside its data.
func (f *Fake) FailGraphQLAt(typ, message string, path ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closingErrs = append(f.closingErrs, map[string]any{"type": typ, "message": message, "path": path})
}

// ForgetCloser makes closed event id of issue number a node with no
// ClosedEvent's fields, so GraphQL gives it no state reason and no closer.
func (f *Fake) ForgetCloser(number int, id int64) {
	f.edit(number, func(is *issue) { delete(is.closingClosers, id) })
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
// written after the lag (LagEvents), whose ID it returns. A closed event's
// ClosedEvent has the close's state reason and no closer. A state the issue
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
	if event == "closed" {
		if is.closingClosers == nil {
			is.closingClosers = map[int64]closingCloser{}
		}
		is.closingClosers[id] = closingCloser{reason: reason}
	}
	return id
}

// closingEvent is a closed or reopened event as the timeline serves it,
// with its node_id, which go-github's Timeline leaves out.
type closingEvent struct {
	*gogithub.Timeline
	NodeID string `json:"node_id"`
}

// closingNode is the node ID of a closed or reopened event.
func closingNode(e *gogithub.Timeline) string {
	if e.GetEvent() == "reopened" {
		return fmt.Sprintf("REE_%d", e.GetID())
	}
	return fmt.Sprintf("CE_%d", e.GetID())
}

// closingServe is e as the timeline serves it: a closed or reopened event
// with its node_id, and any other as issueTypeRevertServe serves it.
func (is *issue) closingServe(e *gogithub.Timeline) any {
	if e.GetEvent() == "closed" || e.GetEvent() == "reopened" {
		return closingEvent{Timeline: e, NodeID: closingNode(e)}
	}
	return is.issueTypeRevertServe(e)
}

// closingAnswer answers the closer query (answeredCloser) for the node id:
// the repo's name and default branch, main, and the ClosedEvent it names,
// with its stateReason and closer, or, as on GitHub, a null node with a
// NOT_FOUND error for an ID it doesn't know. A transferred issue's event is
// answered as any other's, from its new repo (transfer.go). A reopened
// event's node, and a closed event's ForgetCloser forgot, has no
// ClosedEvent's fields. A closed event FailCloser fails is a null node,
// with its error. A repo
// that isn't the fake's, owner/name, is null, with a NOT_FOUND error. f is
// locked.
func (f *Fake) closingAnswer(owner, name, id string) (any, []map[string]any) {
	if !strings.EqualFold(owner+"/"+name, f.repo) {
		return map[string]any{"repository": nil, "node": nil}, []map[string]any{{"type": "NOT_FOUND", "path": []any{"repository"},
			"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name)}}
	}
	repo := map[string]any{"nameWithOwner": f.repo, "defaultBranchRef": map[string]any{"name": "main"}}
	notFound := []map[string]any{{"type": "NOT_FOUND", "path": []any{"node"}, "message": fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)}}
	kind, num, _ := strings.Cut(id, "_")
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || kind != "CE" && kind != "REE" {
		return map[string]any{"repository": repo, "node": nil}, notFound
	}
	for _, is := range f.issues {
		for _, e := range is.labelEvents {
			if e.GetID() != n || closingNode(e) != id {
				continue
			}
			cl, ok := is.closingClosers[n]
			if !ok {
				return map[string]any{"repository": repo, "node": map[string]any{}}, nil
			}
			var closer any
			if cl.fail != nil {
				return map[string]any{"repository": repo, "node": nil}, []map[string]any{cl.fail}
			}
			if cl.forbidden {
				return map[string]any{"repository": repo, "node": map[string]any{"stateReason": strings.ToUpper(cl.reason), "closer": nil}},
					[]map[string]any{{"type": "FORBIDDEN", "path": []any{"node", "closer"}, "message": "Resource not accessible by integration"}}
			}
			switch cl.typename {
			case "PullRequest":
				closer = map[string]any{"__typename": cl.typename, "merged": cl.merged, "baseRefName": cl.base,
					"baseRepository": map[string]any{"nameWithOwner": cl.repo}}
			case "Commit":
				closer = map[string]any{"__typename": cl.typename, "repository": map[string]any{"nameWithOwner": cl.repo}}
			case "ProjectV2":
				closer = map[string]any{"__typename": cl.typename}
			}
			return map[string]any{"repository": repo, "node": map[string]any{"stateReason": strings.ToUpper(cl.reason), "closer": closer}}, nil
		}
	}
	return map[string]any{"repository": repo, "node": nil}, notFound
}

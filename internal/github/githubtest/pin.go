package githubtest

import (
	"fmt"
	"net/http"
	"slices"
)

// FailPin makes every later pinIssue mutation fail as GitHub refuses one,
// such as for a missing permission or a repo with three pins already: the
// answer has no pin and one FORBIDDEN error carrying message, and the issue
// stays unpinned. The mutation is still a write, logged as any other.
// FailPin("") lets pins work again.
func (f *Fake) FailPin(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinFailure = message
}

// SetPinned pins or unpins an issue by hand, as the owner can on GitHub.
func (f *Fake) SetPinned(number int, pinned bool) {
	f.edit(number, func(is *issue) { is.pinned = pinned })
}

// pinIssue answers the pinIssue mutation for the issue whose node ID is id,
// returning the data and the errors. As on GitHub, an unknown node is a
// NOT_FOUND error. Pinning a pinned issue changes nothing and succeeds. A pin
// doesn't change the issue's updated_at, but moves the clock on, as every
// change does.
func (f *Fake) pinIssue(id string) (any, []map[string]any) {
	i := slices.IndexFunc(f.issues, func(is *issue) bool { return is.GetNodeID() == id })
	switch {
	case i < 0 || f.issues[i].IsPullRequest():
		return map[string]any{"pinIssue": nil}, []map[string]any{{"type": "NOT_FOUND", "path": []any{"pinIssue"},
			"message": fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)}}
	case f.pinFailure != "":
		return map[string]any{"pinIssue": nil}, []map[string]any{{"type": "FORBIDDEN", "path": []any{"pinIssue"}, "message": f.pinFailure}}
	}
	f.issues[i].pinned = true
	f.tick()
	return map[string]any{"pinIssue": map[string]any{"issue": map[string]any{"isPinned": true}}}, nil
}

// createIssue files an issue as the App, with the labels it names, which the
// App adds. Only the title is required, as on GitHub.
func (f *Fake) createIssue(c *call) (int, any) {
	var req struct {
		Title  string
		Body   string
		Labels []string
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Title == "" {
		return http.StatusUnprocessableEntity, message("Validation Failed: title is missing")
	}
	return http.StatusCreated, f.fileIssue(f.slug+"[bot]", req.Title, req.Body, req.Labels).Issue
}

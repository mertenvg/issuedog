package githubtest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// FailPinStatus makes every later pinIssue mutation answer HTTP status
// alone, with its status text as the message and no pin, as GitHub might
// for bad credentials (401) or a missing permission (403), or as a glitch
// might (5xx). The mutation is still a write, logged as any other.
// FailPinStatus(0) lets pins work again.
func (f *Fake) FailPinStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinStatus = status
}

// FailIsPinned makes every later isPinned query answer HTTP status alone,
// with its status text as the message, as a glitch might. FailIsPinned(0)
// lets them work again.
func (f *Fake) FailIsPinned(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinnedFail = status
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
// App adds, and the issue type it names, which the App sets with an
// issue_type_added event (issueTypeRevertSet). Only the title is required,
// as on GitHub. A type name the org has no type of, ignoring case, gets a
// 422 and files nothing: the org has the types SetType, SetTypeBy and
// IssueTypeID gave an ID, less those RenameOrgIssueType renamed away. Under
// DropTypeChanges, the issue is filed with no type.
func (f *Fake) createIssue(c *call) (int, any) {
	var req struct {
		Title  string
		Body   string
		Labels []string
		Type   *string
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Title == "" {
		return http.StatusUnprocessableEntity, message("Validation Failed: title is missing")
	}
	if req.Type != nil {
		if _, ok := f.orgTypes[strings.ToLower(*req.Type)]; !ok {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: the org has no issue type %q", *req.Type))
		}
	}
	is := f.fileIssue(f.slug+"[bot]", req.Title, req.Body, req.Labels)
	if req.Type != nil && !f.dropTypes {
		f.issueTypeRevertSet(is, f.slug+"[bot]", *req.Type)
	}
	return http.StatusCreated, is.Issue
}

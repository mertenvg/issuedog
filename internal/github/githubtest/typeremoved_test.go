package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// typeRemovedType is an issue type as the timeline serves it, by its ID
// and name.
type typeRemovedType struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// An issue_type_removed event, the owner's or the App's, gives the type it
// removed as both its issue_type and its prev_issue_type, with that type's
// ID, as GitHub's do (#246). An added or changed event's issue_type is the
// type it set.
func TestTypeRemovedServed(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	f.SetTypeBy(n, "owner", "Task")
	f.SetTypeBy(n, "owner", "")
	f.SetTypeBy(n, "owner", "Bug")
	c := f.Client(t)
	ctx := context.Background()
	req, err := c.NewRequest(ctx, http.MethodPatch, fmt.Sprintf("repos/o/r/issues/%d", n), map[string]any{"type": nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(req, nil); err != nil {
		t.Fatal(err)
	}
	req, err = c.NewRequest(ctx, http.MethodGet, fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []struct {
		Event string           `json:"event"`
		Type  *typeRemovedType `json:"issue_type"`
		Prev  *typeRemovedType `json:"prev_issue_type"`
	}
	if _, err := c.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	str := func(typ *typeRemovedType) string {
		if typ == nil {
			return "none"
		}
		return fmt.Sprintf("%s %d", typ.Name, typ.ID)
	}
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s: %s, from %s", e.Event, str(e.Type), str(e.Prev)))
	}
	task, bug := f.IssueTypeID("Task"), f.IssueTypeID("Bug")
	want := []string{
		fmt.Sprintf("issue_type_added: Task %d, from none", task),
		fmt.Sprintf("issue_type_removed: Task %d, from Task %d", task, task),
		fmt.Sprintf("issue_type_added: Bug %d, from none", bug),
		fmt.Sprintf("issue_type_removed: Bug %d, from Bug %d", bug, bug),
	}
	if !slices.Equal(got, want) {
		t.Errorf("events %q; want %q", got, want)
	}
}

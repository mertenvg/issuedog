package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// GET /app/installations is the App's own: it refuses an installation
// token, and a request with no auth, as GET /app does.
func TestInstallationsNeedJWT(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	// The embedded client is the installation's, with its token.
	_, _, err := f.Client(t).Apps.ListInstallations(context.Background(), nil)
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnauthorized {
		t.Errorf("listing installations with an installation token: %v; want a 401", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.URL()+"/app/installations", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Transport().RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("listing installations with no auth: %d; want 401", resp.StatusCode)
	}
	if got, err := f.Client(t).Installations(context.Background()); err != nil || len(got) != 1 || got[0].Login != "o" {
		t.Errorf("Installations with the JWT = %+v, %v; want the repo owner's", got, err)
	}
}

// typeRenameListed is the org's issue types as the fake lists them, each
// "name id", with " off" for a disabled one.
func typeRenameListed(t *testing.T, f *Fake) []string {
	t.Helper()
	types, _, err := f.Client(t).Organizations.ListIssueTypes(context.Background(), "o")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, typ := range types {
		s := fmt.Sprintf("%s %d", typ.GetName(), typ.GetID())
		if !typ.GetIsEnabled() {
			s += " off"
		}
		out = append(out, s)
	}
	return out
}

// With no SetOrgIssueTypes, the org lists every type it has given an ID,
// in that order, enabled, by the name it gave first. A rename (#245) keeps
// the type's ID and lists the new name, there and in a list
// SetOrgIssueTypes set, and the old name, which is no type any more, isn't
// listed, nor given a new ID.
func TestOrgIssueTypesRenamed(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	f.SetTypeBy(n, "owner", "Task")
	f.SetType(n, "bug")
	task, bug := f.IssueTypeID("task"), f.IssueTypeID("Bug")
	if got, want := typeRenameListed(t, f), []string{fmt.Sprintf("Task %d", task), fmt.Sprintf("bug %d", bug)}; !slices.Equal(got, want) {
		t.Errorf("listed %q; want %q", got, want)
	}
	f.RenameOrgIssueType("Task", "Chore")
	if got, want := typeRenameListed(t, f), []string{fmt.Sprintf("Chore %d", task), fmt.Sprintf("bug %d", bug)}; !slices.Equal(got, want) {
		t.Errorf("after the rename, listed %q; want %q", got, want)
	}
	f.SetOrgIssueTypes(OrgIssueType{Name: "Chore"}, OrgIssueType{Name: "Bug", Disabled: true})
	f.RenameOrgIssueType("bug", "Defect")
	if got, want := typeRenameListed(t, f), []string{fmt.Sprintf("Chore %d", task), fmt.Sprintf("Defect %d off", bug)}; !slices.Equal(got, want) {
		t.Errorf("a set list after a rename: listed %q; want %q", got, want)
	}
}

// The org's issue types keep the IDs IssueTypeID gives their names, and
// only the repo's owner, while an org, lists them.
func TestOrgIssueTypesFake(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	f.SetOrgIssueTypes(OrgIssueType{Name: "Bug"}, OrgIssueType{Name: "Epic", Disabled: true})
	c := f.Client(t)
	types, _, err := c.Organizations.ListIssueTypes(context.Background(), "O")
	if err != nil || len(types) != 2 {
		t.Fatalf("issue types = %v, %v; want two", types, err)
	}
	if types[0].GetID() != f.IssueTypeID("bug") || types[1].GetID() != f.IssueTypeID("Epic") || types[0].GetID() == types[1].GetID() {
		t.Errorf("IDs %d and %d; want IssueTypeID's, %d and %d", types[0].GetID(), types[1].GetID(), f.IssueTypeID("bug"), f.IssueTypeID("Epic"))
	}
	if !types[0].GetIsEnabled() || types[1].GetIsEnabled() || types[1].IsEnabled == nil {
		t.Errorf("enabled %v and %v; want true and false", types[0].IsEnabled, types[1].IsEnabled)
	}
	if _, resp, err := c.Organizations.ListIssueTypes(context.Background(), "other"); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("issue types of another account: %v; want a 404", err)
	}
}

// TypeGoneRefuseList refuses every read of the org's issue types with its
// status, until it's called with 0, when the list is served again (#409).
func TestTypeGoneRefuseList(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	f.SetOrgIssueTypes(OrgIssueType{Name: "Bug"})
	c := f.Client(t)
	f.TypeGoneRefuseList(http.StatusForbidden)
	for read := range 2 {
		if _, resp, err := c.Organizations.ListIssueTypes(context.Background(), "o"); err == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("read %d: issue types: %v; want a 403", read, err)
		}
	}
	f.TypeGoneRefuseList(0)
	if types, _, err := c.Organizations.ListIssueTypes(context.Background(), "o"); err != nil || len(types) != 1 {
		t.Errorf("after the refusal: issue types = %v, %v; want Bug", types, err)
	}
}

// fakeTypeWant422 fails t unless err is a 422 shaped as GitHub's validation
// errors are.
func fakeTypeWant422(t *testing.T, what string, err error) {
	t.Helper()
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(er.Message, "Validation Failed") {
		t.Errorf("%s: err = %v; want a 422, Validation Failed", what, err)
	}
}

// A create or an edit that sets a type the org's list has disabled, in any
// case, is refused with a 422, and so is any type once the owner is a
// personal account, which has none. A refusal files and changes nothing.
// An enabled type is set, and on a personal account an issue is still
// filed with no type, and an edit that removes the type is no refusal.
func TestFakeTypeRefused(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	f.SetOrgIssueTypes(OrgIssueType{Name: "Task"}, OrgIssueType{Name: "Epic", Disabled: true})
	n := f.AddIssue("owner", "Issue", "")
	ctx, c := context.Background(), f.Client(t)
	before := f.State()
	for _, name := range []string{"Epic", "epic"} {
		_, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Refused", Type: new(name)})
		fakeTypeWant422(t, fmt.Sprintf("create with the disabled %q", name), err)
		_, _, err = c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Type: new(name)})
		fakeTypeWant422(t, fmt.Sprintf("edit with the disabled %q", name), err)
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the refusals:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	if is, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Typed", Type: new("Task")}); err != nil || is.GetType().GetName() != "Task" {
		t.Errorf("create with the enabled Task = %q, %v; want Task", is.GetType().GetName(), err)
	}
	if is, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Type: new("Task")}); err != nil || is.GetType().GetName() != "Task" {
		t.Errorf("edit with the enabled Task = %q, %v; want Task", is.GetType().GetName(), err)
	}
	f.SetPersonalAccount()
	untyped := f.AddIssue("owner", "Untyped", "")
	before = f.State()
	_, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Refused", Type: new("Task")})
	fakeTypeWant422(t, "create with a type on a personal account", err)
	_, _, err = c.Issues.Update(ctx, "o", "r", untyped, gogithub.UpdateIssueRequest{Type: new("Task")})
	fakeTypeWant422(t, "edit with a type on a personal account", err)
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the personal account's refusals:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	if is, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Plain"}); err != nil || is.Type != nil {
		t.Errorf("create with no type on a personal account = %q, %v; want an issue with none", is.GetType().GetName(), err)
	}
	req, err := c.Client.NewRequest(ctx, http.MethodPatch, fmt.Sprintf("repos/o/r/issues/%d", n), map[string]any{"type": nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Client.Do(req, nil); err != nil {
		t.Errorf("remove the type on a personal account: %v; want no refusal", err)
	}
	if got := f.State().Issues[n-1].Type; got != "" {
		t.Errorf("type after the removal %q; want none", got)
	}
}

// A type set by its name in another case is stored and served by the name
// the org shows: a create with "task" serves Task, in the answer, a read
// and the timeline's event, and so does an edit with "TASK". The names
// SetOrgIssueTypes lists are the ones the org shows, though a type got its
// ID first in another case.
func TestFakeTypeCanonical(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	task := f.IssueTypeID("Task")
	ctx, c := context.Background(), f.Client(t)
	is, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Lower", Type: new("task")})
	if err != nil || is.GetType().GetName() != "Task" || is.GetType().GetID() != task {
		t.Fatalf("create with task = %q %d, %v; want the org's Task %d", is.GetType().GetName(), is.GetType().GetID(), err, task)
	}
	n := is.GetNumber()
	if got, _, err := c.Issues.Get(ctx, "o", "r", n); err != nil || got.GetType().GetName() != "Task" {
		t.Errorf("read after a create with task = %q, %v; want Task", got.GetType().GetName(), err)
	}
	if got := f.State().Issues[n-1].Type; got != "Task" {
		t.Errorf("state's type %q; want Task", got)
	}
	req, err := c.Client.NewRequest(ctx, http.MethodGet, fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []issueTypeRevertEvent
	if _, err := c.Client.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.IssueType.GetName())
	}
	if want := []string{"issue_type_added Task"}; !slices.Equal(got, want) {
		t.Errorf("timeline %q; want %q", got, want)
	}
	edited := f.AddIssue("owner", "Edited", "")
	if got, _, err := c.Issues.Update(ctx, "o", "r", edited, gogithub.UpdateIssueRequest{Type: new("TASK")}); err != nil || got.GetType().GetName() != "Task" {
		t.Errorf("edit with TASK = %q, %v; want Task", got.GetType().GetName(), err)
	}
	f.SetType(edited, "bug")
	f.SetOrgIssueTypes(OrgIssueType{Name: "Bug"}, OrgIssueType{Name: "Task"})
	if got, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Upper", Type: new("BUG")}); err != nil || got.GetType().GetName() != "Bug" {
		t.Errorf("create with BUG after the org listed Bug = %q, %v; want Bug", got.GetType().GetName(), err)
	}
}

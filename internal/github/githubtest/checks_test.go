package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

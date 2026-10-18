package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// checksWantStatus asserts that err is a wrapped GitHub error answering
// status, which what returned.
func checksWantStatus(t *testing.T, what string, err error, status int) {
	t.Helper()
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil || er.Response.StatusCode != status {
		t.Errorf("%s: %v; want a wrapped %d", what, err, status)
	}
}

// checksAnswerOnce makes the fake's next request at path answer status.
func checksAnswerOnce(f *githubtest.Fake, path string, status int) {
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == path }, status)
}

// A label that exists, in any case, is true, its name path-escaped. A
// missing one is false, with no error. Any other status is an error.
func TestLabelExists(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	for _, l := range []string{"Needs Design", "area/ui", "100%", "why?", "#7"} {
		f.AddRepoLabel(l)
	}
	ctx, c := context.Background(), f.Client(t)
	for name, want := range map[string]bool{
		"Needs Design": true, "needs design": true, "NEEDS DESIGN": true,
		"area/ui": true, "100%": true, "why?": true, "#7": true,
		"missing": false, "area": false, "why": false, "Needs": false,
	} {
		if got, err := c.LabelExists(ctx, "o", "r", name); err != nil || got != want {
			t.Errorf("LabelExists(%q) = %t, %v; want %t", name, got, err, want)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		checksAnswerOnce(f, "/labels/Needs Design", status)
		got, err := c.LabelExists(ctx, "o", "r", "Needs Design")
		checksWantStatus(t, fmt.Sprintf("LabelExists answered %d", status), err, status)
		if got {
			t.Errorf("LabelExists answered %d = true; want false", status)
		}
	}
}

// An org's issue types are listed with their enabled flags. A personal
// account, or an account that isn't the repo's owner, is no org, with no
// error. Any other status is an error.
func TestOrgIssueTypes(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	ctx, c := context.Background(), f.Client(t)
	types, isOrg, err := c.OrgIssueTypes(ctx, "o")
	if err != nil || !isOrg || len(types) != 0 {
		t.Errorf("OrgIssueTypes of an org with none = %v, %t, %v; want none, an org", types, isOrg, err)
	}
	f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}, githubtest.OrgIssueType{Name: "Epic", Disabled: true},
		githubtest.OrgIssueType{Name: "Task"})
	want := []github.IssueTypeInfo{{Name: "Bug", Enabled: true}, {Name: "Epic"}, {Name: "Task", Enabled: true}}
	if types, isOrg, err := c.OrgIssueTypes(ctx, "o"); err != nil || !isOrg || !reflect.DeepEqual(types, want) {
		t.Errorf("OrgIssueTypes = %+v, %t, %v; want %+v, an org", types, isOrg, err, want)
	}
	if types, isOrg, err := c.OrgIssueTypes(ctx, "elsewhere"); err != nil || isOrg || types != nil {
		t.Errorf("OrgIssueTypes of an account it can't see = %+v, %t, %v; want no org, no error", types, isOrg, err)
	}
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		checksAnswerOnce(f, "/orgs/o/issue-types", status)
		types, isOrg, err := c.OrgIssueTypes(ctx, "o")
		checksWantStatus(t, fmt.Sprintf("OrgIssueTypes answered %d", status), err, status)
		if isOrg || types != nil {
			t.Errorf("OrgIssueTypes answered %d = %+v, %t; want nothing", status, types, isOrg)
		}
	}
	f.SetPersonalAccount()
	if types, isOrg, err := c.OrgIssueTypes(ctx, "o"); err != nil || isOrg || types != nil {
		t.Errorf("OrgIssueTypes of a personal account = %+v, %t, %v; want no org, no error", types, isOrg, err)
	}
}

// Only the active rulesets' required checks are listed, org rulesets
// included, from every page. A branch with no rules, or only rules that
// aren't checks, is an empty list, and so is a 404. Any other status is
// an error.
func TestRequiredChecks(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetRequiredChecks("main",
		githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "ci"}, {Context: "lint", IntegrationID: new(int64(15368))}}},
		githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "trying"}}, Enforcement: gogithub.RulesetEnforcementEvaluate},
		githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "off"}}, Enforcement: gogithub.RulesetEnforcementDisabled},
		githubtest.Ruleset{},
		githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "org-ci"}}, Org: true, Enforcement: gogithub.RulesetEnforcementActive},
	)
	f.SetRequiredChecks("release/1.0", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "release"}}})
	f.SetRequiredChecks("bare", githubtest.Ruleset{}, githubtest.Ruleset{Org: true})
	f.SetRequiredChecks("trial", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "ci"}}, Enforcement: gogithub.RulesetEnforcementEvaluate})
	var many []githubtest.Ruleset
	var wantMany []github.RequiredCheck
	for i := range 120 {
		many = append(many, githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: fmt.Sprintf("check-%d", i)}}})
		wantMany = append(wantMany, github.RequiredCheck{Context: fmt.Sprintf("check-%d", i)})
	}
	f.SetRequiredChecks("big", many...)
	ctx, c := context.Background(), f.Client(t)
	for branch, want := range map[string][]github.RequiredCheck{
		"main":        {{Context: "ci"}, {Context: "lint", IntegrationID: 15368}, {Context: "org-ci"}},
		"release/1.0": {{Context: "release"}},
		"big":         wantMany, // 240 rules, over three pages
		"bare":        nil,
		"trial":       nil,
		"nowhere":     nil,
	} {
		if got, err := c.RequiredChecks(ctx, "o", "r", branch); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("RequiredChecks(%s) = %+v, %v; want %+v", branch, got, err, want)
		}
	}
	checksAnswerOnce(f, "/rules/branches/main", http.StatusNotFound)
	if got, err := c.RequiredChecks(ctx, "o", "r", "main"); err != nil || len(got) != 0 {
		t.Errorf("RequiredChecks answered 404 = %+v, %v; want none, no error", got, err)
	}
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		checksAnswerOnce(f, "/rules/branches/main", status)
		got, err := c.RequiredChecks(ctx, "o", "r", "main")
		checksWantStatus(t, fmt.Sprintf("RequiredChecks answered %d", status), err, status)
		if got != nil {
			t.Errorf("RequiredChecks answered %d = %+v; want nothing", status, got)
		}
	}
	// A failure on a later page fails the read too.
	pageTwo := func(r githubtest.Request) bool {
		return r.Path == "/rules/branches/big" && strings.Contains(r.Query, "page=2")
	}
	f.AnswerOnce(pageTwo, http.StatusInternalServerError)
	got, err := c.RequiredChecks(ctx, "o", "r", "big")
	checksWantStatus(t, "RequiredChecks with page 2 answering 500", err, http.StatusInternalServerError)
	if got != nil {
		t.Errorf("RequiredChecks with page 2 answering 500 = %+v; want nothing", got)
	}
}

// The installations are read with the App's JWT, from every page, each
// with its account and the time it was suspended. Any failure is an error.
func TestInstallations(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	ctx, c := context.Background(), f.Client(t)
	got, err := c.Installations(ctx)
	if want := []github.InstallationInfo{{Login: "o"}}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Installations = %+v, %v; want %+v, the default", got, err, want)
	}
	suspended := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := []github.InstallationInfo{{Login: "o"}, {Login: "paused", SuspendedAt: suspended}}
	for i := range 148 {
		want = append(want, github.InstallationInfo{Login: fmt.Sprintf("user-%d", i)})
	}
	var installs []githubtest.Installation
	for _, w := range want {
		installs = append(installs, githubtest.Installation{Login: w.Login, SuspendedAt: w.SuspendedAt})
	}
	f.SetInstallations(installs...)
	from := len(f.Requests())
	got, err = c.Installations(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Installations = %d of them, %v; want %d, the second suspended at %v", len(got), err, len(want), suspended)
	} else if !got[1].SuspendedAt.Equal(suspended) || !got[0].SuspendedAt.IsZero() {
		t.Errorf("suspended at %v and %v; want %v and zero", got[1].SuspendedAt, got[0].SuspendedAt, suspended)
	}
	var reads []githubtest.Request
	for _, r := range f.Requests()[from:] {
		reads = append(reads, githubtest.Request{Method: r.Method, Path: r.Path, Status: r.Status})
	}
	read := githubtest.Request{Method: "GET", Path: "/app/installations", Status: 200}
	wantReads := []githubtest.Request{read, read}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Errorf("requests %+v; want two pages of GET /app/installations with the JWT, and no token minted", reads)
	}
	f.SetInstallations()
	if got, err := c.Installations(ctx); err != nil || len(got) != 0 {
		t.Errorf("Installations of none = %+v, %v; want none", got, err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		checksAnswerOnce(f, "/app/installations", status)
		got, err := c.Installations(ctx)
		checksWantStatus(t, fmt.Sprintf("Installations answered %d", status), err, status)
		if got != nil {
			t.Errorf("Installations answered %d = %+v; want nothing", status, got)
		}
	}
}

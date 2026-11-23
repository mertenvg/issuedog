package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// The reads behind a human task's checks (#250): whether a label exists,
// an org's issue types, the required checks on a branch, and the App's
// installations. Startup's check of the issuedog branches ruleset reads
// the rule types on a branch (BranchRuleTypes). Each fits the App's permissions in §15, and none needs
// admin. A thing that isn't there is an answer, not an error: a missing
// label, a personal account with no issue types, and a branch with no
// rules. Any other failure, such as a 403 or a 500, is an error.

// IssueTypeInfo is one of an org's issue types: its name, its ID, which
// stays the same through a rename, and whether the org has it enabled.
// GitHub lists disabled ones too.
type IssueTypeInfo struct {
	Name    string
	ID      int64
	Enabled bool
}

// RequiredCheck is one status check a ruleset requires: its context, and
// the ID of the App that must set it, or 0 for any source.
type RequiredCheck struct {
	Context       string
	IntegrationID int64
}

// InstallationInfo is one of the App's installations: the login of the
// account it's on, and when it was suspended, or zero if it isn't.
type InstallationInfo struct {
	Login       string
	SuspendedAt time.Time
}

// LabelExists reports whether the repo has a label named name, matched
// ignoring case, as GitHub matches label names. It reads GET
// /repos/{owner}/{repo}/labels/{name}, with name path-escaped, which needs
// the Issues or Pull requests read permission. A 404 is false, with no
// error.
func (c *Client) LabelExists(ctx context.Context, owner, repo, name string) (bool, error) {
	_, resp, err := c.Issues.GetLabel(ctx, owner, repo, url.PathEscape(name))
	switch {
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("label %q in %s/%s: %w", name, owner, repo, err)
	}
	return true, nil
}

// OrgIssueTypes lists org's issue types, each with its enabled flag, and
// reports whether org is an org. It reads GET /orgs/{org}/issue-types,
// which needs the organization permission Issue Types read. A 404 gives
// isOrg false, with no error: a personal account, which has no issue
// types, or an org the App can't see.
func (c *Client) OrgIssueTypes(ctx context.Context, org string) (types []IssueTypeInfo, isOrg bool, err error) {
	list, resp, err := c.Organizations.ListIssueTypes(ctx, org)
	switch {
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("issue types of %s: %w", org, err)
	}
	for _, t := range list {
		types = append(types, IssueTypeInfo{Name: t.GetName(), ID: t.GetID(), Enabled: t.GetIsEnabled()})
	}
	return types, true, nil
}

// RequiredChecks lists the status checks that the active rulesets on
// branch require, org rulesets included, in GitHub's order, reading every
// page. It reads GET /repos/{owner}/{repo}/rules/branches/{branch}, which
// needs the Metadata read permission and gives only active rules: a
// ruleset in evaluate or disabled isn't listed. Classic branch protection
// needs Administration read, which the App doesn't have, so it doesn't
// count. A 404 is an empty list, with no error.
func (c *Client) RequiredChecks(ctx context.Context, owner, repo, branch string) ([]RequiredCheck, error) {
	var checks []RequiredCheck
	opts := &gogithub.ListOptions{PerPage: 100}
	for {
		rules, resp, err := c.Repositories.ListRulesForBranch(ctx, owner, repo, branch, opts)
		switch {
		case resp != nil && resp.StatusCode == http.StatusNotFound:
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("rules on %s in %s/%s: %w", branch, owner, repo, err)
		}
		for _, rule := range rules.RequiredStatusChecks {
			for _, ch := range rule.Parameters.RequiredStatusChecks {
				checks = append(checks, RequiredCheck{Context: ch.Context, IntegrationID: ch.GetIntegrationID()})
			}
		}
		if resp.NextPage == 0 {
			return checks, nil
		}
		opts.Page = resp.NextPage
	}
}

// BranchRuleTypes lists the type of each rule the active rulesets on
// branch carry, such as "creation", org rulesets included, reading every
// page: one entry a rule, so a type two rulesets carry is listed twice. It
// reads the same endpoint as RequiredChecks, with the same permission, and
// like it gives only active rules. GitHub matches each ruleset's branch
// patterns against the name, so branch needn't exist. go-github decodes
// the rules into a field per type, and drops a type it doesn't know, so
// the types are listed by field (branchRuleTypes), not in GitHub's order,
// and an unknown one isn't listed. A 404 is an empty list, with no error.
func (c *Client) BranchRuleTypes(ctx context.Context, owner, repo, branch string) ([]string, error) {
	var types []string
	opts := &gogithub.ListOptions{PerPage: 100}
	for {
		rules, resp, err := c.Repositories.ListRulesForBranch(ctx, owner, repo, branch, opts)
		switch {
		case resp != nil && resp.StatusCode == http.StatusNotFound:
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("rule types on %s in %s/%s: %w", branch, owner, repo, err)
		}
		types = append(types, branchRuleTypes(rules)...)
		if resp.NextPage == 0 {
			return types, nil
		}
		opts.Page = resp.NextPage
	}
}

// branchRuleTypes is the type of each rule in rules, one entry a rule, by
// go-github's field for the type. creation, deletion and non_fast_forward
// carry no parameters, and each still has its own field.
func branchRuleTypes(rules *gogithub.BranchRules) []string {
	if rules == nil {
		return nil
	}
	var types []string
	for _, field := range []struct {
		typ   gogithub.RepositoryRuleType
		count int
	}{
		{gogithub.RulesetRuleTypeCreation, len(rules.Creation)},
		{gogithub.RulesetRuleTypeUpdate, len(rules.Update)},
		{gogithub.RulesetRuleTypeDeletion, len(rules.Deletion)},
		{gogithub.RulesetRuleTypeRequiredLinearHistory, len(rules.RequiredLinearHistory)},
		{gogithub.RulesetRuleTypeMergeQueue, len(rules.MergeQueue)},
		{gogithub.RulesetRuleTypeRequiredDeployments, len(rules.RequiredDeployments)},
		{gogithub.RulesetRuleTypeRequiredSignatures, len(rules.RequiredSignatures)},
		{gogithub.RulesetRuleTypePullRequest, len(rules.PullRequest)},
		{gogithub.RulesetRuleTypeRequiredStatusChecks, len(rules.RequiredStatusChecks)},
		{gogithub.RulesetRuleTypeNonFastForward, len(rules.NonFastForward)},
		{gogithub.RulesetRuleTypeCommitMessagePattern, len(rules.CommitMessagePattern)},
		{gogithub.RulesetRuleTypeCommitAuthorEmailPattern, len(rules.CommitAuthorEmailPattern)},
		{gogithub.RulesetRuleTypeCommitterEmailPattern, len(rules.CommitterEmailPattern)},
		{gogithub.RulesetRuleTypeBranchNamePattern, len(rules.BranchNamePattern)},
		{gogithub.RulesetRuleTypeTagNamePattern, len(rules.TagNamePattern)},
		{gogithub.RulesetRuleTypeWorkflows, len(rules.Workflows)},
		{gogithub.RulesetRuleTypeCodeScanning, len(rules.CodeScanning)},
		{gogithub.RulesetRuleTypeCopilotCodeReview, len(rules.CopilotCodeReview)},
		{gogithub.RulesetRuleTypeFileExtensionRestriction, len(rules.FileExtensionRestriction)},
		{gogithub.RulesetRuleTypeFilePathRestriction, len(rules.FilePathRestriction)},
		{gogithub.RulesetRuleTypeMaxFilePathLength, len(rules.MaxFilePathLength)},
		{gogithub.RulesetRuleTypeMaxFileSize, len(rules.MaxFileSize)},
	} {
		for range field.count {
			types = append(types, string(field.typ))
		}
	}
	return types
}

// Installations lists the App's installations, reading every page, each
// with its account's login and when it was suspended. It reads GET
// /app/installations, which only the App itself may call, with its JWT
// (c.app), as AppSlug does: an installation token is refused there. It
// needs no permission of the App's.
func (c *Client) Installations(ctx context.Context) ([]InstallationInfo, error) {
	all, err := collect(c.app.Apps.ListInstallationsIter(ctx, &gogithub.ListOptions{PerPage: 100}))
	if err != nil {
		return nil, fmt.Errorf("github app installations: %w", err)
	}
	installs := make([]InstallationInfo, 0, len(all))
	for _, in := range all {
		installs = append(installs, InstallationInfo{Login: in.GetAccount().GetLogin(), SuspendedAt: in.GetSuspendedAt().Time})
	}
	return installs, nil
}

package githubtest

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// The reads behind a human task's checks (#282): a repo label by name, the
// org's issue types, the rules on a branch, and the App's installations.
// Each serves what its setter gave. The owner of the repo is an org unless
// SetPersonalAccount says otherwise.

// checkState is what the check reads answer.
type checkState struct {
	personal      bool                 // see SetPersonalAccount
	issueTypes    []OrgIssueType       // see SetOrgIssueTypes
	typesSet      bool                 // SetOrgIssueTypes was called, so issueTypes replaces the default
	rulesets      map[string][]ruleset // by branch, see SetRequiredChecks
	lastRuleset   int64
	installations []Installation // see SetInstallations
	installsSet   bool           // SetInstallations was called, so installations replaces the default
	typesRefused  int            // see TypeGoneRefuseList
}

// OrgIssueType is one of the org's issue types, for SetOrgIssueTypes: its
// name, and whether the org disabled it, which GitHub still lists, with
// is_enabled false.
type OrgIssueType struct {
	Name     string
	Disabled bool
}

// Ruleset is one ruleset that targets a branch, for SetRequiredChecks: the
// status checks it requires, its enforcement, and whether it is the org's
// rather than the repo's. Enforcement "" means active. As on GitHub, only
// an active ruleset's rules are served: one in evaluate or disabled is left
// out.
type Ruleset struct {
	Checks      []gogithub.RuleStatusCheck
	Enforcement gogithub.RulesetEnforcement
	Org         bool
}

// ruleset is a Ruleset with the ID it was given.
type ruleset struct {
	Ruleset
	id int64
}

// Installation is one of the App's installations, for SetInstallations:
// the account's login, and when the installation was suspended, or zero if
// it isn't.
type Installation struct {
	Login       string
	SuspendedAt time.Time
}

// handleChecks registers the check reads. GET /app/installations, like GET
// /app, is the App's own and needs its JWT.
func (f *Fake) handleChecks() {
	f.handle("GET /labels/{name}", f.getLabel)
	f.mux.HandleFunc("GET /orgs/{org}/issue-types", func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, false, f.orgIssueTypes)
	})
	f.handle("GET /rules/branches/{branch}", f.branchRules)
	f.mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			send(w, http.StatusUnauthorized, message("GET /app/installations needs the App's JWT"))
			return
		}
		f.serve(w, r, false, f.listInstallations)
	})
}

// SetPersonalAccount makes the repo's owner a personal account, which has
// no issue types: GET /orgs/{owner}/issue-types answers 404, as GitHub does,
// and a create or an edit that sets any type gets a 422 (fakeTypeRefusal).
func (f *Fake) SetPersonalAccount() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks.personal = true
}

// SetOrgIssueTypes sets the issue types the org lists, in order, each with
// the ID IssueTypeID gives its name, replacing the default: every type the
// org has given an ID, enabled. Each name is the one the org shows from
// then on, so a type set by its name in another case is served by this
// one. RenameOrgIssueType renames a type in the list too. A type left out
// of the list is one the org no longer has, as once it deleted it, though
// the fake still sets it on an issue. A disabled one is refused on a
// create or an edit (fakeTypeRefusal).
func (f *Fake) SetOrgIssueTypes(types ...OrgIssueType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks.issueTypes, f.checks.typesSet = slices.Clone(types), true
	for _, t := range types {
		if id := f.issueTypeNamed(t.Name).GetID(); id != 0 { // gives the name its ID now
			f.orgTypeNames[id] = t.Name
		}
	}
}

// fakeTypeRefusal is the 422 GitHub answers a create or an edit that sets
// the issue type named name, or 0 and nil if it may: any type on a personal
// account (SetPersonalAccount), which has none, and one the org's list
// (SetOrgIssueTypes) marks disabled, matched ignoring case. f is locked.
func (f *Fake) fakeTypeRefusal(name string) (int, any) {
	if f.checks.personal {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: issue type %q: a personal account's repository has no issue types", name))
	}
	for _, t := range f.checks.issueTypes {
		if t.Disabled && strings.EqualFold(t.Name, name) {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: the org's issue type %q is disabled", t.Name))
		}
	}
	return 0, nil
}

// TypeGoneRefuseList makes GET /orgs/{org}/issue-types answer status, such
// as a 403 for an App without the organization permission Issue Types
// read, on every read until it's called with 0 (#409).
func (f *Fake) TypeGoneRefuseList(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks.typesRefused = status
}

// SetRequiredChecks sets the rulesets that target branch, by its exact
// name, replacing any set before. Each gets a new ID. A branch with none
// has no rules.
func (f *Fake) SetRequiredChecks(branch string, rulesets ...Ruleset) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checks.rulesets == nil {
		f.checks.rulesets = map[string][]ruleset{}
	}
	var set []ruleset
	for _, rs := range rulesets {
		f.checks.lastRuleset++
		set = append(set, ruleset{Ruleset: rs, id: f.checks.lastRuleset})
	}
	f.checks.rulesets[branch] = set
}

// SetInstallations sets the App's installations, in order, replacing the
// default: one on the repo's owner (ID 1, as Client's), not suspended.
func (f *Fake) SetInstallations(installs ...Installation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks.installations, f.checks.installsSet = slices.Clone(installs), true
}

// getLabel serves the repo label named name, matched ignoring case, as
// GitHub matches label names, or a 404.
func (f *Fake) getLabel(c *call) (int, any) {
	name := c.r.PathValue("name")
	i := slices.IndexFunc(f.labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), name) })
	if i < 0 {
		return http.StatusNotFound, message("Not Found")
	}
	return http.StatusOK, f.labels[i]
}

// orgIssueTypes serves the org's issue types (SetOrgIssueTypes), or by
// default every type the org has given an ID (IssueTypeID), in the order it
// gave them, enabled, by the name it shows now (RenameOrgIssueType). It
// answers a 404 for any account but the repo's owner, and for the owner
// once SetPersonalAccount made it a personal account, and the status
// TypeGoneRefuseList gave, if any, for every account.
func (f *Fake) orgIssueTypes(c *call) (int, any) {
	if f.checks.typesRefused != 0 {
		return f.checks.typesRefused, message("Resource not accessible by integration")
	}
	owner, _, _ := strings.Cut(f.repo, "/")
	if f.checks.personal || !strings.EqualFold(c.r.PathValue("org"), owner) {
		return http.StatusNotFound, message("Not Found")
	}
	out := []gogithub.IssueType{}
	if !f.checks.typesSet {
		for _, id := range slices.Sorted(maps.Keys(f.orgTypeNames)) {
			out = append(out, gogithub.IssueType{ID: new(id), Name: new(f.orgTypeNames[id]), IsEnabled: new(true)})
		}
		return http.StatusOK, out
	}
	for _, t := range f.checks.issueTypes {
		named := f.issueTypeNamed(t.Name)
		out = append(out, gogithub.IssueType{ID: named.ID, Name: new(t.Name), IsEnabled: new(!t.Disabled)})
	}
	return http.StatusOK, out
}

// branchRule is one rule as GET /rules/branches/{branch} serves it.
type branchRule struct {
	Type string `json:"type"`
	gogithub.BranchRuleMetadata
	Parameters any `json:"parameters,omitempty"`
}

// branchRules serves the rules of the active rulesets that target branch,
// paged: each ruleset's deletion rule, which the rulesets GitHub suggests
// carry, and its required_status_checks rule if it requires any check. A
// branch with none answers an empty list, as on GitHub, where it needn't
// exist.
func (f *Fake) branchRules(c *call) (int, any) {
	owner, _, _ := strings.Cut(f.repo, "/")
	out := []branchRule{}
	for _, rs := range f.checks.rulesets[c.r.PathValue("branch")] {
		if rs.Enforcement != "" && rs.Enforcement != gogithub.RulesetEnforcementActive {
			continue
		}
		meta := gogithub.BranchRuleMetadata{RulesetSourceType: gogithub.RulesetSourceTypeRepository, RulesetSource: f.repo, RulesetID: rs.id}
		if rs.Org {
			meta.RulesetSourceType, meta.RulesetSource = gogithub.RulesetSourceTypeOrganization, owner
		}
		out = append(out, branchRule{Type: string(gogithub.RulesetRuleTypeDeletion), BranchRuleMetadata: meta})
		if len(rs.Checks) > 0 {
			var checks []*gogithub.RuleStatusCheck
			for _, ch := range rs.Checks {
				checks = append(checks, &ch)
			}
			out = append(out, branchRule{Type: string(gogithub.RulesetRuleTypeRequiredStatusChecks), BranchRuleMetadata: meta,
				Parameters: gogithub.RequiredStatusChecksRuleParameters{RequiredStatusChecks: checks}})
		}
	}
	return page(c, out)
}

// listInstallations serves the App's installations (SetInstallations),
// paged, each with an ID from 1 and its account's login.
func (f *Fake) listInstallations(c *call) (int, any) {
	installs := f.checks.installations
	if !f.checks.installsSet {
		owner, _, _ := strings.Cut(f.repo, "/")
		installs = []Installation{{Login: owner}}
	}
	out := []gogithub.Installation{}
	for i, in := range installs {
		inst := gogithub.Installation{ID: new(int64(i + 1)), AppID: new(int64(1)), Account: user(in.Login)}
		if !in.SuspendedAt.IsZero() {
			inst.SuspendedAt = &gogithub.Timestamp{Time: in.SuspendedAt}
		}
		out = append(out, inst)
	}
	return page(c, out)
}

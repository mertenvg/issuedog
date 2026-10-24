package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// checkFromPlan is the check that a human task's "Done when" d records on its
// origin marker (state.CheckFields, SPEC §5, "The check"): the zero Check for
// plain text, or else d's check, by the same kind names, with its one value
// and a file check's contains. It reuses #247's validation (validCheck)
// rather than adding its own, so a check the plan's validation would refuse
// is an error here too, and never recorded (SPEC §11.3, "A human task's
// check": only a check the validation accepted is recorded).
func checkFromPlan(d planDoneWhen) (state.Check, error) {
	c := d.Check
	if c == nil {
		return state.Check{}, nil
	}
	if err := validCheck(*c); err != nil {
		return state.Check{}, fmt.Errorf("convert the done-when check: %w", err)
	}
	value := map[string]string{
		planCheckLabel: c.Label, planCheckIssueType: c.IssueType, planCheckFile: c.Path,
		planCheckRuleset: c.Check, planCheckInstallation: c.Owner,
	}[c.Kind]
	return state.Check{Kind: c.Kind, Value: value, Contains: c.Contains}, nil
}

// humanCheck runs a human task's check c (SPEC §9.3, "The checks"), and
// reports whether it passes, and found, the App's sentence saying what it
// saw, for the reply to a failure. Every value in found is inline code,
// escaped (checkCode). It never quotes a file's contents. A plain-text "Done
// when", the zero Check, has nothing to run, and passes. A check whose
// origin marker fields couldn't be read, state.CheckUnreadable, reads
// nothing and fails, saying so, so the task never closes without its
// check; found quotes none of the fields.
//
//   - label: the repo has the label, matched ignoring case (LabelExists);
//   - issue-type: the repo's owner has an enabled issue type of that name,
//     ignoring case (OrgIssueTypes). A personal account has none, and
//     fails;
//   - file: main has the file, in the daemon's bare clone (ReadMain), and
//     if contains is set, the file holds it as exact text. A path main
//     doesn't have, or has a directory at, fails;
//   - ruleset: an active ruleset on main requires a check whose context is
//     the name (RequiredChecks). A failure lists the contexts the rules do
//     require, or says none do;
//   - installation: the App has an installation on an account whose login
//     is the owner, ignoring case, that isn't suspended (Installations).
//
// A GitHub or git read that fails is err, wrapped, and never a failed
// check: the caller tries again on a later pass.
func (a *Applier) humanCheck(ctx context.Context, c state.Check) (pass bool, found string, err error) {
	switch c.Kind {
	case "":
		return true, `The "Done when" has no check for me to run.`, nil
	case state.CheckUnreadable:
		return false, "I couldn't read the check on this task's `origin` marker, so it can't pass.", nil
	case state.CheckLabel:
		ok, err := a.Client.LabelExists(ctx, a.Owner, a.Repo, c.Value)
		if err != nil {
			return false, "", fmt.Errorf("run the label check: %w", err)
		}
		if ok {
			return true, fmt.Sprintf("The label %s exists.", checkCode(c.Value)), nil
		}
		return false, fmt.Sprintf("This repo has no label %s.", checkCode(c.Value)), nil
	case state.CheckIssueType:
		return a.humanCheckIssueType(ctx, c.Value)
	case state.CheckFile:
		return a.humanCheckFile(ctx, c)
	case state.CheckRuleset:
		return a.humanCheckRuleset(ctx, c.Value)
	case state.CheckInstallation:
		return a.humanCheckInstallation(ctx, c.Value)
	}
	return false, "", fmt.Errorf("run the check: unknown kind %q", c.Kind)
}

// humanCheckIssueType is humanCheck's issue-type check, of the type name.
func (a *Applier) humanCheckIssueType(ctx context.Context, name string) (bool, string, error) {
	types, isOrg, err := a.Client.OrgIssueTypes(ctx, a.Owner)
	if err != nil {
		return false, "", fmt.Errorf("run the issue-type check: %w", err)
	}
	owner, v := checkCode(a.Owner), checkCode(name)
	if !isOrg {
		return false, fmt.Sprintf("%s is a personal account, which has no issue types.", owner), nil
	}
	disabled := false
	for _, t := range types {
		if strings.EqualFold(t.Name, name) {
			if t.Enabled {
				return true, fmt.Sprintf("%s has the issue type %s, enabled.", owner, v), nil
			}
			disabled = true
		}
	}
	if disabled {
		return false, fmt.Sprintf("%s has the issue type %s, but it's disabled.", owner, v), nil
	}
	return false, fmt.Sprintf("%s has no issue type %s.", owner, v), nil
}

// humanCheckFile is humanCheck's file check: c.Value is the path, and
// c.Contains the text the file must hold, if any.
func (a *Applier) humanCheckFile(ctx context.Context, c state.Check) (bool, string, error) {
	if a.ReadMain == nil {
		return false, "", errors.New("run the file check: no clone of main to read")
	}
	path := checkCode(c.Value)
	data, err := a.ReadMain(ctx, c.Value)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return false, fmt.Sprintf("There's no file %s on `main`.", path), nil
	case errors.Is(err, repo.ErrNotFile):
		return false, fmt.Sprintf("%s on `main` isn't a file: it may be a directory.", path), nil
	case err != nil:
		return false, "", fmt.Errorf("run the file check: %w", err)
	case c.Contains == "":
		return true, fmt.Sprintf("The file %s is on `main`.", path), nil
	case bytes.Contains(data, []byte(c.Contains)):
		return true, fmt.Sprintf("The file %s on `main` contains %s.", path, checkCode(c.Contains)), nil
	}
	return false, fmt.Sprintf("The file %s is on `main`, but doesn't contain %s.", path, checkCode(c.Contains)), nil
}

// humanCheckRuleset is humanCheck's ruleset check, of the check's name.
func (a *Applier) humanCheckRuleset(ctx context.Context, name string) (bool, string, error) {
	checks, err := a.Client.RequiredChecks(ctx, a.Owner, a.Repo, "main")
	if err != nil {
		return false, "", fmt.Errorf("run the ruleset check: %w", err)
	}
	v := checkCode(name)
	var required []string
	for _, ch := range checks {
		if ch.Context == name {
			return true, fmt.Sprintf("A ruleset on `main` requires the check %s.", v), nil
		}
		if code := checkCode(ch.Context); !slices.Contains(required, code) {
			required = append(required, code)
		}
	}
	if len(required) == 0 {
		return false, fmt.Sprintf("No ruleset on `main` requires the check %s: none requires any check.", v), nil
	}
	return false, fmt.Sprintf("No ruleset on `main` requires the check %s. They require %s.", v, strings.Join(required, ", ")), nil
}

// humanCheckInstallation is humanCheck's installation check, of the owner's
// login.
func (a *Applier) humanCheckInstallation(ctx context.Context, owner string) (bool, string, error) {
	installs, err := a.Client.Installations(ctx)
	if err != nil {
		return false, "", fmt.Errorf("run the installation check: %w", err)
	}
	v := checkCode(owner)
	suspended := false
	for _, in := range installs {
		if strings.EqualFold(in.Login, owner) {
			if in.SuspendedAt.IsZero() {
				return true, fmt.Sprintf("The App is installed on %s.", v), nil
			}
			suspended = true
		}
	}
	if suspended {
		return false, fmt.Sprintf("The App's installation on %s is suspended.", v), nil
	}
	return false, fmt.Sprintf("The App isn't installed on %s.", v), nil
}

// checkCode is s as an inline code span, for a value in humanCheck's
// sentence: normalized to one line, its markers escaped (SPEC §5.1), and
// fenced by one more backtick than its longest run of them, with a space
// inside each fence if s starts or ends with a backtick or a space, which
// CommonMark strips, so it reads as it is. A code span links no mention
// and makes no HTML. An empty s is one space. The fence is
// github.TypeGoneCodeSpan's, which a revert of the issue type's comment
// uses too (#409).
func checkCode(s string) string {
	return github.TypeGoneCodeSpan(github.Escape(normalize(s, "")))
}

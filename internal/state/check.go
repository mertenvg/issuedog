package state

import (
	"slices"

	"github.com/mertenvg/issuedog/internal/github"
)

// The kinds of a human task's "Done when" check (SPEC §11.3): one set of
// names everywhere, the planner's schema, the plan data, the origin marker
// and the checks the daemon runs.
const (
	CheckLabel        = "label"        // a label exists
	CheckIssueType    = "issue-type"   // an enabled issue type exists
	CheckFile         = "file"         // a file is on main, optionally containing some text
	CheckRuleset      = "ruleset"      // a ruleset on main requires a check
	CheckInstallation = "installation" // the App is installed on an owner
)

// checkKinds are the kinds a Check can have.
var checkKinds = []string{CheckLabel, CheckIssueType, CheckFile, CheckRuleset, CheckInstallation}

// CheckUnreadable is the Kind of the Check that checkOf reads from origin
// marker fields that don't record a whole check (SPEC §5, "The check"),
// such as an unknown kind or a value that doesn't decode. It has no value.
// It isn't one of checkKinds: no plan has it, so the App never writes it.
// The daemon's check fails it, saying the check couldn't be read (SPEC
// §9.3, "The checks"), so the task never closes without its check.
const CheckUnreadable = "unreadable"

// Check is a human task's "Done when" check (SPEC §9.3), as its origin
// marker records it (SPEC §5): a kind, and its one value, which is
// the label, the issue type, the file's path, the required check's name or
// the owner. Contains is a file check's optional text, and "" for every
// other kind. The zero Check is a plain-text "Done when", which the daemon
// doesn't run, and a Check of kind CheckUnreadable one whose fields
// couldn't be read, which always fails.
//
// It holds values that were valid when the plan was, so it adds no
// validation of its own: the daemon converts the plan's check after plan
// validation accepted it (SPEC §11.3).
type Check struct{ Kind, Value, Contains string }

// CheckFields are the origin marker fields that record c (SPEC §5):
// check=<kind> and value=, and for a file check with text, contains=, each
// value percent-encoded (github.EncodeValue), so a space, "<", ">" or "%"
// reads back exactly. A plain-text Check has none, so its marker is as it
// was.
func CheckFields(c Check) map[string]string {
	if c.Kind == "" {
		return nil
	}
	f := map[string]string{"check": c.Kind, "value": github.EncodeValue(c.Value)}
	if c.Kind == CheckFile && c.Contains != "" {
		f["contains"] = github.EncodeValue(c.Contains)
	}
	return f
}

// checkOf is the Check that an origin marker's fields record
// (CheckFields): the zero Check if they have none of check, value and
// contains, as a plain-text "Done when"'s marker has none, or else the
// whole check they record (wholeCheck). Fields that don't record a whole
// one, even a value or contains with no check, are the CheckUnreadable
// Check, which fails, never the zero Check, which passes.
func checkOf(fields map[string]string) Check {
	_, check := fields["check"]
	_, value := fields["value"]
	_, contains := fields["contains"]
	if !check && !value && !contains {
		return Check{}
	}
	if c, ok := wholeCheck(fields); ok {
		return c
	}
	return Check{Kind: CheckUnreadable}
}

// wholeCheck is the whole check that fields record, for checkOf, or false
// if they don't record one: a known kind, a value that decodes and isn't
// empty, and contains only on a file check, decoding too.
func wholeCheck(fields map[string]string) (Check, bool) {
	kind := fields["check"]
	if !slices.Contains(checkKinds, kind) {
		return Check{}, false
	}
	value, err := github.DecodeValue(fields["value"])
	if err != nil || value == "" {
		return Check{}, false
	}
	c := Check{Kind: kind, Value: value}
	if raw, ok := fields["contains"]; ok {
		contains, err := github.DecodeValue(raw)
		if err != nil || kind != CheckFile {
			return Check{}, false
		}
		c.Contains = contains
	}
	return c, true
}

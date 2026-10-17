package github

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	gogithub "github.com/google/go-github/v92/github"
)

// labelSpec is one label the pipeline relies on.
type labelSpec struct {
	name, color, description string
}

// scopeColor is shared by every scope:* label.
const scopeColor = "bfd4f2"

// fixedLabels are the labels every repo gets (SPEC section 15 step 5). The
// colors and descriptions match the ones this repo was set up with by hand.
var fixedLabels = []labelSpec{
	{"stage:intake", "ededed", "Owner's rough idea, not yet picked up"},
	{"stage:clarifying", "fbca04", "Waiting on answers to a question round"},
	{"stage:spec-review", "fbca04", "Spec written; waiting for /approve or /revise"},
	{"stage:planning", "c5def5", "Planner is proposing stories or tasks"},
	{"stage:plan-review", "fbca04", "Plan posted; waiting for /approve or /revise"},
	{"stage:planned", "c5def5", "Sub-issues created; closes when children close"},
	{"stage:ready", "0e8a16", "Task waiting for the scheduler"},
	{"stage:in-progress", "1d76db", "Implementer is running"},
	{"stage:verifying", "1d76db", "Deterministic checks and verifier running"},
	{"stage:pr-open", "5319e7", "PR waiting for human review"},
	{"stage:needs-human", "d93f0b", "Task only the owner can do"},
	{"stage:held", "000000", "Paused by the owner"},
	{"stage:completed", "0e8a16", "Closed as completed, or by a merged PR"},
	{"stage:discarded", "cfd3d7", "Closed as not planned, or as a duplicate"},
	{"priority:p0", "b60205", "Drop everything"},
	{"priority:p1", "d93f0b", "High"},
	{"priority:p2", "fbca04", "Normal (default)"},
	{"priority:p3", "c2e0c6", "Low"},
	{"approved", "0e8a16", "A trusted user approved this issue for the pipeline"},
	{"review:high-risk", "b60205", "Matched an escalation rule; look at this first"},
}

// typeLabels stand in for issue types on repos owned by a personal account
// (SPEC section 4).
var typeLabels = []labelSpec{
	{"type:epic", "3e4b9e", "A rough idea or large feature"},
	{"type:story", "3e4b9e", "One user-visible outcome with acceptance criteria"},
	{"type:task", "3e4b9e", "One PR, one scope"},
	{"type:bug", "b60205", "Something broken"},
}

// The prefixes of the model and effort override labels. model:* and effort:*
// apply to every run on an issue but the verifier's; verifier-model:* and
// verifier-effort:* apply only to the verifier's (#93's plan, assumption 4).
const (
	ModelPrefix          = "model:"
	EffortPrefix         = "effort:"
	VerifierModelPrefix  = "verifier-model:"
	VerifierEffortPrefix = "verifier-effort:"
)

// Colors of the model and effort override labels, one per kind.
const (
	modelColor          = "d4c5f9"
	verifierModelColor  = "bfdadc"
	effortColor         = "fef2c0"
	verifierEffortColor = "f9d0c4"
)

// descriptionMax is GitHub's cap on a label description, in characters.
const descriptionMax = 100

// LabelOptions holds the parts of the repo config that decide which labels
// exist beyond the fixed set.
type LabelOptions struct {
	// Scopes are the allowed scope names, without the scope: prefix.
	Scopes []string
	// TypeLabels is true when the repo uses type:* labels (types: labels).
	TypeLabels bool
	// Models are the allowed models, each with a model:<suffix> and a
	// verifier-model:<suffix> label.
	Models []ModelLabel
	// Efforts are the effort levels, each with an effort:<level> and a
	// verifier-effort:<level> label.
	Efforts []string
}

// ModelLabel is one allowed model: its label suffix and its full name.
type ModelLabel struct {
	Suffix, Name string
}

// wantedLabels returns every label the repo should have under opts.
func wantedLabels(opts LabelOptions) []labelSpec {
	want := append([]labelSpec(nil), fixedLabels...)
	for _, s := range opts.Scopes {
		want = append(want, labelSpec{"scope:" + s, scopeColor, "Work in the " + s + " area"})
	}
	if opts.TypeLabels {
		want = append(want, typeLabels...)
	}
	for _, m := range opts.Models {
		want = append(want,
			labelSpec{ModelPrefix + m.Suffix, modelColor, describe("Runs other than the verifier's use ", m.Name)},
			labelSpec{VerifierModelPrefix + m.Suffix, verifierModelColor, describe("The verifier's runs use ", m.Name)})
	}
	for _, e := range opts.Efforts {
		want = append(want,
			labelSpec{EffortPrefix + e, effortColor, "Runs other than the verifier's use " + e + " effort"},
			labelSpec{VerifierEffortPrefix + e, verifierEffortColor, "The verifier's runs use " + e + " effort"})
	}
	return want
}

// describe is prefix and name, with the start of name cut to fit GitHub's cap
// on a description. A long model name differs in its end.
func describe(prefix, name string) string {
	r := []rune(name)
	if room := descriptionMax - utf8.RuneCountInString(prefix); len(r) > room {
		name = "…" + string(r[len(r)-room+1:])
	}
	return prefix + name
}

// EnsureLabels creates the pipeline labels missing from owner/repo and
// returns the names it created. It never edits or deletes a label, so an
// existing label keeps whatever color and description it has. GitHub label
// names are case-insensitive, and so is the match.
func EnsureLabels(ctx context.Context, c *Client, owner, repo string, opts LabelOptions) ([]string, error) {
	have := map[string]bool{}
	for l, err := range c.Issues.ListLabelsIter(ctx, owner, repo, &gogithub.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("list labels of %s/%s: %w", owner, repo, err)
		}
		have[strings.ToLower(l.Name)] = true
	}
	var created []string
	for _, l := range wantedLabels(opts) {
		if have[strings.ToLower(l.name)] {
			continue
		}
		req := gogithub.CreateIssueLabelRequest{Name: l.name, Color: &l.color, Description: &l.description}
		if _, _, err := c.Issues.CreateLabel(ctx, owner, repo, req); err != nil {
			return created, fmt.Errorf("create label %s in %s/%s: %w", l.name, owner, repo, err)
		}
		have[strings.ToLower(l.name)] = true
		created = append(created, l.name)
	}
	return created, nil
}

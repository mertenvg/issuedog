package githubtest

import (
	"slices"
	"strings"
)

// repoLabelState returns the repo's labels for State, each as "<name>
// <color> <description>", sorted: GitHub's label names are a set, ignoring
// case, so a replay may create them in another order. f.mu must be held.
func (f *Fake) repoLabelState() []string {
	var out []string
	for _, l := range f.labels {
		out = append(out, strings.Join([]string{l.GetName(), l.GetColor(), l.GetDescription()}, " "))
	}
	slices.Sort(out)
	return out
}

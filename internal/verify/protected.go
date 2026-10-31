// Package verify judges a task's head by SPEC §10.1's deterministic
// rules, from the daemon's own reads of the bare repo, and runs its
// verify.commands as agent in a fresh checkout.
package verify

import (
	"cmp"
	"slices"
	"strings"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/trust"
)

// builtinProtected are the protected paths of SPEC §6.3 that every repo
// has, before verify.protected_paths: CODEOWNERS in each of the three
// places GitHub reads it (codeownersPaths), .github/**, .issuedog/** and
// the agent-config paths.
var builtinProtected = slices.Concat(codeownersPaths(), []string{".github/**", ".issuedog/**",
	".claude/**", "**/CLAUDE.md", "**/CLAUDE.local.md", ".mcp.json"})

// codeownersPaths lists the places GitHub reads CODEOWNERS from, as
// trust.Locations lists them, in the order the implementer's preamble
// names them: the root's first, then the others in GitHub's order. So the
// list is trust's, and the preamble's text stays as it was.
func codeownersPaths() []string {
	paths := slices.Clone(trust.Locations)
	slices.SortStableFunc(paths, func(a, b string) int { return cmp.Compare(strings.Count(a, "/"), strings.Count(b, "/")) })
	return paths
}

// Protected returns the protected paths of SPEC §6.3, then c's
// verify.protected_paths, as path patterns (glob.Match). The implementer's
// preamble names them, and rule 2 of SPEC §10.1 checks a head against
// them.
func Protected(c config.Verify) []string {
	return slices.Concat(builtinProtected, c.ProtectedPaths)
}

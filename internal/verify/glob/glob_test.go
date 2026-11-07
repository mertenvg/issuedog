package glob

import (
	"strings"
	"testing"
)

// Paths match from the repo root: "*" and "?" within one segment, and "**"
// across zero or more whole segments (SPEC §13).
func TestMatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		// **/*_test.go matches a test at the root and at any depth.
		{"**/*_test.go", "main_test.go", true},
		{"**/*_test.go", "internal/verify/glob_test.go", true},
		{"**/*_test.go", "internal/verify/glob.go", false},
		{"**/*_test.go", "internal/verify_test.go/x.go", false},
		// e2e/** matches everything below e2e, and e2e itself.
		{"e2e/**", "e2e/login.spec.ts", true},
		{"e2e/**", "e2e/a/b/c.ts", true},
		{"e2e/**", "e2e", true},
		{"e2e/**", "src/e2e/x.ts", false},
		{"e2e/**", "e2e2/x.ts", false},
		// .github/** matches .github itself and anything below it.
		{".github/**", ".github", true},
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", "docs/.github/x", false},
		// **/CLAUDE.md matches at the root and below.
		{"**/CLAUDE.md", "CLAUDE.md", true},
		{"**/CLAUDE.md", "a/b/CLAUDE.md", true},
		{"**/CLAUDE.md", "a/b/NOTCLAUDE.md", false},
		{"**/CLAUDE.md", "CLAUDE.md/x", false},
		// ** in the middle spans zero or more segments.
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"**/migrations/**", "migrations", true},
		{"**/migrations/**", "db/migrations/001.sql", true},
		{"**/**/x", "x", true},
		{"**", "anything/at/all", true},
		// * and ? stay within a segment; a pattern with no ** keeps its depth.
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/*.go", "cmd/main.go", true},
		{"cmd/*.go", "cmd/x/main.go", false},
		{"cmd/*", "cmd", false},
		{"?.md", "a.md", true},
		{"?.md", "ab.md", false},
		{"?.md", "/.md", false},
		{"a?b", "a/b", false},
		{"*", "a/b", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "x/go.mod", false},
		{"CODEOWNERS", "docs/CODEOWNERS", false},
		{"docs/CODEOWNERS", "docs/CODEOWNERS", true},
		// Classes and escapes are path.Match's.
		{"[ab].txt", "a.txt", true},
		{"[ab].txt", "c.txt", false},
		{`\*.txt`, "*.txt", true},
		{`\*.txt`, "a.txt", false},
		// Paths with spaces and odd characters are just names.
		{"docs/**", "docs/a file\nname.md", true},
		{"deploy/<prod>", "deploy/<prod>", true},
		// A bad pattern matches nothing.
		{"[", "[", false},
		{"a**/b", "a/b", false},
		{"/a", "/a", false},
		{"", "", false},
	} {
		if got := Match(tc.pattern, tc.path); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// Valid refuses a pattern that won't parse, saying why, and takes one that
// does.
func TestValid(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"**/*_test.go", "e2e/**", ".github/**", "**/CLAUDE.md", "?.md", "[ab]/x", `\[x`, "deploy/<prod>", "**"} {
		if err := Valid(p); err != nil {
			t.Errorf("Valid(%q) = %v, want nil", p, err)
		}
	}
	for _, tc := range []struct{ pattern, want string }{
		{"", "empty pattern"},
		{"[", "syntax error in pattern"},
		{"a/[b-", `segment "[b-"`},
		{`x\`, "syntax error in pattern"},
		{"/a", "empty segment"},
		{"a/", "empty segment"},
		{"a//b", "empty segment"},
		{"a**", "** inside a segment"},
		{"**.go", "** inside a segment"},
		{"a/***/b", "** inside a segment"},
	} {
		err := Valid(tc.pattern)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Valid(%q) = %v, want an error containing %q", tc.pattern, err, tc.want)
		}
	}
}

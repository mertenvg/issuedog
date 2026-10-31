// Package glob matches repo paths against the path patterns of
// .issuedog/config.yaml's verify keys and SPEC §6.3's protected paths
// (SPEC §13). It's a package of its own so that config can check the
// patterns it parses, while verify, which reads config, matches them.
package glob

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// anySegments is the segment that matches zero or more whole segments.
const anySegments = "**"

// Valid returns an error saying why pattern isn't a path pattern, or nil
// when it is one. A pattern is one or more segments joined by "/", from the
// repo root, with no empty segment, so no leading, trailing or doubled "/".
// A segment is "**", or a path.Match pattern with no "**" in it: "*" and
// "?" match within the segment, "[...]" is a class, and "\" escapes the
// character after it.
func Valid(pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	for _, seg := range strings.Split(pattern, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("%q has an empty segment: a leading, trailing or doubled /", pattern)
		case seg == anySegments:
		case strings.Contains(seg, anySegments):
			return fmt.Errorf("%q has ** inside a segment; ** must be a whole segment", pattern)
		default:
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("%q: segment %q: %w", pattern, seg, err)
			}
		}
	}
	return nil
}

// Match reports whether the slash-separated path, from the repo root,
// matches pattern. "*" and "?" stay within one segment, and a "**" segment
// matches zero or more whole segments, so "**/*_test.go" matches a test at
// the root, and ".github/**" matches .github itself. A pattern with no
// "**" matches only paths of its own depth. A pattern that isn't Valid
// matches nothing.
func Match(pattern, name string) bool {
	if Valid(pattern) != nil {
		return false
	}
	pat, segs := strings.Split(pattern, "/"), strings.Split(name, "/")
	// ok[j] reports whether the pattern's segments so far match segs[:j].
	// Before any segment, only the empty prefix matches.
	ok := make([]bool, len(segs)+1)
	ok[0] = true
	for _, p := range pat {
		next := make([]bool, len(segs)+1)
		for j := range next {
			if p == anySegments {
				// ** takes segs[i:j] for any i <= j whose prefix matched.
				next[j] = ok[j] || (j > 0 && next[j-1])
				continue
			}
			if j > 0 && ok[j-1] {
				// Valid checked the segment, so Match can't fail.
				m, err := path.Match(p, segs[j-1])
				next[j] = m && err == nil
			}
		}
		ok = next
	}
	return ok[len(segs)]
}

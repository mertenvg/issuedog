package verify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/verify/glob"
)

// Finding is one rule of SPEC §10.1 that a head fails. Its text is plain,
// not Markdown, and holds paths from the diff as they are, or quoted when
// one isn't printable, so whoever posts it fences and escapes it.
type Finding struct {
	Rule   int    // the rule's number in SPEC §10.1
	Name   string // the rule's short name
	Detail string // what failed, naming each path
	Output string // a command's output, for the rules that run one
}

// Facts are what the rules judge a head by. Each SHA is a full one in
// Repo, the daemon's bare clone.
type Facts struct {
	Repo *repo.Repo
	// Baseline is the main commit every protected path must match (SPEC
	// §10.1, rule 2).
	Baseline string
	// Base is the merge base the task's own diff starts at (BaseOf).
	Base string
	// Head is the commit judged.
	Head string
	// Target is the commit the head must merge cleanly with (rule 7).
	Target string
	// MustHold are the commits the head's history must hold (rule 7), or
	// none: for a conflict bug, the main commit it names and its story
	// branch's tip, which its head merges (SPEC §7.5, A conflict).
	MustHold []string
	// Conflicts are the conflict's files, for a conflict bug: each path
	// that conflicts in its conflicted merge, Base (repo.ConflictMerge),
	// whose resolution rules 3, 5 and 7 judge apart (SPEC §7.5, A
	// conflict). Empty for any other task, whose rules they leave as
	// they are.
	Conflicts []string
	// Scope is the task's scope, without its "scope:" prefix; empty for
	// none.
	Scope  string
	Config config.Verify
}

// listMax is how many paths a detail names before it says how many more.
const listMax = 20

// lineMax is how many characters of an added line a detail quotes.
const lineMax = 120

// Rule2 fails a head that touches a protected path (SPEC §6.3, then
// verify.protected_paths): one the task's own diff, from Base, changes, or
// one that differs between Baseline and the head. Each diff finds no
// renames, so a rename counts as both of its paths.
func Rule2(ctx context.Context, f Facts) (*Finding, error) {
	protected := Protected(f.Config)
	own, err := f.Repo.Changes(ctx, f.Base, f.Head, false)
	if err != nil {
		return nil, fmt.Errorf("rule 2: the task's own diff: %w", err)
	}
	all, err := f.Repo.Changes(ctx, f.Baseline, f.Head, false)
	if err != nil {
		return nil, fmt.Errorf("rule 2: the diff from the baseline: %w", err)
	}
	touched := matching(own, protected)
	var behind []string
	for _, p := range matching(all, protected) {
		if !slices.Contains(touched, p) {
			behind = append(behind, p)
		}
	}
	if len(touched) == 0 && len(behind) == 0 {
		return nil, nil
	}
	var parts []string
	if len(touched) > 0 {
		parts = append(parts, "The task's diff touches protected paths (SPEC 6.3 and verify.protected_paths):\n"+list(touched))
	}
	if len(behind) > 0 {
		parts = append(parts, fmt.Sprintf("Protected paths differ from the baseline %s, which every protected path must match:\n%s", f.Baseline, list(behind)))
	}
	return &Finding{Rule: 2, Name: "protected paths", Detail: strings.Join(parts, "\n\n")}, nil
}

// Rule3 fails a head whose diff from Base deletes a file matching
// verify.test_globs, or renames one out of them, or adds a line holding a
// verify.skip_markers marker (markerAt) to a file matching them. For a
// conflict bug, deleting one of the conflict's files (Conflicts) that its
// main commit, Baseline, lacks isn't a deleted test: the resolution takes
// main's deletion of it (SPEC §7.5, A conflict).
func Rule3(ctx context.Context, f Facts) (*Finding, error) {
	changes, err := f.Repo.Changes(ctx, f.Base, f.Head, true)
	if err != nil {
		return nil, fmt.Errorf("rule 3: %w", err)
	}
	mainDeleted, err := conflictsMainDeleted(ctx, f, changes)
	if err != nil {
		return nil, fmt.Errorf("rule 3: %w", err)
	}
	var deleted, tests []string
	for _, c := range changes {
		switch {
		case c.Status == 'D' && matchAny(f.Config.TestGlobs, c.Path) && !slices.Contains(mainDeleted, c.Path):
			deleted = append(deleted, show(c.Path))
		case c.Status == 'R' && matchAny(f.Config.TestGlobs, c.From) && !matchAny(f.Config.TestGlobs, c.Path):
			deleted = append(deleted, show(c.From)+" (renamed to "+show(c.Path)+")")
		}
		if c.Status != 'D' && matchAny(f.Config.TestGlobs, c.Path) {
			tests = append(tests, c.Path)
		}
	}
	added, err := f.Repo.AddedLines(ctx, f.Base, f.Head, tests)
	if err != nil {
		return nil, fmt.Errorf("rule 3: %w", err)
	}
	var markers []string
	for _, a := range added {
		for _, m := range f.Config.SkipMarkers {
			if markerAt(a.Text, m) {
				markers = append(markers, fmt.Sprintf("%s:%d adds %s: %s", show(a.Path), a.Line, strconv.Quote(m), show(cut(strings.TrimSpace(a.Text)))))
				break
			}
		}
	}
	if len(deleted) == 0 && len(markers) == 0 {
		return nil, nil
	}
	var parts []string
	if len(deleted) > 0 {
		parts = append(parts, "The diff deletes test files (verify.test_globs):\n"+lines(deleted))
	}
	if len(markers) > 0 {
		parts = append(parts, "The diff adds skip markers (verify.skip_markers) to test files:\n"+lines(markers))
	}
	return &Finding{Rule: 3, Name: "test deletions and skip markers", Detail: strings.Join(parts, "\n\n")}, nil
}

// conflictsMainDeleted returns each test file, matching verify.test_globs,
// that changes deletes, that's one of the conflict's files (f.Conflicts),
// and that f.Baseline, a conflict bug's main commit, has no file at. With
// no Conflicts, as for any task but a conflict bug, it's none, and reads
// nothing.
func conflictsMainDeleted(ctx context.Context, f Facts, changes []repo.Change) ([]string, error) {
	var candidates []string
	for _, c := range changes {
		if c.Status == 'D' && matchAny(f.Config.TestGlobs, c.Path) && slices.Contains(f.Conflicts, c.Path) {
			candidates = append(candidates, c.Path)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	atMain, err := f.Repo.Blobs(ctx, f.Baseline, candidates)
	if err != nil {
		return nil, fmt.Errorf("the conflict's test files at main's %s: %w", f.Baseline, err)
	}
	var deleted []string
	for _, p := range candidates {
		if _, ok := atMain[p]; !ok {
			deleted = append(deleted, p)
		}
	}
	return deleted, nil
}

// Rule4 fails a head whose diff from Base changes a file matching
// verify.config_paths, either side of a rename, unless the task's scope is
// verify.tooling_scope. With no tooling scope set, no task may.
func Rule4(ctx context.Context, f Facts) (*Finding, error) {
	tooling := f.Config.ToolingScope
	if tooling != "" && f.Scope == tooling {
		return nil, nil
	}
	changes, err := f.Repo.Changes(ctx, f.Base, f.Head, true)
	if err != nil {
		return nil, fmt.Errorf("rule 4: %w", err)
	}
	changed := matching(changes, f.Config.ConfigPaths)
	if len(changed) == 0 {
		return nil, nil
	}
	who := "no task may, as verify.tooling_scope is unset"
	if tooling != "" {
		who = "only a task in scope " + strconv.Quote(tooling) + " (verify.tooling_scope) may"
	}
	return &Finding{Rule: 4, Name: "config paths", Detail: fmt.Sprintf("The diff changes config paths (verify.config_paths), which %s:\n%s", who, list(changed))}, nil
}

// Rule5 fails a head whose diff from Base adds, modifies or renames a file
// matching verify.source_globs and no test glob, but adds, modifies or
// renames no file matching verify.test_globs. A diff of other files, such
// as docs, passes. For a conflict bug, a source file among the conflict's
// files (Conflicts) needs no test, as its change resolves the merge (SPEC
// §7.5, A conflict). Any other source file it touches still does.
func Rule5(ctx context.Context, f Facts) (*Finding, error) {
	changes, err := f.Repo.Changes(ctx, f.Base, f.Head, true)
	if err != nil {
		return nil, fmt.Errorf("rule 5: %w", err)
	}
	var source []string
	for _, c := range changes {
		if c.Status != 'A' && c.Status != 'M' && c.Status != 'R' {
			continue
		}
		if matchAny(f.Config.TestGlobs, c.Path) {
			return nil, nil
		}
		if matchAny(f.Config.SourceGlobs, c.Path) && !slices.Contains(f.Conflicts, c.Path) {
			source = append(source, c.Path)
		}
	}
	if len(source) == 0 {
		return nil, nil
	}
	return &Finding{Rule: 5, Name: "tests alongside source", Detail: "The diff adds or changes source files (verify.source_globs), " +
		"but adds or changes no test file (verify.test_globs):\n" + list(source)}, nil
}

// TooLarge starts rule 6's finding for a diff over verify.max_diff_lines
// (SPEC §10.1).
const TooLarge = "task too large, split it"

// Rule6 fails a head whose diff from Base changes more than
// verify.max_diff_lines lines, added plus deleted, in files outside
// verify.test_globs, as a hard limit: lines in test files count for
// nothing. It finds renames, so a file that only moved counts no lines. It
// also fails a binary file the diff adds or changes that doesn't match
// verify.binary_allow, in or out of the test globs. A binary's lines count
// for nothing. Deleting one passes, and so does a change that leaves its
// content as it was, such as a move. A file is binary by its new content
// (repo.Stat), so a binary turned into text counts its new lines as
// added, and text turned into a binary is a binary.
func Rule6(ctx context.Context, f Facts) (*Finding, error) {
	stats, err := f.Repo.NumStat(ctx, f.Base, f.Head)
	if err != nil {
		return nil, fmt.Errorf("rule 6: %w", err)
	}
	count := 0
	var binaries []string
	for _, s := range stats {
		switch {
		case s.Same:
			// Its content is as it was, so it adds no binary and no lines.
		case s.Binary:
			if !matchAny(f.Config.BinaryAllow, s.Path) {
				binaries = append(binaries, s.Path)
			}
		case !matchAny(f.Config.TestGlobs, s.Path):
			count += s.Added + s.Deleted
		}
	}
	var parts []string
	if limit := int(f.Config.MaxDiffLines); count > limit {
		parts = append(parts, fmt.Sprintf("%s: the diff changes %d lines in files outside verify.test_globs, "+
			"over verify.max_diff_lines of %d.", TooLarge, count, limit))
	}
	if len(binaries) > 0 {
		parts = append(parts, "The diff adds or changes binary files outside verify.binary_allow:\n"+list(binaries))
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return &Finding{Rule: 6, Name: "diff size and binaries", Detail: strings.Join(parts, "\n\n")}, nil
}

// Rule7 fails a head that shares no history with Target, or doesn't merge
// with it cleanly, judged in the bare repo with no worktree
// (repo.MergeTree), or whose history lacks any of MustHold, or, for a
// conflict bug, that still holds a conflict marker line
// (conflictMarkerLine) in any of the conflict's files (Conflicts) whose
// blob in the conflicted merge, Base, held one (SPEC §7.5, A conflict).
// The finding names each commit the head lacks, then each conflicting
// path, then each file with markers left.
func Rule7(ctx context.Context, f Facts) (*Finding, error) {
	var lacks []string
	for _, c := range f.MustHold {
		has, err := f.Repo.IsAncestor(ctx, c, f.Head)
		if err != nil {
			return nil, fmt.Errorf("rule 7: %w", err)
		}
		if !has {
			lacks = append(lacks, c)
		}
	}
	var parts []string
	if len(lacks) > 0 {
		parts = append(parts, fmt.Sprintf("The head %s lacks these commits in its history, which it must merge:\n%s", f.Head, lines(lacks)))
	}
	bases, err := f.Repo.MergeBase(ctx, f.Target, f.Head)
	if err != nil {
		return nil, fmt.Errorf("rule 7: %w", err)
	}
	if len(bases) == 0 {
		parts = append(parts, fmt.Sprintf("The head %s shares no history with its target %s.", f.Head, f.Target))
	} else {
		conflicts, err := f.Repo.MergeTree(ctx, f.Target, f.Head)
		if err != nil {
			return nil, fmt.Errorf("rule 7: %w", err)
		}
		if len(conflicts) > 0 {
			parts = append(parts, fmt.Sprintf("The head %s doesn't merge cleanly with its target %s. "+
				"These files conflict:\n%s", f.Head, f.Target, list(conflicts)))
		}
	}
	left, err := markersLeft(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("rule 7: %w", err)
	}
	if len(left) > 0 {
		parts = append(parts, fmt.Sprintf("The head %s leaves conflict markers (lines starting %q, %q or %q) "+
			"in files that conflicted in the merge it resolves:\n%s", f.Head, "<<<<<<< ", "=======", ">>>>>>> ", list(left)))
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return &Finding{Rule: 7, Name: "merge with the target", Detail: strings.Join(parts, "\n\n")}, nil
}

// markersLeft returns, in f.Conflicts' order, each of the conflict's files
// whose blob in the conflicted merge, f.Base, holds a conflict marker line
// (conflictMarkerLine), as merge-tree wrote it, and whose blob at f.Head
// still holds one. A file the head deleted holds none. With no Conflicts,
// as for any task but a conflict bug, it's none, and reads nothing.
func markersLeft(ctx context.Context, f Facts) ([]string, error) {
	if len(f.Conflicts) == 0 {
		return nil, nil
	}
	merged, err := f.Repo.Blobs(ctx, f.Base, f.Conflicts)
	if err != nil {
		return nil, fmt.Errorf("the conflict's files in the conflicted merge: %w", err)
	}
	var marked []string
	for _, p := range f.Conflicts {
		if hasConflictMarker(merged[p]) {
			marked = append(marked, p)
		}
	}
	if len(marked) == 0 {
		return nil, nil
	}
	head, err := f.Repo.Blobs(ctx, f.Head, marked)
	if err != nil {
		return nil, fmt.Errorf("the conflict's files at the head: %w", err)
	}
	var left []string
	for _, p := range marked {
		if hasConflictMarker(head[p]) {
			left = append(left, p)
		}
	}
	return left, nil
}

// hasConflictMarker reports whether content has a conflict marker line
// (conflictMarkerLine).
func hasConflictMarker(content []byte) bool {
	for line := range strings.Lines(string(content)) {
		if conflictMarkerLine(line) {
			return true
		}
	}
	return false
}

// conflictMarkerLine reports whether line, with or without its line
// ending, is a conflict marker as git writes one: starting "<<<<<<< " or
// ">>>>>>> ", or "=======" as the whole line.
func conflictMarkerLine(line string) bool {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	return strings.HasPrefix(line, "<<<<<<< ") || line == "=======" || strings.HasPrefix(line, ">>>>>>> ")
}

// ErrMergeBases is BaseOf's error when the head and its target don't have
// exactly one merge base, which fails rule 2 (SPEC §10.1).
var ErrMergeBases = errors.New("not exactly one merge base")

// BaseOf returns the commit the task's own diff starts at: the one merge
// base of f.Head and f.Target. None, or more than one, is an error
// matching ErrMergeBases.
func BaseOf(ctx context.Context, f Facts) (string, error) {
	bases, err := f.Repo.MergeBase(ctx, f.Head, f.Target)
	if err != nil {
		return "", fmt.Errorf("base of %s: %w", f.Head, err)
	}
	if len(bases) != 1 {
		return "", fmt.Errorf("base of %s: %w with %s: %d of them", f.Head, ErrMergeBases, f.Target, len(bases))
	}
	return bases[0], nil
}

// markerAt reports whether line holds marker. A marker that starts with a
// letter matches only where no letter, digit or "_" comes before it, so
// "xit(" matches `xit("works")` but not "os.Exit(", while ".skip(" matches
// "describe.skip(". An empty marker matches nothing.
func markerAt(line, marker string) bool {
	if marker == "" {
		return false
	}
	first, size := utf8.DecodeRuneInString(marker)
	word := unicode.IsLetter(first)
	for from := 0; ; {
		i := strings.Index(line[from:], marker)
		if i < 0 {
			return false
		}
		at := from + i
		before, _ := utf8.DecodeLastRuneInString(line[:at])
		if !word || at == 0 || !(unicode.IsLetter(before) || unicode.IsDigit(before) || before == '_') {
			return true
		}
		from = at + size
	}
}

// matching returns, in the diff's order and once each, the paths of
// changes, either side of a rename, that match any of patterns.
func matching(changes []repo.Change, patterns []string) []string {
	var paths []string
	for _, c := range changes {
		for _, p := range []string{c.From, c.Path} {
			if p != "" && matchAny(patterns, p) && !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// matchAny reports whether path matches any of patterns.
func matchAny(patterns []string, path string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return glob.Match(p, path) })
}

// list is paths as a detail's list, one to a line, each shown (show).
func list(paths []string) string {
	shown := make([]string, len(paths))
	for i, p := range paths {
		shown[i] = show(p)
	}
	return lines(shown)
}

// lines is items as a detail's list, one to a line: the first listMax,
// then how many more there are.
func lines(items []string) string {
	var b strings.Builder
	for i, item := range items {
		if i == listMax {
			fmt.Fprintf(&b, "…and %d more\n", len(items)-listMax)
			break
		}
		b.WriteString("- " + item + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// show is a path or line as a detail gives it: as it is, or Go-quoted when
// it has a character that isn't printable, such as a newline, so it stays
// on one line.
func show(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(s)
	}
	return s
}

// cut is s cut to lineMax characters, ending in "…" when it was longer.
func cut(s string) string {
	if utf8.RuneCountInString(s) <= lineMax {
		return s
	}
	return string([]rune(s)[:lineMax]) + "…"
}

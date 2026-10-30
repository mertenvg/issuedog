package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// diffFlags keep every diff here to git's own text comparison, whatever
// the repo's attributes or config say: no external diff, no textconv and no
// color. The diffs are diff-tree's, which is plumbing: it reads none of the
// diff.* settings that change porcelain output, and it compares commits'
// trees, so no filter driver runs.
var diffFlags = []string{"--no-ext-diff", "--no-textconv", "--no-color"}

// Change is one file a diff changes, as git's --name-status gives it.
type Change struct {
	// Status is git's status letter: 'A' added, 'D' deleted, 'M' modified,
	// 'T' changed type, and, when renames are found, 'R' renamed.
	Status byte
	// Path is the file's path in the diff's to side, or its from side for a
	// deletion.
	Path string
	// From is a rename's path in the from side; empty otherwise.
	From string
}

// Added is one line a diff adds.
type Added struct {
	Path string // the file's path in the to side
	Line int    // its line number there, from 1
	Text string // the line, without its newline
}

// Changes returns the files that differ between the commits from and to,
// both full SHAs, read in the bare repo with diffFlags. With renames, git
// finds renames (Status 'R', with From); without, a rename is a deletion
// of its old path and an addition of its new one, so both paths appear.
func (r *Repo) Changes(ctx context.Context, from, to string, renames bool) ([]Change, error) {
	if err := checkSHAs(from, to); err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	find := "--no-renames"
	if renames {
		find = "--find-renames"
	}
	args := slices.Concat([]string{"diff-tree", "-r", "-z", "--name-status", find}, diffFlags, []string{from, to, "--"})
	out, err := r.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("changes %s..%s: %w", from, to, err)
	}
	changes, err := parseNameStatus(out)
	if err != nil {
		return nil, fmt.Errorf("changes %s..%s: %w", from, to, err)
	}
	return changes, nil
}

// parseNameStatus parses --name-status -z output: a status, then its path,
// each ended by a NUL, or for a rename or copy, a status with a score, its
// old path and its new one.
func parseNameStatus(out []byte) ([]Change, error) {
	fields := nulFields(out)
	var changes []Change
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			return nil, fmt.Errorf("empty status at field %d", i)
		}
		c := Change{Status: status[0]}
		switch c.Status {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("status %q has no old and new path", status)
			}
			c.From, c.Path = fields[i+1], fields[i+2]
			i += 3
		default:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("status %q has no path", status)
			}
			c.Path = fields[i+1]
			i += 2
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// hunkHeader matches a -U0 hunk header and captures its old and new
// counts, and its new start: @@ -a[,b] +c[,d] @@. A missing count is 1.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// AddedLines returns the lines the diff from from to to, both full SHAs,
// adds to the files at paths in the to side. It reads git's own -U0 patch
// in the bare repo with diffFlags, finding renames, so a file that only
// moved adds no lines. A file git takes as binary adds none either.
func (r *Repo) AddedLines(ctx context.Context, from, to string, paths []string) ([]Added, error) {
	if err := checkSHAs(from, to); err != nil {
		return nil, fmt.Errorf("added lines: %w", err)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	// The diff isn't limited to paths, since a rename's old path outside
	// them would then read as a whole new file.
	args := slices.Concat([]string{"diff-tree", "-r", "-p", "-U0", "--find-renames", "--submodule=short",
		"--src-prefix=a/", "--dst-prefix=b/"}, diffFlags, []string{from, to, "--"})
	out, err := r.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("added lines %s..%s: %w", from, to, err)
	}
	added, err := parseAdded(out, paths)
	if err != nil {
		return nil, fmt.Errorf("added lines %s..%s: %w", from, to, err)
	}
	return added, nil
}

// parseAdded reads a -U0 patch and returns its added lines in files at
// paths. Each file's patch starts with a "diff --git" line; its "+++" line
// names the file in the to side; each hunk header says how many lines
// follow, so a line's text is never taken for a header.
func parseAdded(patch []byte, paths []string) ([]Added, error) {
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	var added []Added
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(nil, 64<<20)
	file := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			file = ""
		case strings.HasPrefix(line, "+++ "):
			name, err := patchPath(strings.TrimPrefix(line, "+++ "))
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", line, err)
			}
			file = name
		case strings.HasPrefix(line, "@@ "):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("bad hunk header %q", line)
			}
			counts, err := hunkCounts(m[1], m[2], m[3])
			if err != nil {
				return nil, fmt.Errorf("hunk %q of %q: %w", line, file, err)
			}
			lines, err := hunk(sc, counts[0], counts[2])
			if err != nil {
				return nil, fmt.Errorf("hunk %q of %q: %w", line, file, err)
			}
			if !want[file] {
				continue
			}
			for i, text := range lines {
				added = append(added, Added{Path: file, Line: counts[1] + i, Text: text})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read patch: %w", err)
	}
	return added, nil
}

// hunk reads a -U0 hunk's body from sc, the removed lines and then the
// added ones, each maybe followed by git's "\ No newline at end of file",
// and returns the added lines' text.
func hunk(sc *bufio.Scanner, removed, adds int) ([]string, error) {
	var lines []string
	for removed > 0 || adds > 0 {
		if !sc.Scan() {
			return nil, fmt.Errorf("ends with %d removed and %d added lines to go", removed, adds)
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, `\`):
		case strings.HasPrefix(line, "-") && removed > 0:
			removed--
		case strings.HasPrefix(line, "+") && adds > 0:
			lines = append(lines, line[1:])
			adds--
		default:
			return nil, fmt.Errorf("unexpected line %q", line)
		}
	}
	return lines, nil
}

// hunkCounts reads a hunk header's numbers, in order: its removed lines'
// count, its new start, and its added lines' count. A missing count is 1.
func hunkCounts(nums ...string) ([]int, error) {
	counts := make([]int, len(nums))
	for i, s := range nums {
		if s == "" {
			counts[i] = 1
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("hunk count: %w", err)
		}
		counts[i] = n
	}
	return counts, nil
}

// patchPath reads a "+++" line's name: b/<path>, C-quoted by git when the
// path has a special character, then a tab when it has a space, or
// /dev/null for a deletion, which gives "". A path that ends in a tab is
// quoted, so the tab is never the path's.
func patchPath(name string) (string, error) {
	if name == "/dev/null" {
		return "", nil
	}
	name = strings.TrimSuffix(name, "\t")
	if strings.HasPrefix(name, `"`) {
		// Git's C quoting (\n, \t, \", \\ and octal \ooo bytes) is valid Go.
		unq, err := strconv.Unquote(name)
		if err != nil {
			return "", fmt.Errorf("unquote %s: %w", name, err)
		}
		name = unq
	}
	p, ok := strings.CutPrefix(name, "b/")
	if !ok {
		return "", fmt.Errorf("%q doesn't start with b/", name)
	}
	return p, nil
}

// Stat is one file's line counts in a diff, as git's --numstat gives them.
type Stat struct {
	// Path is the file's path in the diff's to side, or its from side for
	// a deletion. A rename has its new path.
	Path    string
	Added   int // lines added
	Deleted int // lines deleted
	// Binary says git took the file as binary, so it counted no lines.
	Binary bool
}

// NumStat returns each file's added and deleted lines in the diff from
// from to to, both full SHAs, read in the bare repo with diffFlags. It
// finds renames, so a file that only moved counts no lines, as a PR's
// stats count them.
func (r *Repo) NumStat(ctx context.Context, from, to string) ([]Stat, error) {
	if err := checkSHAs(from, to); err != nil {
		return nil, fmt.Errorf("numstat: %w", err)
	}
	args := slices.Concat([]string{"diff-tree", "-r", "-z", "--numstat", "--find-renames"}, diffFlags, []string{from, to, "--"})
	out, err := r.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	stats, err := parseNumStat(out)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	return stats, nil
}

// parseNumStat parses --numstat -z output: the added and deleted counts,
// each ended by a tab, then the path, ended by a NUL; or for a rename, no
// path, then its old path and its new one, each ended by a NUL. A binary
// file's counts are "-".
func parseNumStat(out []byte) ([]Stat, error) {
	fields := nulFields(out)
	var stats []Stat
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("bad numstat field %q", fields[i])
		}
		s := Stat{Path: parts[2], Binary: parts[0] == "-" && parts[1] == "-"}
		if s.Path == "" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("rename %q has no old and new path", fields[i])
			}
			s.Path = fields[i+2]
			i += 2
		}
		if !s.Binary {
			var err error
			if s.Added, err = strconv.Atoi(parts[0]); err != nil {
				return nil, fmt.Errorf("added count of %q: %w", s.Path, err)
			}
			if s.Deleted, err = strconv.Atoi(parts[1]); err != nil {
				return nil, fmt.Errorf("deleted count of %q: %w", s.Path, err)
			}
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// MergeBase returns every merge base of the commits a and b, both full
// SHAs, as git merge-base --all finds them. Commits with no common history
// have none, which is no error.
func (r *Repo) MergeBase(ctx context.Context, a, b string) ([]string, error) {
	if err := checkSHAs(a, b); err != nil {
		return nil, fmt.Errorf("merge base: %w", err)
	}
	out, err := r.git(ctx, "", "merge-base", "--all", a, b)
	if err != nil {
		// merge-base exits 1, and prints nothing, when it finds none.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("merge base of %s and %s: %w", a, b, err)
	}
	return strings.Fields(string(out)), nil
}

// MergeTree merges head into target, both full SHAs, as git merge-tree
// --write-tree does, in the bare repo with no worktree or index, and
// returns the paths that conflict, each once; none when the merge is
// clean. Commits with no common history are an error, as merge-tree
// refuses to merge them.
//
// merge-tree writes no ref, but it does write the merged blobs and trees,
// and any merge base it builds from several, as new objects. They go to a
// quarantine (quarantine), removed after, so the merge leaves nothing in
// the bare repo.
func (r *Repo) MergeTree(ctx context.Context, target, head string) (conflicts []string, err error) {
	if err := checkSHAs(target, head); err != nil {
		return nil, fmt.Errorf("merge tree: %w", err)
	}
	dir, q, err := r.quarantine()
	if err != nil {
		return nil, fmt.Errorf("merge tree of %s and %s: %w", target, head, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove quarantine: %w", rerr))
		}
	}()
	out, err := q.git(ctx, "", "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", target, head)
	// With -z, the output is the merged tree's ID, then each conflicting
	// path, each ended by a NUL. It exits 1 when there are conflicts.
	fields := nulFields(out)
	var exit *exec.ExitError
	switch {
	case err == nil && len(fields) == 1:
		return nil, nil
	case err != nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(fields) > 1:
		return fields[1:], nil
	case err != nil:
		return nil, fmt.Errorf("merge tree of %s and %s: %w", target, head, err)
	default:
		return nil, fmt.Errorf("merge tree of %s and %s: unexpected output %q", target, head, out)
	}
}

// nulFields splits -z output into its NUL-ended fields.
func nulFields(out []byte) []string {
	fields := strings.Split(string(out), "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	return fields
}

// checkSHAs refuses a diff end that isn't a full SHA.
func checkSHAs(shas ...string) error {
	for _, s := range shas {
		if !shaPattern.MatchString(s) {
			return fmt.Errorf("invalid SHA %q", s)
		}
	}
	return nil
}

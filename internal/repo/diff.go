package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// diffFlags keep every diff here to git's own text comparison, whatever
// the repo's attributes or config say: no external diff, no textconv and no
// color. The diffs are diff-tree's, which is plumbing: it reads none of the
// diff.* settings that change porcelain output, and it compares commits'
// trees, so no filter driver runs. Each runs in an attrless repo
// (attrless), so no attribute, such as -diff, changes what it reads either.
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
// both full SHAs, read from the bare repo with diffFlags and no
// attributes (attrless). With renames, git finds renames (Status 'R', with
// From); without, a rename is a deletion of its old path and an addition
// of its new one, so both paths appear.
func (r *Repo) Changes(ctx context.Context, from, to string, renames bool) ([]Change, error) {
	if err := checkSHAs(from, to); err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	find := "--no-renames"
	if renames {
		find = "--find-renames"
	}
	args := slices.Concat([]string{"diff-tree", "-r", "-z", "--name-status", find}, diffFlags, []string{from, to, "--"})
	out, err := r.gitAttrless(ctx, from, args...)
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
// from the bare repo with diffFlags and no attributes (attrless), finding
// renames, so a file that only moved adds no lines. A file git takes as
// binary, by its content alone, adds none either.
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
	out, err := r.gitAttrless(ctx, from, args...)
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
// and returns the added lines' text. A line sc can't read, such as one
// past its buffer's limit, is sc's error, such as bufio.ErrTooLong.
func hunk(sc *bufio.Scanner, removed, adds int) ([]string, error) {
	var lines []string
	for removed > 0 || adds > 0 {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return nil, fmt.Errorf("read a line with %d removed and %d added lines to go: %w", removed, adds, err)
			}
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

// Stat is one file's line counts in a diff, as git's --numstat gives them,
// with the to side's content deciding whether the file is binary.
type Stat struct {
	// Path is the file's path in the diff's to side, or its from side for
	// a deletion. A rename has its new path.
	Path    string
	Added   int // lines added
	Deleted int // lines deleted
	// Binary says git takes the file's content in the to side as binary,
	// so it counts no lines. A file binary only in the from side isn't
	// Binary, and its from side counts no lines: a deleted one counts
	// none, and one turned into text counts its new lines as added.
	Binary bool
	// Same says the file's content is the same in both sides, so it
	// counts no lines: it only moved (git's R100), or only its mode
	// changed.
	Same bool
}

// NumStat returns each file's added and deleted lines in the diff from
// from to to, both full SHAs, read from the bare repo with diffFlags and
// no attributes (attrless), so git takes a file as binary by its content
// alone. It finds renames, so a file that only moved counts no lines, as a
// PR's stats count them.
//
// git's --numstat counts no lines for a file binary in either side, even
// one whose content is the same in both, such as a binary moved
// unchanged. So NumStat also reads --raw, whose blob IDs tell a file
// whose content is the same (Same) from one that changed. For a file that
// changed and that git counted no lines for, it reads the to side alone,
// as the diff from the empty tree, which says whether the to side is
// binary and, when it isn't, counts its lines.
func (r *Repo) NumStat(ctx context.Context, from, to string) (stats []Stat, err error) {
	if err := checkSHAs(from, to); err != nil {
		return nil, fmt.Errorf("numstat: %w", err)
	}
	dir, a, err := r.attrless(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	args := slices.Concat([]string{"diff-tree", "-r", "-z", "--raw", "--numstat", "--find-renames"}, diffFlags, []string{from, to, "--"})
	out, err := a.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	stats, toRead, err := parseRawNumStat(out)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	if len(toRead) == 0 {
		return stats, nil
	}
	paths := make([]string, len(toRead))
	for j, i := range toRead {
		paths[j] = stats[i].Path
	}
	sides, err := toSideStats(ctx, a, to, paths)
	if err != nil {
		return nil, fmt.Errorf("numstat %s..%s: %w", from, to, err)
	}
	for _, i := range toRead {
		side, ok := sides[stats[i].Path]
		if !ok {
			return nil, fmt.Errorf("numstat %s..%s: no to side read for %q", from, to, stats[i].Path)
		}
		stats[i].Added, stats[i].Binary = side.Added, side.Binary
	}
	return stats, nil
}

// toSideStats reads the to side alone of the files at paths in to, a full
// SHA: each one's lines, all added, from the empty tree, or that git takes
// it as binary. It runs in a, an attrless repo, and writes the empty tree
// there, so it never relies on git knowing that tree without the object.
func toSideStats(ctx context.Context, a *Repo, to string, paths []string) (map[string]Stat, error) {
	empty, err := a.run(ctx, strings.NewReader(""), "", "mktree")
	if err != nil {
		return nil, fmt.Errorf("write the empty tree: %w", err)
	}
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = ":(literal)" + p
	}
	args := slices.Concat([]string{"diff-tree", "-r", "-z", "--numstat", "--no-renames"}, diffFlags,
		[]string{strings.TrimSpace(string(empty)), to, "--"}, specs)
	out, err := a.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("read the to side of %d files: %w", len(paths), err)
	}
	stats, err := parseNumStat(nulFields(out))
	if err != nil {
		return nil, fmt.Errorf("read the to side of %d files: %w", len(paths), err)
	}
	sides := make(map[string]Stat, len(stats))
	for _, s := range stats {
		sides[s.Path] = s
	}
	return sides, nil
}

// rawEntry is one file of --raw -z output: its status letter, its blob IDs
// in each side, all zeros for a side it isn't in, and its path.
type rawEntry struct {
	status           byte
	fromBlob, toBlob string
	path             string // as Stat's Path
}

// parseRawNumStat parses the -z output of --raw and --numstat together:
// every file's --raw entry, then every file's --numstat entry, in the same
// order. It returns each file's Stat, with Same set from the blob IDs,
// and the index of each Stat git counted no lines for that changed and
// isn't deleted, whose to side the caller reads (toSideStats). A deleted
// one's Binary is cleared, as it has no to side.
func parseRawNumStat(out []byte) ([]Stat, []int, error) {
	fields := nulFields(out)
	var raws []rawEntry
	i := 0
	// A --numstat entry starts with a count or "-", never ":".
	for i < len(fields) && strings.HasPrefix(fields[i], ":") {
		parts := strings.Fields(fields[i])
		if len(parts) != 5 || parts[4] == "" {
			return nil, nil, fmt.Errorf("bad raw field %q", fields[i])
		}
		e := rawEntry{status: parts[4][0], fromBlob: parts[2], toBlob: parts[3]}
		switch e.status {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return nil, nil, fmt.Errorf("raw %q has no old and new path", fields[i])
			}
			e.path = fields[i+2]
			i += 3
		default:
			if i+1 >= len(fields) {
				return nil, nil, fmt.Errorf("raw %q has no path", fields[i])
			}
			e.path = fields[i+1]
			i += 2
		}
		raws = append(raws, e)
	}
	stats, err := parseNumStat(fields[i:])
	if err != nil {
		return nil, nil, fmt.Errorf("parse numstat: %w", err)
	}
	if len(stats) != len(raws) {
		return nil, nil, fmt.Errorf("%d raw entries but %d numstat entries", len(raws), len(stats))
	}
	var toRead []int
	for j, e := range raws {
		s := &stats[j]
		if s.Path != e.path {
			return nil, nil, fmt.Errorf("raw entry %d is %q but numstat entry %d is %q", j, e.path, j, s.Path)
		}
		s.Same = e.fromBlob == e.toBlob
		switch {
		case !s.Binary || s.Same:
		case e.status == 'D':
			s.Binary = false
		default:
			toRead = append(toRead, j)
		}
	}
	return stats, toRead, nil
}

// parseNumStat parses --numstat -z output's fields: the added and deleted
// counts, each ended by a tab, then the path, ended by a NUL; or for a
// rename, no path, then its old path and its new one, each ended by a NUL.
// A file git counted no lines for, as binary, has "-" counts.
func parseNumStat(fields []string) ([]Stat, error) {
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
// The merge runs in an attrless repo (attrless), so no attribute, such as
// merge=union, changes how a file merges. merge-tree writes no ref, but it
// does write the merged blobs and trees, and any merge base it builds from
// several, as new objects. They go to the attrless repo, removed after, so
// the merge leaves nothing in the bare repo.
func (r *Repo) MergeTree(ctx context.Context, target, head string) (conflicts []string, err error) {
	if err := checkSHAs(target, head); err != nil {
		return nil, fmt.Errorf("merge tree: %w", err)
	}
	dir, a, err := r.attrless(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("merge tree of %s and %s: %w", target, head, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	out, err := a.git(ctx, "", "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", target, head)
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

// attrTreeSince is the first git version, major and minor, with attr.tree.
var attrTreeSince = [2]int{2, 42}

// emptyTrees are the empty tree's ID in each object format git has.
var emptyTrees = map[string]string{
	"sha1":   "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
	"sha256": "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321",
}

// gitVersion is the major and minor version of the git on PATH, read once
// it's read without an error.
var gitVersion = &gitVersionCache{read: readGitVersion}

// gitVersionCache keeps the git version read gives once a read succeeds.
// A failed read is returned and not kept, so the next get reads again: one
// failed git version call doesn't fail every diff until a restart.
type gitVersionCache struct {
	read func() ([2]int, error)

	mu      sync.Mutex
	version [2]int
	ok      bool
}

// get returns the kept version, or reads it, keeping it if the read
// succeeds.
func (c *gitVersionCache) get() ([2]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok {
		return c.version, nil
	}
	v, err := c.read()
	if err != nil {
		return [2]int{}, fmt.Errorf("git version: %w", err)
	}
	c.version, c.ok = v, true
	return v, nil
}

// readGitVersion reads the major and minor version of the git on PATH.
func readGitVersion() ([2]int, error) {
	cmd := exec.Command("git", "version")
	cmd.Env = gitEnv(os.Environ(), "", "")
	out, err := cmd.Output()
	if err != nil {
		return [2]int{}, fmt.Errorf("read git's version: %w", err)
	}
	v, err := parseGitVersion(string(out))
	if err != nil {
		return [2]int{}, fmt.Errorf("read git's version: %w", err)
	}
	return v, nil
}

// versionLine matches git version's output, capturing the major and minor
// version: "git version 2.47.3", maybe followed by more, as in "2.39.5
// (Apple Git-154)" or "2.41.0.windows.1".
var versionLine = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// parseGitVersion reads git version's output as its major and minor
// version.
func parseGitVersion(out string) ([2]int, error) {
	m := versionLine.FindStringSubmatch(out)
	if m == nil {
		return [2]int{}, fmt.Errorf("unrecognized version %q", strings.TrimSpace(out))
	}
	var v [2]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [2]int{}, fmt.Errorf("version %q: %w", strings.TrimSpace(out), err)
		}
		v[i] = n
	}
	return v, nil
}

// attrOptions are the git options that turn off the attributes git reads
// from outside a repo, for the given git version and object format:
// core.attributesFile=/dev/null for the user's attributes file and, on a
// git with attr.tree, attr.tree naming the empty tree, so no tree's
// .gitattributes is read, even where git would default to HEAD's. An older
// git has no attr.tree, nor reads any tree's .gitattributes in a bare
// repo. The system's attributes file is GIT_ATTR_NOSYSTEM's, in the env.
func attrOptions(version [2]int, format string) []string {
	opts := []string{"-c", "core.attributesFile=/dev/null"}
	if slices.Compare(version[:], attrTreeSince[:]) >= 0 {
		opts = append(opts, "-c", "attr.tree="+emptyTrees[format])
	}
	return opts
}

// attrless makes a fresh, empty bare repo in r's objects dir, as
// quarantine makes a quarantine, and returns its dir, for the caller to
// remove, and a Repo for it. That Repo reads r's objects, as alternates,
// and nothing else of r's: not its config, its refs or its
// info/attributes. New objects, such as merge-tree's, go to its own
// objects dir. sha is a full SHA in r, whose length gives r's object
// format.
//
// Its git calls read no attribute from anywhere (SPEC §10.1): it's made
// with no template, so it has no info/attributes, and its HEAD names no
// commit, so no .gitattributes is read from it; GIT_ATTR_NOSYSTEM=1 skips
// the system's attributes file; and attrOptions turn off the rest.
func (r *Repo) attrless(ctx context.Context, sha string) (string, *Repo, error) {
	version, err := gitVersion.get()
	if err != nil {
		return "", nil, fmt.Errorf("attrless repo: %w", err)
	}
	format := "sha1"
	if len(sha) == 64 {
		format = "sha256"
	}
	objects, err := filepath.Abs(filepath.Join(r.dir, "objects"))
	if err != nil {
		return "", nil, fmt.Errorf("attrless repo: %w", err)
	}
	dir, err := os.MkdirTemp(objects, "tmp_objdir-attrless-")
	if err != nil {
		return "", nil, fmt.Errorf("create attrless repo: %w", err)
	}
	a := &Repo{remote: r.remote, dir: dir, opts: attrOptions(version, format), env: []string{"GIT_ATTR_NOSYSTEM=1"}}
	if _, err := a.git(ctx, "", "init", "--bare", "--quiet", "--template=", "--object-format="+format); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			return "", nil, errors.Join(fmt.Errorf("init attrless repo: %w", err), fmt.Errorf("remove attrless repo: %w", rerr))
		}
		return "", nil, fmt.Errorf("init attrless repo: %w", err)
	}
	a.env = append(a.env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects)
	return dir, a, nil
}

// gitAttrless runs git with args in an attrless repo for r (attrless),
// made for the call and removed after, and returns its stdout, even when
// it fails, as git does. sha is a full SHA in r.
func (r *Repo) gitAttrless(ctx context.Context, sha string, args ...string) (out []byte, err error) {
	dir, a, err := r.attrless(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("without attributes: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	out, err = a.git(ctx, "", args...)
	if err != nil {
		return out, fmt.Errorf("without attributes: %w", err)
	}
	return out, nil
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

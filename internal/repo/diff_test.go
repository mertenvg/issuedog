package repo

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// diffRig is a work tree whose commits are pushed to a bare repo, read
// through a Repo as the daemon's bare clone.
type diffRig struct {
	t    *testing.T
	work string
	r    *Repo
}

func newDiffRig(t *testing.T) *diffRig {
	t.Helper()
	dir := t.TempDir()
	d := &diffRig{t: t, work: filepath.Join(dir, "work"), r: New("", filepath.Join(dir, "r.git"))}
	run(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", d.r.dir)
	run(t, dir, "init", "--quiet", "--initial-branch=main", d.work)
	return d
}

// commit writes files (path to content), removes the paths in remove,
// commits everything, pushes it as main and returns the commit's SHA.
func (d *diffRig) commit(files map[string]string, remove ...string) string {
	d.t.Helper()
	for path, content := range files {
		full := filepath.Join(d.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			d.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			d.t.Fatal(err)
		}
	}
	for _, path := range remove {
		if err := os.Remove(filepath.Join(d.work, path)); err != nil {
			d.t.Fatal(err)
		}
	}
	run(d.t, d.work, "add", "-A")
	run(d.t, d.work, "commit", "--quiet", "--allow-empty", "-m", "change")
	run(d.t, d.work, "push", "--quiet", "--force", d.r.dir, "HEAD:refs/heads/main")
	return run(d.t, d.work, "rev-parse", "HEAD")
}

// diffMarker is a skip marker of this repo's verify.skip_markers, joined
// from two strings, so no line here holds one and rule 3 (SPEC §10.1)
// doesn't fail a task that adds these lines.
const diffMarker = "t." + "Skip("

// odd is a path with a space and a newline in it, and accented is one
// with a byte past ASCII: git quotes both.
const odd, accented = "docs/a file\nname.md", "docs/café.md"

// diffCommits makes two commits, base and head, that add, delete, modify and
// rename files, among them a binary file, odd and accented.
func diffCommits(d *diffRig) (base, head string) {
	d.t.Helper()
	base = d.commit(map[string]string{
		"keep.go":       "package keep\n\nfunc A() {}\n",
		"gone.go":       "package gone\n",
		"old/moved.go":  "package moved\n\n// one\n// two\n// three\n// four\n",
		"img.bin":       "\x00\x01\x02",
		"space name.go": "package x\n",
	})
	head = d.commit(map[string]string{
		"keep.go":       "package keep\n\nfunc A() {}\n\nfunc B() {}\nfunc C() {}\n",
		"new/moved.go":  "package moved\n\n// one\n// two\n// three\n// four\n// five\n",
		"img.bin":       "\x00\x03\x04\x05",
		"added.go":      "package added",
		odd:             "first\nsecond\n",
		accented:        "é\n",
		"space name.go": "package x\n\nvar y = 1\n",
	}, "gone.go", "old/moved.go")
	return base, head
}

// Changes lists each file the diff changes. Without renames, a rename is
// its old path's deletion and its new path's addition; with them, one R
// change names both. Paths with spaces and newlines come back as they are.
func TestChanges(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	base, head := diffCommits(d)
	ctx := context.Background()

	got, err := d.r.Changes(ctx, base, head, false)
	if err != nil {
		t.Fatalf("Changes without renames: %v", err)
	}
	want := []Change{
		{Status: 'A', Path: "added.go"},
		{Status: 'A', Path: odd},
		{Status: 'A', Path: accented},
		{Status: 'D', Path: "gone.go"},
		{Status: 'M', Path: "img.bin"},
		{Status: 'M', Path: "keep.go"},
		{Status: 'A', Path: "new/moved.go"},
		{Status: 'D', Path: "old/moved.go"},
		{Status: 'M', Path: "space name.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changes without renames =\n%+v\nwant\n%+v", got, want)
	}

	got, err = d.r.Changes(ctx, base, head, true)
	if err != nil {
		t.Fatalf("Changes with renames: %v", err)
	}
	want = []Change{
		{Status: 'A', Path: "added.go"},
		{Status: 'A', Path: odd},
		{Status: 'A', Path: accented},
		{Status: 'D', Path: "gone.go"},
		{Status: 'M', Path: "img.bin"},
		{Status: 'M', Path: "keep.go"},
		{Status: 'R', Path: "new/moved.go", From: "old/moved.go"},
		{Status: 'M', Path: "space name.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changes with renames =\n%+v\nwant\n%+v", got, want)
	}

	if got, err := d.r.Changes(ctx, head, head, false); err != nil || len(got) != 0 {
		t.Errorf("Changes(head, head) = %+v, %v; want none", got, err)
	}
}

// AddedLines gives the lines the diff adds to the files asked for, with
// their line numbers in the head: only added lines, none from a binary
// file, only the moved file's new line, and none from files not asked for.
func TestAddedLines(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	base, head := diffCommits(d)
	ctx := context.Background()

	got, err := d.r.AddedLines(ctx, base, head, []string{"keep.go", "new/moved.go", "img.bin", "added.go", odd, accented, "space name.go", "gone.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	want := []Added{
		{Path: "added.go", Line: 1, Text: "package added"},
		{Path: odd, Line: 1, Text: "first"},
		{Path: odd, Line: 2, Text: "second"},
		{Path: accented, Line: 1, Text: "é"},
		{Path: "keep.go", Line: 4, Text: ""},
		{Path: "keep.go", Line: 5, Text: "func B() {}"},
		{Path: "keep.go", Line: 6, Text: "func C() {}"},
		{Path: "new/moved.go", Line: 7, Text: "// five"},
		{Path: "space name.go", Line: 2, Text: ""},
		{Path: "space name.go", Line: 3, Text: "var y = 1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddedLines =\n%+v\nwant\n%+v", got, want)
	}

	got, err = d.r.AddedLines(ctx, base, head, []string{"keep.go"})
	if err != nil {
		t.Fatalf("AddedLines of keep.go: %v", err)
	}
	if len(got) != 3 || got[0].Path != "keep.go" || got[2].Text != "func C() {}" {
		t.Errorf("AddedLines of keep.go = %+v, want its 3 lines only", got)
	}

	if got, err := d.r.AddedLines(ctx, base, head, nil); err != nil || len(got) != 0 {
		t.Errorf("AddedLines of no paths = %+v, %v; want none", got, err)
	}
}

// A line whose text looks like a patch header, such as "++ b/x" added as
// "+++ b/x", stays a line of its file: the hunk header's counts say how
// many lines follow.
func TestAddedLinesHeaderLikeText(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	base := d.commit(map[string]string{"a_test.go": "one\n"})
	head := d.commit(map[string]string{"a_test.go": "one\n++ b/other_test.go\n@@ -1 +1 @@\ndiff --git a/x b/x\n" + diffMarker + "\n"})
	got, err := d.r.AddedLines(context.Background(), base, head, []string{"a_test.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	want := []Added{
		{Path: "a_test.go", Line: 2, Text: "++ b/other_test.go"},
		{Path: "a_test.go", Line: 3, Text: "@@ -1 +1 @@"},
		{Path: "a_test.go", Line: 4, Text: "diff --git a/x b/x"},
		{Path: "a_test.go", Line: 5, Text: diffMarker},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddedLines =\n%+v\nwant\n%+v", got, want)
	}
}

// Git's "\ No newline at end of file" after a removed line, or an added
// one, is no line of the file.
func TestAddedLinesNoNewline(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	base := d.commit(map[string]string{"a_test.go": "one\ntwo"})
	head := d.commit(map[string]string{"a_test.go": "one\ntwo\nthree"})
	got, err := d.r.AddedLines(context.Background(), base, head, []string{"a_test.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	want := []Added{{Path: "a_test.go", Line: 2, Text: "two"}, {Path: "a_test.go", Line: 3, Text: "three"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddedLines =\n%+v\nwant\n%+v", got, want)
	}
}

// Both reads refuse an end that isn't a full SHA, before running git.
func TestDiffRefusesNonSHA(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	sha := d.commit(map[string]string{"a": "a\n"})
	ctx := context.Background()
	for _, tc := range []struct{ from, to string }{
		{"main", sha},
		{sha, "HEAD"},
		{sha[:12], sha},
		{sha, "--output=/tmp/x"},
		{"", sha},
	} {
		if _, err := d.r.Changes(ctx, tc.from, tc.to, false); err == nil || !strings.Contains(err.Error(), "invalid SHA") {
			t.Errorf("Changes(%q, %q) error = %v, want invalid SHA", tc.from, tc.to, err)
		}
		if _, err := d.r.AddedLines(ctx, tc.from, tc.to, []string{"a"}); err == nil || !strings.Contains(err.Error(), "invalid SHA") {
			t.Errorf("AddedLines(%q, %q) error = %v, want invalid SHA", tc.from, tc.to, err)
		}
		if _, err := d.r.NumStat(ctx, tc.from, tc.to); err == nil || !strings.Contains(err.Error(), "invalid SHA") {
			t.Errorf("NumStat(%q, %q) error = %v, want invalid SHA", tc.from, tc.to, err)
		}
		if _, err := d.r.MergeBase(ctx, tc.from, tc.to); err == nil || !strings.Contains(err.Error(), "invalid SHA") {
			t.Errorf("MergeBase(%q, %q) error = %v, want invalid SHA", tc.from, tc.to, err)
		}
		if _, err := d.r.MergeTree(ctx, tc.from, tc.to); err == nil || !strings.Contains(err.Error(), "invalid SHA") {
			t.Errorf("MergeTree(%q, %q) error = %v, want invalid SHA", tc.from, tc.to, err)
		}
	}
}

// The diff reads run no diff or filter driver, textconv or external diff,
// whatever the attributes say: here a .gitattributes in the head, the same
// lines in the bare repo's info/attributes and attr.tree pointing at the
// head, with every driver defined in the bare repo's config. Plain git diff
// with the same setup does run them, so the setup would catch a read that
// let them.
func TestDiffRunsNoDriver(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	scripts := t.TempDir()
	marker := func(name string) string { return filepath.Join(scripts, name+".ran") }
	script := func(name, body string) string {
		path := filepath.Join(scripts, name+".sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker(name)+"'\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	attrs := "* diff=evil filter=evil\n"
	base := d.commit(map[string]string{"a_test.go": "one\n"})
	head := d.commit(map[string]string{"a_test.go": "one\n" + diffMarker + "\n", ".gitattributes": attrs})
	if err := os.WriteFile(filepath.Join(d.r.dir, "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"diff.evil.command", script("ext", "")},
		{"diff.evil.textconv", script("textconv", "cat \"$1\"\n")},
		{"diff.external", script("external", "")},
		{"filter.evil.clean", script("clean", "cat\n")},
		{"filter.evil.smudge", script("smudge", "cat\n")},
		{"attr.tree", head},
	} {
		run(t, d.r.dir, "--git-dir=.", "config", kv[0], kv[1])
	}
	ran := func() []string {
		var names []string
		for _, n := range []string{"ext", "textconv", "external", "clean", "smudge"} {
			if _, err := os.Stat(marker(n)); err == nil {
				names = append(names, n)
				if err := os.Remove(marker(n)); err != nil {
					t.Fatal(err)
				}
			}
		}
		return names
	}

	run(t, d.r.dir, "--git-dir=.", "diff", base, head)
	run(t, d.r.dir, "--git-dir=.", "diff", "--no-ext-diff", "--textconv", base, head)
	if got := ran(); !slices.Contains(got, "ext") || !slices.Contains(got, "textconv") {
		t.Fatalf("plain git diff ran %v, want ext and textconv among them, or the setup proves nothing", got)
	}

	ctx := context.Background()
	if _, err := d.r.Changes(ctx, base, head, false); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if _, err := d.r.Changes(ctx, base, head, true); err != nil {
		t.Fatalf("Changes with renames: %v", err)
	}
	added, err := d.r.AddedLines(ctx, base, head, []string{"a_test.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	stats, err := d.r.NumStat(ctx, base, head)
	if err != nil {
		t.Fatalf("NumStat: %v", err)
	}
	if want := []Stat{{Path: ".gitattributes", Added: 1}, {Path: "a_test.go", Added: 1}}; !reflect.DeepEqual(stats, want) {
		t.Errorf("NumStat = %+v, want git's own %+v", stats, want)
	}
	if got := ran(); len(got) != 0 {
		t.Errorf("the diff reads ran %v, want no driver", got)
	}
	if want := []Added{{Path: "a_test.go", Line: 2, Text: diffMarker}}; !reflect.DeepEqual(added, want) {
		t.Errorf("AddedLines = %+v, want git's own %+v", added, want)
	}
}

// NumStat gives each file's added and deleted lines, as a PR's stats
// count them: a rename counts only its changed lines, under its new path,
// a deletion its old lines, a binary file none, and a path git quotes
// comes back as it is.
func TestNumStat(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	base, head := diffCommits(d)
	got, err := d.r.NumStat(context.Background(), base, head)
	if err != nil {
		t.Fatalf("NumStat: %v", err)
	}
	want := []Stat{
		{Path: "added.go", Added: 1},
		{Path: odd, Added: 2},
		{Path: accented, Added: 1},
		{Path: "gone.go", Deleted: 1},
		{Path: "img.bin", Binary: true},
		{Path: "keep.go", Added: 3},
		{Path: "new/moved.go", Added: 1},
		{Path: "space name.go", Added: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NumStat =\n%+v\nwant\n%+v", got, want)
	}

	changed := d.commit(map[string]string{"keep.go": "package keep\n\nfunc A() { _ = 1 }\n\nfunc B() {}\nfunc C() {}\n"})
	if got, err := d.r.NumStat(context.Background(), head, changed); err != nil || !reflect.DeepEqual(got, []Stat{{Path: "keep.go", Added: 1, Deleted: 1}}) {
		t.Errorf("NumStat of one changed line = %+v, %v; want 1 added and 1 deleted", got, err)
	}
}

// binaryContent is n bytes git takes as binary, as it holds a NUL, and
// that differ from line to line, so git can find a rename of it.
func binaryContent(n int) string {
	var b strings.Builder
	b.WriteString("\x00")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "row %d\n", i)
	}
	return b.String()
}

// git's --numstat counts "-" for a file binary in either side, even a
// binary moved unchanged, and without renames, a move is a deletion and an
// addition. NumStat finds renames and tells them apart by blob: a binary
// moved unchanged, or with only its mode changed, is Same; one moved and
// changed isn't. A file is Binary by its to side: a binary turned into
// text counts its new lines as added, text turned into a binary counts
// none, and a deleted binary isn't Binary. A path that starts with ":",
// as pathspec magic would, is read as it is.
func TestNumStatBinarySides(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	img, changed := binaryContent(2000), binaryContent(3000)
	base := d.commit(map[string]string{
		"img.bin":   img,
		"chg.bin":   changed,
		":b2t.dat":  binaryContent(100),
		"t2b.dat":   "one\ntwo\nthree\n",
		"gone.bin":  binaryContent(100),
		"mode.bin":  binaryContent(200),
		"plain.txt": "plain\n",
	})
	if err := os.Chmod(filepath.Join(d.work, "mode.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := d.commit(map[string]string{
		"pics/img.bin":  img,
		"moved/chg.bin": changed + "row extra\n",
		":b2t.dat":      "a\nb\nc\nd\n",
		"t2b.dat":       "one\x00two\n",
		"text/plain.md": "plain\n",
	}, "img.bin", "chg.bin", "gone.bin", "plain.txt")

	plain := run(t, d.r.dir, "--git-dir=.", "diff-tree", "-r", "--numstat", "--find-renames", base, head)
	for _, line := range []string{"-\t-\timg.bin => pics/img.bin", "-\t-\tchg.bin => moved/chg.bin", "-\t-\t:b2t.dat", "-\t-\tt2b.dat", "-\t-\tmode.bin"} {
		if !strings.Contains(plain, line) {
			t.Fatalf("plain numstat =\n%s\nwant %q, or the setup proves nothing", plain, line)
		}
	}
	noRenames := run(t, d.r.dir, "--git-dir=.", "diff-tree", "-r", "--numstat", "--no-renames", base, head)
	if lines := strings.Split(noRenames, "\n"); !slices.Contains(lines, "-\t-\timg.bin") || !slices.Contains(lines, "-\t-\tpics/img.bin") {
		t.Fatalf("numstat without renames =\n%s\nwant img.bin's deletion and pics/img.bin's addition", noRenames)
	}

	got, err := d.r.NumStat(context.Background(), base, head)
	if err != nil {
		t.Fatalf("NumStat: %v", err)
	}
	want := map[string]Stat{
		"pics/img.bin":  {Path: "pics/img.bin", Binary: true, Same: true},
		"moved/chg.bin": {Path: "moved/chg.bin", Binary: true},
		":b2t.dat":      {Path: ":b2t.dat", Added: 4},
		"t2b.dat":       {Path: "t2b.dat", Binary: true},
		"gone.bin":      {Path: "gone.bin"},
		"mode.bin":      {Path: "mode.bin", Binary: true, Same: true},
		"text/plain.md": {Path: "text/plain.md", Same: true},
	}
	if len(got) != len(want) {
		t.Errorf("NumStat =\n%+v\nwant %d files", got, len(want))
	}
	for _, s := range got {
		if s != want[s.Path] {
			t.Errorf("NumStat of %s = %+v, want %+v", s.Path, s, want[s.Path])
		}
	}
}

// parseRawNumStat refuses output whose --raw and --numstat entries don't
// match, one for one and path for path, or that it can't read.
func TestParseRawNumStatErrors(t *testing.T) {
	t.Parallel()
	zero := strings.Repeat("0", 40)
	blob := strings.Repeat("a", 40)
	for name, out := range map[string]string{
		"fewer numstat": ":000000 100644 " + zero + " " + blob + " A\x00a.go\x00",
		"more numstat":  "1\t0\ta.go\x00",
		"other path":    ":000000 100644 " + zero + " " + blob + " A\x00a.go\x001\t0\tb.go\x00",
		"short raw":     ":000000 100644 " + zero + " A\x00a.go\x001\t0\ta.go\x00",
		"raw no path":   ":000000 100644 " + zero + " " + blob + " A",
		"rename no new": ":100644 100644 " + blob + " " + blob + " R100\x00a.go",
		"bad numstat":   ":000000 100644 " + zero + " " + blob + " A\x00a.go\x00x\t0\ta.go\x00",
	} {
		if got, toRead, err := parseRawNumStat([]byte(out)); err == nil {
			t.Errorf("parseRawNumStat(%s) = %+v, %v; want an error", name, got, toRead)
		}
	}
}

// mergeRig is a diffRig whose work tree branches. Each commit still goes
// to the bare repo.
type mergeRig struct{ *diffRig }

// on checks out a new branch, name, at sha, or an orphan one with nothing
// staged for an empty sha, for the next commits.
func (m mergeRig) on(name, sha string) {
	m.t.Helper()
	if sha == "" {
		run(m.t, m.work, "checkout", "--quiet", "--orphan", name)
		run(m.t, m.work, "rm", "-r", "--quiet", "--cached", "--ignore-unmatch", ".")
		return
	}
	run(m.t, m.work, "checkout", "--quiet", "-b", name, sha)
}

// merge merges sha into what's checked out, pushes the merge to the bare
// repo and returns its SHA.
func (m mergeRig) merge(sha string) string {
	m.t.Helper()
	run(m.t, m.work, "merge", "--quiet", "--no-ff", "--no-edit", sha)
	run(m.t, m.work, "push", "--quiet", "--force", m.r.dir, "HEAD:refs/heads/main")
	return run(m.t, m.work, "rev-parse", "HEAD")
}

// MergeBase finds the one merge base of two branches, both of a
// criss-cross merge, and none for commits with no common history. A commit
// the repo lacks is an error.
func TestMergeBase(t *testing.T) {
	t.Parallel()
	m := mergeRig{newDiffRig(t)}
	ctx := context.Background()
	base := m.commit(map[string]string{"a": "a\n"})
	m.on("x", base)
	x1 := m.commit(map[string]string{"x": "x\n"})
	m.on("y", base)
	y1 := m.commit(map[string]string{"y": "y\n"})
	if got, err := m.r.MergeBase(ctx, x1, y1); err != nil || !slices.Equal(got, []string{base}) {
		t.Errorf("MergeBase of two branches = %q, %v; want [%s]", got, err, base)
	}

	// y2 merges x1 into y1, and x2 y1 into x1, so x1 and y1 are both best
	// common ancestors of x2 and y2.
	y2 := m.merge(x1)
	m.on("x2", x1)
	x2 := m.merge(y1)
	got, err := m.r.MergeBase(ctx, x2, y2)
	slices.Sort(got)
	want := []string{x1, y1}
	slices.Sort(want)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("MergeBase of a criss-cross = %q, %v; want %q", got, err, want)
	}

	m.on("orphan", "")
	alone := m.commit(map[string]string{"b": "b\n"})
	if got, err := m.r.MergeBase(ctx, x1, alone); err != nil || len(got) != 0 {
		t.Errorf("MergeBase with no common history = %q, %v; want none and no error", got, err)
	}

	missing := strings.Repeat("1", len(base))
	if got, err := m.r.MergeBase(ctx, x1, missing); err == nil {
		t.Errorf("MergeBase with a missing commit = %q, want an error", got)
	}
}

// bareState is what a merge must leave as it was in the bare repo: its
// refs, its objects, and the entries of its objects dir.
func bareState(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return run(t, dir, "--git-dir=.", "for-each-ref") + "\n" + run(t, dir, "--git-dir=.", "count-objects", "-v") + "\n" + strings.Join(names, " ")
}

// MergeTree finds a clean merge clean, and names each conflicting path
// once: a line changed on both sides, a path git quotes, and a file one
// side deletes while the other changes it. It leaves the bare repo's refs
// and objects as they were. Commits with no common history are an error.
func TestMergeTree(t *testing.T) {
	t.Parallel()
	m := mergeRig{newDiffRig(t)}
	ctx := context.Background()
	base := m.commit(map[string]string{"a.go": "one\n", odd: "two\n", "gone.go": "three\n", "same.go": "four\n"})
	m.on("target", base)
	target := m.commit(map[string]string{"a.go": "one, target\n", odd: "two, target\n", "gone.go": "three, target\n", "other.go": "x\n"})
	m.on("clean", base)
	clean := m.commit(map[string]string{"same.go": "four, head\n", "new.go": "y\n"})
	m.on("conflict", base)
	conflict := m.commit(map[string]string{"a.go": "one, head\n", odd: "two, head\n", "same.go": "four, head\n"}, "gone.go")
	m.on("orphan", "")
	alone := m.commit(map[string]string{"b": "b\n"})
	before := bareState(t, m.r.dir)

	if got, err := m.r.MergeTree(ctx, target, clean); err != nil || len(got) != 0 {
		t.Errorf("MergeTree of a clean merge = %q, %v; want no conflicts", got, err)
	}
	got, err := m.r.MergeTree(ctx, target, conflict)
	slices.Sort(got)
	want := []string{"a.go", odd, "gone.go"}
	slices.Sort(want)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("MergeTree of a conflicting merge = %q, %v; want %q", got, err, want)
	}
	if got, err := m.r.MergeTree(ctx, target, alone); err == nil {
		t.Errorf("MergeTree with no common history = %q, want an error", got)
	}
	if after := bareState(t, m.r.dir); after != before {
		t.Errorf("the merges changed the bare repo from\n%s\nto\n%s", before, after)
	}
}

// The diff reads ignore every attribute: here -diff on a test file and a
// Go file, and diff on a binary file, both in the bare repo's
// info/attributes and in a .gitattributes on main, with attr.tree naming
// main for a git that has it. A plain diff-tree in the bare repo with the
// same setup takes the test file as binary and the binary file as text, so
// the setup would catch a read that let them. The reads leave the bare
// repo's objects dir as it was.
func TestDiffIgnoresAttributes(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	attrs := "*_test.go -diff\n*.go -diff\nimg.bin diff\n"
	base := d.commit(map[string]string{"a_test.go": "one\n", "b.go": "package b\n", "img.bin": "\x00\x01", ".gitattributes": attrs})
	head := d.commit(map[string]string{"a_test.go": "one\n" + diffMarker + "\n", "b.go": "package b\n\nvar x = 1\n", "img.bin": "\x00\x02"})
	if err := os.WriteFile(filepath.Join(d.r.dir, "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, d.r.dir, "--git-dir=.", "config", "attr.tree", "main")

	plain := run(t, d.r.dir, "--git-dir=.", "diff-tree", "-p", base, head)
	stat := run(t, d.r.dir, "--git-dir=.", "diff-tree", "--numstat", base, head)
	if !strings.Contains(plain, "Binary files a/a_test.go and b/a_test.go differ") || !strings.Contains(stat, "1\t1\timg.bin") {
		t.Fatalf("plain diff-tree =\n%s\n%s\nwant a_test.go binary and img.bin text, or the setup proves nothing", plain, stat)
	}
	before := bareState(t, d.r.dir)

	ctx := context.Background()
	added, err := d.r.AddedLines(ctx, base, head, []string{"a_test.go", "b.go", "img.bin"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	want := []Added{
		{Path: "a_test.go", Line: 2, Text: diffMarker},
		{Path: "b.go", Line: 2, Text: ""},
		{Path: "b.go", Line: 3, Text: "var x = 1"},
	}
	if !reflect.DeepEqual(added, want) {
		t.Errorf("AddedLines =\n%+v\nwant git's own\n%+v", added, want)
	}
	stats, err := d.r.NumStat(ctx, base, head)
	if err != nil {
		t.Fatalf("NumStat: %v", err)
	}
	if want := []Stat{{Path: "a_test.go", Added: 1}, {Path: "b.go", Added: 2}, {Path: "img.bin", Binary: true}}; !reflect.DeepEqual(stats, want) {
		t.Errorf("NumStat = %+v, want git's own %+v", stats, want)
	}
	if after := bareState(t, d.r.dir); after != before {
		t.Errorf("the reads changed the bare repo from\n%s\nto\n%s", before, after)
	}
}

// The diff reads ignore the user's attributes file too, which git reads
// though no config names it. It sets HOME and XDG_CONFIG_HOME, so it
// doesn't run in parallel.
func TestDiffIgnoresUserAttributes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "git", "attributes"), []byte("*_test.go -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newDiffRig(t)
	base := d.commit(map[string]string{"a_test.go": "one\n"})
	head := d.commit(map[string]string{"a_test.go": "one\n" + diffMarker + "\n"})
	if plain := run(t, d.r.dir, "--git-dir=.", "diff-tree", "-p", base, head); !strings.Contains(plain, "Binary files") {
		t.Fatalf("plain diff-tree =\n%s\nwant a_test.go binary, or the setup proves nothing", plain)
	}

	added, err := d.r.AddedLines(context.Background(), base, head, []string{"a_test.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	if want := []Added{{Path: "a_test.go", Line: 2, Text: diffMarker}}; !reflect.DeepEqual(added, want) {
		t.Errorf("AddedLines = %+v, want git's own %+v", added, want)
	}
}

// The diff reads and the merge work in a SHA-256 repo too: the attrless
// repo they run in takes the bare repo's object format.
func TestDiffSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := &diffRig{t: t, work: filepath.Join(dir, "work"), r: New("", filepath.Join(dir, "r.git"))}
	run(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", "--object-format=sha256", d.r.dir)
	run(t, dir, "init", "--quiet", "--initial-branch=main", "--object-format=sha256", d.work)
	base := d.commit(map[string]string{"a_test.go": "one\n"})
	head := d.commit(map[string]string{"a_test.go": "one\n" + diffMarker + "\n"})
	if len(head) != 64 {
		t.Fatalf("head = %q, want a SHA-256", head)
	}
	ctx := context.Background()

	if got, err := d.r.Changes(ctx, base, head, true); err != nil || !reflect.DeepEqual(got, []Change{{Status: 'M', Path: "a_test.go"}}) {
		t.Errorf("Changes = %+v, %v; want a_test.go modified", got, err)
	}
	if got, err := d.r.AddedLines(ctx, base, head, []string{"a_test.go"}); err != nil || !reflect.DeepEqual(got, []Added{{Path: "a_test.go", Line: 2, Text: diffMarker}}) {
		t.Errorf("AddedLines = %+v, %v; want the marker", got, err)
	}
	if got, err := d.r.NumStat(ctx, base, head); err != nil || !reflect.DeepEqual(got, []Stat{{Path: "a_test.go", Added: 1}}) {
		t.Errorf("NumStat = %+v, %v; want 1 added", got, err)
	}
	if got, err := d.r.MergeTree(ctx, base, head); err != nil || len(got) != 0 {
		t.Errorf("MergeTree = %q, %v; want a clean merge", got, err)
	}
}

// A hunk line too long to scan is the scanner's error, bufio.ErrTooLong,
// not a hunk that ends early.
func TestHunkLineTooLong(t *testing.T) {
	t.Parallel()
	sc := bufio.NewScanner(strings.NewReader("+" + strings.Repeat("x", 100) + "\n"))
	sc.Buffer(nil, 16)
	if got, err := hunk(sc, 0, 1); !errors.Is(err, bufio.ErrTooLong) || got != nil {
		t.Errorf("hunk of a line past the buffer = %q, %v; want bufio.ErrTooLong", got, err)
	}
}

// attrOptions turn off the user's attributes file on every git, and set
// attr.tree to the object format's empty tree only from git 2.42, which
// has it.
func TestAttrOptions(t *testing.T) {
	t.Parallel()
	for format, want := range map[string]string{
		"sha1":   fmt.Sprintf("%x", sha1.Sum([]byte("tree 0\x00"))),
		"sha256": fmt.Sprintf("%x", sha256.Sum256([]byte("tree 0\x00"))),
	} {
		if emptyTrees[format] != want {
			t.Errorf("emptyTrees[%s] = %s, want %s", format, emptyTrees[format], want)
		}
		for _, tc := range []struct {
			version [2]int
			want    []string
		}{
			{[2]int{2, 39}, []string{"-c", "core.attributesFile=/dev/null"}},
			{[2]int{2, 41}, []string{"-c", "core.attributesFile=/dev/null"}},
			{[2]int{2, 42}, []string{"-c", "core.attributesFile=/dev/null", "-c", "attr.tree=" + want}},
			{[2]int{2, 47}, []string{"-c", "core.attributesFile=/dev/null", "-c", "attr.tree=" + want}},
			{[2]int{3, 0}, []string{"-c", "core.attributesFile=/dev/null", "-c", "attr.tree=" + want}},
			{[2]int{1, 50}, []string{"-c", "core.attributesFile=/dev/null"}},
		} {
			if got := attrOptions(tc.version, format); !slices.Equal(got, tc.want) {
				t.Errorf("attrOptions(%v, %s) = %q, want %q", tc.version, format, got, tc.want)
			}
		}
	}
}

// parseGitVersion reads the major and minor version from git version's
// output, as git prints it on each platform, and refuses other text.
func TestParseGitVersion(t *testing.T) {
	t.Parallel()
	for out, want := range map[string][2]int{
		"git version 2.39.5\n":                 {2, 39},
		"git version 2.47.3\n":                 {2, 47},
		"git version 2.39.3 (Apple Git-146)\n": {2, 39},
		"git version 2.41.0.windows.1\n":       {2, 41},
		"git version 3.0.0-rc1\n":              {3, 0},
	} {
		if got, err := parseGitVersion(out); err != nil || got != want {
			t.Errorf("parseGitVersion(%q) = %v, %v; want %v", out, got, err, want)
		}
	}
	for _, out := range []string{"", "version 2.39.5", "git version x.y", "git version 99999999999999999999.1"} {
		if got, err := parseGitVersion(out); err == nil {
			t.Errorf("parseGitVersion(%q) = %v, want an error", out, got)
		}
	}
}

// gitVersion keeps the version once a read succeeds, and keeps no failed
// read: after a failure, the next get reads again, so one failed git
// version call doesn't fail every diff until a restart.
func TestGitVersionCacheRetries(t *testing.T) {
	t.Parallel()
	failure := errors.New("git version failed")
	reads := 0
	c := &gitVersionCache{read: func() ([2]int, error) {
		reads++
		if reads == 1 {
			return [2]int{}, failure
		}
		return [2]int{2, 47}, nil
	}}
	if got, err := c.get(); !errors.Is(err, failure) || got != [2]int{} {
		t.Errorf("get after a failed read = %v, %v; want the read's error", got, err)
	}
	if got, err := c.get(); err != nil || got != [2]int{2, 47} {
		t.Errorf("get after a failure = %v, %v; want 2.47, read again", got, err)
	}
	if got, err := c.get(); err != nil || got != [2]int{2, 47} || reads != 2 {
		t.Errorf("get after a success = %v, %v with %d reads; want 2.47, kept from the second read", got, err, reads)
	}
}

package repo

import (
	"context"
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
	head := d.commit(map[string]string{"a_test.go": "one\n++ b/other_test.go\n@@ -1 +1 @@\ndiff --git a/x b/x\nt.Skip(\n"})
	got, err := d.r.AddedLines(context.Background(), base, head, []string{"a_test.go"})
	if err != nil {
		t.Fatalf("AddedLines: %v", err)
	}
	want := []Added{
		{Path: "a_test.go", Line: 2, Text: "++ b/other_test.go"},
		{Path: "a_test.go", Line: 3, Text: "@@ -1 +1 @@"},
		{Path: "a_test.go", Line: 4, Text: "diff --git a/x b/x"},
		{Path: "a_test.go", Line: 5, Text: "t.Skip("},
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
	head := d.commit(map[string]string{"a_test.go": "one\nt.Skip(\n", ".gitattributes": attrs})
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
	if got := ran(); len(got) != 0 {
		t.Errorf("the diff reads ran %v, want no driver", got)
	}
	if want := []Added{{Path: "a_test.go", Line: 2, Text: "t.Skip("}}; !reflect.DeepEqual(added, want) {
		t.Errorf("AddedLines = %+v, want git's own %+v", added, want)
	}
}

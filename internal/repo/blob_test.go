package repo

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Blobs reads each asked-for path that's a file in a commit's tree, as git
// stores it: a regular file, an empty one, a binary one with no newline at
// its end, a nested one, a symlink, as its target, and paths git quotes,
// odd and accented. It matches each path as it is, so a path that looks
// like a pattern, a[1].txt, gives that file alone, not a1.txt, one that
// starts as pathspec magic does, :x.txt, gives that file, not x.txt, and
// a*.txt, which names no file, gives none. A path it lacks, a dir, a
// submodule, a path at an older commit only, and a file under a dir asked
// for but not named are left out. A commit that isn't a full SHA is
// refused, and no paths read nothing.
func TestBlobs(t *testing.T) {
	t.Parallel()
	d := newDiffRig(t)
	ctx := context.Background()
	old := d.commit(map[string]string{"gone.txt": "gone\n"})
	binary := "\x00\x01binary\nno newline"
	files := map[string]string{
		"a.txt": "a\n<<<<<<< x\n", "empty.txt": "", "bin.dat": binary, "dir/b.txt": "b\n", "dir/c.txt": "c\n",
		"a[1].txt": "bracket\n", "a1.txt": "a one\n", ":x.txt": "colon\n", "x.txt": "x\n", odd: "odd\n", accented: "accented\n",
	}
	if err := os.Symlink("a.txt", filepath.Join(d.work, "link")); err != nil {
		t.Fatal(err)
	}
	// sub is a repo of its own in the work tree, which git adds as a
	// submodule's entry, naming a commit the bare repo lacks.
	sub := filepath.Join(d.work, "sub")
	run(t, d.work, "init", "--quiet", sub)
	run(t, sub, "commit", "--quiet", "--allow-empty", "-m", "sub")
	head := d.commit(files, "gone.txt")
	if got := run(t, d.work, "ls-tree", head, "--", "sub"); !strings.HasPrefix(got, "160000 commit ") {
		t.Fatalf("sub in the head's tree is %q, want a submodule's entry", got)
	}

	ask := []string{"a.txt", "empty.txt", "bin.dat", "dir/b.txt", "a[1].txt", ":x.txt", odd, accented, "link",
		"missing.txt", "dir", "a*.txt", "gone.txt", "dir/missing.txt", "sub"}
	got, err := d.r.Blobs(ctx, head, ask)
	if err != nil {
		t.Fatalf("Blobs: %v", err)
	}
	want := map[string]string{"a.txt": files["a.txt"], "empty.txt": "", "bin.dat": binary, "dir/b.txt": "b\n",
		"a[1].txt": "bracket\n", ":x.txt": "colon\n", odd: "odd\n", accented: "accented\n", "link": "a.txt"}
	gotText := map[string]string{}
	for p, c := range got {
		gotText[p] = string(c)
	}
	if !maps.Equal(gotText, want) {
		t.Errorf("Blobs =\n%q\nwant\n%q", gotText, want)
	}

	got, err = d.r.Blobs(ctx, old, []string{"gone.txt", "a.txt"})
	if err != nil || len(got) != 1 || string(got["gone.txt"]) != "gone\n" {
		t.Errorf("Blobs at the older commit = %q, %v; want gone.txt alone", got, err)
	}
	if got, err := d.r.Blobs(ctx, head, nil); err != nil || len(got) != 0 {
		t.Errorf("Blobs of no paths = %q, %v; want none", got, err)
	}
	if got, err := d.r.Blobs(ctx, head[:12], []string{"a.txt"}); err == nil || !strings.Contains(err.Error(), "invalid SHA") || got != nil {
		t.Errorf("Blobs at a short SHA = %q, %v; want an invalid SHA error", got, err)
	}
	missing := strings.Repeat("1", len(head))
	if got, err := d.r.Blobs(ctx, missing, []string{"a.txt"}); err == nil || got != nil {
		t.Errorf("Blobs at a commit the repo lacks = %q, %v; want an error", got, err)
	}
}

// parseBatch refuses cat-file output that isn't a blob's header and its
// content, ended by a newline.
func TestParseBatch(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("a", 40)
	got, err := parseBatch([]byte(id + " blob 3\nabc\n" + strings.Repeat("b", 40) + " blob 0\n\n"))
	if err != nil || len(got) != 2 || string(got[id]) != "abc" || len(got[strings.Repeat("b", 40)]) != 0 {
		t.Errorf("parseBatch of two blobs = %q, %v; want abc and an empty one", got, err)
	}
	for name, out := range map[string]string{
		"missing":    id + " missing\n",
		"a tree":     id + " tree 3\nabc\n",
		"bad size":   id + " blob x\nabc\n",
		"negative":   id + " blob -1\n\n",
		"short":      id + " blob 9\nabc\n",
		"no newline": id + " blob 3\nabcd",
		"no header":  id + " blob 3",
	} {
		if got, err := parseBatch([]byte(out)); err == nil || got != nil {
			t.Errorf("parseBatch of %s = %q, %v; want an error", name, got, err)
		}
	}
}

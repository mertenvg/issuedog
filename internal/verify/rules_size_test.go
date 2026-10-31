package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// sizeLines is n lines of Go, each different, ending in a newline.
func sizeLines(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("// line " + strings.Repeat("x", i%7) + "\n")
	}
	return b.String()
}

// Rule 6 counts added and deleted lines only in files outside the test
// globs: with the limit at 400, 400 lines of source pass with 1,000 test
// lines beside them, and 401 fail with TooLarge, the count and the limit.
// A changed line counts 2, and a file that only moved counts none.
func TestRule6Size(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	v := rulesConfig()
	if v.MaxDiffLines != 400 {
		t.Fatalf("verify.max_diff_lines = %d, want the default 400", v.MaxDiffLines)
	}
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Config: v}

	f.Head = rr.commit(map[string]string{"big.go": sizeLines(400), "big_test.go": sizeLines(1000), "e2e/flow.ts": sizeLines(50)})
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of 400 source lines and 1,050 test lines = %+v, want nil", got)
	}

	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"big.go": sizeLines(401), "big_test.go": sizeLines(1000)})
	got := check(t, Rule6, f)
	wantFinding(t, got, 6, TooLarge+": the diff changes 401 lines in files outside verify.test_globs, over verify.max_diff_lines of 400.")
	if got != nil && strings.Contains(got.Detail, "binary") {
		t.Errorf("detail =\n%s\nwant no binary part", got.Detail)
	}

	// main.go's one line changes, so it counts as one deleted and one added.
	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"main.go": "package other\n"})
	f.Config.MaxDiffLines = 1
	wantFinding(t, check(t, Rule6, f), 6, "changes 2 lines", "of 1.")
	f.Config.MaxDiffLines = 2
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of a changed line with the limit at 2 = %+v, want nil", got)
	}

	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"cmd/web/app.ts": "export {}\n"}, "web/app.ts")
	f.Config.MaxDiffLines = 1
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of a file that only moved = %+v, want nil, as it counts no lines", got)
	}
}

// Rule 6 fails a binary file the diff adds or changes, in or out of the
// test globs, unless it matches verify.binary_allow, naming each one. A
// binary's lines count for nothing, and deleting one passes.
func TestRule6Binaries(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	v := rulesConfig()
	v.BinaryAllow = []string{"img/*.png"}
	v.MaxDiffLines = 1
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Config: v}
	binary := "\x00" + strings.Repeat("\n", 500)

	f.Head = rr.commit(map[string]string{"img/logo.png": binary})
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of an allowed binary = %+v, want nil, with its lines counting none", got)
	}

	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"img/logo.png": binary, "img/raw.bin": binary, "e2e/fixture.png": binary})
	got := check(t, Rule6, f)
	wantFinding(t, got, 6, "The diff adds or changes binary files outside verify.binary_allow:\n- e2e/fixture.png\n- img/raw.bin")
	if got != nil && (strings.Contains(got.Detail, "logo.png") || strings.Contains(got.Detail, TooLarge)) {
		t.Errorf("detail =\n%s\nwant only the binaries not allowed, and no size part", got.Detail)
	}

	rr.checkout(base)
	withBinary := rr.commit(map[string]string{"img/raw.bin": binary})
	f.Base, f.Head = withBinary, rr.commit(nil, "img/raw.bin")
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of a deleted binary = %+v, want nil", got)
	}

	// Both parts at once, the size first.
	rr.checkout(base)
	f.Base, f.Head = base, rr.commit(map[string]string{"img/raw.bin": binary, "main.go": "package other\n"})
	wantFinding(t, check(t, Rule6, f), 6, TooLarge+": the diff changes 2 lines", "over verify.max_diff_lines of 1.\n\nThe diff adds or changes binary files")
}

// rule6Binary is n bytes git takes as binary, as it holds a NUL, and that
// differ from line to line, so git can find a rename of it.
func rule6Binary(n int) string {
	var b strings.Builder
	b.WriteString("\x00")
	for i := 0; b.Len() < n; i++ {
		b.WriteString("row " + strings.Repeat("y", i%11) + "\n")
	}
	return b.String()
}

// Rule 6 passes a binary moved with its content unchanged, outside
// verify.binary_allow, and fails one moved and changed. A file is binary
// by its new content: a binary turned into text counts its new lines, and
// text turned into a binary fails as a binary, counting no lines (SPEC
// §10.1).
func TestRule6BinaryMoves(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	img := rule6Binary(3000)
	files := map[string]string{"assets/img.bin": img, "data.dat": rule6Binary(100), "notes.txt": sizeLines(500)}
	for p, c := range rulesBase {
		files[p] = c
	}
	base := rr.commit(files)
	v := rulesConfig()
	v.MaxDiffLines = 3
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Config: v}

	f.Head = rr.commit(map[string]string{"pics/img.bin": img}, "assets/img.bin")
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of a binary moved unchanged = %+v, want nil", got)
	}

	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"pics/img.bin": img + "row extra\n"}, "assets/img.bin")
	if renamed := rulesGit(t, rr.bare.Dir(), "--git-dir=.", "diff-tree", "-r", "--name-status", "--find-renames", base, f.Head); !strings.HasPrefix(renamed, "R") {
		t.Fatalf("diff-tree =\n%s\nwant a rename, or the setup proves nothing", renamed)
	}
	got := check(t, Rule6, f)
	wantFinding(t, got, 6, "The diff adds or changes binary files outside verify.binary_allow:\n- pics/img.bin")
	if got != nil && strings.Contains(got.Detail, "assets/img.bin") {
		t.Errorf("detail =\n%s\nwant only the new path", got.Detail)
	}

	rr.checkout(base)
	f.Head = rr.commit(map[string]string{"data.dat": "a\nb\nc\nd\n"})
	got = check(t, Rule6, f)
	wantFinding(t, got, 6, TooLarge+": the diff changes 4 lines", "of 3.")
	if got != nil && strings.Contains(got.Detail, "binary") {
		t.Errorf("detail =\n%s\nwant no binary part for a binary turned into text", got.Detail)
	}
	f.Config.MaxDiffLines = 4
	if got := check(t, Rule6, f); got != nil {
		t.Errorf("Rule6 of a binary turned into 4 lines of text, with the limit at 4 = %+v, want nil", got)
	}

	rr.checkout(base)
	f.Config.MaxDiffLines = 3
	f.Head = rr.commit(map[string]string{"notes.txt": "now\x00binary\n"})
	got = check(t, Rule6, f)
	wantFinding(t, got, 6, "The diff adds or changes binary files outside verify.binary_allow:\n- notes.txt")
	if got != nil && strings.Contains(got.Detail, TooLarge) {
		t.Errorf("detail =\n%s\nwant no size part, as the binary's 500 old lines count for nothing", got.Detail)
	}
}

// Rule 7 passes a head that merges cleanly with its target, and fails one
// that conflicts, naming each conflicting file, or that shares no history
// with it.
func TestRule7(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	target := rr.commit(map[string]string{"main.go": "package main\n\n// target\n", "docs/guide.md": "target guide\n"})
	rr.checkout(base)
	clean := rr.commit(map[string]string{"README.md": "head\n", "main_test.go": "package main\n\n// head\n"})
	rr.checkout(base)
	conflict := rr.commit(map[string]string{"main.go": "package main\n\n// head\n", "docs/guide.md": "head guide\n", "README.md": "head\n"})
	rulesGit(t, rr.work, "checkout", "--quiet", "--orphan", "alone")
	alone := rr.commit(map[string]string{"other.md": "x\n"})
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Target: target, Config: rulesConfig()}

	f.Head = clean
	if got := check(t, Rule7, f); got != nil {
		t.Errorf("Rule7 of a head that merges cleanly = %+v, want nil", got)
	}

	f.Head = conflict
	got := check(t, Rule7, f)
	wantFinding(t, got, 7, "The head "+conflict+" doesn't merge cleanly with its target "+target, "- docs/guide.md\n- main.go")
	if got != nil && strings.Contains(got.Detail, "README") {
		t.Errorf("detail =\n%s\nwant no README.md, which merges cleanly", got.Detail)
	}

	f.Head = alone
	wantFinding(t, check(t, Rule7, f), 7, "The head "+alone+" shares no history with its target "+target+".")
}

// BaseOf is the one merge base of the head and its target. A criss-cross
// merge's two, or none, are ErrMergeBases.
func TestBaseOf(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	x1 := rr.commit(map[string]string{"x.md": "x\n"})
	rr.checkout(base)
	y1 := rr.commit(map[string]string{"y.md": "y\n"})
	ctx := context.Background()

	if got, err := BaseOf(ctx, Facts{Repo: rr.bare, Head: x1, Target: y1}); err != nil || got != base {
		t.Errorf("BaseOf two branches = %q, %v; want %s", got, err, base)
	}

	merge := func(sha string) string {
		rulesGit(t, rr.work, "merge", "--quiet", "--no-ff", "--no-edit", sha)
		head := rulesGit(t, rr.work, "rev-parse", "HEAD")
		rulesGit(t, rr.work, "push", "--quiet", rr.bare.Dir(), "HEAD:refs/heads/c-"+head)
		return head
	}
	y2 := merge(x1)
	rr.checkout(x1)
	x2 := merge(y1)
	if got, err := BaseOf(ctx, Facts{Repo: rr.bare, Head: x2, Target: y2}); !errors.Is(err, ErrMergeBases) || got != "" {
		t.Errorf("BaseOf a criss-cross merge = %q, %v; want ErrMergeBases", got, err)
	}

	rulesGit(t, rr.work, "checkout", "--quiet", "--orphan", "alone")
	alone := rr.commit(map[string]string{"other.md": "x\n"})
	if got, err := BaseOf(ctx, Facts{Repo: rr.bare, Head: alone, Target: base}); !errors.Is(err, ErrMergeBases) || got != "" {
		t.Errorf("BaseOf with no common history = %q, %v; want ErrMergeBases", got, err)
	}
}

// Rules 6 and 7, and BaseOf, return a failed git read as an error, saying
// which, and no finding.
func TestRules6And7ReadError(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	missing := strings.Repeat("0", len(base))
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Head: missing, Target: base, Config: rulesConfig()}
	for want, rule := range map[string]func(context.Context, Facts) (*Finding, error){"rule 6": Rule6, "rule 7": Rule7} {
		if got, err := rule(context.Background(), f); err == nil || !strings.HasPrefix(err.Error(), want) || got != nil {
			t.Errorf("%s of a missing head = %+v, %v; want an error starting %q", want, got, err, want)
		}
	}
	if got, err := BaseOf(context.Background(), f); err == nil || errors.Is(err, ErrMergeBases) || got != "" {
		t.Errorf("BaseOf a missing head = %q, %v; want a read error", got, err)
	}
}

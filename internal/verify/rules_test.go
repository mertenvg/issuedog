package verify

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/repo"
)

// rulesGit runs git in dir, with no system or global config and a fixed
// identity, and returns its trimmed stdout.
func rulesGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = exit.Stderr
		}
		t.Fatalf("git %v: %v: %s", args, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// rulesRepo is a work tree whose every commit is pushed to a bare repo,
// read through repo.Repo as the daemon's bare clone.
type rulesRepo struct {
	t    *testing.T
	work string
	bare *repo.Repo
}

func newRulesRepo(t *testing.T) *rulesRepo {
	t.Helper()
	dir := t.TempDir()
	rr := &rulesRepo{t: t, work: filepath.Join(dir, "work"), bare: repo.New("", filepath.Join(dir, "r.git"))}
	rulesGit(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", rr.bare.Dir())
	rulesGit(t, dir, "init", "--quiet", "--initial-branch=main", rr.work)
	return rr
}

// commit writes files (path to content), removes the paths in remove,
// commits everything on what's checked out, pushes the commit to the bare
// repo and returns its SHA.
func (rr *rulesRepo) commit(files map[string]string, remove ...string) string {
	rr.t.Helper()
	for path, content := range files {
		full := filepath.Join(rr.work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			rr.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			rr.t.Fatal(err)
		}
	}
	for _, path := range remove {
		if err := os.Remove(filepath.Join(rr.work, path)); err != nil {
			rr.t.Fatal(err)
		}
	}
	rulesGit(rr.t, rr.work, "add", "-A")
	rulesGit(rr.t, rr.work, "commit", "--quiet", "--allow-empty", "-m", "change")
	sha := rulesGit(rr.t, rr.work, "rev-parse", "HEAD")
	rulesGit(rr.t, rr.work, "push", "--quiet", rr.bare.Dir(), "HEAD:refs/heads/c-"+sha)
	return sha
}

// checkout moves the work tree to sha, detached, for the next commit.
func (rr *rulesRepo) checkout(sha string) {
	rr.t.Helper()
	rulesGit(rr.t, rr.work, "checkout", "--quiet", "--detach", sha)
}

// rulesConfig is verify's config with SPEC §13's example globs.
func rulesConfig() config.Verify {
	v := config.Defaults().Verify
	v.TestGlobs = []string{"**/*_test.go", "**/*.test.ts", "e2e/**"}
	v.SourceGlobs = []string{"**/*.go", "**/*.ts"}
	v.ConfigPaths = []string{".golangci.yml", ".golangci.yaml"}
	v.ToolingScope = "tooling"
	return v
}

// rulesBase is the main commit the tests' tasks start from, with a file in
// each kind of place the rules look at.
var rulesBase = map[string]string{
	"README.md":                "hello\n",
	"main.go":                  "package main\n",
	"main_test.go":             "package main\n",
	"web/app.ts":               "export {}\n",
	"web/app.test.ts":          "it('works', () => {})\n",
	".golangci.yml":            "linters: {}\n",
	".github/CODEOWNERS":       "/.github/CODEOWNERS @owner\n",
	".github/workflows/ci.yml": "on: push\n",
	"docs/guide.md":            "guide\n",
	"CLAUDE.md":                "rules\n",
}

// The skip markers of SPEC §13's example, which this repo's own
// verify.skip_markers holds too. Each is joined from two strings, so that
// no line here holds one, and rule 3 doesn't fail a task that adds these
// lines (TestNoSkipMarkersInTests).
const (
	markSkip   = "t." + "Skip("
	markDot    = "." + "skip("
	markXit    = "x" + "it("
	markIgnore = "#[" + "ignore]"
)

// check runs rule on f, and fails the test on an error.
func check(t *testing.T, rule func(context.Context, Facts) (*Finding, error), f Facts) *Finding {
	t.Helper()
	got, err := rule(context.Background(), f)
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	return got
}

// wantFinding checks that got is rule n's finding, with every one of
// parts in its detail.
func wantFinding(t *testing.T, got *Finding, n int, parts ...string) {
	t.Helper()
	if got == nil {
		t.Fatalf("finding = nil, want rule %d's", n)
	}
	if got.Rule != n || got.Name == "" || got.Output != "" {
		t.Errorf("finding = %+v, want rule %d with a name and no output", got, n)
	}
	for _, p := range parts {
		if !strings.Contains(got.Detail, p) {
			t.Errorf("rule %d's detail =\n%s\nwant it to contain %q", n, got.Detail, p)
		}
	}
}

// Rule 2 fails a head whose own diff touches a protected path, a rename
// into or out of one included, or whose protected paths differ from the
// baseline, naming each path. A head that changes none passes.
func TestRule2(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	v := rulesConfig()
	v.ProtectedPaths = []string{"secrets/**"}
	facts := func(head string) Facts {
		return Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: v}
	}

	clean := rr.commit(map[string]string{"main.go": "package main\n\nfunc f() {}\n", "main_test.go": "package main\n\n// test\n"})
	if got := check(t, Rule2, facts(clean)); got != nil {
		t.Errorf("Rule2 of a head with no protected path = %+v, want nil", got)
	}

	for name, tc := range map[string]struct {
		files  map[string]string
		remove []string
		want   []string
		not    []string
	}{
		"workflow":          {files: map[string]string{".github/workflows/ci.yml": "on: pull_request\n"}, want: []string{"- .github/workflows/ci.yml"}},
		"root CODEOWNERS":   {files: map[string]string{"CODEOWNERS": "* @x\n"}, want: []string{"- CODEOWNERS"}},
		"nested CLAUDE.md":  {files: map[string]string{"web/CLAUDE.md": "x\n"}, want: []string{"- web/CLAUDE.md"}},
		"root CLAUDE.md":    {files: map[string]string{"CLAUDE.md": "other rules\n"}, want: []string{"- CLAUDE.md"}},
		"agent config":      {files: map[string]string{".mcp.json": "{}\n", ".claude/settings.json": "{}\n"}, want: []string{"- .claude/settings.json", "- .mcp.json"}},
		"issuedog config":   {files: map[string]string{".issuedog/config.yaml": "types: labels\n"}, want: []string{"- .issuedog/config.yaml"}},
		"protected_paths":   {files: map[string]string{"secrets/a/key.pem": "k\n"}, want: []string{"- secrets/a/key.pem"}},
		"deletion":          {remove: []string{".github/CODEOWNERS"}, want: []string{"- .github/CODEOWNERS"}},
		"rename out of":     {files: map[string]string{"ci/ci.yml": "on: push\n"}, remove: []string{".github/workflows/ci.yml"}, want: []string{"- .github/workflows/ci.yml"}, not: []string{"ci/ci.yml"}},
		"rename into":       {files: map[string]string{".claude/guide.md": "guide\n"}, remove: []string{"docs/guide.md"}, want: []string{"- .claude/guide.md"}, not: []string{"docs/guide.md"}},
		"with a clean file": {files: map[string]string{"main.go": "package main\n\n// x\n", ".github/x.yml": "x\n"}, want: []string{"- .github/x.yml"}, not: []string{"main.go"}},
	} {
		rr.checkout(base)
		head := rr.commit(tc.files, tc.remove...)
		got := check(t, Rule2, facts(head))
		wantFinding(t, got, 2, append([]string{"The task's diff touches protected paths"}, tc.want...)...)
		if got == nil {
			continue
		}
		if strings.Contains(got.Detail, "differ from the baseline") {
			t.Errorf("%s: detail =\n%s\nwant no baseline part, as the task's diff names them all", name, got.Detail)
		}
		for _, n := range tc.not {
			if strings.Contains(got.Detail, n) {
				t.Errorf("%s: detail =\n%s\nwant no %q, which isn't protected", name, got.Detail, n)
			}
		}
	}
}

// Rule 2 also fails a head behind a protected path main changed since the
// task's merge base, though its own diff doesn't touch it. A head whose
// base main moved past only in other files passes.
func TestRule2Baseline(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	head := rr.commit(map[string]string{"main.go": "package main\n\n// task\n", "main_test.go": "package main\n\n// task\n"})
	rr.checkout(base)
	moved := rr.commit(map[string]string{"CLAUDE.md": "newer rules\n", "docs/guide.md": "newer guide\n"})
	rr.checkout(base)
	other := rr.commit(map[string]string{"docs/guide.md": "newer guide\n"})
	v := rulesConfig()

	got := check(t, Rule2, Facts{Repo: rr.bare, Baseline: moved, Base: base, Head: head, Config: v})
	wantFinding(t, got, 2, "Protected paths differ from the baseline "+moved, "- CLAUDE.md")
	if got != nil && (strings.Contains(got.Detail, "task's diff touches") || strings.Contains(got.Detail, "guide.md")) {
		t.Errorf("detail =\n%s\nwant only CLAUDE.md, against the baseline", got.Detail)
	}

	if got := check(t, Rule2, Facts{Repo: rr.bare, Baseline: other, Base: base, Head: head, Config: v}); got != nil {
		t.Errorf("Rule2 against a baseline that moved only outside protected paths = %+v, want nil", got)
	}
}

// Rule 3 fails a diff that deletes a test file, or renames one out of the
// test globs, and one that adds a skip marker to a test file, naming the
// path and line. Renaming a test to another test, adding markers outside
// tests, and adding a letter-led marker's text inside a word all pass.
func TestRule3(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	v := rulesConfig()
	facts := func(head string) Facts {
		return Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: v}
	}

	for name, tc := range map[string]struct {
		files  map[string]string
		remove []string
		want   []string // nil: the rule passes
	}{
		"deleted test":   {remove: []string{"main_test.go"}, want: []string{"The diff deletes test files", "- main_test.go"}},
		"added e2e file": {files: map[string]string{"e2e/login.ts": "x\n"}, want: nil},
		"renamed out of the test globs": {files: map[string]string{"main_test.go.bak": "package main\n"}, remove: []string{"main_test.go"},
			want: []string{"- main_test.go (renamed to main_test.go.bak)"}},
		"renamed to another test": {files: map[string]string{"cmd/main_test.go": "package main\n"}, remove: []string{"main_test.go"}},
		"skip marker": {files: map[string]string{"main_test.go": "package main\n\nfunc TestX(t *testing.T) {\n\t" + markSkip + "\"later\")\n}\n"},
			want: []string{"The diff adds skip markers", `- main_test.go:4 adds "` + markSkip + `": ` + markSkip + `"later")`}},
		"describe.skip": {files: map[string]string{"web/app.test.ts": "it('works', () => {})\ndescribe" + markDot + "'x', () => {})\n"},
			want: []string{`- web/app.test.ts:2 adds "` + markDot + `": describe` + markDot + `'x', () => {})`}},
		"xit at a word start": {files: map[string]string{"web/app.test.ts": "it('works', () => {})\n" + markXit + "\"works\")\n"},
			want: []string{`- web/app.test.ts:2 adds "` + markXit + `": ` + markXit + `"works")`}},
		"new test with Rust's ignore attribute": {files: map[string]string{"e2e/a.rs": markIgnore + "\nfn a() {}\n"},
			want: []string{`- e2e/a.rs:1 adds "` + markIgnore + `": ` + markIgnore}},
		"os.Exit is no xit":             {files: map[string]string{"main_test.go": "package main\n\nfunc init() { os.Exit(0) }\n"}},
		"marker outside tests":          {files: map[string]string{"main.go": "package main\n\n// " + markSkip + " " + markXit + "\n"}},
		"plain test lines":              {files: map[string]string{"main_test.go": "package main\n\nfunc TestY(t *testing.T) {}\n"}},
		"marker only on a removed line": {files: map[string]string{"web/app.test.ts": "it('works', () => { done() })\n"}},
	} {
		rr.checkout(base)
		if name == "marker only on a removed line" {
			// Its base holds a marker, which the head removes.
			rr.commit(map[string]string{"web/app.test.ts": markXit + "'works', () => {})\n"})
		}
		start := rulesGit(t, rr.work, "rev-parse", "HEAD")
		head := rr.commit(tc.files, tc.remove...)
		f := facts(head)
		f.Base = start
		got := check(t, Rule3, f)
		if tc.want == nil {
			if got != nil {
				t.Errorf("%s: Rule3 = %+v, want nil", name, got)
			}
			continue
		}
		wantFinding(t, got, 3, tc.want...)
	}
}

// Rule 3 names a path git quotes, shown on one line, and cuts its list
// past listMax entries.
func TestRule3ManyMarkers(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	files := map[string]string{"e2e/odd\nname.ts": markXit + "'a')\n"}
	for i := range 24 {
		files[fmt.Sprintf("e2e/t%02d.ts", i)] = markXit + "'a')\n"
	}
	head := rr.commit(files)
	got := check(t, Rule3, Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: rulesConfig()})
	wantFinding(t, got, 3, `- "e2e/odd\nname.ts":1 adds "`+markXit+`"`, "- e2e/t00.ts:1", "- e2e/t18.ts:1", "…and 5 more")
	if got != nil && strings.Contains(got.Detail, "e2e/t19.ts") {
		t.Errorf("detail =\n%s\nwant the list cut after %d entries", got.Detail, listMax)
	}
}

// Rule 3 names a marker added to a test file the diff renames, at its new
// path and line, and the rename is no deletion.
func TestRule3RenamedTestGainsMarker(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	files := maps.Clone(rulesBase)
	files["pkg/long_test.go"] = "package pkg\n\n" + sizeLines(20)
	base := rr.commit(files)
	head := rr.commit(map[string]string{"cmd/long_test.go": "package pkg\n\n" + sizeLines(20) + markSkip + "\"later\")\n"}, "pkg/long_test.go")
	got := check(t, Rule3, Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: rulesConfig()})
	wantFinding(t, got, 3, "The diff adds skip markers", `- cmd/long_test.go:23 adds "`+markSkip+`": `+markSkip+`"later")`)
	if got != nil && strings.Contains(got.Detail, "deletes") {
		t.Errorf("detail =\n%s\nwant no deletion, as the test was renamed to another test", got.Detail)
	}
}

// Rules 3 to 5 read the task's own diff, from its base, though main moved
// after it: main dropped a marker from a test the task adds a line to,
// added a test, changed a config path, or changed source, and the task's
// head has none of it. Read from the baseline, each would be the head's
// change, and fail the rule.
func TestRules3To5ReadTheTasksOwnDiff(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	files := maps.Clone(rulesBase)
	files["web/app.test.ts"] = markXit + "'works', () => {})\n"
	base := rr.commit(files)
	docs := map[string]string{"docs/guide.md": "task guide\n"}
	for name, tc := range map[string]struct {
		rule        func(context.Context, Facts) (*Finding, error)
		head, moved map[string]string
	}{
		"rule 3, main dropped a marker": {Rule3, map[string]string{"web/app.test.ts": markXit + "'works', () => {})\nit('more', () => {})\n"},
			map[string]string{"web/app.test.ts": "it('works', () => {})\n"}},
		"rule 3, main added a test":   {Rule3, docs, map[string]string{"cmd/new_test.go": "package cmd\n"}},
		"rule 4, main changed config": {Rule4, docs, map[string]string{".golangci.yml": "linters: {enable: [gofmt]}\n"}},
		"rule 5, main changed source": {Rule5, docs, map[string]string{"main.go": "package main\n\n// main moved\n"}},
	} {
		rr.checkout(base)
		head := rr.commit(tc.head)
		rr.checkout(base)
		moved := rr.commit(tc.moved)
		f := Facts{Repo: rr.bare, Baseline: moved, Base: base, Head: head, Target: moved, Scope: "daemon", Config: rulesConfig()}
		if got := check(t, tc.rule, f); got != nil {
			t.Errorf("%s: finding = %+v, want nil, as the task's own diff changes only docs", name, got)
		}
	}
}

// Rule 3 sees a marker added to a test file whatever the attributes say:
// -diff and binary for test files, in the bare repo's info/attributes and
// in a .gitattributes on main, which some versions of git read in a bare
// repo by default. A plain diff-tree in the bare repo with the same setup
// takes both test files as binary, so the setup would catch a read that
// let them.
func TestRule3IgnoresAttributes(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	attrs := "*_test.go -diff\n*.test.ts binary\n"
	files := maps.Clone(rulesBase)
	files[".gitattributes"] = attrs
	base := rr.commit(files)
	rulesGit(t, rr.work, "push", "--quiet", rr.bare.Dir(), base+":refs/heads/main")
	if err := os.WriteFile(filepath.Join(rr.bare.Dir(), "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	head := rr.commit(map[string]string{
		"main_test.go":    "package main\n\nfunc TestX(t *testing.T) {\n\t" + markSkip + "\"later\")\n}\n",
		"web/app.test.ts": "it('works', () => {})\n" + markXit + "'a')\n",
	})
	plain := rulesGit(t, rr.bare.Dir(), "--git-dir=.", "diff-tree", "-p", base, head)
	if !strings.Contains(plain, "Binary files a/main_test.go") || !strings.Contains(plain, "Binary files a/web/app.test.ts") {
		t.Fatalf("plain diff-tree =\n%s\nwant both test files binary, or the setup proves nothing", plain)
	}

	got := check(t, Rule3, Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: rulesConfig()})
	wantFinding(t, got, 3, `- main_test.go:4 adds "`+markSkip+`"`, `- web/app.test.ts:2 adds "`+markXit+`"`)
}

// Rule 7 finds a conflict whatever the merge attributes say: merge=union
// in the bare repo's info/attributes doesn't merge it. A plain merge-tree
// in the bare repo with the same setup merges cleanly (rulesGit fails the
// test on a conflict), so the setup would catch a merge that let it.
func TestRule7IgnoresAttributes(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	target := rr.commit(map[string]string{"docs/guide.md": "target guide\n"})
	rr.checkout(base)
	head := rr.commit(map[string]string{"docs/guide.md": "head guide\n"})
	if err := os.WriteFile(filepath.Join(rr.bare.Dir(), "info", "attributes"), []byte("* merge=union\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rulesGit(t, rr.bare.Dir(), "--git-dir=.", "merge-tree", "--write-tree", target, head)

	got := check(t, Rule7, Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Target: target, Config: rulesConfig()})
	wantFinding(t, got, 7, "doesn't merge cleanly", "- docs/guide.md")
}

// No test file in this repo holds a marker of its own verify.skip_markers
// where rule 3 matches it (markerAt), so rule 3 fails no task for this
// repo's test data. The markers and test globs are read from the repo's
// config, so this test holds no marker either. Dirs whose names start with
// "." are skipped, as Go skips them: in a main checkout, .claude holds
// other worktrees.
func TestNoSkipMarkersInTests(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(config.Path)))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Verify
	if len(v.SkipMarkers) == 0 || len(v.TestGlobs) == 0 {
		t.Fatalf("verify.skip_markers = %q and verify.test_globs = %q, want both set, or this test checks nothing", v.SkipMarkers, v.TestGlobs)
	}
	var found []string
	self := false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !matchAny(v.TestGlobs, rel) {
			return nil
		}
		self = self || rel == "internal/verify/rules_test.go"
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(content), "\n") {
			for _, m := range v.SkipMarkers {
				if markerAt(line, m) {
					found = append(found, fmt.Sprintf("%s:%d holds %q", rel, i+1, m))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if !self {
		t.Errorf("the walk of %s didn't check internal/verify/rules_test.go, want every test file", root)
	}
	if len(found) > 0 {
		t.Errorf("test files hold skip markers literally; join each from two strings:\n%s", strings.Join(found, "\n"))
	}
}

// markerAt matches SPEC §10.1's cases: a letter-led marker only at a word
// start, any other anywhere.
func TestMarkerAt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line, marker string
		want         bool
	}{
		{markXit + `"works", func() {})`, markXit, true},
		{`  ` + markXit + `"works")`, markXit, true},
		{`os.Exit(1)`, markXit, false},
		{`exit(1)`, markXit, false},
		{`_xit(1)`, markXit, false},
		{`2xit(1)`, markXit, false},
		{`éxit(1)`, markXit, false},
		{`os.Exit(1); ` + markXit + `"a")`, markXit, true},
		{`(` + markXit + `"a"))`, markXit, true},
		{`describe` + markDot + `"x")`, markDot, true},
		{`it` + markDot + `"x")`, markDot, true},
		{markSkip + `"later")`, markSkip, true},
		{`a` + markSkip + `"later")`, markSkip, false},
		{markIgnore, markIgnore, true},
		{`a` + markIgnore, markIgnore, true},
		{`plain line`, "", false},
		{`t.Skipf("x")`, markSkip, false},
	} {
		if got := markerAt(tc.line, tc.marker); got != tc.want {
			t.Errorf("markerAt(%q, %q) = %v, want %v", tc.line, tc.marker, got, tc.want)
		}
	}
}

// Rule 4 fails a diff that changes a config path, unless the task's scope
// is the tooling scope; with none set, every task fails it. A diff that
// changes no config path passes.
func TestRule4(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	changed := rr.commit(map[string]string{".golangci.yml": "linters: {enable: [gofmt]}\n"})
	rr.checkout(base)
	renamed := rr.commit(map[string]string{".golangci.yaml": "linters: {}\n"}, ".golangci.yml")
	rr.checkout(base)
	other := rr.commit(map[string]string{"main.go": "package main\n\n// x\n", "docs/guide.md": "x\n"})
	f := Facts{Repo: rr.bare, Baseline: base, Base: base, Config: rulesConfig()}

	f.Head, f.Scope = changed, "daemon"
	wantFinding(t, check(t, Rule4, f), 4, `only a task in scope "tooling"`, "- .golangci.yml")
	f.Head = renamed
	wantFinding(t, check(t, Rule4, f), 4, "- .golangci.yml", "- .golangci.yaml")
	f.Head, f.Scope = changed, ""
	wantFinding(t, check(t, Rule4, f), 4, "- .golangci.yml")

	f.Scope = "tooling"
	if got := check(t, Rule4, f); got != nil {
		t.Errorf("Rule4 in the tooling scope = %+v, want nil", got)
	}
	f.Head, f.Scope = other, "daemon"
	if got := check(t, Rule4, f); got != nil {
		t.Errorf("Rule4 of a diff with no config path = %+v, want nil", got)
	}

	f.Head, f.Scope = changed, ""
	f.Config.ToolingScope = ""
	wantFinding(t, check(t, Rule4, f), 4, "no task may, as verify.tooling_scope is unset", "- .golangci.yml")
}

// Rule 5 fails a diff that adds, changes or renames source files and no
// test file, naming the source files. One with a test, one of docs only,
// and one that only deletes source pass.
func TestRule5(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	v := rulesConfig()
	for name, tc := range map[string]struct {
		files  map[string]string
		remove []string
		want   []string // nil: the rule passes
	}{
		"changed source":     {files: map[string]string{"main.go": "package main\n\n// x\n"}, want: []string{"- main.go"}},
		"added source":       {files: map[string]string{"web/new.ts": "export {}\n", "docs/new.md": "x\n"}, want: []string{"- web/new.ts"}},
		"renamed source":     {files: map[string]string{"cmd/main.go": "package main\n"}, remove: []string{"main.go"}, want: []string{"- cmd/main.go"}},
		"source and test":    {files: map[string]string{"main.go": "package main\n\n// x\n", "main_test.go": "package main\n\n// x\n"}},
		"source and e2e":     {files: map[string]string{"web/app.ts": "export const a = 1\n", "e2e/flow.ts": "x\n"}},
		"source and ts test": {files: map[string]string{"web/app.ts": "export const a = 1\n", "web/app.test.ts": "it('a', () => {})\n"}},
		"docs only":          {files: map[string]string{"README.md": "more\n", "docs/guide.md": "more\n"}},
		"deleted source":     {remove: []string{"web/app.ts"}},
		"test only":          {files: map[string]string{"main_test.go": "package main\n\n// x\n"}},
	} {
		rr.checkout(base)
		head := rr.commit(tc.files, tc.remove...)
		got := check(t, Rule5, Facts{Repo: rr.bare, Baseline: base, Base: base, Head: head, Config: v})
		if tc.want == nil {
			if got != nil {
				t.Errorf("%s: Rule5 = %+v, want nil", name, got)
			}
			continue
		}
		wantFinding(t, got, 5, append([]string{"adds or changes no test file"}, tc.want...)...)
		if got != nil && strings.Contains(got.Detail, "docs/") {
			t.Errorf("%s: detail =\n%s\nwant no docs, which aren't source", name, got.Detail)
		}
	}
}

// A rule whose git read fails returns the error, saying which rule, and
// no finding.
func TestRulesReadError(t *testing.T) {
	t.Parallel()
	rr := newRulesRepo(t)
	base := rr.commit(rulesBase)
	missing := strings.Repeat("0", len(base))
	for n, rule := range []func(context.Context, Facts) (*Finding, error){Rule2, Rule3, Rule4, Rule5} {
		got, err := rule(context.Background(), Facts{Repo: rr.bare, Baseline: base, Base: base, Head: missing, Config: rulesConfig()})
		if want := fmt.Sprintf("rule %d", n+2); err == nil || !strings.HasPrefix(err.Error(), want) || got != nil {
			t.Errorf("rule %d of a missing head = %+v, %v; want an error starting %q", n+2, got, err, want)
		}
	}
}

// Protected is SPEC §6.3's list, CODEOWNERS's places root first, then
// verify.protected_paths.
func TestProtected(t *testing.T) {
	t.Parallel()
	v := config.Defaults().Verify
	v.ProtectedPaths = []string{"secrets/**"}
	want := []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS", ".github/**", ".issuedog/**",
		".claude/**", "**/CLAUDE.md", "**/CLAUDE.local.md", ".mcp.json", "secrets/**"}
	if got := Protected(v); !slices.Equal(got, want) {
		t.Errorf("Protected = %q, want %q", got, want)
	}
	if got := Protected(config.Defaults().Verify); !slices.Equal(got, want[:len(want)-1]) {
		t.Errorf("Protected with no protected_paths = %q, want %q", got, want[:len(want)-1])
	}
}

// lines cuts a list past listMax entries, and show quotes only what isn't
// printable.
func TestLinesAndShow(t *testing.T) {
	t.Parallel()
	var items []string
	for i := range listMax + 3 {
		items = append(items, fmt.Sprint(i))
	}
	got := lines(items)
	if !strings.HasPrefix(got, "- 0\n- 1\n") || !strings.HasSuffix(got, "- 19\n…and 3 more") || strings.Contains(got, "- 20") {
		t.Errorf("lines of %d = %q, want %d then …and 3 more", len(items), got, listMax)
	}
	if got := lines(items[:listMax]); strings.Contains(got, "more") || !strings.HasSuffix(got, "- 19") {
		t.Errorf("lines of %d = %q, want them all", listMax, got)
	}
	for in, want := range map[string]string{"a b/c.go": "a b/c.go", "a\nb": `"a\nb"`, "tab\there": `"tab\there"`, "café": "café"} {
		if got := show(in); got != want {
			t.Errorf("show(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cut(strings.Repeat("é", lineMax+1)); got != strings.Repeat("é", lineMax)+"…" {
		t.Errorf("cut of %d runes = %q, want %d and …", lineMax+1, got, lineMax)
	}
}

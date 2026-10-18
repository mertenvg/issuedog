package daemon

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// A plan item's check of each kind converts to the Check with its one
// value, and a file check's contains, and round-trips through the origin
// marker: CheckFields, FormatMarker, then state.Derive reads it back as
// Origin.Check exactly, with spaces, "%", "<" and ">" in its values.
// Plain text is the zero Check, and adds nothing to the marker.
func TestCheckFromPlan(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		d    planDoneWhen
		want state.Check
	}{
		{"plain text", planDoneWhen{Text: "The owner says so."}, state.Check{}},
		{"label", planDoneWhen{Check: &planCheck{Kind: planCheckLabel, Label: "needs <design> 100%"}},
			state.Check{Kind: state.CheckLabel, Value: "needs <design> 100%"}},
		{"issue-type", planDoneWhen{Check: &planCheck{Kind: planCheckIssueType, IssueType: "Human task"}},
			state.Check{Kind: state.CheckIssueType, Value: "Human task"}},
		{"file", planDoneWhen{Check: &planCheck{Kind: planCheckFile, Path: "docs/setup guide.md"}},
			state.Check{Kind: state.CheckFile, Value: "docs/setup guide.md"}},
		{"file with contains", planDoneWhen{Check: &planCheck{Kind: planCheckFile, Path: ".github/CODEOWNERS", Contains: "* @o/<team> 50%"}},
			state.Check{Kind: state.CheckFile, Value: ".github/CODEOWNERS", Contains: "* @o/<team> 50%"}},
		{"ruleset", planDoneWhen{Check: &planCheck{Kind: planCheckRuleset, Check: "ci / test (1.25, %)"}},
			state.Check{Kind: state.CheckRuleset, Value: "ci / test (1.25, %)"}},
		{"installation", planDoneWhen{Check: &planCheck{Kind: planCheckInstallation, Owner: "Acme-Corp"}},
			state.Check{Kind: state.CheckInstallation, Value: "Acme-Corp"}},
	} {
		got, err := checkFromPlan(tt.d)
		if err != nil || got != tt.want {
			t.Errorf("%s: checkFromPlan = %+v, %v; want %+v", tt.name, got, err, tt.want)
			continue
		}
		fields := map[string]string{"plan": "#250", "rev": "1", "item": "T3", "human": "1"}
		maps.Copy(fields, state.CheckFields(got))
		marker, err := github.FormatMarker(github.Marker{Kind: "origin", Fields: fields})
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		s := &github.Snapshot{Number: 7, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Body: "The task.\n\n" + marker}
		if back := state.Derive(s, "issuedog", func(int64) bool { return false }).Origin.Check; back != tt.want {
			t.Errorf("%s: %s reads back as %+v, want %+v", tt.name, marker, back, tt.want)
		}
	}
}

// checkFromPlan reuses S1's validation (validCheck): a check it refuses is
// an error, which names what's wrong, and no Check.
func TestCheckFromPlanInvalid(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		c    planCheck
		want string
	}{
		{planCheck{Kind: "app-installed", Owner: "o"}, "unknown kind"},
		{planCheck{Kind: planCheckLabel, Label: strings.Repeat("x", 51)}, "over 50"},
		{planCheck{Kind: planCheckLabel}, "isn't one line"},
		{planCheck{Kind: planCheckFile, Path: "../secret"}, `segment ".."`},
		{planCheck{Kind: planCheckFile, Path: "/etc/passwd"}, "starts with /"},
		{planCheck{Kind: planCheckFile, Path: "go.mod", Contains: "a\nb"}, "isn't one line"},
		{planCheck{Kind: planCheckRuleset, Check: " "}, "isn't one line"},
		{planCheck{Kind: planCheckInstallation, Owner: "-o"}, "login"},
		{planCheck{Kind: planCheckLabel, Label: "x", Path: "go.mod"}, "path isn't a field of kind label"},
	} {
		got, err := checkFromPlan(planDoneWhen{Text: "t", Check: &tt.c})
		if err == nil || !strings.Contains(err.Error(), tt.want) || got != (state.Check{}) {
			t.Errorf("checkFromPlan(%+v) = %+v, %v; want an error with %q", tt.c, got, err, tt.want)
		}
	}
}

// humanCheckApplier is an Applier of o/r on f, which reads main from
// clone if it's set.
func humanCheckApplier(t *testing.T, f *githubtest.Fake, clone *repo.Repo) *Applier {
	t.Helper()
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	if clone != nil {
		a.ReadMain = clone.ReadFile
	}
	return a
}

// Each GitHub kind passes and fails on the fake, and found says what it
// saw, every value as inline code. A plain-text "Done when" passes with
// nothing to run.
func TestHumanCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	check := func(kind, value string) state.Check { return state.Check{Kind: kind, Value: value} }
	for _, tt := range []struct {
		name  string
		setup func(f *githubtest.Fake)
		c     state.Check
		pass  bool
		found string
	}{
		{"plain text", nil, state.Check{}, true, `The "Done when" has no check for me to run.`},
		{"a label", func(f *githubtest.Fake) { f.AddRepoLabel("Needs Design") }, check(state.CheckLabel, "needs design"),
			true, "The label `needs design` exists."},
		{"a missing label", func(f *githubtest.Fake) { f.AddRepoLabel("Needs Design") }, check(state.CheckLabel, "needs"),
			false, "This repo has no label `needs`."},
		{"an issue type", func(f *githubtest.Fake) {
			f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}, githubtest.OrgIssueType{Name: "Human Task"})
		}, check(state.CheckIssueType, "human task"), true, "`o` has the issue type `human task`, enabled."},
		{"a disabled issue type", func(f *githubtest.Fake) {
			f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}, githubtest.OrgIssueType{Name: "Human Task", Disabled: true})
		}, check(state.CheckIssueType, "Human Task"), false, "`o` has the issue type `Human Task`, but it's disabled."},
		{"a missing issue type", func(f *githubtest.Fake) { f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}) },
			check(state.CheckIssueType, "Human"), false, "`o` has no issue type `Human`."},
		{"an issue type on a personal account", func(f *githubtest.Fake) {
			f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"})
			f.SetPersonalAccount()
		}, check(state.CheckIssueType, "Bug"), false, "`o` is a personal account, which has no issue types."},
		{"a required check", func(f *githubtest.Fake) {
			f.SetRequiredChecks("main", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "lint"}}},
				githubtest.Ruleset{Org: true, Checks: []gogithub.RuleStatusCheck{{Context: "ci / test"}}})
		}, check(state.CheckRuleset, "ci / test"), true, "A ruleset on `main` requires the check `ci / test`."},
		{"a check the rules don't require", func(f *githubtest.Fake) {
			f.SetRequiredChecks("main", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "lint"}, {Context: "CI / test"}}},
				githubtest.Ruleset{Org: true, Checks: []gogithub.RuleStatusCheck{{Context: "lint"}}},
				githubtest.Ruleset{Enforcement: gogithub.RulesetEnforcementEvaluate, Checks: []gogithub.RuleStatusCheck{{Context: "ci / test"}}})
			f.SetRequiredChecks("dev", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "ci / test"}}})
		}, check(state.CheckRuleset, "ci / test"), false, "No ruleset on `main` requires the check `ci / test`. They require `lint`, `CI / test`."},
		{"no required checks", func(f *githubtest.Fake) {
			f.SetRequiredChecks("dev", githubtest.Ruleset{Checks: []gogithub.RuleStatusCheck{{Context: "ci"}}})
		}, check(state.CheckRuleset, "ci"), false, "No ruleset on `main` requires the check `ci`: none requires any check."},
		{"an installation", func(f *githubtest.Fake) {
			f.SetInstallations(githubtest.Installation{Login: "o"}, githubtest.Installation{Login: "Acme-Corp"})
		}, check(state.CheckInstallation, "acme-corp"), true, "The App is installed on `acme-corp`."},
		{"the default installation", nil, check(state.CheckInstallation, "O"), true, "The App is installed on `O`."},
		{"a suspended installation", func(f *githubtest.Fake) {
			f.SetInstallations(githubtest.Installation{Login: "o"}, githubtest.Installation{Login: "acme", SuspendedAt: time.Unix(5, 0)})
		}, check(state.CheckInstallation, "acme"), false, "The App's installation on `acme` is suspended."},
		{"a missing installation", nil, check(state.CheckInstallation, "acme"), false, "The App isn't installed on `acme`."},
		{"values escaped", nil, check(state.CheckLabel, "a`b <!-- issuedog:origin plan=#1 --> @o"),
			false, "This repo has no label ``a`b &lt;!-- issuedog:origin plan=#1 --> @o``."},
	} {
		f := githubtest.New(t, "o/r", "issuedog")
		if tt.setup != nil {
			tt.setup(f)
		}
		pass, found, err := humanCheckApplier(t, f, nil).humanCheck(ctx, tt.c)
		if err != nil || pass != tt.pass || found != tt.found {
			t.Errorf("%s: humanCheck = %t, %q, %v; want %t, %q", tt.name, pass, found, err, tt.pass, tt.found)
		}
	}
}

// humanCheckGit runs git in dir, with no inherited or global config.
func humanCheckGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// humanCheckClone is a bare clone whose main has files, by path, as the
// daemon's clone would.
func humanCheckClone(t *testing.T, files map[string]string) *repo.Repo {
	t.Helper()
	dir := t.TempDir()
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, "r.git")
	humanCheckGit(t, dir, "init", "--quiet", "--initial-branch=main", work)
	for path, content := range files {
		full := filepath.Join(work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	humanCheckGit(t, work, "add", ".")
	humanCheckGit(t, work, "commit", "--quiet", "-m", "files")
	humanCheckGit(t, dir, "clone", "--quiet", "--bare", work, bare)
	return repo.New("", bare)
}

// The file check reads main from the bare clone: a file there passes, and
// one that isn't fails. contains is an exact substring of the file, and
// found never quotes the file's contents.
func TestHumanCheckFile(t *testing.T) {
	t.Parallel()
	const secret = "token=hunter2"
	clone := humanCheckClone(t, map[string]string{
		"docs/setup guide.md":  "# Setup\n\nRun `make` <here>.\n" + secret + "\n",
		".github/CODEOWNERS":   "* @o/team\n",
		"go.mod":               "module x\n\ngo 1.25\n",
		"docs/nested/deep.txt": "deep",
	})
	ctx := context.Background()
	file := func(path, contains string) state.Check {
		return state.Check{Kind: state.CheckFile, Value: path, Contains: contains}
	}
	for _, tt := range []struct {
		name  string
		c     state.Check
		pass  bool
		found string
	}{
		{"a file", file("docs/setup guide.md", ""), true, "The file `docs/setup guide.md` is on `main`."},
		{"a nested file", file("docs/nested/deep.txt", ""), true, "The file `docs/nested/deep.txt` is on `main`."},
		{"a missing file", file("docs/missing.md", ""), false, "There's no file `docs/missing.md` on `main`."},
		{"a missing directory", file("nope/x.md", ""), false, "There's no file `nope/x.md` on `main`."},
		{"a path under a file", file("go.mod/x", ""), false, "There's no file `go.mod/x` on `main`."},
		{"a file in another case", file("GO.MOD", ""), false, "There's no file `GO.MOD` on `main`."},
		{"a directory", file("docs", ""), false, "`docs` on `main` isn't a file: it may be a directory."},
		{"contains on a directory", file("docs/nested", "deep"), false, "`docs/nested` on `main` isn't a file: it may be a directory."},
		{"contains", file("docs/setup guide.md", "Run `make` <here>."), true,
			"The file `docs/setup guide.md` on `main` contains ``Run `make` <here>.``."},
		{"contains across a line", file("go.mod", "x\n\ngo"), true, "The file `go.mod` on `main` contains `x  go`."},
		{"contains in another case", file("go.mod", "GO 1.25"), false,
			"The file `go.mod` is on `main`, but doesn't contain `GO 1.25`."},
		{"contains that isn't there", file(".github/CODEOWNERS", "@o/other"), false,
			"The file `.github/CODEOWNERS` is on `main`, but doesn't contain `@o/other`."},
		{"contains on a missing file", file("docs/missing.md", "x"), false, "There's no file `docs/missing.md` on `main`."},
	} {
		pass, found, err := humanCheckApplier(t, githubtest.New(t, "o/r", "issuedog"), clone).humanCheck(ctx, tt.c)
		if err != nil || pass != tt.pass || found != tt.found {
			t.Errorf("%s: humanCheck = %t, %q, %v; want %t, %q", tt.name, pass, found, err, tt.pass, tt.found)
		}
		if strings.Contains(found, secret) || strings.Contains(found, "Setup") {
			t.Errorf("%s: found quotes the file: %q", tt.name, found)
		}
	}
}

// A GitHub read that fails, a git read that fails, and a file check with
// no clone to read are errors, wrapped, and never a failed check: pass is
// false, found is empty. An unknown kind is an error too.
func TestHumanCheckReadErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tt := range []struct {
		path string
		c    state.Check
	}{
		{"/labels/x", state.Check{Kind: state.CheckLabel, Value: "x"}},
		{"/orgs/o/issue-types", state.Check{Kind: state.CheckIssueType, Value: "Bug"}},
		{"/rules/branches/main", state.Check{Kind: state.CheckRuleset, Value: "ci"}},
		{"/app/installations", state.Check{Kind: state.CheckInstallation, Value: "o"}},
	} {
		for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
			f := githubtest.New(t, "o/r", "issuedog")
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == tt.path }, status)
			pass, found, err := humanCheckApplier(t, f, nil).humanCheck(ctx, tt.c)
			var er *gogithub.ErrorResponse
			if !errors.As(err, &er) || er.Response.StatusCode != status || !strings.Contains(err.Error(), "run the "+tt.c.Kind+" check") {
				t.Errorf("%s answered %d: err = %v; want a wrapped %d", tt.path, status, err, status)
			}
			if pass || found != "" {
				t.Errorf("%s answered %d: humanCheck = %t, %q; want false and no sentence", tt.path, status, pass, found)
			}
		}
	}
	file := state.Check{Kind: state.CheckFile, Value: "go.mod"}
	broken := repo.New("", filepath.Join(t.TempDir(), "missing.git"))
	gitErr := errors.New("git cat-file: exit status 128")
	for name, a := range map[string]*Applier{
		"a clone that isn't there": {Owner: "o", Repo: "r", ReadMain: broken.ReadFile},
		"a git error":              {Owner: "o", Repo: "r", ReadMain: func(context.Context, string) ([]byte, error) { return nil, gitErr }},
		"no clone":                 {Owner: "o", Repo: "r"},
	} {
		pass, found, err := a.humanCheck(ctx, file)
		if err == nil || errors.Is(err, repo.ErrNotFound) || !strings.Contains(err.Error(), "run the file check") || pass || found != "" {
			t.Errorf("%s: humanCheck = %t, %q, %v; want a file check error that isn't ErrNotFound", name, pass, found, err)
		}
		if name == "a git error" && !errors.Is(err, gitErr) {
			t.Errorf("%s: %v doesn't wrap %v", name, err, gitErr)
		}
	}
	if pass, found, err := (&Applier{}).humanCheck(ctx, state.Check{Kind: "app-installed", Value: "o"}); err == nil || pass || found != "" {
		t.Errorf("an unknown kind: humanCheck = %t, %q, %v; want an error", pass, found, err)
	}
}

// checkCode fences a value as inline code that reads as it is: one more
// backtick than its longest run, padded when it starts or ends with a
// backtick or a space, on one line, with its markers escaped.
func TestCheckCode(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"go.mod":          "`go.mod`",
		"a`b":             "``a`b``",
		"a``b`c":          "```a``b`c```",
		"`a":              "`` `a ``",
		"a`":              "`` a` ``",
		"`":               "`` ` ``",
		" a":              "`  a `",
		"a ":              "` a  `",
		"a\nb\tc":         "`a b c`",
		"<!-- x --> @o":   "`&lt;!-- x --> @o`",
		"":                "` `",
		"\ufeffa":         "`a`",
		"<b>bold</b> *x*": "`<b>bold</b> *x*`",
	} {
		if got := checkCode(in); got != want {
			t.Errorf("checkCode(%q) = %q, want %q", in, got, want)
		}
	}
}

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/repo"
)

func TestTaskBranchPrefix(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{1: "issuedog/1-", 12: "issuedog/12-", 756: "issuedog/756-"} {
		if got := taskBranchPrefix(n); got != want {
			t.Errorf("taskBranchPrefix(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTaskSlug(t *testing.T) {
	t.Parallel()
	tests := []struct{ title, want string }{
		{"The daemon pushes a verified head", "the-daemon-pushes-a-verified-head"},
		{"Fix `Push` (again): 2 bugs!", "fix-push-again-2-bugs"},
		{"  --leading and trailing--  ", "leading-and-trailing"},
		{"feat: snake_case and CamelCase", "feat-snake-case-and-camelcase"},
		{"Café über naïve", "caf-ber-na-ve"},
		{"issuedog/12-x..y", "issuedog-12-x-y"},
		{"0123456789012345678901234567890123456789", "0123456789012345678901234567890123456789"},
		{"0123456789012345678901234567890123456789x", "0123456789012345678901234567890123456789"},
		{"012345678901234567890123456789012345678 more", "012345678901234567890123456789012345678"},
		{"", "task"},
		{"!!! ??? ...", "task"},
		{"日本語", "task"},
	}
	for _, tt := range tests {
		got := taskSlug(tt.title)
		if got != tt.want {
			t.Errorf("taskSlug(%q) = %q, want %q", tt.title, got, tt.want)
		}
		if len(got) > taskSlugMax {
			t.Errorf("taskSlug(%q) = %q, longer than %d", tt.title, got, taskSlugMax)
		}
	}
}

// branchRemote is a bare repo standing in for GitHub, with main and the
// given branches all at one commit, synced into a bare clone as the daemon
// keeps it, which it returns with that commit.
func branchRemote(t *testing.T, branches ...string) (*repo.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	url, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(dir, "init", "--bare", "--quiet", "--initial-branch=main", url)
	git(dir, "init", "--quiet", "--initial-branch=main", work)
	git(work, "commit", "--quiet", "--allow-empty", "-m", "first")
	tip := git(work, "rev-parse", "HEAD")
	git(work, "push", "--quiet", url, "main")
	for _, b := range branches {
		git(work, "push", "--quiet", url, "main:refs/heads/"+b)
	}
	clone := repo.New(url, filepath.Join(dir, "clone.git"))
	if err := clone.Sync(context.Background(), ""); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return clone, tip
}

// TestTaskBranch names and finds task 7's branch from the bare clone's
// branches with its prefix, as the last sync left them: a new name from the
// title when it has none, its one branch whatever its slug, so a retitled
// task keeps it, and no branch when it has two. Another task's branches,
// whose number starts with 7, are never its.
func TestTaskBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		branches []string
		want     string // the branch; "" with errTwoBranches
		found    bool   // the branch is on GitHub, so it has a tip
	}{
		{name: "none", branches: []string{"issuedog/70-other", "issuedog/17-other", "feature/7-push"},
			want: "issuedog/7-push-a-verified-head"},
		{name: "one by this title", branches: []string{"issuedog/7-push-a-verified-head", "issuedog/70-other"},
			want: "issuedog/7-push-a-verified-head", found: true},
		{name: "one by an old title", branches: []string{"issuedog/7-old-title", "issuedog/70-push-a-verified-head"},
			want: "issuedog/7-old-title", found: true},
		{name: "one with a slash in its slug", branches: []string{"issuedog/7-old/title"},
			want: "issuedog/7-old/title", found: true},
		{name: "two", branches: []string{"issuedog/7-push-a-verified-head", "issuedog/7-old-title"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clone, tip := branchRemote(t, tt.branches...)
			branches, err := clone.Branches(context.Background(), taskBranchPrefix(7))
			if err != nil {
				t.Fatalf("Branches: %v", err)
			}
			name, gotTip, err := taskBranch(7, "Push a verified head!", branches)
			if tt.want == "" {
				if !errors.Is(err, errTwoBranches) || name != "" || gotTip != "" {
					t.Fatalf("taskBranch = %q, %q, %v; want an error matching errTwoBranches", name, gotTip, err)
				}
				for _, b := range tt.branches {
					if !strings.Contains(err.Error(), b) {
						t.Errorf("taskBranch's error %v doesn't name %s", err, b)
					}
				}
				return
			}
			wantTip := ""
			if tt.found {
				wantTip = tip
			}
			if err != nil || name != tt.want || gotTip != wantTip {
				t.Errorf("taskBranch = %q, %q, %v; want %q, %q", name, gotTip, err, tt.want, wantTip)
			}
		})
	}
}

// TestTaskBranchFilters finds the task's branch among all the bare clone's
// branches too, not only those Branches listed with its prefix.
func TestTaskBranchFilters(t *testing.T) {
	t.Parallel()
	branches := map[string]string{"main": "m", "issuedog/7-a": "a", "issuedog/70-b": "b", "issuedog/71-c": "c"}
	if name, tip, err := taskBranch(7, "title", branches); err != nil || name != "issuedog/7-a" || tip != "a" {
		t.Errorf("taskBranch = %q, %q, %v; want issuedog/7-a, a", name, tip, err)
	}
	if name, tip, err := taskBranch(8, "A new task", branches); err != nil || name != "issuedog/8-a-new-task" || tip != "" {
		t.Errorf("taskBranch = %q, %q, %v; want issuedog/8-a-new-task, no tip", name, tip, err)
	}
}

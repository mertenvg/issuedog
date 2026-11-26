package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mergeTask is the task number the merge tests build a merge result for.
const mergeTask = 7

// mergeTaskBranch is the task's branch on the remote, holding its head.
const mergeTaskBranch = "issuedog/7-merge-with-main"

// runAt runs git in dir as run does, with the commit's dates set to date,
// git's raw "<seconds> <zone>", and returns its trimmed stdout.
func runAt(t *testing.T, dir, date string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(os.Environ(), "", ""),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mergeRemote is a remote with a base commit on main, then main's tip,
// target, and the task's branch, head, each off base and changing files
// to the given contents. target is committed at targetDate and head at
// headDate, git's raw "<seconds> <zone>", so a merge's date says whose it
// took.
type mergeRemote struct {
	*remote
	base, target, head string
}

const targetDate, headDate = "1700000000 +0200", "1600000000 -0500"

func newMergeRemote(t *testing.T, baseFiles, targetFiles, headFiles map[string]string) *mergeRemote {
	t.Helper()
	rem := newRemote(t)
	m := &mergeRemote{remote: rem}
	commitFiles := func(files map[string]string, date string) string {
		t.Helper()
		for name, content := range files {
			path := filepath.Join(rem.work, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		run(t, rem.work, "add", "-A")
		runAt(t, rem.work, date, "commit", "--quiet", "--allow-empty", "-m", "change")
		return run(t, rem.work, "rev-parse", "HEAD")
	}
	m.base = commitFiles(baseFiles, "1500000000 +0000")
	m.target = commitFiles(targetFiles, targetDate)
	run(t, rem.work, "push", "--quiet", rem.url, "main")
	run(t, rem.work, "checkout", "--quiet", "-b", "task", m.base)
	m.head = commitFiles(headFiles, headDate)
	run(t, rem.work, "push", "--quiet", rem.url, "HEAD:refs/heads/"+mergeTaskBranch)
	run(t, rem.work, "checkout", "--quiet", "main")
	return m
}

// clone syncs a new bare repo from the remote, as the daemon's.
func (m *mergeRemote) clone() *Repo {
	m.t.Helper()
	r := New(m.url, filepath.Join(m.t.TempDir(), "target.git"))
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		m.t.Fatalf("Sync: %v", err)
	}
	return r
}

// cleanMergeRemote is a remote whose head and target change different
// files, so they merge cleanly.
func cleanMergeRemote(t *testing.T) *mergeRemote {
	t.Helper()
	return newMergeRemote(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "a, main\n"},
		map[string]string{"b.txt": "b, head\n", "new.txt": "new\n"})
}

// TestMergeCommit builds a clean merge of the task's head with main: a
// commit whose parents are main and the head, in that order, whose tree
// holds both sides' changes, by issuedog, dated as main's tip was
// committed, with a fixed message that names main and the head as
// commits, at the task's ref under MergesRef. Built again, in the same bare
// repo and in another, it's the same SHA, and a build again moves no
// objects in. The bare repo has every object the commit reaches, and no
// quarantine or attrless dir is left. A sync after it leaves the ref and
// the commit alone.
func TestMergeCommit(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()

	sha, conflicts, err := r.MergeCommit(ctx, mergeTask, m.target, m.head)
	if err != nil || len(conflicts) != 0 || !shaPattern.MatchString(sha) {
		t.Fatalf("MergeCommit = %q, %q, %v; want a merge commit and no conflicts", sha, conflicts, err)
	}
	ref := MergesRef + "7"
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if got := run(t, r.dir, "rev-list", "--parents", "--no-walk", sha); got != sha+" "+m.target+" "+m.head {
		t.Errorf("the merge's parents: %q, want main %s then the head %s", got, m.target, m.head)
	}
	for path, want := range map[string]string{"a.txt": "a, main", "b.txt": "b, head", "new.txt": "new"} {
		if got := run(t, r.dir, "cat-file", "blob", sha+":"+path); got != want {
			t.Errorf("%s in the merge = %q, want %q", path, got, want)
		}
	}
	format := "%an <%ae> %ad%n%cn <%ce> %cd%n%B"
	want := "issuedog <issuedog@localhost> " + targetDate + "\n" +
		"issuedog <issuedog@localhost> " + targetDate + "\n" +
		"issuedog: merge " + m.head + " with " + m.target
	if got := run(t, r.dir, "log", "-1", "--date=raw", "--format="+format, sha); got != want {
		t.Errorf("the merge's identity, dates and message =\n%s\nwant\n%s", got, want)
	}
	if got := run(t, r.dir, "cat-file", "commit", sha); strings.Contains(got, "gpgsig") {
		t.Errorf("the merge is signed:\n%s", got)
	}
	// Every object the merge reaches is in the bare repo itself, past the
	// attrless repo it was built in.
	run(t, r.dir, "rev-list", "--objects", "--missing=error", sha)
	run(t, r.dir, "fsck", "--connectivity-only", "--no-dangling")
	for _, f := range objectFiles(t, r) {
		if strings.HasPrefix(filepath.Base(f), "tmp_objdir") || strings.HasPrefix(f, "tmp_objdir") {
			t.Errorf("the merge left %s in the bare repo's objects dir", f)
		}
	}

	before := objectFiles(t, r)
	again, conflicts, err := r.MergeCommit(ctx, mergeTask, m.target, m.head)
	if err != nil || len(conflicts) != 0 || again != sha {
		t.Errorf("MergeCommit again = %q, %q, %v; want the same %s", again, conflicts, err, sha)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("building the same merge again changed the objects dir: %q, then %q", before, after)
	}

	other := m.clone()
	elsewhere, conflicts, err := other.MergeCommit(ctx, mergeTask, m.target, m.head)
	if err != nil || len(conflicts) != 0 || elsewhere != sha {
		t.Errorf("MergeCommit in another bare repo = %q, %q, %v; want the same %s", elsewhere, conflicts, err, sha)
	}

	m.commit("c.txt", "c\n")
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) after a sync = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if has, err := r.HasCommit(ctx, sha); err != nil || !has {
		t.Errorf("HasCommit(%s) after a sync = %v, %v; want true", sha, has, err)
	}
}

// TestMergeCommitConflict names each conflicting path of a merge that
// doesn't apply cleanly, returns no SHA, writes no ref and leaves the bare
// repo's objects dir as it was, though merge-tree writes the merged
// objects, the conflicted files' included.
func TestMergeCommitConflict(t *testing.T) {
	t.Parallel()
	m := newMergeRemote(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"},
		map[string]string{"a.txt": "a, main\n", "b.txt": "b, main\n"},
		map[string]string{"a.txt": "a, head\n", "b.txt": "b, head\n", "c.txt": "c, head\n"})
	r := m.clone()
	ctx := context.Background()
	refsBefore, before := refs(t, r), objectFiles(t, r)

	sha, conflicts, err := r.MergeCommit(ctx, mergeTask, m.target, m.head)
	slices.Sort(conflicts)
	if err != nil || sha != "" || !slices.Equal(conflicts, []string{"a.txt", "b.txt"}) {
		t.Errorf("MergeCommit = %q, %q, %v; want no SHA and the conflicts [a.txt b.txt]", sha, conflicts, err)
	}
	if got := refs(t, r); got != refsBefore {
		t.Errorf("refs after a conflict =\n%s\nwant\n%s", got, refsBefore)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("a conflict left objects behind: %q, then %q", before, after)
	}
}

// TestMergeCommitIgnoresAttributes merges a file both sides change, with
// merge=union set for it in a .gitattributes on both sides and in the bare
// repo's info/attributes, with attr.tree naming main for a git that has
// it. A plain merge-tree in the bare repo with the same setup merges it
// cleanly, so the setup would catch a merge that let the attribute apply.
// MergeCommit finds the conflict, as rule 7 does, and writes no ref.
func TestMergeCommitIgnoresAttributes(t *testing.T) {
	t.Parallel()
	attrs := "*.txt merge=union\n"
	m := newMergeRemote(t,
		map[string]string{"a.txt": "a\n", ".gitattributes": attrs},
		map[string]string{"a.txt": "a, main\n"},
		map[string]string{"a.txt": "a, head\n"})
	r := m.clone()
	if err := os.WriteFile(filepath.Join(r.dir, "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, r.dir, "--git-dir=.", "config", "attr.tree", "main")
	plain := exec.Command("git", "--git-dir=.", "merge-tree", "--write-tree", m.target, m.head)
	plain.Dir, plain.Env = r.dir, gitEnv(os.Environ(), "", "")
	if out, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("plain merge-tree: %v: %s\nwant merge=union to merge a.txt cleanly, or the setup proves nothing", err, out)
	}
	refsBefore := refs(t, r)

	sha, conflicts, err := r.MergeCommit(context.Background(), mergeTask, m.target, m.head)
	if err != nil || sha != "" || !slices.Equal(conflicts, []string{"a.txt"}) {
		t.Errorf("MergeCommit = %q, %q, %v; want no SHA and the conflict [a.txt], with merge=union ignored", sha, conflicts, err)
	}
	if got := refs(t, r); got != refsBefore {
		t.Errorf("refs after a conflict =\n%s\nwant\n%s", got, refsBefore)
	}
}

// TestMergeCommitRunsNoHooks plants hooks in the bare repo, and a config
// include that points core.hooksPath at more of them, each of which would
// leave a mark. None runs as the merge is committed and its ref written,
// nor as a story's merge of main or a conflicted merge is.
func TestMergeCommitRunsNoHooks(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	marks, planted := t.TempDir(), t.TempDir()
	hook := func(dir, name string) {
		script := "#!/bin/sh\ntouch '" + filepath.Join(marks, filepath.Base(dir)+"-"+name) + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"pre-commit", "post-commit", "reference-transaction", "post-merge"} {
		hook(filepath.Join(r.dir, "hooks"), name)
		hook(planted, name)
	}
	included := filepath.Join(t.TempDir(), "included.config")
	if err := os.WriteFile(included, []byte("[core]\n\thooksPath = "+planted+"\n[issuedog]\n\tprobe = included\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, r.dir, "config", "includeIf.gitdir:"+r.dir+".path", included)
	// Read as MergeCommit runs git in the bare repo, with --git-dir, so the
	// include applies to its commands, and only gitEnv's own core.hooksPath
	// stands in its way.
	if got, err := r.git(context.Background(), "", "config", "issuedog.probe"); err != nil || string(got) != "included\n" {
		t.Fatalf("the include doesn't apply to the bare repo's commands, so the test shows nothing: issuedog.probe = %q, %v", got, err)
	}

	sha, conflicts, err := r.MergeCommit(context.Background(), mergeTask, m.target, m.head)
	if err != nil || len(conflicts) != 0 || sha == "" {
		t.Fatalf("MergeCommit = %q, %q, %v; want a merge commit", sha, conflicts, err)
	}
	if got := run(t, r.dir, "rev-parse", MergesRef+"7"); got != sha {
		t.Errorf("%s7 = %s, want %s", MergesRef, got, sha)
	}
	sha, conflicts, err = r.MainMerge(context.Background(), mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || sha == "" {
		t.Fatalf("MainMerge = %q, %q, %v; want a merge commit", sha, conflicts, err)
	}
	if got := run(t, r.dir, "rev-parse", StoryMergesRef+"5"); got != sha {
		t.Errorf("%s5 = %s, want %s", StoryMergesRef, got, sha)
	}
	sha, conflicts, err = r.ConflictMerge(context.Background(), conflictBug, m.target, m.head)
	if err != nil || len(conflicts) != 0 || sha == "" {
		t.Fatalf("ConflictMerge = %q, %q, %v; want a merge commit", sha, conflicts, err)
	}
	if got := run(t, r.dir, "rev-parse", ConflictsRef+"9"); got != sha {
		t.Errorf("%s9 = %s, want %s", ConflictsRef, got, sha)
	}
	entries, err := os.ReadDir(marks)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the hook %s ran", e.Name())
	}
}

// TestMergeCommitRefuses refuses a task number that isn't one, a target or
// head that isn't a full SHA, one the bare repo lacks, and one that names
// a tree, not a commit. Each writes no ref and leaves the objects dir as
// it was.
func TestMergeCommitRefuses(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()
	missing := strings.Repeat("1", len(m.head))
	tree := run(t, r.dir, "rev-parse", m.head+"^{tree}")
	tests := []struct {
		name         string
		n            int
		target, head string
		want         string // in the error
	}{
		{name: "task zero", n: 0, target: m.target, head: m.head, want: "invalid task number"},
		{name: "task negative", n: -1, target: m.target, head: m.head, want: "invalid task number"},
		{name: "short head", n: mergeTask, target: m.target, head: m.head[:12], want: "invalid SHA"},
		{name: "short target", n: mergeTask, target: m.target[:12], head: m.head, want: "invalid SHA"},
		{name: "uppercase head", n: mergeTask, target: m.target, head: strings.ToUpper(m.head), want: "invalid SHA"},
		{name: "head a ref", n: mergeTask, target: m.target, head: "refs/heads/" + mergeTaskBranch, want: "invalid SHA"},
		{name: "missing head", n: mergeTask, target: m.target, head: missing, want: "no commit " + missing},
		{name: "missing target", n: mergeTask, target: missing, head: m.head, want: "no commit " + missing},
		{name: "head a tree", n: mergeTask, target: m.target, head: tree, want: "no commit " + tree},
	}
	refsBefore, before := refs(t, r), objectFiles(t, r)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sha, conflicts, err := r.MergeCommit(ctx, tt.n, tt.target, tt.head)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("MergeCommit = %q, %q, %v; want an error containing %q", sha, conflicts, err, tt.want)
			}
			if got := refs(t, r); got != refsBefore {
				t.Errorf("refs after a refusal =\n%s\nwant\n%s", got, refsBefore)
			}
			if after := objectFiles(t, r); !slices.Equal(after, before) {
				t.Errorf("a refusal changed the objects dir: %q, then %q", before, after)
			}
		})
	}
}

// TestMergeDate reads a commit's committer date as git wrote it, zone
// included, and refuses a committer line with no date.
func TestMergeDate(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()
	if got, err := mergeDate(ctx, r, m.target); err != nil || got != "@"+targetDate {
		t.Errorf("mergeDate(target) = %q, %v; want @%s", got, err, targetDate)
	}
	body := "tree " + run(t, r.dir, "rev-parse", m.head+"^{tree}") + "\nauthor t <t@t> 1 +0000\ncommitter t <t@t>\n\nno date\n"
	cmd := exec.Command("git", "--git-dir=.", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = r.dir, gitEnv(os.Environ(), "", ""), strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	undated := strings.TrimSpace(string(out))
	if got, err := mergeDate(ctx, r, undated); err == nil {
		t.Errorf("mergeDate of a commit with no committer date = %q, want an error", got)
	}
}

// mergeStory is the story the merge of main tests build a merge for, and
// mergeStoryBranch its story branch. Its tip is the remote's head.
const (
	mergeStory       = 5
	mergeStoryBranch = "issuedog/5-keep-story-current"
)

// conflictBug is the conflict bug the conflicted merge tests build a merge
// for.
const conflictBug = 9

// botName and botEmail are the App's bot identity, as the daemon passes it
// to MainMerge.
const botName, botEmail = "issuedog[bot]", "issuedog[bot]@users.noreply.github.com"

// checkMergeStored requires every object sha reaches to be in the bare
// repo itself, past the attrless repo it was built in, and no quarantine or
// attrless dir to be left in its objects dir.
func checkMergeStored(t *testing.T, r *Repo, sha string) {
	t.Helper()
	run(t, r.dir, "rev-list", "--objects", "--missing=error", sha)
	run(t, r.dir, "fsck", "--connectivity-only", "--no-dangling")
	for _, f := range objectFiles(t, r) {
		if strings.HasPrefix(filepath.Base(f), "tmp_objdir") || strings.HasPrefix(f, "tmp_objdir") {
			t.Errorf("the merge left %s in the bare repo's objects dir", f)
		}
	}
}

// TestMainMerge builds a story's clean merge of main: a commit whose
// parents are the story branch's tip and main, in that order, whose tree
// holds both sides' changes, by the App's bot identity, dated as main's tip
// was committed, with the message "Merge `main` at <SHA> into <branch>",
// unsigned, at the story's ref under StoryMergesRef. The bare repo has
// every object it reaches. Built again, in the same bare repo and in
// another, it's the same SHA, and a build again moves no objects in. A sync
// after it leaves the ref and the commit alone.
func TestMainMerge(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()

	sha, conflicts, err := r.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || !shaPattern.MatchString(sha) {
		t.Fatalf("MainMerge = %q, %q, %v; want a merge commit and no conflicts", sha, conflicts, err)
	}
	ref := StoryMergesRef + "5"
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if got := run(t, r.dir, "rev-list", "--parents", "--no-walk", sha); got != sha+" "+m.head+" "+m.target {
		t.Errorf("the merge's parents: %q, want the tip %s then main %s", got, m.head, m.target)
	}
	for path, want := range map[string]string{"a.txt": "a, main", "b.txt": "b, head", "new.txt": "new"} {
		if got := run(t, r.dir, "cat-file", "blob", sha+":"+path); got != want {
			t.Errorf("%s in the merge = %q, want %q", path, got, want)
		}
	}
	format := "%an <%ae> %ad%n%cn <%ce> %cd%n%B"
	want := botName + " <" + botEmail + "> " + targetDate + "\n" +
		botName + " <" + botEmail + "> " + targetDate + "\n" +
		"Merge `main` at " + m.target + " into " + mergeStoryBranch
	if got := run(t, r.dir, "log", "-1", "--date=raw", "--format="+format, sha); got != want {
		t.Errorf("the merge's identity, dates and message =\n%s\nwant\n%s", got, want)
	}
	if got := run(t, r.dir, "cat-file", "commit", sha); strings.Contains(got, "gpgsig") {
		t.Errorf("the merge is signed:\n%s", got)
	}
	checkMergeStored(t, r, sha)

	before := objectFiles(t, r)
	again, conflicts, err := r.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || again != sha {
		t.Errorf("MainMerge again = %q, %q, %v; want the same %s", again, conflicts, err, sha)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("building the same merge again changed the objects dir: %q, then %q", before, after)
	}

	other := m.clone()
	elsewhere, conflicts, err := other.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || elsewhere != sha {
		t.Errorf("MainMerge in another bare repo = %q, %q, %v; want the same %s", elsewhere, conflicts, err, sha)
	}

	m.commit("c.txt", "c\n")
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) after a sync = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if has, err := r.HasCommit(ctx, sha); err != nil || !has {
		t.Errorf("HasCommit(%s) after a sync = %v, %v; want true", sha, has, err)
	}
}

// TestMainMergeTipBehindMain merges main into a tip that's only behind it,
// as a story branch is just after its cut once main moves: still a merge
// commit, with the tip and main as its parents, whose tree is main's.
func TestMainMergeTipBehindMain(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()

	sha, conflicts, err := r.MainMerge(context.Background(), mergeStory, mergeStoryBranch, m.base, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || !shaPattern.MatchString(sha) {
		t.Fatalf("MainMerge = %q, %q, %v; want a merge commit and no conflicts", sha, conflicts, err)
	}
	if got := run(t, r.dir, "rev-list", "--parents", "--no-walk", sha); got != sha+" "+m.base+" "+m.target {
		t.Errorf("the merge's parents: %q, want the tip %s then main %s", got, m.base, m.target)
	}
	if got, want := run(t, r.dir, "rev-parse", sha+"^{tree}"), run(t, r.dir, "rev-parse", m.target+"^{tree}"); got != want {
		t.Errorf("the merge's tree = %s, want main's %s", got, want)
	}
	if got := run(t, r.dir, "rev-parse", StoryMergesRef+"5"); got != sha {
		t.Errorf("%s5 = %s, want %s", StoryMergesRef, got, sha)
	}
	checkMergeStored(t, r, sha)
}

// TestMainMergeConflict names each conflicting path of a merge of main
// that doesn't apply cleanly, returns no SHA, writes no ref and leaves the
// bare repo's objects dir as it was.
func TestMainMergeConflict(t *testing.T) {
	t.Parallel()
	m := newMergeRemote(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"},
		map[string]string{"a.txt": "a, main\n", "b.txt": "b, main\n"},
		map[string]string{"a.txt": "a, tip\n", "b.txt": "b, tip\n", "c.txt": "c, tip\n"})
	r := m.clone()
	refsBefore, before := refs(t, r), objectFiles(t, r)

	sha, conflicts, err := r.MainMerge(context.Background(), mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	slices.Sort(conflicts)
	if err != nil || sha != "" || !slices.Equal(conflicts, []string{"a.txt", "b.txt"}) {
		t.Errorf("MainMerge = %q, %q, %v; want no SHA and the conflicts [a.txt b.txt]", sha, conflicts, err)
	}
	if got := refs(t, r); got != refsBefore {
		t.Errorf("refs after a conflict =\n%s\nwant\n%s", got, refsBefore)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("a conflict left objects behind: %q, then %q", before, after)
	}
}

// TestMainMergeRebuild builds a story's merge of main again after a crash:
// once with the commit stored but its ref not yet written, which writes
// the ref and moves no objects in, and once with the ref left at another
// commit, as an older main's merge, which moves the ref to the same SHA.
func TestMainMergeRebuild(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()
	ref := StoryMergesRef + "5"

	sha, conflicts, err := r.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || sha == "" {
		t.Fatalf("MainMerge = %q, %q, %v; want a merge commit", sha, conflicts, err)
	}
	if err := r.DeleteRef(ctx, ref); err != nil {
		t.Fatalf("DeleteRef(%s): %v", ref, err)
	}
	before := objectFiles(t, r)
	again, conflicts, err := r.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || again != sha {
		t.Errorf("MainMerge with no ref = %q, %q, %v; want the same %s", again, conflicts, err, sha)
	}
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) after the rebuild = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("rebuilding a stored merge changed the objects dir: %q, then %q", before, after)
	}

	if err := r.UpdateRef(ctx, ref, m.base); err != nil {
		t.Fatalf("UpdateRef(%s): %v", ref, err)
	}
	again, conflicts, err = r.MainMerge(ctx, mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || len(conflicts) != 0 || again != sha {
		t.Errorf("MainMerge with the ref at %s = %q, %q, %v; want the same %s", m.base, again, conflicts, err, sha)
	}
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) after the rebuild = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
}

// TestMainMergeIgnoresAttributes merges main into a tip that changes the
// same file, with merge=union set for it in the tip's .gitattributes and
// in the bare repo's info/attributes, with attr.tree naming the tip for a
// git that has it. A plain merge-tree in the bare repo with the same setup
// merges it cleanly, so the setup would catch a merge that let the
// attribute apply. MainMerge finds the conflict and writes no ref.
func TestMainMergeIgnoresAttributes(t *testing.T) {
	t.Parallel()
	attrs := "*.txt merge=union\n"
	m := newMergeRemote(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a, main\n"},
		map[string]string{"a.txt": "a, tip\n", ".gitattributes": attrs})
	r := m.clone()
	if err := os.WriteFile(filepath.Join(r.dir, "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, r.dir, "--git-dir=.", "config", "attr.tree", m.head)
	plain := exec.Command("git", "--git-dir=.", "merge-tree", "--write-tree", m.head, m.target)
	plain.Dir, plain.Env = r.dir, gitEnv(os.Environ(), "", "")
	if out, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("plain merge-tree: %v: %s\nwant merge=union to merge a.txt cleanly, or the setup proves nothing", err, out)
	}
	refsBefore := refs(t, r)

	sha, conflicts, err := r.MainMerge(context.Background(), mergeStory, mergeStoryBranch, m.head, m.target, botName, botEmail)
	if err != nil || sha != "" || !slices.Equal(conflicts, []string{"a.txt"}) {
		t.Errorf("MainMerge = %q, %q, %v; want no SHA and the conflict [a.txt], with merge=union ignored", sha, conflicts, err)
	}
	if got := refs(t, r); got != refsBefore {
		t.Errorf("refs after a conflict =\n%s\nwant\n%s", got, refsBefore)
	}
}

// TestMainMergeRefuses refuses a story number that isn't one, a tip or
// main that isn't a full SHA, one the bare repo lacks, one that names a
// tree, a branch that isn't a valid branch name, and an identity that's
// empty or holds a character git drops. Each writes no ref and leaves the
// objects dir as it was.
func TestMainMergeRefuses(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()
	missing := strings.Repeat("1", len(m.head))
	tree := run(t, r.dir, "rev-parse", m.head+"^{tree}")
	tests := []struct {
		name                  string
		badStory              bool // story is the one to pass, not mergeStory
		story                 int
		branch, tip, main     string
		identName, identEmail string
		want                  string // in the error
	}{
		{name: "story zero", badStory: true, story: 0, want: "invalid story number"},
		{name: "story negative", badStory: true, story: -1, want: "invalid story number"},
		{name: "short tip", tip: m.head[:12], want: "invalid SHA"},
		{name: "short main", main: m.target[:12], want: "invalid SHA"},
		{name: "uppercase main", main: strings.ToUpper(m.target), want: "invalid SHA"},
		{name: "main a ref", main: "refs/heads/main", want: "invalid SHA"},
		{name: "missing tip", tip: missing, want: "no commit " + missing},
		{name: "missing main", main: missing, want: "no commit " + missing},
		{name: "tip a tree", tip: tree, want: "no commit " + tree},
		{name: "branch empty", branch: "-", want: "invalid branch"},
		{name: "branch with a space", branch: "issuedog/5 story", want: "invalid branch"},
		{name: "branch with dots", branch: "issuedog/5..story", want: "invalid branch"},
		{name: "branch with a newline", branch: "issuedog/5\nstory", want: "invalid branch"},
		{name: "empty name", identName: "-", want: "empty identity name"},
		{name: "empty email", identEmail: "-", want: "empty identity email"},
		{name: "name with <", identName: "issuedog <x>", want: "invalid identity name"},
		{name: "email with >", identEmail: "a>b@example.com", want: "invalid identity email"},
		{name: "name with a newline", identName: "issuedog\nx", want: "invalid identity name"},
		{name: "email with a NUL", identEmail: "a\x00@example.com", want: "invalid identity email"},
	}
	refsBefore, before := refs(t, r), objectFiles(t, r)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case sets only what it refuses; the rest are valid, and
			// "-" stands for an empty value.
			or := func(v, def string) string {
				switch v {
				case "":
					return def
				case "-":
					return ""
				}
				return v
			}
			story := mergeStory
			if tt.badStory {
				story = tt.story
			}
			sha, conflicts, err := r.MainMerge(ctx, story, or(tt.branch, mergeStoryBranch), or(tt.tip, m.head), or(tt.main, m.target),
				or(tt.identName, botName), or(tt.identEmail, botEmail))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("MainMerge = %q, %q, %v; want an error containing %q", sha, conflicts, err, tt.want)
			}
			if got := refs(t, r); got != refsBefore {
				t.Errorf("refs after a refusal =\n%s\nwant\n%s", got, refsBefore)
			}
			if after := objectFiles(t, r); !slices.Equal(after, before) {
				t.Errorf("a refusal changed the objects dir: %q, then %q", before, after)
			}
		})
	}
}

// TestConflictMerge keeps a conflict bug's conflicted merge of its story
// tip into its main commit: a commit whose parents are main and the tip,
// in that order, whose conflicting file holds its markers as merge-tree
// --write-tree writes them, labelled by the two SHAs, whose other files
// are merged, by issuedog, dated as main was committed, with a fixed
// message, at the bug's ref under ConflictsRef. merge=union, set for the
// file in the tip's .gitattributes and in the bare repo's info/attributes,
// with attr.tree naming the tip for a git that has it, changes none of it,
// though a plain merge-tree with that setup merges the file cleanly. The
// bare repo has every object it reaches, the markers' blob included. It
// names the conflicting file, a.txt, alone, as the conflict's files.
// Built again, it's the same SHA and the same file, and moves no objects
// in, and a sync leaves the ref alone.
func TestConflictMerge(t *testing.T) {
	t.Parallel()
	attrs := "*.txt merge=union\n"
	m := newMergeRemote(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"},
		map[string]string{"a.txt": "a, main\n", "b.txt": "b, main\n"},
		map[string]string{"a.txt": "a, tip\n", "c.txt": "c, tip\n", ".gitattributes": attrs})
	r := m.clone()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(r.dir, "info", "attributes"), []byte(attrs), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, r.dir, "--git-dir=.", "config", "attr.tree", m.head)
	plain := exec.Command("git", "--git-dir=.", "merge-tree", "--write-tree", m.target, m.head)
	plain.Dir, plain.Env = r.dir, gitEnv(os.Environ(), "", "")
	if out, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("plain merge-tree: %v: %s\nwant merge=union to merge a.txt cleanly, or the setup proves nothing", err, out)
	}

	sha, conflicts, err := r.ConflictMerge(ctx, conflictBug, m.target, m.head)
	if err != nil || !shaPattern.MatchString(sha) {
		t.Fatalf("ConflictMerge = %q, %q, %v; want a merge commit", sha, conflicts, err)
	}
	if !slices.Equal(conflicts, []string{"a.txt"}) {
		t.Errorf("ConflictMerge's conflicts = %q, want a.txt alone", conflicts)
	}
	ref := ConflictsRef + "9"
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
	if got := run(t, r.dir, "rev-list", "--parents", "--no-walk", sha); got != sha+" "+m.target+" "+m.head {
		t.Errorf("the merge's parents: %q, want main %s then the tip %s", got, m.target, m.head)
	}
	markers := "<<<<<<< " + m.target + "\na, main\n=======\na, tip\n>>>>>>> " + m.head
	for path, want := range map[string]string{"a.txt": markers, "b.txt": "b, main", "c.txt": "c, tip", ".gitattributes": strings.TrimSpace(attrs)} {
		if got := run(t, r.dir, "cat-file", "blob", sha+":"+path); got != want {
			t.Errorf("%s in the conflicted merge =\n%s\nwant\n%s", path, got, want)
		}
	}
	format := "%an <%ae> %ad%n%cn <%ce> %cd%n%B"
	want := "issuedog <issuedog@localhost> " + targetDate + "\n" +
		"issuedog <issuedog@localhost> " + targetDate + "\n" +
		"issuedog: conflicted merge of " + m.head + " into " + m.target
	if got := run(t, r.dir, "log", "-1", "--date=raw", "--format="+format, sha); got != want {
		t.Errorf("the merge's identity, dates and message =\n%s\nwant\n%s", got, want)
	}
	if got := run(t, r.dir, "cat-file", "commit", sha); strings.Contains(got, "gpgsig") {
		t.Errorf("the merge is signed:\n%s", got)
	}
	checkMergeStored(t, r, sha)

	before := objectFiles(t, r)
	again, conflicts, err := r.ConflictMerge(ctx, conflictBug, m.target, m.head)
	if err != nil || again != sha || !slices.Equal(conflicts, []string{"a.txt"}) {
		t.Errorf("ConflictMerge again = %q, %q, %v; want the same %s, and a.txt", again, conflicts, err, sha)
	}
	if after := objectFiles(t, r); !slices.Equal(after, before) {
		t.Errorf("building the same merge again changed the objects dir: %q, then %q", before, after)
	}

	m.commit("d.txt", "d\n")
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, ok, err := r.ReadRef(ctx, ref); err != nil || !ok || got != sha {
		t.Errorf("ReadRef(%s) after a sync = %q, %v, %v; want %s", ref, got, ok, err, sha)
	}
}

// TestConflictMergeClean keeps a merge with no conflicts the same way: a
// commit of the merged tree, with both as its parents, at the bug's ref,
// naming no conflicting file.
func TestConflictMergeClean(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()

	sha, conflicts, err := r.ConflictMerge(context.Background(), conflictBug, m.target, m.head)
	if err != nil || !shaPattern.MatchString(sha) || len(conflicts) != 0 {
		t.Fatalf("ConflictMerge = %q, %q, %v; want a merge commit, and no conflicts", sha, conflicts, err)
	}
	if got := run(t, r.dir, "rev-list", "--parents", "--no-walk", sha); got != sha+" "+m.target+" "+m.head {
		t.Errorf("the merge's parents: %q, want main %s then the tip %s", got, m.target, m.head)
	}
	for path, want := range map[string]string{"a.txt": "a, main", "b.txt": "b, head", "new.txt": "new"} {
		if got := run(t, r.dir, "cat-file", "blob", sha+":"+path); got != want {
			t.Errorf("%s in the merge = %q, want %q", path, got, want)
		}
	}
	if got := run(t, r.dir, "rev-parse", ConflictsRef+"9"); got != sha {
		t.Errorf("%s9 = %s, want %s", ConflictsRef, got, sha)
	}
	checkMergeStored(t, r, sha)
}

// TestConflictMergeRefuses refuses an issue number that isn't one, a
// commit that isn't a full SHA, one the bare repo lacks, and one that
// names a tree. Each writes no ref and leaves the objects dir as it was.
func TestConflictMergeRefuses(t *testing.T) {
	t.Parallel()
	m := cleanMergeRemote(t)
	r := m.clone()
	ctx := context.Background()
	missing := strings.Repeat("1", len(m.head))
	tree := run(t, r.dir, "rev-parse", m.head+"^{tree}")
	tests := []struct {
		name string
		n    int
		a, b string
		want string // in the error
	}{
		{name: "issue zero", n: 0, a: m.target, b: m.head, want: "invalid issue number"},
		{name: "issue negative", n: -1, a: m.target, b: m.head, want: "invalid issue number"},
		{name: "short main", n: conflictBug, a: m.target[:12], b: m.head, want: "invalid SHA"},
		{name: "short tip", n: conflictBug, a: m.target, b: m.head[:12], want: "invalid SHA"},
		{name: "tip a ref", n: conflictBug, a: m.target, b: "refs/heads/" + mergeTaskBranch, want: "invalid SHA"},
		{name: "missing main", n: conflictBug, a: missing, b: m.head, want: "no commit " + missing},
		{name: "missing tip", n: conflictBug, a: m.target, b: missing, want: "no commit " + missing},
		{name: "tip a tree", n: conflictBug, a: m.target, b: tree, want: "no commit " + tree},
	}
	refsBefore, before := refs(t, r), objectFiles(t, r)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sha, conflicts, err := r.ConflictMerge(ctx, tt.n, tt.a, tt.b)
			if err == nil || !strings.Contains(err.Error(), tt.want) || sha != "" || conflicts != nil {
				t.Errorf("ConflictMerge = %q, %q, %v; want an error containing %q, and nothing else", sha, conflicts, err, tt.want)
			}
			if got := refs(t, r); got != refsBefore {
				t.Errorf("refs after a refusal =\n%s\nwant\n%s", got, refsBefore)
			}
			if after := objectFiles(t, r); !slices.Equal(after, before) {
				t.Errorf("a refusal changed the objects dir: %q, then %q", before, after)
			}
		})
	}
}

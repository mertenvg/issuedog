package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MergesRef is the prefix of each task's merge result in the bare repo,
// refs/issuedog/merges/<n>: its head's merge with main, or for a task in a
// story its story branch's tip, which MergeCommit builds for the checks
// when that target moves (SPEC §3.2, §7.5, §10.4). Sync leaves it alone,
// and nothing pushes it.
const MergesRef = "refs/issuedog/merges/"

// StoryMergesRef is the prefix of each story's merge of main in the bare
// repo, refs/issuedog/story-merges/<story>: main merged into its story
// branch's tip, which MainMerge builds for the push that brings main into
// the branch (SPEC §3.2, §7.5). Sync leaves it alone.
const StoryMergesRef = "refs/issuedog/story-merges/"

// ConflictsRef is the prefix of each conflict bug's conflicted merge in the
// bare repo, refs/issuedog/conflicts/<n>: the merge of its story branch's
// tip into the main commit it names, conflicts and their markers included,
// which ConflictMerge builds for the bug's checks to diff from (SPEC
// §10.1). Sync leaves it alone, and nothing pushes it.
const ConflictsRef = "refs/issuedog/conflicts/"

// mergeName and mergeEmail are the author and committer of every merge
// MergeCommit and ConflictMerge build: fixed, with the date fixed too
// (mergeDate), so one pair of commits always gives one merge commit.
const mergeName, mergeEmail = "issuedog", "issuedog@localhost"

// mergeSpec is who a merge commit is by, as its author and committer, and
// its message: the parts of a merge commit that commitMerge doesn't take
// from its parents.
type mergeSpec struct {
	Name, Email, Message string
}

// MergeCommit builds task n's merge result: head, a full SHA, merged into
// target, main's full SHA or a story branch's tip's, as a merge commit in
// the bare repo, with target
// and head as its parents, in that order. It returns the merge commit's
// SHA, which it writes to MergesRef+n, or, for a merge that doesn't apply
// cleanly, each conflicting path once and no SHA.
//
// The merge is rule 7's, as MergeTree judges it (SPEC §10.1): git
// merge-tree in an attrless repo (attrless), so no attribute, such as
// merge=union, changes how a file merges. The commit is commit-tree's in
// that repo too, by a fixed identity (mergeName), dated as target was
// committed, with a fixed message and no signature, so the same pair
// always gives the same SHA. Its objects move into the bare repo, packed,
// as a bundle's do once checked (migrate), and only then is the ref
// written. A conflict writes no ref, and the attrless repo, with every
// object the merge wrote, is removed, so it leaves nothing in the bare
// repo. Like every call here, these run with no hooks and no system or
// global config (gitEnv).
//
// A target or head that isn't a full SHA, or that names no commit the bare
// repo has, is refused, and so is an n that isn't a task's number.
func (r *Repo) MergeCommit(ctx context.Context, n int, target, head string) (sha string, conflicts []string, err error) {
	if n <= 0 {
		return "", nil, fmt.Errorf("merge commit: invalid task number %d", n)
	}
	if err := checkSHAs(target, head); err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	if err := r.hasCommits(ctx, target, head); err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	dir, a, err := r.attrless(ctx, target)
	if err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	tree, conflicts, err := mergeTree(ctx, a, target, head)
	if err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	if len(conflicts) > 0 {
		return "", conflicts, nil
	}
	spec := mergeSpec{Name: mergeName, Email: mergeEmail, Message: "issuedog: merge " + head + " with " + target}
	sha, err = r.commitMerge(ctx, a, tree, target, head, target, spec)
	if err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	if err := r.UpdateRef(ctx, MergesRef+strconv.Itoa(n), sha); err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	return sha, nil, nil
}

// MainMerge builds story's merge of main: main, a full SHA, merged into
// tip, its story branch's tip's full SHA, as a merge commit in the bare
// repo, with tip and main as its parents, in that order, ready to push to
// branch, the story branch's name. It returns the merge commit's SHA, which
// it writes to StoryMergesRef+story, or, for a merge that doesn't apply
// cleanly, each conflicting path once and no SHA.
//
// The merge is built as MergeCommit's is (SPEC §3.2): git merge-tree in an
// attrless repo, so no attribute, such as merge=union, changes how a file
// merges, and commit-tree there, with no signature. Its author and
// committer are name and email, the App's bot identity, it's dated as main
// was committed, and its message is "Merge `main` at <main> into
// <branch>", so the same pair, branch and identity always give the same
// SHA, and a build again after a crash gives the commit it built before.
// Its objects move into the bare repo, packed, and only then is the ref
// written. A conflict writes no ref and leaves no object in the bare repo.
// Like every call here, these run with no hooks and no system or global
// config (gitEnv).
//
// A tip or main that isn't a full SHA, or that names no commit the bare
// repo has, is refused, and so is a story that isn't an issue's number, a
// branch that isn't a valid branch name, and a name or email that's empty
// or holds a character git would drop from an identity.
func (r *Repo) MainMerge(ctx context.Context, story int, branch, tip, main, name, email string) (sha string, conflicts []string, err error) {
	if story <= 0 {
		return "", nil, fmt.Errorf("merge main: invalid story number %d", story)
	}
	if err := checkSHAs(tip, main); err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	if err := checkIdent(name, email); err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	if _, err := r.git(ctx, "", "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: invalid branch %q: %w", story, branch, err)
	}
	if err := r.hasCommits(ctx, tip, main); err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	dir, a, err := r.attrless(ctx, tip)
	if err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	tree, conflicts, err := mergeTree(ctx, a, tip, main)
	if err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	if len(conflicts) > 0 {
		return "", conflicts, nil
	}
	spec := mergeSpec{Name: name, Email: email, Message: "Merge `main` at " + main + " into " + branch}
	sha, err = r.commitMerge(ctx, a, tree, tip, main, main, spec)
	if err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	if err := r.UpdateRef(ctx, StoryMergesRef+strconv.Itoa(story), sha); err != nil {
		return "", nil, fmt.Errorf("merge main into story %d: %w", story, err)
	}
	return sha, nil, nil
}

// ConflictMerge builds conflict bug n's conflicted merge: b, a full SHA,
// merged into a, a full SHA, as git merge-tree --write-tree writes it, so a
// conflicting file holds its conflict markers, kept as a merge commit in
// the bare repo with a and b as its parents, in that order, for the bug's
// checks to diff from (SPEC §10.1). For a conflict bug, a is the main
// commit it names and b its story branch's tip. It returns the commit's
// SHA, which it writes to ConflictsRef+n. A merge with no conflicts is kept
// the same way.
//
// The merge and its commit are built as MergeCommit's are: in an attrless
// repo, so no attribute, such as merge=union, changes how a file merges,
// by the fixed identity (mergeName), dated as a was committed, with a
// fixed message and no signature, so the same pair always gives the same
// SHA. Its objects, the conflicted files' included, move into the bare
// repo, packed, and only then is the ref written. Like every call here,
// these run with no hooks and no system or global config (gitEnv).
//
// An a or b that isn't a full SHA, or that names no commit the bare repo
// has, is refused, and so is an n that isn't an issue's number.
func (r *Repo) ConflictMerge(ctx context.Context, n int, a, b string) (sha string, err error) {
	if n <= 0 {
		return "", fmt.Errorf("conflicted merge: invalid issue number %d", n)
	}
	if err := checkSHAs(a, b); err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	if err := r.hasCommits(ctx, a, b); err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	dir, at, err := r.attrless(ctx, a)
	if err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove attrless repo: %w", rerr))
		}
	}()
	// The tree is kept whether or not the merge conflicts: a conflicting
	// file's blob holds its markers, as merge-tree wrote it.
	tree, _, err := mergeTree(ctx, at, a, b)
	if err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	spec := mergeSpec{Name: mergeName, Email: mergeEmail, Message: "issuedog: conflicted merge of " + b + " into " + a}
	sha, err = r.commitMerge(ctx, at, tree, a, b, a, spec)
	if err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	if err := r.UpdateRef(ctx, ConflictsRef+strconv.Itoa(n), sha); err != nil {
		return "", fmt.Errorf("conflicted merge of issue %d: %w", n, err)
	}
	return sha, nil
}

// hasCommits refuses a SHA that names no commit the bare repo has.
func (r *Repo) hasCommits(ctx context.Context, shas ...string) error {
	for _, c := range shas {
		has, err := r.HasCommit(ctx, c)
		if err != nil {
			return fmt.Errorf("find commit %s: %w", c, err)
		}
		if !has {
			return fmt.Errorf("the bare repo has no commit %s", c)
		}
	}
	return nil
}

// checkIdent refuses a merge's author name or email that's empty, or that
// holds a character git drops from an identity or that would end its
// header line: '<', '>', a newline or a NUL.
func checkIdent(name, email string) error {
	for _, f := range []struct{ what, v string }{{"name", name}, {"email", email}} {
		if f.v == "" {
			return fmt.Errorf("empty identity %s", f.what)
		}
		if strings.ContainsAny(f.v, "<>\n\x00") {
			return fmt.Errorf("invalid identity %s %q", f.what, f.v)
		}
	}
	return nil
}

// commitMerge writes the merge commit of tree, with first and second as
// its parents, in that order, by spec's identity as author and committer,
// dated as dated, one of the parents, was committed, with spec's message,
// in a, an attrless repo for r. It moves what the bare repo lacks of it
// there: the commit, and every tree and blob the merge wrote that it
// reaches. A commit the bare repo has already, from an earlier build of
// the same pair, moves nothing.
func (r *Repo) commitMerge(ctx context.Context, a *Repo, tree, first, second, dated string, spec mergeSpec) (string, error) {
	date, err := mergeDate(ctx, a, dated)
	if err != nil {
		return "", fmt.Errorf("commit the merge: %w", err)
	}
	c := *a
	c.env = append(append([]string(nil), a.env...),
		"GIT_AUTHOR_NAME="+spec.Name, "GIT_AUTHOR_EMAIL="+spec.Email, "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME="+spec.Name, "GIT_COMMITTER_EMAIL="+spec.Email, "GIT_COMMITTER_DATE="+date)
	out, err := c.git(ctx, "", "commit-tree", "--no-gpg-sign", "-p", first, "-p", second, "-m", spec.Message, tree)
	if err != nil {
		return "", fmt.Errorf("commit the merge: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("commit the merge: commit-tree gave %q, not a SHA", out)
	}
	has, err := r.HasCommit(ctx, sha)
	if err != nil {
		return "", fmt.Errorf("commit the merge: %w", err)
	}
	if has {
		return sha, nil
	}
	// --revs reads the commits to pack, and --not the ones whose history
	// the bare repo has already; --local leaves out every object it has.
	objects := filepath.Join(a.dir, "objects")
	revs := strings.NewReader(sha + "\n--not\n" + first + "\n" + second + "\n")
	if _, err := a.run(ctx, revs, "", "pack-objects", "--revs", "--local", "--quiet", filepath.Join(objects, "pack", "pack")); err != nil {
		return "", fmt.Errorf("pack the merge %s: %w", sha, err)
	}
	if err := r.migrate(objects); err != nil {
		return "", fmt.Errorf("store the merge %s: %w", sha, err)
	}
	return sha, nil
}

// mergeDate returns the committer date of commit, a full SHA, as git
// wrote it, "@<seconds> <zone>", for a merge commit's dates. It reads the
// commit's own header, so no config or format changes it.
func mergeDate(ctx context.Context, r *Repo, commit string) (string, error) {
	out, err := r.git(ctx, "", "cat-file", "commit", commit)
	if err != nil {
		return "", fmt.Errorf("read %s's date: %w", commit, err)
	}
	header, _, _ := bytes.Cut(out, []byte("\n\n"))
	for line := range strings.Lines(string(header)) {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "committer ")
		if !ok {
			continue
		}
		// The identity ends at the last '>', then come the seconds and the
		// zone.
		i := strings.LastIndexByte(rest, '>')
		fields := strings.Fields(rest[i+1:])
		if i < 0 || len(fields) != 2 {
			return "", fmt.Errorf("read %s's date: bad committer line %q", commit, line)
		}
		if _, err := strconv.ParseInt(fields[0], 10, 64); err != nil {
			return "", fmt.Errorf("read %s's date: bad committer line %q: %w", commit, line, err)
		}
		return "@" + fields[0] + " " + fields[1], nil
	}
	return "", fmt.Errorf("read %s's date: no committer line", commit)
}

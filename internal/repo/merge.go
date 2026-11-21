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
// refs/issuedog/merges/<n>: its head's merge with main, which MergeCommit
// builds for the checks when main moves (SPEC §3.2, §10.4). Sync leaves it
// alone, and nothing pushes it.
const MergesRef = "refs/issuedog/merges/"

// mergeName and mergeEmail are the author and committer of every merge
// MergeCommit builds: fixed, with the date fixed too (mergeDate), so one
// pair of commits always gives one merge commit.
const mergeName, mergeEmail = "issuedog", "issuedog@localhost"

// MergeCommit builds task n's merge result: head, a full SHA, merged into
// target, main's full SHA, as a merge commit in the bare repo, with target
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
	for _, c := range []string{target, head} {
		has, err := r.HasCommit(ctx, c)
		if err != nil {
			return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
		}
		if !has {
			return "", nil, fmt.Errorf("merge commit of task %d: the bare repo has no commit %s", n, c)
		}
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
	sha, err = r.commitMerge(ctx, a, tree, target, head)
	if err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	if err := r.UpdateRef(ctx, MergesRef+strconv.Itoa(n), sha); err != nil {
		return "", nil, fmt.Errorf("merge commit of task %d: %w", n, err)
	}
	return sha, nil, nil
}

// commitMerge writes the merge commit of tree, with target and head as its
// parents, in a, an attrless repo for r, and moves what the bare repo
// lacks of it there: the commit, and every tree and blob the merge wrote
// that it reaches. A commit the bare repo has already, from an earlier
// build of the same pair, moves nothing.
func (r *Repo) commitMerge(ctx context.Context, a *Repo, tree, target, head string) (string, error) {
	date, err := mergeDate(ctx, a, target)
	if err != nil {
		return "", fmt.Errorf("commit the merge: %w", err)
	}
	c := *a
	c.env = append(append([]string(nil), a.env...),
		"GIT_AUTHOR_NAME="+mergeName, "GIT_AUTHOR_EMAIL="+mergeEmail, "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME="+mergeName, "GIT_COMMITTER_EMAIL="+mergeEmail, "GIT_COMMITTER_DATE="+date)
	msg := "issuedog: merge " + head + " with main at " + target
	out, err := c.git(ctx, "", "commit-tree", "--no-gpg-sign", "-p", target, "-p", head, "-m", msg, tree)
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
	revs := strings.NewReader(sha + "\n--not\n" + target + "\n" + head + "\n")
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

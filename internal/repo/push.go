package repo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// BranchPrefix is the prefix of every branch Push may write, the App's own
// branches on GitHub, story and task branches alike (SPEC §7.5, §15).
const BranchPrefix = "issuedog/"

// ErrPushStale is returned by Push when the branch's tip on the remote isn't
// the one the lease names, so it moved since the sync that saw it.
var ErrPushStale = errors.New("the branch moved since the lease")

// Push sends sha, a commit the bare repo has, to branch, a branch under
// issuedog/, on the remote: the only push the daemon makes (SPEC §3.2). It
// pushes from the bare repo as the daemon, with the token as an HTTP header,
// as Sync sends it, never on disk or in argv, and with no hooks and no
// system or global config (gitEnv).
//
// It's a lease push. old is the branch's tip the caller saw, from the last
// sync, and the push lands only while the remote's tip is still old, or,
// with old "", only while the remote has no such branch. So a branch with no
// tip is created, and one at old is moved to sha, whether that's a
// fast-forward or not: the caller decides which tips may be forced over.
// A branch that moved since fails the push with an error matching
// ErrPushStale, naming the branch, and nothing changes, on the remote or in
// the bare repo. A branch already at sha is left as it is, whatever old
// names.
//
// Once the push lands, the bare repo's own refs/heads/<branch> is moved to
// sha too, so it matches the remote's until the next sync.
//
// A sha or an old that isn't a full SHA is refused, and so is a sha the bare
// repo doesn't have, or a branch outside issuedog/, each before any request.
func (r *Repo) Push(ctx context.Context, token, sha, branch, old string) error {
	if err := checkSHAs(sha); err != nil {
		return fmt.Errorf("push to %s: %w", branch, err)
	}
	if old != "" {
		if err := checkSHAs(old); err != nil {
			return fmt.Errorf("push %s to %s: the lease: %w", sha, branch, err)
		}
	}
	ref, err := r.branchRef(ctx, branch)
	if err != nil {
		return fmt.Errorf("push %s: %w", sha, err)
	}
	has, err := r.HasCommit(ctx, sha)
	if err != nil {
		return fmt.Errorf("push %s to %s: %w", sha, branch, err)
	}
	if !has {
		return fmt.Errorf("push %s to %s: the bare repo has no such commit", sha, branch)
	}
	// The lease's value is the tip the remote must still have, or, empty,
	// says it must have none. --no-verify skips the pre-push hook, which
	// gitEnv's core.hooksPath already turns off.
	out, err := r.git(ctx, token, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":"+old, "--", r.remote, sha+":"+ref)
	if err != nil {
		if pushStale(out) {
			return fmt.Errorf("push %s to %s, leased at %s: %w: %w", sha, branch, leased(old), ErrPushStale, err)
		}
		return fmt.Errorf("push %s to %s: %w", sha, branch, err)
	}
	if _, err := r.git(ctx, "", "update-ref", "--no-deref", ref, sha); err != nil {
		return fmt.Errorf("push %s to %s: pushed, but moving the bare repo's %s: %w", sha, branch, ref, err)
	}
	return nil
}

// leased names a lease's tip for an error: the SHA, or "no branch" for "".
func leased(old string) string {
	if old == "" {
		return "no branch"
	}
	return old
}

// pushStale reports whether git push's --porcelain output says the remote
// refused the ref because its lease was stale.
func pushStale(out []byte) bool {
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "!") && strings.HasSuffix(line, "(stale info)") {
			return true
		}
	}
	return false
}

// branchRef returns refs/heads/<branch> for a branch under BranchPrefix
// that's a valid branch name, and refuses any other.
func (r *Repo) branchRef(ctx context.Context, branch string) (string, error) {
	if !strings.HasPrefix(branch, BranchPrefix) || branch == BranchPrefix {
		return "", fmt.Errorf("branch %q isn't under %s", branch, BranchPrefix)
	}
	ref := "refs/heads/" + branch
	if _, err := r.git(ctx, "", "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("invalid branch %q: %w", branch, err)
	}
	return ref, nil
}

// Branches returns the bare repo's branches whose names start with prefix,
// each name, without refs/heads/, to the SHA of its tip. They're the
// remote's as the last Sync left them, plus what Push has pushed since, read
// locally, at no request. A prefix of "" returns every branch.
func (r *Repo) Branches(ctx context.Context, prefix string) (map[string]string, error) {
	// for-each-ref matches a pattern literally only up to a slash, so it's
	// given the prefix's directory, and the rest is matched here.
	dir := prefix[:strings.LastIndex(prefix, "/")+1]
	out, err := r.git(ctx, "", "for-each-ref", "--format=%(objectname) %(refname)", "--", "refs/heads/"+dir)
	if err != nil {
		return nil, fmt.Errorf("list branches %s*: %w", prefix, err)
	}
	branches := map[string]string{}
	for line := range strings.Lines(string(out)) {
		oid, ref, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok && strings.HasPrefix(name, prefix) {
			branches[name] = oid
		}
	}
	return branches, nil
}

// IsAncestor reports whether the commit a is in b's history, b itself
// included, read in the bare repo. Both are full SHAs of commits it has.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if err := checkSHAs(a, b); err != nil {
		return false, fmt.Errorf("is %s an ancestor of %s: %w", a, b, err)
	}
	if _, err := r.git(ctx, "", "merge-base", "--is-ancestor", a, b); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("is %s an ancestor of %s: %w", a, b, err)
	}
	return true, nil
}

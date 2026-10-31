// Package repo keeps the daemon's bare clone of the target repo and reads
// trusted files from its main branch (SPEC §3.2, §3.4).
package repo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotFound is returned by ReadFile when main has no file at the path.
var ErrNotFound = errors.New("not found")

// ErrNotFile is returned by ReadFile when what main has at the path isn't
// a file, such as a directory.
var ErrNotFile = errors.New("not a file")

// refspec mirrors every remote branch, main included, into refs/heads.
// Together with --prune, the bare repo's branches match the remote's after
// each Sync. Refs outside refs/heads, such as refs/issuedog/runs/*, are left
// alone.
const refspec = "+refs/heads/*:refs/heads/*"

// RunsRef is the prefix of the refs FetchBundle writes, one per run:
// refs/issuedog/runs/<run-id>.
const RunsRef = "refs/issuedog/runs/"

// TasksRef is the prefix of each task's head in the bare repo,
// refs/issuedog/tasks/<n>, which the run harness moves once a run's result
// applies (SPEC §3.2).
const TasksRef = "refs/issuedog/tasks/"

// ownRefs is the prefix of every ref ReadRef, UpdateRef and DeleteRef
// touch: the daemon's own, which Sync leaves alone.
const ownRefs = "refs/issuedog/"

// shaPattern matches a full SHA-1 or SHA-256 object name.
var shaPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// redirected matches git's error when the remote answers with a redirect,
// which gitEnv tells git not to follow. Git doesn't say where it pointed.
var redirected = regexp.MustCompile(`The requested URL returned error: (3[0-9][0-9])`)

// Repo is a daemon-owned bare clone.
type Repo struct {
	remote string
	dir    string
	env    []string // extra git environment, after gitEnv's: a quarantine's (quarantine)

	beforeRunRef func() // for tests only: runs just before FetchBundle writes the run's ref
}

// New returns the bare clone at dir (for example /work/<repo>.git) of the
// remote URL. The URL must not carry credentials; Sync passes the token.
func New(remote, dir string) *Repo {
	return &Repo{remote: remote, dir: dir}
}

// Sync creates the bare repo if it's missing, then fetches every branch from
// the remote. A first sync that fails part way leaves a repo the next Sync
// fetches into. The token is sent as an HTTP header and is never written to
// disk or passed in argv. A remote that redirects fails the fetch, with an
// error that points at ISSUEDOG_REPO.
func (r *Repo) Sync(ctx context.Context, token string) error {
	_, err := os.Stat(filepath.Join(r.dir, "HEAD"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, err := r.git(ctx, "", "init", "--bare", "--quiet", "--initial-branch=main"); err != nil {
			return fmt.Errorf("init bare repo %s: %w", r.dir, err)
		}
	case err != nil:
		return fmt.Errorf("stat bare repo %s: %w", r.dir, err)
	}
	if _, err := r.git(ctx, token, "fetch", "--quiet", "--prune", "--no-tags", "--", r.remote, refspec); err != nil {
		if m := redirected.FindStringSubmatch(err.Error()); m != nil {
			return fmt.Errorf("fetch %s: the remote redirected (HTTP %s), and git doesn't follow redirects "+
				"while it carries the token; if the repo was renamed or transferred, check ISSUEDOG_REPO: %w", r.remote, m[1], err)
		}
		return fmt.Errorf("fetch %s: %w", r.remote, err)
	}
	return nil
}

// ReadFile returns the contents of path on main. It reads the blob with
// cat-file, so no textconv or other driver runs, and a directory is an
// error, matching ErrNotFile. A path main doesn't have gives an error
// matching ErrNotFound.
func (r *Repo) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("read main: empty path")
	}
	out, err := r.git(ctx, "", "cat-file", "blob", "main:"+path)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist in 'main'") {
			return nil, fmt.Errorf("read main:%s: %w", path, ErrNotFound)
		}
		if strings.HasSuffix(err.Error(), ": bad file") {
			return nil, fmt.Errorf("read main:%s: %w: %w", path, ErrNotFile, err)
		}
		return nil, fmt.Errorf("read main:%s: %w", path, err)
	}
	return out, nil
}

// Dir returns the bare repo's path.
func (r *Repo) Dir() string { return r.dir }

// Resolve returns the SHA of the commit ref names in the bare repo, such as
// main's, read locally. A ref the repo lacks, or one that names no commit,
// is an error.
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	out, err := r.git(ctx, "", "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadRef returns the object ref names in the bare repo, and whether it
// exists. ref is a full ref name under refs/issuedog/, matched exactly, so
// a branch synced from GitHub never stands in for it, whatever its name.
func (r *Repo) ReadRef(ctx context.Context, ref string) (string, bool, error) {
	if err := r.checkOwnRef(ctx, ref); err != nil {
		return "", false, fmt.Errorf("read ref: %w", err)
	}
	out, err := r.git(ctx, "", "for-each-ref", "--format=%(objectname) %(refname)", "--", ref)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", ref, err)
	}
	// The pattern also matches refs below ref, so only the exact name counts.
	for line := range strings.Lines(string(out)) {
		if oid, name, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " "); name == ref {
			return oid, true, nil
		}
	}
	return "", false, nil
}

// UpdateRef points ref, a full ref name under refs/issuedog/, at sha, a
// full SHA of an object the bare repo has, creating the ref if it's
// missing.
func (r *Repo) UpdateRef(ctx context.Context, ref, sha string) error {
	if err := r.checkOwnRef(ctx, ref); err != nil {
		return fmt.Errorf("update ref: %w", err)
	}
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("update %s: invalid SHA %q", ref, sha)
	}
	if _, err := r.git(ctx, "", "update-ref", "--no-deref", ref, sha); err != nil {
		return fmt.Errorf("update %s: %w", ref, err)
	}
	return nil
}

// DeleteRef deletes ref, a full ref name under refs/issuedog/. A missing
// ref is fine.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	if err := r.checkOwnRef(ctx, ref); err != nil {
		return fmt.Errorf("delete ref: %w", err)
	}
	if _, err := r.git(ctx, "", "update-ref", "--no-deref", "-d", ref); err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	return nil
}

// checkOwnRef refuses a ref that isn't a valid full ref name under
// refs/issuedog/.
func (r *Repo) checkOwnRef(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, ownRefs) {
		return fmt.Errorf("%q isn't under %s", ref, ownRefs)
	}
	if _, err := r.git(ctx, "", "check-ref-format", ref); err != nil {
		return fmt.Errorf("invalid ref %q: %w", ref, err)
	}
	return nil
}

// FetchBundle takes a run's commits into the bare repo, as the daemon, from
// the bundle at path: a file in a dir only the daemon can write, holding
// what workspace.Bundle wrote of start..HEAD in the run's clone (SPEC §3.2).
// It reads only that file, never the clone, and returns the bundle's HEAD,
// which it writes to RunsRef+runID, and nothing else: no tags and no other
// ref.
//
// git bundle verify checks the file is a bundle whose prerequisites the
// bare repo has. git index-pack --strict then stores its objects in a
// quarantine, checking each as fsck does, and that every object they name
// is there. (A git fetch from a bundle checks no objects before git 2.48,
// whatever fetch.fsckObjects says.) The ref is written only when the HEAD
// is a commit that differs from start and has it in its history; otherwise
// the error says why, and no ref is written. The objects move into the
// bare repo only once those checks pass, so a refused bundle leaves
// nothing behind. Like every call here, these run with no hooks and no
// system or global config (gitEnv).
//
// A run whose ref exists already gets that ref's commit back, and the file
// isn't read, so fetching a run's bundle again writes nothing. A runID
// that wouldn't be a valid name for the run's dir in the workspace, or
// for a ref, is refused, and so is a start that isn't a full SHA.
func (r *Repo) FetchBundle(ctx context.Context, path, runID, start string) (string, error) {
	if runID == "" || strings.ContainsRune(runID, filepath.Separator) || strings.HasPrefix(runID, ".") {
		return "", fmt.Errorf("fetch bundle: invalid run ID %q", runID)
	}
	ref := RunsRef + runID
	if _, err := r.git(ctx, "", "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("fetch bundle: invalid run ID %q: %w", runID, err)
	}
	if !shaPattern.MatchString(start) {
		return "", fmt.Errorf("fetch bundle of run %s: invalid start SHA %q", runID, start)
	}
	out, err := r.git(ctx, "", "for-each-ref", "--format=%(objectname)", "--", ref)
	if err != nil {
		return "", fmt.Errorf("fetch bundle of run %s: read %s: %w", runID, ref, err)
	}
	if head := strings.TrimSpace(string(out)); head != "" {
		return head, nil
	}
	head, err := r.storeBundle(ctx, path, start)
	if err != nil {
		return "", fmt.Errorf("fetch bundle of run %s: %w", runID, err)
	}
	if r.beforeRunRef != nil {
		r.beforeRunRef()
	}
	// The empty old value means the ref mustn't exist yet, as another
	// fetch of the run may have written it since the read above. The
	// objects stay then: that fetch's ref may need the same pack.
	if _, err := r.git(ctx, "", "update-ref", ref, head, ""); err != nil {
		return "", fmt.Errorf("fetch bundle of run %s: write %s: %w", runID, ref, err)
	}
	return head, nil
}

// storeBundle verifies the bundle at path, checks its objects and its HEAD
// (checkHead) in a quarantine, then moves them into the bare repo, and
// returns the commit its HEAD names. A bundle that fails a check leaves
// nothing in the bare repo.
func (r *Repo) storeBundle(ctx context.Context, path, start string) (head string, err error) {
	// Not --quiet, which drops the list of missing prerequisites from the
	// error. What it prints of the bundle's refs goes to stdout, unused.
	if _, err := r.git(ctx, "", "bundle", "verify", "--", path); err != nil {
		return "", fmt.Errorf("verify bundle %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open bundle: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close bundle %s: %w", path, cerr)
		}
	}()
	br := bufio.NewReader(f)
	head, err = bundleHead(br)
	if err != nil {
		return "", fmt.Errorf("bundle %s: %w", path, err)
	}
	dir, q, err := r.quarantine()
	if err != nil {
		return "", fmt.Errorf("bundle %s: %w", path, err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove quarantine: %w", rerr))
		}
	}()
	// br is now at the pack. --fix-thin completes deltas against the
	// prerequisites' objects, as git's own unbundle does.
	if _, err := q.run(ctx, br, "", "index-pack", "--stdin", "--fix-thin", "--strict"); err != nil {
		return "", fmt.Errorf("store bundle %s: %w", path, err)
	}
	if err := q.checkHead(ctx, head, start); err != nil {
		return "", fmt.Errorf("bundle %s: %w", path, err)
	}
	if err := r.migrate(dir); err != nil {
		return "", fmt.Errorf("store bundle %s: %w", path, err)
	}
	return head, nil
}

// quarantine makes a dir in the bare repo's objects dir for new objects,
// as git's receive-pack does, and returns it and a Repo whose git calls
// store new objects there while reading the bare repo's too. Nothing else
// sees them until migrate moves them in.
func (r *Repo) quarantine() (string, *Repo, error) {
	objects, err := filepath.Abs(filepath.Join(r.dir, "objects"))
	if err != nil {
		return "", nil, fmt.Errorf("quarantine: %w", err)
	}
	dir, err := os.MkdirTemp(objects, "tmp_objdir-incoming-")
	if err != nil {
		return "", nil, fmt.Errorf("create quarantine: %w", err)
	}
	q := &Repo{remote: r.remote, dir: r.dir, env: []string{
		"GIT_OBJECT_DIRECTORY=" + dir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects,
	}}
	return dir, q, nil
}

// migrate moves the packs index-pack wrote in the quarantine dir into the
// bare repo's objects/pack, each .idx last, as git does, so git never sees
// an index whose pack isn't there yet. A pack's name is its checksum, so
// one already there by that name has the same bytes, and the rename keeps
// them.
func (r *Repo) migrate(dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, "pack"))
	if err != nil {
		return fmt.Errorf("read quarantined packs: %w", err)
	}
	var names, indexes []string
	for _, e := range entries {
		switch name := e.Name(); {
		case !strings.HasPrefix(name, "pack-"): // not a finished pack's file
		case strings.HasSuffix(name, ".idx"):
			indexes = append(indexes, name)
		default:
			names = append(names, name)
		}
	}
	for _, name := range append(names, indexes...) {
		if err := os.Rename(filepath.Join(dir, "pack", name), filepath.Join(r.dir, "objects", "pack", name)); err != nil {
			return fmt.Errorf("move %s into the bare repo: %w", name, err)
		}
	}
	return nil
}

// bundleHead reads a bundle's header from br, up to the blank line before
// its pack, and returns the object its one HEAD names. The header is the
// signature line, then capabilities (@...), prerequisites (-<oid> ...) and
// refs (<oid> <name>), one to a line.
func bundleHead(br *bufio.Reader) (string, error) {
	sig, err := br.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read header: %w", err)
	}
	if sig != "# v2 git bundle\n" && sig != "# v3 git bundle\n" {
		return "", fmt.Errorf("not a v2 or v3 bundle: %q", strings.TrimSpace(sig))
	}
	var heads []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read header: %w", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "-") {
			continue
		}
		if oid, name, _ := strings.Cut(line, " "); name == "HEAD" {
			heads = append(heads, oid)
		}
	}
	if len(heads) != 1 || !shaPattern.MatchString(heads[0]) {
		return "", fmt.Errorf("want one HEAD, a full SHA, in its refs; got %q", heads)
	}
	return heads[0], nil
}

// checkHead checks that head is a commit other than start, with start in
// its history.
func (r *Repo) checkHead(ctx context.Context, head, start string) error {
	if head == start {
		return fmt.Errorf("its HEAD is the start %s, so it holds no commits past it", start)
	}
	commit, err := r.Resolve(ctx, head)
	if err != nil {
		return fmt.Errorf("its HEAD: %w", err)
	}
	if commit != head {
		return fmt.Errorf("its HEAD %s isn't a commit", head)
	}
	if _, err := r.git(ctx, "", "merge-base", "--is-ancestor", start, head); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("its HEAD %s doesn't have the start %s in its history", head, start)
		}
		return fmt.Errorf("check that its HEAD %s descends from %s: %w", head, start, err)
	}
	return nil
}

// git runs one git command against the bare repo and returns its stdout,
// even when it fails, as a command that exits non-zero, such as
// merge-tree with conflicts, may still have written what its caller
// reads. The error includes git's stderr.
func (r *Repo) git(ctx context.Context, token string, args ...string) ([]byte, error) {
	return r.run(ctx, nil, token, args...)
}

// run is git, with stdin as the command's input; nil gives it none.
func (r *Repo) run(ctx context.Context, stdin io.Reader, token string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + r.dir}, args...)...)
	cmd.Env = append(gitEnv(os.Environ(), r.remote, token), r.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// gitEnv builds git's environment from environ (os.Environ format).
//   - It drops every inherited GIT_* variable and SSH_ASKPASS, so nothing
//     redirects git to another repo or config, or runs a password program.
//   - It ignores the system and global config, so no diff, filter or
//     credential settings apply.
//   - It passes config through GIT_CONFIG_*, so none lands in the repo's
//     config file.
//   - It sets http.followRedirects=false, so git sends the token only to the
//     URL it was given. Git's default follows the first request's redirect,
//     then sends http.extraHeader to wherever that pointed. It sets it for
//     remote too, if given: a per-URL key in the repo's config would beat the
//     generic one, while on an exact match the env's later entry wins. A
//     repo-config key more specific than remote (a trailing "//" on the path,
//     or a trailing dot on the host) still beats it, and so does a
//     url.*.insteadOf rewrite. Only the daemon can write that config.
func gitEnv(environ []string, remote, token string) []string {
	env := make([]string, 0, len(environ)+10)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "SSH_ASKPASS=") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", // empty, so git doesn't fall back to core.askPass or SSH_ASKPASS
		"LC_ALL=C",     // ReadFile and Sync match git's English error text.
	)
	config := [][2]string{{"core.hooksPath", "/dev/null"}, {"http.followRedirects", "false"}}
	if remote != "" {
		config = append(config, [2]string{"http." + remote + ".followRedirects", "false"})
	}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		config = append(config, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	return env
}

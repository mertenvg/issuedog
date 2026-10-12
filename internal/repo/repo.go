// Package repo keeps the daemon's bare clone of the target repo and reads
// trusted files from its main branch (SPEC §3.2, §3.4).
package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNotFound is returned by ReadFile when main has no file at the path.
var ErrNotFound = errors.New("not found")

// refspec mirrors every remote branch, main included, into refs/heads.
// Together with --prune, the bare repo's branches match the remote's after
// each Sync. Refs outside refs/heads, such as refs/issuedog/runs/*, are left
// alone.
const refspec = "+refs/heads/*:refs/heads/*"

// Repo is a daemon-owned bare clone.
type Repo struct {
	remote string
	dir    string
}

// New returns the bare clone at dir (for example /work/<repo>.git) of the
// remote URL. The URL must not carry credentials; Sync passes the token.
func New(remote, dir string) *Repo {
	return &Repo{remote: remote, dir: dir}
}

// Sync creates the bare repo if it's missing, then fetches every branch from
// the remote. A first sync that fails part way leaves a repo the next Sync
// fetches into. The token is sent as an HTTP header and is never written to
// disk or passed in argv.
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
		return fmt.Errorf("fetch %s: %w", r.remote, err)
	}
	return nil
}

// ReadFile returns the contents of path on main. It reads the blob with
// cat-file, so no textconv or other driver runs, and a directory is an
// error. A path main doesn't have gives an error matching ErrNotFound.
func (r *Repo) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("read main: empty path")
	}
	out, err := r.git(ctx, "", "cat-file", "blob", "main:"+path)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist in 'main'") {
			return nil, fmt.Errorf("read main:%s: %w", path, ErrNotFound)
		}
		return nil, fmt.Errorf("read main:%s: %w", path, err)
	}
	return out, nil
}

// git runs one git command against the bare repo and returns its stdout.
// The error includes git's stderr.
func (r *Repo) git(ctx context.Context, token string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + r.dir}, args...)...)
	cmd.Env = gitEnv(os.Environ(), token)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
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
func gitEnv(environ []string, token string) []string {
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
		"LC_ALL=C",     // ReadFile matches git's English error text.
	)
	config := [][2]string{{"core.hooksPath", "/dev/null"}}
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

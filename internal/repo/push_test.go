package repo

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// pushBranch is the task branch the push tests write.
const pushBranch = "issuedog/7-push-a-task"

// pushRig is a synced bare repo and its remote, standing in for GitHub,
// with a task's head in the bare repo that the remote doesn't have, taken
// in from a run's bundle as a run's commits are (SPEC §3.2).
type pushRig struct {
	rem  *remote
	r    *Repo
	main string // main's tip, on both sides
	head string // the task's head, one commit past main, in the bare repo only
}

func newPushRig(t *testing.T) *pushRig {
	t.Helper()
	rem, r := synced(t)
	p := &pushRig{rem: rem, r: r, main: run(t, rem.work, "rev-parse", "HEAD")}
	run(t, rem.work, "checkout", "--quiet", "--detach")
	p.head = rem.commitOnly("task.txt", "the task\n")
	if _, err := r.FetchBundle(context.Background(), rem.bundle(p.main+"..HEAD"), "r1", p.main); err != nil {
		t.Fatalf("FetchBundle: %v", err)
	}
	run(t, rem.work, "checkout", "--quiet", p.main)
	return p
}

// other commits name on main in the remote's work tree, without pushing it,
// as someone else's commit, so its history doesn't hold the task's head.
func (p *pushRig) other(name string) string {
	p.rem.t.Helper()
	run(p.rem.t, p.rem.work, "checkout", "--quiet", "--detach", p.main)
	return p.rem.commitOnly(name, name+"\n")
}

// setRemote points branch at sha on the remote, as a push someone else
// made, or deletes it when sha is "".
func (p *pushRig) setRemote(branch, sha string) {
	p.rem.t.Helper()
	if sha == "" {
		run(p.rem.t, p.rem.work, "push", "--quiet", p.rem.url, "--delete", "refs/heads/"+branch)
		return
	}
	run(p.rem.t, p.rem.work, "push", "--quiet", "--force", p.rem.url, sha+":refs/heads/"+branch)
}

// sync syncs the bare repo, as each pass does.
func (p *pushRig) sync() {
	p.rem.t.Helper()
	if err := p.r.Sync(context.Background(), fakeToken); err != nil {
		p.rem.t.Fatalf("Sync: %v", err)
	}
}

// remoteRefs lists the remote's refs, with the objects they name.
func (p *pushRig) remoteRefs() string {
	p.rem.t.Helper()
	return run(p.rem.t, p.rem.url, "for-each-ref", "--format=%(objectname) %(refname)")
}

// tips returns branch's tip on the remote and in the bare repo, each ""
// when it has no such branch.
func (p *pushRig) tips(branch string) (remote, bare string) {
	p.rem.t.Helper()
	tip := func(dir string) string {
		return run(p.rem.t, dir, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	}
	return tip(p.rem.url), tip(p.r.dir)
}

// TestPush pushes the task's head to its branch: a create, a fast-forward
// from the tip the lease names, a forced push over the tip the lease names,
// and a push of the tip the branch has already. Each time the remote's
// branch and the bare repo's are at the head after.
func TestPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// before sets the branch on the remote, and syncs, and returns the
		// lease.
		before func(p *pushRig) string
	}{
		{name: "create", before: func(*pushRig) string { return "" }},
		{name: "fast-forward", before: func(p *pushRig) string {
			p.setRemote(pushBranch, p.main)
			p.sync()
			return p.main
		}},
		{name: "forced over the lease's tip", before: func(p *pushRig) string {
			dropped := p.other("dropped.txt")
			p.setRemote(pushBranch, dropped)
			p.sync()
			return dropped
		}},
		{name: "at the head already, leased at none", before: func(p *pushRig) string {
			if err := p.r.Push(context.Background(), fakeToken, p.head, pushBranch, ""); err != nil {
				p.rem.t.Fatalf("the first Push: %v", err)
			}
			return ""
		}},
		{name: "at the head already, leased at another tip", before: func(p *pushRig) string {
			if err := p.r.Push(context.Background(), fakeToken, p.head, pushBranch, ""); err != nil {
				p.rem.t.Fatalf("the first Push: %v", err)
			}
			return p.main
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newPushRig(t)
			ctx := context.Background()
			old := tt.before(p)
			if err := p.r.Push(ctx, fakeToken, p.head, pushBranch, old); err != nil {
				t.Fatalf("Push(%s, leased at %q): %v", pushBranch, old, err)
			}
			if remote, bare := p.tips(pushBranch); remote != p.head || bare != p.head {
				t.Errorf("after the push the branch is at %q on the remote and %q in the bare repo, want both at the head %s", remote, bare, p.head)
			}
			if remote, _ := p.tips("main"); remote != p.main {
				t.Errorf("main on the remote = %s, want it left at %s", remote, p.main)
			}
			got, err := p.r.Branches(ctx, "issuedog/7-")
			if want := map[string]string{pushBranch: p.head}; err != nil || !maps.Equal(got, want) {
				t.Errorf("Branches after the push = %v, %v; want %v, before any sync", got, err, want)
			}
			checkNoToken(t, p.r.dir)
		})
	}
}

// TestPushStaleLease pushes with a lease the remote's branch no longer
// matches: one that names no branch where there's one, one whose branch
// moved since the sync, and one whose branch was deleted since. Each fails
// with ErrPushStale, naming the branch, and changes nothing on the remote
// or in the bare repo.
func TestPushStaleLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		before func(p *pushRig) string // returns the lease
	}{
		{name: "a branch where the lease names none", before: func(p *pushRig) string {
			p.setRemote(pushBranch, p.main)
			return ""
		}},
		{name: "a branch moved since the sync", before: func(p *pushRig) string {
			p.setRemote(pushBranch, p.main)
			p.sync()
			p.setRemote(pushBranch, p.other("someone.txt"))
			return p.main
		}},
		{name: "a branch deleted since the sync", before: func(p *pushRig) string {
			p.setRemote(pushBranch, p.main)
			p.sync()
			p.setRemote(pushBranch, "")
			return p.main
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newPushRig(t)
			old := tt.before(p)
			remote, bare := p.remoteRefs(), refs(t, p.r)
			err := p.r.Push(context.Background(), fakeToken, p.head, pushBranch, old)
			if !errors.Is(err, ErrPushStale) {
				t.Fatalf("Push = %v, want an error matching ErrPushStale", err)
			}
			if !strings.Contains(err.Error(), pushBranch) {
				t.Errorf("Push = %v, want it to name %s", err, pushBranch)
			}
			if got := p.remoteRefs(); got != remote {
				t.Errorf("the remote's refs after the stale push =\n%s\nwant\n%s", got, remote)
			}
			if got := refs(t, p.r); got != bare {
				t.Errorf("the bare repo's refs after the stale push =\n%s\nwant\n%s", got, bare)
			}
		})
	}
}

// TestPushRefuses refuses a SHA that isn't full, or is a commit the bare
// repo doesn't have, a lease that isn't a full SHA, and a branch outside
// issuedog/ or one that isn't a valid name. Nothing changes on the remote
// or in the bare repo.
func TestPushRefuses(t *testing.T) {
	t.Parallel()
	p := newPushRig(t)
	ctx := context.Background()
	missing := p.other("never-fetched.txt")
	remote, bare := p.remoteRefs(), refs(t, p.r)
	type push struct{ sha, branch, old, want string } // want is in the error
	var pushes []push
	for _, sha := range []string{"", "main", "HEAD", p.head[:12], strings.ToUpper(p.head)} {
		pushes = append(pushes, push{sha: sha, branch: pushBranch, want: "invalid SHA"})
	}
	pushes = append(pushes, push{sha: missing, branch: pushBranch, want: "the bare repo has no such commit"})
	for _, branch := range []string{"", "main", "feature/7-x", "issuedog", "issuedog/", "issuedogs/7-x", "refs/heads/" + pushBranch} {
		pushes = append(pushes, push{sha: p.head, branch: branch, want: "isn't under issuedog/"})
	}
	for _, branch := range []string{"issuedog/7-a..b", "issuedog/7-x.lock", "issuedog/7-x~1", "issuedog/7 x", "issuedog/7-x/", "issuedog//7-x"} {
		pushes = append(pushes, push{sha: p.head, branch: branch, want: "invalid branch"})
	}
	for _, old := range []string{"main", p.main[:12], strings.ToUpper(p.main)} {
		pushes = append(pushes, push{sha: p.head, branch: pushBranch, old: old, want: "the lease: invalid SHA"})
	}
	for _, tt := range pushes {
		err := p.r.Push(ctx, fakeToken, tt.sha, tt.branch, tt.old)
		if err == nil || errors.Is(err, ErrPushStale) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Push(%q, %q, leased at %q) = %v, want an error saying %q", tt.sha, tt.branch, tt.old, err, tt.want)
		}
	}
	if got := p.remoteRefs(); got != remote {
		t.Errorf("the remote's refs after the refusals =\n%s\nwant\n%s", got, remote)
	}
	if got := refs(t, p.r); got != bare {
		t.Errorf("the bare repo's refs after the refusals =\n%s\nwant\n%s", got, bare)
	}
}

// TestPushRunsNoHooks plants hooks in the bare repo, and a config include
// that points core.hooksPath at more of them, each of which would leave a
// mark. None runs on a push, nor on the update of the bare repo's own branch
// after it.
func TestPushRunsNoHooks(t *testing.T) {
	t.Parallel()
	p := newPushRig(t)
	marks, planted := t.TempDir(), t.TempDir()
	hook := func(dir, name string) {
		script := "#!/bin/sh\ntouch '" + filepath.Join(marks, filepath.Base(dir)+"-"+name) + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"pre-push", "reference-transaction", "post-update"} {
		hook(filepath.Join(p.r.dir, "hooks"), name)
		hook(planted, name)
	}
	included := filepath.Join(t.TempDir(), "included.config")
	if err := os.WriteFile(included, []byte("[core]\n\thooksPath = "+planted+"\n[issuedog]\n\tprobe = included\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, p.r.dir, "config", "includeIf.gitdir:"+p.r.dir+".path", included)
	// Read as Push runs git, with --git-dir, so the include applies to its
	// commands, and only gitEnv's own core.hooksPath stands in its way.
	if got, err := p.r.git(context.Background(), "", "config", "issuedog.probe"); err != nil || string(got) != "included\n" {
		t.Fatalf("the include doesn't apply to the bare repo's commands, so the test shows nothing: issuedog.probe = %q, %v", got, err)
	}

	if err := p.r.Push(context.Background(), fakeToken, p.head, pushBranch, ""); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if remote, bare := p.tips(pushBranch); remote != p.head || bare != p.head {
		t.Errorf("after the push the branch is at %q on the remote and %q in the bare repo, want both at %s", remote, bare, p.head)
	}
	entries, err := os.ReadDir(marks)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the hook %s ran", e.Name())
	}
}

// TestPushSendsTokenAsHeader pushes to a remote served over HTTP by git's
// own http-backend. Every request, the push's included, carries the token
// in the Authorization header Sync sends, and the push lands. No file in the
// bare repo holds the token after.
func TestPushSendsTokenAsHeader(t *testing.T) {
	t.Parallel()
	rem := newRemote(t)
	run(t, rem.url, "config", "http.receivepack", "true")
	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeToken))
	backend := filepath.Join(run(t, rem.work, "--exec-path"), "git-http-backend")
	var mu sync.Mutex
	var asked []string // each request, as "METHOD /path?query", with its Authorization
	cgiHandler := &cgi.Handler{Path: backend, Root: "/git", Env: []string{
		"GIT_PROJECT_ROOT=" + filepath.Dir(rem.url), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.RequestURI()+" Authorization="+r.Header.Get("Authorization"))
		mu.Unlock()
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	r := New(server.URL+"/git/"+filepath.Base(rem.url), filepath.Join(t.TempDir(), "target.git"))
	ctx := context.Background()
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	main := run(t, rem.work, "rev-parse", "HEAD")
	head := rem.commitOnly("task.txt", "the task\n")
	if _, err := r.FetchBundle(ctx, rem.bundle(main+"..HEAD"), "r1", main); err != nil {
		t.Fatalf("FetchBundle: %v", err)
	}
	mu.Lock()
	asked = nil
	mu.Unlock()
	if err := r.Push(ctx, fakeToken, head, pushBranch, ""); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := run(t, rem.url, "for-each-ref", "--format=%(objectname)", "refs/heads/"+pushBranch); got != head {
		t.Errorf("the remote's %s = %q, want the head %s", pushBranch, got, head)
	}
	mu.Lock()
	defer mu.Unlock()
	receivePack := 0
	for _, a := range asked {
		if !strings.HasSuffix(a, " Authorization="+header) {
			t.Errorf("the push sent %q, without the token's header", a)
		}
		if strings.HasPrefix(a, "POST /git/remote.git/git-receive-pack ") {
			receivePack++
		}
	}
	if receivePack != 1 {
		t.Errorf("the push made %d receive-pack requests, want 1: %q", receivePack, asked)
	}
	checkNoToken(t, r.dir)
}

// TestPushTokenOnlyInGitEnv runs git through a wrapper on PATH that logs
// each command's argv and environment. The push's argv doesn't hold the
// token, its environment holds it only as gitEnv's Authorization header,
// and no other git command Push runs gets it at all. It sets PATH, so it
// can't run in parallel.
func TestPushTokenOnlyInGitEnv(t *testing.T) {
	p := newPushRig(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "git.log")
	wrapper := "#!/bin/sh\n{ printf 'argv'; printf ' %s' \"$@\"; printf '\\n'; env; printf '%s\\n' '-- end --'; } >> '" + log + "'\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := p.r.Push(context.Background(), fakeToken, p.head, pushBranch, ""); err != nil {
		t.Fatalf("Push: %v", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeToken))
	secrets := []string{fakeToken, basic}
	var pushes []string
	for _, call := range strings.Split(strings.TrimSuffix(string(b), "-- end --\n"), "-- end --\n") {
		argv, env, _ := strings.Cut(call, "\n")
		for _, s := range secrets {
			if strings.Contains(argv, s) {
				t.Errorf("%s holds the token", argv)
			}
		}
		if !strings.HasPrefix(argv, "argv --git-dir="+p.r.dir+" push ") {
			if slices.ContainsFunc(secrets, func(s string) bool { return strings.Contains(env, s) }) {
				t.Errorf("%s got the token in its environment", argv)
			}
			continue
		}
		pushes = append(pushes, argv)
		var holding []string
		for line := range strings.Lines(env) {
			if slices.ContainsFunc(secrets, func(s string) bool { return strings.Contains(line, s) }) {
				holding = append(holding, strings.TrimSuffix(line, "\n"))
			}
		}
		if len(holding) != 1 || !strings.HasPrefix(holding[0], "GIT_CONFIG_VALUE_") ||
			!strings.HasSuffix(holding[0], "=Authorization: Basic "+basic) {
			t.Errorf("the push's environment holds the token in %q, want it only as gitEnv's Authorization header", holding)
		}
	}
	if len(pushes) != 1 {
		t.Errorf("Push ran %d git pushes, want 1: %q", len(pushes), pushes)
	}
}

// TestBranches lists the bare repo's branches with a prefix, as the last
// sync left them, with the remote gone, so at no request. A prefix
// matches the start of a name, across slashes, and nothing else.
func TestBranches(t *testing.T) {
	t.Parallel()
	p := newPushRig(t)
	other := p.other("other.txt")
	for _, b := range []string{"issuedog/1-a", "issuedog/1-c/d", "issuedog/12-b", "issuedog-1-x", "other/issuedog/1-x"} {
		p.setRemote(b, other)
	}
	p.sync()
	if err := os.RemoveAll(p.rem.url); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		prefix string
		want   []string // with other as the tip; main has p.main
	}{
		{prefix: "issuedog/1-", want: []string{"issuedog/1-a", "issuedog/1-c/d"}},
		{prefix: "issuedog/12-", want: []string{"issuedog/12-b"}},
		{prefix: "issuedog/", want: []string{"issuedog/1-a", "issuedog/1-c/d", "issuedog/12-b"}},
		{prefix: "issuedog", want: []string{"issuedog/1-a", "issuedog/1-c/d", "issuedog/12-b", "issuedog-1-x"}},
		{prefix: "", want: []string{"issuedog/1-a", "issuedog/1-c/d", "issuedog/12-b", "issuedog-1-x", "other/issuedog/1-x", "main"}},
		{prefix: "issuedog/2-"},
		{prefix: "nope/"},
	}
	for _, tt := range tests {
		got, err := p.r.Branches(context.Background(), tt.prefix)
		want := map[string]string{}
		for _, b := range tt.want {
			want[b] = other
		}
		if _, ok := want["main"]; ok {
			want["main"] = p.main
		}
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("Branches(%q) = %v, %v; want %v", tt.prefix, got, err, want)
		}
	}
}

// TestIsAncestor reads whether one commit is in another's history, itself
// included, and refuses what isn't a full SHA of a commit the bare repo
// has.
func TestIsAncestor(t *testing.T) {
	t.Parallel()
	p := newPushRig(t)
	ctx := context.Background()
	other := p.other("other.txt")
	p.setRemote("issuedog/1-other", other)
	p.sync()
	tests := []struct {
		a, b string
		want bool
	}{
		{a: p.main, b: p.head, want: true},
		{a: p.head, b: p.head, want: true},
		{a: p.head, b: p.main},
		{a: other, b: p.head},
		{a: p.head, b: other},
	}
	for _, tt := range tests {
		if got, err := p.r.IsAncestor(ctx, tt.a, tt.b); err != nil || got != tt.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v", tt.a, tt.b, got, err, tt.want)
		}
	}
	missing := p.other("never-fetched.txt")
	for _, pair := range [][2]string{{"main", p.head}, {p.main, "HEAD"}, {p.main[:12], p.head}, {missing, p.head}, {p.main, missing}} {
		if got, err := p.r.IsAncestor(ctx, pair[0], pair[1]); err == nil {
			t.Errorf("IsAncestor(%s, %s) = %v; want an error", pair[0], pair[1], got)
		}
	}
}

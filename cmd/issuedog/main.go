// Command issuedog turns GitHub issues into reviewed pull requests.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/appkey"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/egress"
	"github.com/mertenvg/issuedog/internal/firewall"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/trust"
	"github.com/mertenvg/issuedog/internal/verify"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	stop := setupLogging(os.Stdout, os.Stderr)
	code := run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout)
	logr.Wait()
	stop()
	os.Exit(code)
}

// setupLogging sends every log message to stdout, and errors and panics to
// stderr as well. The returned func removes both writers.
func setupLogging(stdout, stderr io.Writer) (stop func()) {
	stopOut := logr.AddWriter(stdout, logr.WithFilter(logr.All))
	stopErr := logr.AddWriter(stderr, logr.WithFilter(logr.Critical))
	return func() {
		stopErr()
		stopOut()
	}
}

// run is the whole program. It writes through logr, which main's setupLogging
// sends to stdout and stderr, except for the version and help text, which are
// the program's own output rather than logs and go to stdout as plain text.
func run(args, environ []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("issuedog", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage goes through logr
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return printOut(stdout, usage+"\n")
	} else if err != nil {
		logr.Errorf("issuedog: %v\n%s", err, usage)
		return 2
	}
	if *showVersion {
		return printOut(stdout, "issuedog "+version+"\n")
	}
	// Commands don't need a Claude credential, so they run before its check.
	if fs.NArg() > 0 {
		return command(fs.Args(), environ, stdin, stdout)
	}
	cred, err := agent.Credential(environ)
	if err != nil {
		logr.Errorf("checking the Claude credential: %v", err)
		return 1
	}
	set, err := readSettings(environ)
	if err != nil {
		logr.Errorf("reading the settings: %v", err)
		return 1
	}
	// A missing or loose key stops startup before the firewall (SPEC §3.1).
	key, err := appkey.Load(appKeyPath)
	if err != nil {
		logr.Errorf("loading the App key: %v", err)
		return 1
	}
	logr.Infof("loaded the App key from %s", appKeyPath)
	ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d, err := startDaemon(ctx, set, key)
	if err != nil && ctx.Err() != nil {
		logr.Infof("stopping on a signal during startup: %v", err)
		return 0
	}
	if err != nil {
		logr.Errorf("starting the daemon: %v", err)
		return 1
	}
	// Like the steps after it, up to the loop, the ruleset check doesn't
	// watch for a signal (SPEC §3.5): its read isn't canceled by one, and a
	// check that fails exits 1 even once a signal has come in.
	if err := checkBranchRules(context.WithoutCancel(ctx), d.Step.Client, set.owner, set.name); err != nil {
		logr.Errorf("checking the issuedog branches ruleset: %v", err)
		return 1
	}
	runs, err := stageWork(d, cred)
	if err != nil {
		logr.Errorf("setting up stage runs: %v", err)
		return 1
	}
	d.Work = runs.Work
	// network.allow is read once, so a change to it takes effect on restart.
	proxy, err := startEgress(d.Config.Network.Allow)
	if err != nil {
		logr.Errorf("starting egress: %v", err)
		return 1
	}
	defer func() {
		if err := proxy.Close(); err != nil {
			logr.Errorf("stopping the egress proxy: %v", err)
		}
	}()
	d.Run(ctx)
	logr.Info("stopping on a signal")
	return 0
}

// settings are the daemon's own, from the environment (SPEC §3.5).
type settings struct {
	owner, name           string
	appID, installationID int64
}

// readSettings reads ISSUEDOG_REPO (owner/name), ISSUEDOG_APP_ID and
// ISSUEDOG_INSTALLATION_ID from environ.
func readSettings(environ []string) (settings, error) {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	var set settings
	var ok bool
	set.owner, set.name, ok = strings.Cut(env["ISSUEDOG_REPO"], "/")
	if !ok || set.owner == "" || set.name == "" || strings.Contains(set.name, "/") {
		return settings{}, fmt.Errorf("ISSUEDOG_REPO is %q, not owner/name", env["ISSUEDOG_REPO"])
	}
	for _, id := range []struct {
		name string
		to   *int64
	}{{"ISSUEDOG_APP_ID", &set.appID}, {"ISSUEDOG_INSTALLATION_ID", &set.installationID}} {
		n, err := strconv.ParseInt(env[id.name], 10, 64)
		if err != nil || n <= 0 {
			return settings{}, fmt.Errorf("%s is %q, not a positive number", id.name, env[id.name])
		}
		*id.to = n
	}
	return set, nil
}

// connect runs the startup steps the daemon and adopt share, after the
// key check: the client, the App's slug, a token, the bare clone, synced,
// and trust from main (SPEC §3.5). It returns the client, the slug, the
// clone and the trust.
func connect(ctx context.Context, set settings, key []byte) (*github.Client, string, *repo.Repo, *trust.Set, error) {
	client, err := github.New(github.Config{AppID: set.appID, InstallationID: set.installationID, Key: key, BaseURL: apiURL})
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("creating the GitHub client: %w", err)
	}
	slug, err := client.AppSlug(ctx)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("reading the App's slug: %w", err)
	}
	token, err := client.Token(ctx)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("minting a token: %w", err)
	}
	clone := repo.New(gitRemote(set.owner, set.name), filepath.Join(workDir, set.name+".git"))
	if err := clone.Sync(ctx, token); err != nil {
		return nil, "", nil, nil, fmt.Errorf("syncing the clone: %w", err)
	}
	tr, err := trust.Load(ctx, clone, client.Client, set.owner, set.name)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("loading trust from main: %w", err)
	}
	return client, slug, clone, tr, nil
}

// startDaemon runs the startup steps after the key check: connect's, then
// config from main, and the labels. It returns the daemon, as daemon.New
// builds it, which the whole-run tests in internal/daemon build too, ready
// to run, keeping the digest: its loop creates the digest, not this
// startup (SPEC §3.5).
func startDaemon(ctx context.Context, set settings, key []byte) (*daemon.Daemon, error) {
	client, slug, clone, tr, err := connect(ctx, set, key)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	cfg, err := config.Load(ctx, clone)
	if err != nil {
		return nil, fmt.Errorf("loading the config from main: %w", err)
	}
	created, err := github.EnsureLabels(ctx, client, set.owner, set.name, daemon.LabelOptions(cfg))
	if err != nil {
		return nil, fmt.Errorf("creating the labels: %w", err)
	}
	if len(created) > 0 {
		logr.Infof("created the labels %s", strings.Join(created, ", "))
	}
	logr.Infof("running as %s[bot] on %s/%s", slug, set.owner, set.name)
	a := &daemon.Applier{Client: client, Owner: set.owner, Repo: set.name, AppSlug: slug}
	return daemon.New(a, clone, cfg, tr), nil
}

// branchRulesBranch is the branch name whose rules the startup check reads
// (SPEC §3.5). It's under issuedog/, so the issuedog branches ruleset's
// issuedog/* pattern covers it, and no story or task branch uses it, as
// theirs start issuedog/<n>-. GitHub matches the pattern against the name,
// so the branch needn't exist.
const branchRulesBranch = "issuedog/ruleset-check"

// branchRulesNeeded are the rules story and task branches rely on the
// issuedog branches ruleset for (SPEC §15, step 4): restrict creations,
// restrict deletions and block force pushes, by their rule types.
var branchRulesNeeded = []string{"creation", "deletion", "non_fast_forward"}

// checkBranchRules checks that the active rules on branchRulesBranch,
// org rulesets' included, have every rule in branchRulesNeeded, and
// returns an error naming each one missing, or the read's error. The App
// can't read a ruleset's bypass list, so the check can't confirm it (SPEC
// §16).
func checkBranchRules(ctx context.Context, client *github.Client, owner, name string) error {
	types, err := client.BranchRuleTypes(ctx, owner, name, branchRulesBranch)
	if err != nil {
		return fmt.Errorf("reading the rules on %s: %w", branchRulesBranch, err)
	}
	var missing []string
	for _, rule := range branchRulesNeeded {
		if !slices.Contains(types, rule) {
			missing = append(missing, rule)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the active rules on %s lack %s, which story and task branches rely on (SPEC §15, step 4)",
			branchRulesBranch, strings.Join(missing, ", "))
	}
	return nil
}

// registerStages returns the Stages to register on d, by stage, and the
// Unlabeled stage: daemon.RegisterStages, which the whole-run tests in
// internal/daemon register too. Tests replace it.
var registerStages = daemon.RegisterStages

// stageWork is the stage-run harness for d's stage work, with the Stages
// registerStages gives. Runs.CheckRegistered fails startup if one is at
// intake beside Unlabeled. Runs, their clones in /work/runs and claude,
// are agent's, and cred is the only credential in the agent's environment.
// The clones fetch from d.Clone, the bare repo the daemon syncs and reads
// main from, so the two can't differ. usage_limit.pattern is read once, so
// a change to it takes effect on restart.
func stageWork(d *daemon.Daemon, cred agent.Cred) (*daemon.Runs, error) {
	uid, err := agentCredential(agentUser)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", agentUser, err)
	}
	gate, err := agent.NewGate(d.Config.UsageLimit.Pattern)
	if err != nil {
		return nil, fmt.Errorf("the usage-limit gate: %w", err)
	}
	stages, unlabeled := registerStages(d)
	proxy := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	ws := &workspace.Workspace{Bare: d.Clone.Dir(), RunsDir: filepath.Join(workDir, "runs"), ContextDir: runDir, Credential: uid}
	runs := &daemon.Runs{
		Daemon:    d,
		Stages:    stages,
		Unlabeled: unlabeled,
		Runner:    &agent.Runner{Claude: claudePath, StateDir: stateDir, Credential: uid},
		Workspace: ws,
		Gate:      gate,
		Env:       agent.Env(cred, proxy, agentHome),
		// The checks' commands run in the runs' workspace, as agent, through
		// the egress proxy, with no credential, under verify.command_timeout
		// as main's config gives it when each command starts.
		Commands: &verify.Commands{Workspace: ws, Credential: uid, Proxy: proxy,
			Timeout: func() time.Duration { return d.Config.Verify.CommandTimeout }},
	}
	if err := runs.CheckRegistered(); err != nil {
		return nil, fmt.Errorf("registering the stages: %w", err)
	}
	return runs, nil
}

// printOut writes the program's own plain output, the version or help, and
// returns the exit code: 0, or 1 if stdout can't take it.
func printOut(stdout io.Writer, text string) int {
	if _, err := io.WriteString(stdout, text); err != nil {
		logr.Errorf("writing to stdout: %v", err)
		return 1
	}
	return 0
}

const usage = `usage:
  issuedog [-version]
  issuedog key import [-path file] < app-key.pem
  issuedog key check [-path file]     the file defaults to ` + appkey.DefaultPath + `
  issuedog adopt -login <login> [-n]`

// command runs "key import" (store the App key from stdin), "key check" or
// "adopt", with the daemon's settings from environ. The unlisted "egress"
// serves the firewall and proxy, the unlisted "as-agent" runs one command
// as agent, and the unlisted "handoff" hands a clone's commits to a bare
// repo, all for sandbox/test.sh.
func command(args, environ []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) > 0 && args[0] == "adopt" {
		return runAdopt(args[1:], environ, stdout)
	}
	if len(args) == 1 && args[0] == "egress" {
		return serveEgress()
	}
	if len(args) > 0 && args[0] == "as-agent" {
		return runAsAgent(args[1:], stdin, stdout)
	}
	if len(args) > 0 && args[0] == "handoff" {
		return runHandoff(args[1:])
	}
	if len(args) < 2 || args[0] != "key" || (args[1] != "import" && args[1] != "check") {
		logr.Error(usage)
		return 2
	}
	fs := flag.NewFlagSet("issuedog key "+args[1], flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage goes through logr
	path := fs.String("path", appkey.DefaultPath, "the key file")
	err := fs.Parse(args[2:])
	if errors.Is(err, flag.ErrHelp) {
		return printOut(stdout, usage+"\n")
	}
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if err != nil {
		logr.Errorf("issuedog key %s: %v\n%s", args[1], err, usage)
		return 2
	}
	if args[1] == "import" {
		if err := appkey.Import(*path, stdin); err != nil {
			logr.Errorf("importing the App key: %v", err)
			return 1
		}
		logr.Infof("imported the App key to %s", *path)
		return 0
	}
	if _, err := appkey.Load(*path); err != nil {
		logr.Errorf("checking the App key: %v", err)
		return 1
	}
	logr.Infof("the App key at %s is usable", *path)
	return 0
}

// runAdopt runs "adopt": it reposts as the App, once, the markers the
// trusted user -login wrote on each open pipeline issue while the pipeline
// ran by hand, so a daemon started after it reads them (SPEC §15, going
// live), and with -n, lists what it would post and posts nothing. It reads
// the daemon's settings and the App key, then runs connect's steps, but
// needs no Claude credential, never runs the loop and never posts a check
// run. The exit code is 0 once it's done, 1 on a failed step, or a login or
// an issue it refuses (daemon.Applier.Adopt), and 2 on bad usage. -h prints
// the usage on stdout.
func runAdopt(args, environ []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("issuedog adopt", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage goes through logr
	login := fs.String("login", "", "the trusted user whose markers to repost")
	dryRun := fs.Bool("n", false, "list what it would post, and post nothing")
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return printOut(stdout, usage+"\n")
	}
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if err == nil && *login == "" {
		err = errors.New("-login is required")
	}
	if err != nil {
		logr.Errorf("issuedog adopt: %v\n%s", err, usage)
		return 2
	}
	set, err := readSettings(environ)
	if err != nil {
		logr.Errorf("reading the settings: %v", err)
		return 1
	}
	key, err := appkey.Load(appKeyPath)
	if err != nil {
		logr.Errorf("loading the App key: %v", err)
		return 1
	}
	// A signal gets the default handling, as a crash: a rerun posts the
	// rest once, in order.
	ctx := context.Background()
	client, slug, _, tr, err := connect(ctx, set, key)
	if err != nil {
		logr.Errorf("connecting: %v", err)
		return 1
	}
	a := &daemon.Applier{Client: client, Owner: set.owner, Repo: set.name, AppSlug: slug}
	report, err := a.Adopt(ctx, tr, *login, *dryRun)
	if err != nil {
		logr.Errorf("adopting: %v", err)
		return 1
	}
	if len(report.Refused) > 0 {
		return 1
	}
	return 0
}

// The agent user, and the port on 127.0.0.1 where the egress proxy listens.
// The firewall lets agent connect to that port and nothing else, so stage
// runs pass http://127.0.0.1:3128 to agent.Env as the proxy.
const (
	agentUser = "agent"
	agentHome = "/home/agent"
	proxyPort = 3128
)

// The App key's path, the startup steps that need privileges or a fixed
// port, where GitHub and the clone are, and how signals stop the daemon.
// Tests replace them.
var (
	appKeyPath    = appkey.DefaultPath
	applyFirewall = firewall.Apply
	listenProxy   = egress.Listen
	apiURL        = "" // api.github.com
	gitRemote     = func(owner, name string) string { return "https://github.com/" + owner + "/" + name + ".git" }
	workDir       = "/work"
	stateDir      = "/state"        // the lease and run results (SPEC §8.2)
	runDir        = "/run/issuedog" // each run's context files
	claudePath    = "claude"
	notifyContext = signal.NotifyContext
)

// startEgress applies the agent firewall, then serves the egress proxy,
// which allows the hosts in allow (network.allow, SPEC §13), until the
// returned server is closed.
func startEgress(allow []string) (*http.Server, error) {
	if err := applyFirewall(agentUser); err != nil {
		return nil, fmt.Errorf("applying the agent firewall: %w", err)
	}
	l, err := listenProxy(proxyPort)
	if err != nil {
		return nil, fmt.Errorf("listening for the egress proxy: %w", err)
	}
	// Tunnels are hijacked, so the header timeout only limits the request.
	srv := &http.Server{Handler: egress.New(allow), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logr.Errorf("serving the egress proxy: %v", err)
		}
	}()
	logr.Infof("egress proxy listening on http://%s, allowing %s", l.Addr(), strings.Join(allow, ", "))
	return srv, nil
}

// serveEgress runs startEgress, with the default network.allow, until SIGINT
// or SIGTERM. It lets sandbox/test.sh check egress without GitHub.
func serveEgress() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxy, err := startEgress(config.Defaults().Network.Allow)
	if err != nil {
		logr.Errorf("starting egress: %v", err)
		return 1
	}
	<-ctx.Done()
	if err := proxy.Close(); err != nil {
		logr.Errorf("stopping the egress proxy: %v", err)
		return 1
	}
	return 0
}

// agentCredential looks up agent's uid and gid. Tests replace it.
var agentCredential = agent.UserCredential

// agentEnv is the whole environment of an as-agent command.
var agentEnv = []string{"HOME=" + agentHome, "PATH=/usr/local/bin:/usr/bin:/bin"}

const asAgentUsage = "usage: issuedog as-agent -- <command> [args...]"

// runAsAgent runs "as-agent -- <command> [args...]": one command as agent,
// through the uid switch stage runs use, so sandbox/test.sh can check what an
// agent process gets. The command's stdout is this program's output, and its
// stderr is logged. It exits with the command's exit code.
func runAsAgent(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) < 2 || args[0] != "--" {
		logr.Error(asAgentUsage)
		return 2
	}
	cred, err := agentCredential(agentUser)
	if err != nil {
		logr.Errorf("running %s as %s: %v", args[1], agentUser, err)
		return 1
	}
	var stderr bytes.Buffer
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Dir = "/"
	cmd.Env = agentEnv
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = agent.SysProcAttr(cred)
	err = cmd.Run()
	if stderr.Len() > 0 {
		logr.Warnf("%s as %s wrote to stderr: %s", args[1], agentUser, bytes.TrimSpace(stderr.Bytes()))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return exitErr.ExitCode()
	}
	if err != nil {
		logr.Errorf("running %s as %s: %v", args[1], agentUser, err)
		return 1
	}
	return 0
}

const handoffUsage = "usage: issuedog handoff <clone> <start-sha> <bare-repo> <run-id>   (absolute paths)"

// runHandoff runs "handoff <clone> <start-sha> <bare-repo> <run-id>": the
// daemon's own handoff of a run's commits (SPEC §3.2), so sandbox/test.sh can
// check it between the two uids. It exits 0 once the commits are in the bare
// repo, or when the clone has none past start. It rewrites the shared
// /run/issuedog/.agent.gitconfig, naming bare as agent's safe.directory, so
// it's for the sandbox only, never beside a running daemon.
func runHandoff(args []string) int {
	if len(args) != 4 || !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[2]) {
		logr.Error(handoffUsage)
		return 2
	}
	clone, start, bare, runID := filepath.Clean(args[0]), args[1], args[2], args[3]
	if err := handoff(context.Background(), clone, start, bare, runID); err != nil {
		logr.Errorf("handing off run %s from %s: %v", runID, clone, err)
		return 1
	}
	return 0
}

// handoff bundles start..HEAD in clone as agent, with workspace.Bundle and
// the uid switch stage runs use, onto a file in a dir only this process's
// uid can write. Then repo.FetchBundle fetches that file into bare, as this
// process, under repo.RunsRef+runID, and the head is logged. A clone with no
// commits past start gives no bundle, and nothing is fetched. The bundle and
// fetch are the daemon's own, daemon.BundleAndFetch, and agent's git runs
// with the global config a stage run's Prepare writes, so the sandbox checks
// the path a stage run takes.
func handoff(ctx context.Context, clone, start, bare, runID string) (err error) {
	cred, err := agentCredential(agentUser)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", agentUser, err)
	}
	// MkdirTemp makes the dir 0700, so only this uid can write in it.
	dir, err := os.MkdirTemp("", "issuedog-handoff-")
	if err != nil {
		return fmt.Errorf("making the bundle's dir: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the bundle's dir: %w", rerr))
		}
	}()
	// The clone is the run in its parent dir named by its base, as a stage
	// run's clone is RunsDir/<run-id>. The context dir is the daemon's, as
	// for stage runs: agent can't write it, so it can't plant a git config
	// there.
	w := &workspace.Workspace{Bare: bare, RunsDir: filepath.Dir(clone), ContextDir: runDir, Credential: cred}
	if err := w.WriteGitConfig(); err != nil {
		return fmt.Errorf("writing %s's git config: %w", agentUser, err)
	}
	head, err := daemon.BundleAndFetch(ctx, w, repo.New("", bare), filepath.Base(clone), runID, start, filepath.Join(dir, "run.bundle"))
	if err != nil {
		return fmt.Errorf("bundling as %s and fetching into %s: %w", agentUser, bare, err)
	}
	if head == "" {
		logr.Infof("run %s has no commits past %s in %s, so there's no bundle to fetch", runID, start, clone)
		return nil
	}
	logr.Infof("fetched run %s's bundle into %s: %s%s is %s", runID, bare, repo.RunsRef, runID, head)
	return nil
}

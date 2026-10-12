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
	"syscall"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/appkey"
	"github.com/mertenvg/issuedog/internal/egress"
	"github.com/mertenvg/issuedog/internal/firewall"
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
		fmt.Fprintln(stdout, usage)
		return 0
	} else if err != nil {
		logr.Errorf("issuedog: %v\n%s", err, usage)
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "issuedog %s\n", version)
		return 0
	}
	// Key commands don't need a Claude credential, so they run before its check.
	if fs.NArg() > 0 {
		return command(fs.Args(), stdin, stdout)
	}
	if _, err := agent.Credential(environ); err != nil {
		logr.Errorf("checking the Claude credential: %v", err)
		return 1
	}
	proxy, err := startEgress()
	if err != nil {
		logr.Errorf("starting egress: %v", err)
		return 1
	}
	defer proxy.Close()
	logr.Error("the daemon is not implemented yet")
	return 1
}

const usage = `usage:
  issuedog [-version]
  issuedog key import [-path file] < app-key.pem
  issuedog key check [-path file]     the file defaults to ` + appkey.DefaultPath

// command runs "key import" (store the App key from stdin) or "key check".
// The unlisted "egress" serves the firewall and proxy, and the unlisted
// "as-agent" runs one command as agent, both for sandbox/test.sh.
func command(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 1 && args[0] == "egress" {
		return serveEgress()
	}
	if len(args) > 0 && args[0] == "as-agent" {
		return runAsAgent(args[1:], stdin, stdout)
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
		fmt.Fprintln(stdout, usage)
		return 0
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

// The agent user, and the port on 127.0.0.1 where the egress proxy listens.
// The firewall lets agent connect to that port and nothing else, so stage
// runs pass http://127.0.0.1:3128 to agent.Env as the proxy.
const (
	agentUser = "agent"
	proxyPort = 3128
)

// allowHosts is network.allow (SPEC §13), at its default for now.
// TODO(#37): read network.allow from .issuedog/config.yaml on main.
var allowHosts = []string{"api.anthropic.com", "proxy.golang.org", "sum.golang.org"}

// The startup steps that need privileges or a fixed port. Tests replace them.
var (
	applyFirewall = firewall.Apply
	listenProxy   = egress.Listen
)

// startEgress applies the agent firewall, then serves the egress proxy until
// the returned server is closed.
func startEgress() (*http.Server, error) {
	if err := applyFirewall(agentUser); err != nil {
		return nil, fmt.Errorf("applying the agent firewall: %w", err)
	}
	l, err := listenProxy(proxyPort)
	if err != nil {
		return nil, fmt.Errorf("listening for the egress proxy: %w", err)
	}
	// Tunnels are hijacked, so the header timeout only limits the request.
	srv := &http.Server{Handler: egress.New(allowHosts), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logr.Errorf("serving the egress proxy: %v", err)
		}
	}()
	logr.Infof("egress proxy listening on http://%s", l.Addr())
	return srv, nil
}

// serveEgress runs startEgress until SIGINT or SIGTERM. It lets
// sandbox/test.sh check egress while the daemon can't yet keep running.
func serveEgress() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxy, err := startEgress()
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
var agentEnv = []string{"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin"}

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

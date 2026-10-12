// Command issuedog turns GitHub issues into reviewed pull requests.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/appkey"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	stop := setupLogging(os.Stdout, os.Stderr)
	code := run(os.Args[1:], os.Environ(), os.Stdin)
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

// run is the whole program. Like everything else, it writes only through
// logr, which main's setupLogging sends to stdout and stderr.
func run(args, environ []string, stdin io.Reader) int {
	fs := flag.NewFlagSet("issuedog", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage goes through logr
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		logr.Info(usage)
		return 0
	} else if err != nil {
		logr.Errorf("issuedog: %v\n%s", err, usage)
		return 2
	}
	if *showVersion {
		logr.Infof("issuedog %s", version)
		return 0
	}
	// Key commands don't need a Claude credential, so they run before its check.
	if fs.NArg() > 0 {
		return command(fs.Args(), stdin)
	}
	if _, err := agent.Credential(environ); err != nil {
		logr.Errorf("checking the Claude credential: %v", err)
		return 1
	}
	logr.Error("the daemon is not implemented yet")
	return 1
}

const usage = `usage:
  issuedog [-version]
  issuedog key import [-path file] < app-key.pem
  issuedog key check [-path file]     the file defaults to ` + appkey.DefaultPath

// command runs "key import" (store the App key from stdin) or "key check".
func command(args []string, stdin io.Reader) int {
	if len(args) < 2 || args[0] != "key" || (args[1] != "import" && args[1] != "check") {
		logr.Error(usage)
		return 2
	}
	fs := flag.NewFlagSet("issuedog key "+args[1], flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage goes through logr
	path := fs.String("path", appkey.DefaultPath, "the key file")
	err := fs.Parse(args[2:])
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

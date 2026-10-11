// Command issuedog turns GitHub issues into reviewed pull requests.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/agent"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	stop := setupLogging(os.Stdout, os.Stderr)
	code := run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
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

func run(args, environ []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("issuedog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "issuedog %s\n", version)
		return 0
	}
	if _, err := agent.Credential(environ); err != nil {
		logr.Error(fmt.Errorf("checking the Claude credential: %w", err).Error())
		return 1
	}
	logr.Error("the daemon is not implemented yet")
	return 1
}

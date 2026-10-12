// Command issuedog turns GitHub issues into reviewed pull requests.
package main

import (
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
	code := run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr)
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

func run(args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	// Key commands don't need a Claude credential, so they run before its check.
	if fs.NArg() > 0 {
		return command(fs.Args(), stdin, stdout, stderr)
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
  issuedog key check [-path file]
`

// command runs a subcommand: "key import" stores the App private key from
// stdin on the state volume, and "key check" loads it as the daemon would.
func command(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "key" || (args[1] != "import" && args[1] != "check") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("issuedog key "+args[1], flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", appkey.DefaultPath, "the key file")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[1] == "import" {
		if err := appkey.Import(*path, stdin); err != nil {
			logr.Errorf("importing the App key: %v", err)
			return 1
		}
		fmt.Fprintf(stdout, "imported the App key to %s\n", *path)
		return 0
	}
	if _, err := appkey.Load(*path); err != nil {
		logr.Errorf("checking the App key: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "the App key at %s is usable\n", *path)
	return 0
}

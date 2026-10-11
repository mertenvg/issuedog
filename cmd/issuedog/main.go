// Command issuedog turns GitHub issues into reviewed pull requests.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mertenvg/logr/v2"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logr.AddWriter(os.Stderr)
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	logr.Wait()
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
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
	logr.Error("the daemon is not implemented yet")
	return 1
}

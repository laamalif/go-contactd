package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/laamalif/go-contactd/internal/ctl"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(prog string, args []string, stdin io.Reader, stdout, stderr *os.File) int {
	return runMainProgramWithInput(prog, args, currentEnvMap(), stdin, stdout, stderr)
}

func runMainProgramWithInput(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	return ctl.RunCLI(filepath.Base(prog), args, env, stdin, stdout, stderr, runVersionNamed)
}

func runVersionNamed(prog string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printVersionHelp(stdout, prog)
		return 0
	}
	fs := newCLIFlagSet("version")
	format := "text"
	fs.StringVar(&format, "format", format, "text|json")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if format != "text" && format != "json" {
		_, _ = fmt.Fprintf(stderr, "usage error: invalid --format %q\n", format)
		return 2
	}

	if format == "json" {
		out := map[string]string{
			"version":    version,
			"commit":     commit,
			"build_date": buildDate,
			"go_version": runtime.Version(),
			"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "internal error: %v\n", err)
			return 1
		}
		return 0
	}

	name := strings.TrimSpace(prog)
	if name == "" {
		name = "contactctl"
	}
	_, _ = fmt.Fprintf(stdout, "%s %s (commit %s, built %s, %s, %s/%s)\n", name, version, commit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return 0
}

func isHelpToken(s string) bool {
	return s == "-h" || s == "--help"
}

func newCLIFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func printVersionHelp(w io.Writer, prog string) {
	name := strings.TrimSpace(prog)
	if name == "" {
		name = "contactctl"
	}
	_, _ = fmt.Fprintf(w, "usage: %s version [--format text|json]\n", name)
}

func currentEnvMap() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return env
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/laamalif/go-contactd/internal/ctl"
	contactdaemon "github.com/laamalif/go-contactd/internal/daemon"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr))
}

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func run(prog string, args []string, stdout, stderr *os.File) int {
	return runMainProgramWithInput(prog, args, currentEnvMap(), os.Stdin, stdout, stderr)
}

func runMain(args []string, env map[string]string, stdout, stderr io.Writer) int {
	return runMainProgramWithInput("go-contactd", args, env, os.Stdin, stdout, stderr)
}

func runMainWithInput(args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runMainProgramWithInput("go-contactd", args, env, stdin, stdout, stderr)
}

func runMainProgramWithInput(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	base := filepath.Base(prog)
	if cliModeForProgram(base) == cliModeAdmin {
		return runAdminCLI(base, args, env, stdin, stdout, stderr)
	}
	return runDaemonCLI(base, args, env, stdin, stdout, stderr)
}

type cliMode int

const (
	cliModeDaemon cliMode = iota
	cliModeAdmin
)

func cliModeForProgram(base string) cliMode {
	switch base {
	case "contactctl":
		return cliModeAdmin
	default:
		return cliModeDaemon
	}
}

func runDaemonCLI(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runServeNamed(prog, nil, env, stderr)
	}
	if args[0] == "--version" || args[0] == "-V" {
		return runVersionNamed(prog, nil, stdout, stderr)
	}
	if isHelpToken(args[0]) || args[0] == "help" {
		printDaemonHelp(stdout, prog)
		return 0
	}
	if strings.HasPrefix(args[0], "-") {
		if containsHelpToken(args) {
			printDaemonHelp(stdout, prog)
			return 0
		}
		return runServeNamed(prog, args, env, stderr)
	}

	switch args[0] {
	case "serve": // backward compatibility alias (undocumented)
		if containsHelpToken(args[1:]) {
			printDaemonHelp(stdout, prog)
			return 0
		}
		return runServeNamed(prog, args[1:], env, stderr)
	case "version": // backward compatibility alias (undocumented)
		return runVersionNamed(prog, args[1:], stdout, stderr)
	case "user": // backward compatibility alias (undocumented); prefer contactctl
		return runUser(prog, args[1:], env, stdin, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "%s: unknown command: %s\n", prog, args[0])
		printDaemonUsage(stderr, prog)
		return 2
	}
}

func runAdminCLI(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	return ctl.RunCLI(prog, args, env, stdin, stdout, stderr, runVersionNamed)
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
		name = "contactd"
	}
	_, _ = fmt.Fprintf(stdout, "%s %s (commit %s, built %s, %s, %s/%s)\n", name, version, commit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return 0
}

func runServeNamed(prog string, args []string, env map[string]string, stderr io.Writer) int {
	return contactdaemon.RunServeNamed(prog, args, env, stderr, printServeHelp)
}

func runUser(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	return ctl.RunUser(prog, args, env, stdin, stdout, stderr)
}

func isHelpToken(s string) bool {
	return s == "-h" || s == "--help"
}

func containsHelpToken(args []string) bool {
	for _, arg := range args {
		if isHelpToken(arg) || arg == "help" {
			return true
		}
	}
	return false
}

func printDaemonUsage(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s [flags]\n", prog)
}

func printDaemonHelp(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s [flags]\n", prog)
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "flags:")
	_, _ = fmt.Fprintln(w, `  -l, --listen-addr addr       listen address (default ":8080")`)
	_, _ = fmt.Fprintln(w, "  -p, --port port             listen on :PORT (cannot combine with -l/--listen-addr)")
	_, _ = fmt.Fprintln(w, `  -d, --db-path path          sqlite database path (default "/var/db/contactd.db")`)
	_, _ = fmt.Fprintln(w, "      --base-url url          public base URL for redirects")
	_, _ = fmt.Fprintln(w, "      --log-level level       log level: debug|info|warn|error")
	_, _ = fmt.Fprintln(w, "      --log-format fmt        log format: text|json")
	_, _ = fmt.Fprintln(w, "      --trust-proxy-headers   trust X-Forwarded-* headers")
	_, _ = fmt.Fprintln(w, "  -V, --version               print version and exit")
	_, _ = fmt.Fprintln(w, "  -h, --help                  print help and exit")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "environment:")
	_, _ = fmt.Fprintln(w, "  core:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_LISTEN_ADDR, PORT, CONTACTD_DB_PATH, CONTACTD_BASE_URL")
	_, _ = fmt.Fprintln(w, "  logging:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_LOG_LEVEL, CONTACTD_LOG_FORMAT, CONTACTD_TRUST_PROXY_HEADERS")
	_, _ = fmt.Fprintln(w, "  limits:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_REQUEST_MAX_BYTES, CONTACTD_VCARD_MAX_BYTES")
	_, _ = fmt.Fprintln(w, "  bootstrap:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_USERS, CONTACTD_USER_*, CONTACTD_FORCE_SEED")
	_, _ = fmt.Fprintln(w, "    CONTACTD_DEFAULT_BOOK_SLUG, CONTACTD_DEFAULT_BOOK_NAME")
	_, _ = fmt.Fprintln(w, "  retention/maintenance:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_CHANGE_RETENTION_DAYS, CONTACTD_CHANGE_RETENTION_MAX_REVISIONS")
	_, _ = fmt.Fprintln(w, "    CONTACTD_PRUNE_INTERVAL")
	_, _ = fmt.Fprintln(w, "  feature flags:")
	_, _ = fmt.Fprintln(w, "    CONTACTD_ENABLE_ADDRESSBOOK_COLOR")
	_, _ = fmt.Fprintln(w, "  flags override environment; environment overrides defaults")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "admin commands:")
	_, _ = fmt.Fprintln(w, "  use contactctl user <add|list|delete|passwd>")
}

func printVersionHelp(w io.Writer, prog string) {
	name := strings.TrimSpace(prog)
	if name == "" {
		name = "contactd"
	}
	_, _ = fmt.Fprintf(w, "usage: %s version [--format text|json]\n", name)
}

func printServeHelp(w io.Writer) {
	printDaemonHelp(w, "contactd")
}

func newCLIFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
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

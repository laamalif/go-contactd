package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	contactcarddav "github.com/laamalif/go-contactd/internal/carddav"
	"github.com/laamalif/go-contactd/internal/carddavx"
	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/db"
	"github.com/laamalif/go-contactd/internal/server"
	"golang.org/x/crypto/bcrypt"
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
	if len(args) == 0 {
		printAdminUsage(stderr, prog)
		return 2
	}
	if args[0] == "--version" || args[0] == "-V" {
		return runVersionNamed(prog, nil, stdout, stderr)
	}
	if isHelpToken(args[0]) || args[0] == "help" {
		printAdminHelp(stdout, prog)
		return 0
	}
	switch args[0] {
	case "user":
		return runUser(prog, args[1:], env, stdin, stdout, stderr)
	case "version":
		return runVersionNamed(prog, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "%s: unknown command: %s\n", prog, args[0])
		printAdminUsage(stderr, prog)
		return 2
	}
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
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printServeHelp(stderr)
		return 0
	}
	startupLogs := newDeferredLogWriter(stderr)
	rt, err := prepareServeRuntime(context.Background(), args, env, startupLogs)
	if err != nil {
		err = humanizeServeFatalError(extractDBPathForFatal(args, env), err)
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		}
		return 2
	}
	_ = startupLogs.Activate()
	defer func() { _ = rt.close() }()

	rt.logger.Info(
		"server starting",
		"event", "server starting",
		"listen", rt.cfg.ListenAddr,
		"db_path", rt.cfg.DBPath,
		"log_level", rt.cfg.LogLevel,
		"log_format", rt.cfg.LogFormat,
		"trust_proxy_headers", rt.cfg.TrustProxyHeaders,
		"base_url", rt.cfg.BaseURL,
	)

	srv := &http.Server{
		Addr:    rt.cfg.ListenAddr,
		Handler: rt.handler,
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopPrune := startConfiguredPruneLoop(sigCtx, rt.store, rt.cfg, rt.logger)
	defer stopPrune()
	return serveHTTPGracefully(sigCtx, srv, rt.logger)
}

type deferredLogWriter struct {
	mu     sync.Mutex
	live   io.Writer
	buf    []byte
	active bool
}

func newDeferredLogWriter(live io.Writer) *deferredLogWriter {
	return &deferredLogWriter{live: live}
}

func (w *deferredLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.active {
		if w.live == nil {
			return len(p), nil
		}
		return w.live.Write(p)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *deferredLogWriter) Activate() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.active {
		return nil
	}
	w.active = true
	if w.live == nil || len(w.buf) == 0 {
		w.buf = nil
		return nil
	}
	_, err := w.live.Write(w.buf)
	if err != nil {
		return err
	}
	w.buf = nil
	return nil
}

func humanizeServeFatalError(dbPath string, err error) error {
	err = humanizeDBOpenError(dbPath, err)
	if err == nil {
		return nil
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "load config: ")
	msg = strings.TrimPrefix(msg, "parse serve flags: ")
	if msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

func extractDBPathForFatal(args []string, env map[string]string) string {
	cfg, err := config.LoadServeConfig(args, env)
	if err != nil {
		// Fallback to the known default if even config parsing failed.
		return "/var/db/contactd.db"
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		return "/var/db/contactd.db"
	}
	return cfg.DBPath
}

func humanizeDBOpenError(dbPath string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, "unable to open database file") {
		return err
	}

	parent := filepath.Dir(dbPath)
	if _, statErr := os.Stat(parent); statErr != nil {
		if os.IsNotExist(statErr) {
			return fmt.Errorf("cannot open database %s: no such file or directory", dbPath)
		}
		if os.IsPermission(statErr) {
			return fmt.Errorf("cannot open database %s: permission denied", dbPath)
		}
	}

	// SQLite CANTOPEN (14) is often surfaced by modernc with misleading "out of memory (14)" text.
	if strings.Contains(msg, "(14)") || strings.Contains(msg, "unable to open database file") {
		return fmt.Errorf("cannot open database %s: permission denied", dbPath)
	}
	return err
}

type serveHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

const serveShutdownTimeout = 5 * time.Second

func serveHTTPGracefully(runCtx context.Context, srv serveHTTPServer, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	serveDone := make(chan struct{})
	shutdownDone := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(shutdownDone)
		select {
		case <-runCtx.Done():
			// If serving already ended, do not trigger a redundant shutdown from a later cancel/stop.
			select {
			case <-serveDone:
				return
			default:
			}
			logger.Info("server shutdown", "event", "server shutdown")
			ctx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
			defer cancel()
			shutdownErr = srv.Shutdown(ctx)
		case <-serveDone:
			return
		}
	}()

	err := srv.ListenAndServe()
	close(serveDone)
	<-shutdownDone

	if shutdownErr != nil {
		logger.Error("shutdown failed", "event", "shutdown failed", "error", shutdownErr)
		return 1
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("listen failed", "event", "listen failed", "error", err)
		return 1
	}
	logger.Info("server stopped", "event", "server stopped")
	return 0
}

func runUser(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUserUsage(stderr)
		return 2
	}
	if isHelpToken(args[0]) || args[0] == "help" {
		printUserHelp(stdout)
		return 0
	}

	switch args[0] {
	case "add":
		return runUserAdd(args[1:], env, stdin, stdout, stderr)
	case "list":
		return runUserList(args[1:], env, stdout, stderr)
	case "delete":
		return runUserDelete(args[1:], env, stdout, stderr)
	case "passwd":
		return runUserPasswd(args[1:], env, stdin, stdout, stderr)
	default:
		if strings.TrimSpace(prog) == "" {
			prog = "contactctl"
		}
		_, _ = fmt.Fprintf(stderr, "%s: unknown user subcommand: %s\n", prog, args[0])
		printUserUsage(stderr)
		return 2
	}
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
	_, _ = fmt.Fprintln(w, "      --listen addr            alias for --listen-addr")
	_, _ = fmt.Fprintln(w, "      --bind addr              alias for --listen-addr")
	_, _ = fmt.Fprintln(w, "      --addr addr              alias for --listen-addr")
	_, _ = fmt.Fprintln(w, "  -p, --port port             listen on :PORT (cannot combine with -l/--listen-addr)")
	_, _ = fmt.Fprintln(w, `  -d, --db-path path          sqlite database path (default "/var/db/contactd.db")`)
	_, _ = fmt.Fprintln(w, "      --db path               alias for --db-path")
	_, _ = fmt.Fprintln(w, "      --base-url url          public base URL for redirects")
	_, _ = fmt.Fprintln(w, "      --log-level level       log level: debug|info|warn|error")
	_, _ = fmt.Fprintln(w, "      --log-format fmt        log format: text|json")
	_, _ = fmt.Fprintln(w, "      --trust-proxy-headers   trust X-Forwarded-* headers")
	_, _ = fmt.Fprintln(w, "  -V, --version               print version and exit")
	_, _ = fmt.Fprintln(w, "  -h, --help                  print help and exit")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "environment:")
	_, _ = fmt.Fprintln(w, "  CONTACTD_LISTEN_ADDR, CONTACTD_DB_PATH, CONTACTD_LOG_LEVEL, ...")
	_, _ = fmt.Fprintln(w, "  flags override environment; environment overrides defaults")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "admin commands:")
	_, _ = fmt.Fprintln(w, "  use contactctl user <add|list|delete|passwd>")
}

func printAdminUsage(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s user <add|list|delete|passwd>\n", prog)
}

func printAdminHelp(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s user <add|list|delete|passwd>\n", prog)
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	_, _ = fmt.Fprintln(w, "  user add      create user")
	_, _ = fmt.Fprintln(w, "  user list     list users")
	_, _ = fmt.Fprintln(w, "  user delete   delete user")
	_, _ = fmt.Fprintln(w, "  user passwd   change user password")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "run 'contactctl user -h' for user subcommand help")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "flags:")
	_, _ = fmt.Fprintln(w, "  -V, --version  print version and exit")
	_, _ = fmt.Fprintln(w, "  -h, --help     print help and exit")
}

func printUserUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user <add|list|delete|passwd>")
}

func printUserHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user <add|list|delete|passwd>")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "subcommands:")
	_, _ = fmt.Fprintln(w, "  add     create a user (use --password-stdin to avoid argv leaks)")
	_, _ = fmt.Fprintln(w, "  list    list users")
	_, _ = fmt.Fprintln(w, "  delete  delete a user by --username or --id")
	_, _ = fmt.Fprintln(w, "  passwd  update a user password (supports --password-stdin)")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "notes:")
	_, _ = fmt.Fprintln(w, "  --db is a short alias for --db-path on all user subcommands")
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

func printUserAddHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user add --username <name> (--password <pw> | --password-stdin) [--db-path <path>|--db <path>|-d <path>] [--default-book-slug <slug>] [--default-book-name <name>]")
}

func printUserListHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user list [--db-path <path>|--db <path>|-d <path>] [--format table|json]")
}

func printUserDeleteHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user delete (--username <name> | --id <id>) [--db-path <path>|--db <path>|-d <path>]")
}

func printUserPasswdHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user passwd (--username <name> | --id <id>) (--password <pw> | --password-stdin) [--db-path <path>|--db <path>|-d <path>]")
}

const defaultCLIDBPath = "/var/db/contactd.db"

func runUserAdd(args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserAddHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user add")
	var (
		dbPath   = defaultCLIOpt(env["CONTACTD_DB_PATH"], defaultCLIDBPath)
		username string
		password string
		pwStdin  bool
		bookSlug = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_SLUG"], "contacts")
		bookName = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_NAME"], "Contacts")
	)
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.StringVar(&password, "password", "", "password")
	fs.BoolVar(&pwStdin, "password-stdin", false, "read password from stdin (safer than argv)")
	fs.StringVar(&bookSlug, "default-book-slug", bookSlug, "default addressbook slug")
	fs.StringVar(&bookName, "default-book-name", bookName, "default addressbook name")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if strings.TrimSpace(username) == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: missing required --username")
		return 2
	}
	if err := validateUsername(username); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	var err error
	password, err = resolvePasswordInput(password, pwStdin, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	id, err := store.CreateUser(context.Background(), username, string(hash))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			_, _ = fmt.Fprintf(stderr, "usage error: username already exists: %s\n", username)
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	if _, _, err := store.EnsureAddressbook(context.Background(), id, bookSlug, bookName); err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "user added: id=%d username=%s\n", id, username)
	return 0
}

func runUserList(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserListHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user list")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], defaultCLIDBPath)
	format := "table"
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&format, "format", format, "table|json")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if format != "table" && format != "json" {
		_, _ = fmt.Fprintf(stderr, "usage error: invalid --format %q\n", format)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	users, err := store.ListUsers(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if format == "json" {
		type outUser struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		}
		out := make([]outUser, 0, len(users))
		for _, u := range users {
			out = append(out, outUser{ID: u.ID, Username: u.Username})
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "internal error: %v\n", err)
			return 1
		}
		return 0
	}

	_, _ = fmt.Fprintln(stdout, "ID\tUSERNAME")
	for _, u := range users {
		_, _ = fmt.Fprintf(stdout, "%d\t%s\n", u.ID, u.Username)
	}
	return 0
}

func runUserDelete(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserDeleteHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user delete")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], defaultCLIDBPath)
	var username string
	var id int64
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.Int64Var(&id, "id", 0, "user id")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if (username == "" && id == 0) || (username != "" && id != 0) {
		_, _ = fmt.Fprintln(stderr, "usage error: specify exactly one of --username or --id")
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	if username != "" {
		err = store.DeleteUserByUsername(context.Background(), username)
	} else {
		err = store.DeleteUserByID(context.Background(), id)
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if username != "" {
		_, _ = fmt.Fprintf(stdout, "user deleted: username=%s\n", username)
	} else {
		_, _ = fmt.Fprintf(stdout, "user deleted: id=%d\n", id)
	}
	return 0
}

func runUserPasswd(args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserPasswdHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user passwd")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], defaultCLIDBPath)
	var username string
	var id int64
	var password string
	var pwStdin bool
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.Int64Var(&id, "id", 0, "user id")
	fs.StringVar(&password, "password", "", "password")
	fs.BoolVar(&pwStdin, "password-stdin", false, "read password from stdin (safer than argv)")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if (username == "" && id == 0) || (username != "" && id != 0) {
		_, _ = fmt.Fprintln(stderr, "usage error: specify exactly one of --username or --id")
		return 2
	}
	var err error
	password, err = resolvePasswordInput(password, pwStdin, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	if username != "" {
		var userID int64
		userID, err = store.UserIDByUsername(context.Background(), username)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				_, _ = fmt.Fprintln(stderr, "not found")
				return 3
			}
			_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
			return 1
		}
		err = store.SetUserPasswordHash(context.Background(), userID, string(hash))
	} else {
		err = store.SetUserPasswordHash(context.Background(), id, string(hash))
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if username != "" {
		_, _ = fmt.Fprintf(stdout, "user password updated: username=%s\n", username)
	} else {
		_, _ = fmt.Fprintf(stdout, "user password updated: id=%d\n", id)
	}
	return 0
}

func resolvePasswordInput(password string, passwordStdin bool, stdin io.Reader) (string, error) {
	hasPassword := password != ""
	if hasPassword == passwordStdin {
		return "", fmt.Errorf("specify exactly one of --password or --password-stdin")
	}
	if hasPassword {
		return password, nil
	}
	if stdin == nil {
		return "", fmt.Errorf("stdin is not available")
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("password from stdin is empty")
	}
	return pw, nil
}

func newCLIFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func defaultCLIOpt(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

var usernameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)

func validateUsername(username string) error {
	if !usernameRE.MatchString(username) {
		return fmt.Errorf("invalid --username: use 1-64 chars [a-z0-9_-], start/end with [a-z0-9]")
	}
	switch username {
	case ".well-known", "healthz", "readyz":
		return fmt.Errorf("invalid --username: %q is reserved", username)
	}
	return nil
}

type serveRuntime struct {
	cfg     config.ServeConfig
	store   *db.Store
	handler http.Handler
	logger  *slog.Logger
}

func (rt *serveRuntime) close() error {
	if rt == nil || rt.store == nil {
		return nil
	}
	return rt.store.Close()
}

func prepareServeRuntime(ctx context.Context, args []string, env map[string]string, logOut io.Writer) (*serveRuntime, error) {
	cfg, err := config.LoadServeConfig(args, env)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	logger := newServeLogger(cfg.LogFormat, cfg.LogLevel, logOut)

	store, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("db error", "event", "db error", "op", "open", "error", err)
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := startupStore(ctx, store, cfg, logger); err != nil {
		logger.Error("db error", "event", "db error", "op", "startup", "error", err)
		_ = store.Close()
		return nil, err
	}

	h := server.NewHandler(server.HandlerOptions{
		Logger:                 logger,
		ReadyCheck:             store.Ready,
		Backend:                contactcarddav.NewBackend(store),
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: cfg.EnableAddressbookColor,
		BaseURL:                cfg.BaseURL,
		TrustProxyHeaders:      cfg.TrustProxyHeaders,
		RequestMaxBytes:        cfg.RequestMaxBytes,
		VCardMaxBytes:          cfg.VCardMaxBytes,
		AttachPrincipal:        contactcarddav.WithPrincipal,
		Authenticate: func(ctx context.Context, username, password string) (string, bool, error) {
			ok, _, err := store.AuthenticateUser(ctx, username, password)
			if err != nil {
				return "", false, err
			}
			if !ok {
				return "", false, nil
			}
			return username, true, nil
		},
	})

	return &serveRuntime{
		cfg:     cfg,
		store:   store,
		handler: h,
		logger:  logger,
	}, nil
}

func newServeLogger(format, level string, out io.Writer) *slog.Logger {
	if out == nil {
		out = io.Discard
	}
	lvl := parseSlogLevel(level)
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lvl}))
	default:
		return slog.New(newDaemonTextHandler(out, lvl, "contactd"))
	}
}

type daemonTextHandler struct {
	out    io.Writer
	level  slog.Leveler
	prog   string
	pid    int
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func newDaemonTextHandler(out io.Writer, level slog.Leveler, prog string) slog.Handler {
	if out == nil {
		out = io.Discard
	}
	if level == nil {
		level = slog.LevelInfo
	}
	prog = strings.TrimSpace(prog)
	if prog == "" {
		prog = "contactd"
	}
	return &daemonTextHandler{
		out:   out,
		level: level,
		prog:  prog,
		pid:   os.Getpid(),
		mu:    &sync.Mutex{},
	}
}

func (h *daemonTextHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *daemonTextHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	b.WriteString(ts.Format("Jan _2 15:04:05"))
	b.WriteByte(' ')
	b.WriteString(h.prog)
	b.WriteByte('[')
	b.WriteString(strconv.Itoa(h.pid))
	b.WriteString("]: ")

	if pfx := daemonLevelPrefix(r.Level); pfx != "" {
		b.WriteString(pfx)
	}
	msg := strings.TrimSpace(r.Message)
	if msg == "" {
		msg = "log"
	}
	b.WriteString(msg)

	attrs := make([]slog.Attr, 0, len(h.attrs)+8)
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	for _, a := range attrs {
		for _, fa := range flattenDaemonAttr(h.groups, a) {
			if skipDaemonTextAttr(msg, fa) {
				continue
			}
			b.WriteByte(' ')
			b.WriteString(fa.Key)
			b.WriteByte('=')
			b.WriteString(formatDaemonAttrValue(msg, fa))
		}
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *daemonTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(slices.Clone(h.attrs), attrs...)
	return &nh
}

func (h *daemonTextHandler) WithGroup(name string) slog.Handler {
	if strings.TrimSpace(name) == "" {
		return h
	}
	nh := *h
	nh.groups = append(slices.Clone(h.groups), name)
	return &nh
}

func daemonLevelPrefix(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error: "
	case l >= slog.LevelWarn:
		return "warning: "
	case l < slog.LevelInfo:
		return "debug: "
	default:
		return ""
	}
}

func flattenDaemonAttr(groups []string, a slog.Attr) []slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return nil
	}
	key := a.Key
	if len(groups) > 0 && key != "" {
		key = strings.Join(append(slices.Clone(groups), key), ".")
	}
	if a.Value.Kind() != slog.KindGroup {
		return []slog.Attr{{Key: key, Value: a.Value}}
	}
	var out []slog.Attr
	for _, ga := range a.Value.Group() {
		nextGroups := groups
		if a.Key != "" {
			nextGroups = append(slices.Clone(groups), a.Key)
		}
		out = append(out, flattenDaemonAttr(nextGroups, ga)...)
	}
	return out
}

func skipDaemonTextAttr(msg string, a slog.Attr) bool {
	if a.Key == "" {
		return true
	}
	if a.Key == "event" && a.Value.Kind() == slog.KindString && strings.TrimSpace(a.Value.String()) == msg {
		return true
	}
	return false
}

func formatDaemonAttrValue(msg string, a slog.Attr) string {
	if msg == "request" && a.Key == "user" && a.Value.Kind() == slog.KindString && a.Value.String() == "" {
		return "-"
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return quoteDaemonTextIfNeeded(a.Value.String())
	case slog.KindBool:
		if a.Value.Bool() {
			return "true"
		}
		return "false"
	case slog.KindInt64:
		return fmt.Sprintf("%d", a.Value.Int64())
	case slog.KindUint64:
		return fmt.Sprintf("%d", a.Value.Uint64())
	case slog.KindFloat64:
		return fmt.Sprintf("%g", a.Value.Float64())
	case slog.KindDuration:
		return a.Value.Duration().String()
	case slog.KindTime:
		return a.Value.Time().Format(time.RFC3339)
	case slog.KindAny:
		return quoteDaemonTextIfNeeded(fmt.Sprint(a.Value.Any()))
	default:
		return quoteDaemonTextIfNeeded(a.Value.String())
	}
}

func quoteDaemonTextIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\r\n\"=") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func parseSlogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info":
		fallthrough
	default:
		return slog.LevelInfo
	}
}

func startupStore(ctx context.Context, store *db.Store, cfg config.ServeConfig, logger *slog.Logger) error {
	for _, seed := range cfg.Users {
		if _, err := bcrypt.Cost([]byte(seed.PasswordHash)); err != nil {
			return fmt.Errorf("invalid bcrypt hash for user %q: %w", seed.Username, err)
		}
	}

	if err := pruneOnce(ctx, store, cfg, logger, "startup"); err != nil {
		return err
	}

	userCount, err := store.UserCount(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if userCount > 0 && !cfg.ForceSeed {
		logger.Info("seed skipped", "event", "seed skipped", "reason", "db_non_empty")
		return nil
	}
	for _, seed := range cfg.Users {
		if err := seedUser(ctx, store, cfg, seed, cfg.ForceSeed); err != nil {
			return err
		}
		logger.Info("user seeded", "event", "user seeded", "user", seed.Username)
	}
	return nil
}

func pruneOnce(ctx context.Context, store *db.Store, cfg config.ServeConfig, logger *slog.Logger, source string) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var prunedAge int64
	if cfg.ChangeRetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.ChangeRetentionDays)
		n, err := store.PruneCardChangesByAge(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("%s prune by age: %w", source, err)
		}
		prunedAge = n
	}

	var prunedMax int64
	if cfg.ChangeRetentionMaxRevisions > 0 {
		n, err := store.PruneCardChangesByMaxRevisions(ctx, cfg.ChangeRetentionMaxRevisions)
		if err != nil {
			return fmt.Errorf("%s prune by max revisions: %w", source, err)
		}
		prunedMax = n
	}

	logger.Info("changes pruned", "event", "changes pruned", "source", source, "by_age", prunedAge, "by_max_revisions", prunedMax)
	return nil
}

func startConfiguredPruneLoop(ctx context.Context, store *db.Store, cfg config.ServeConfig, logger *slog.Logger) func() {
	if store == nil {
		return func() {}
	}
	return startBackgroundPruneLoop(ctx, cfg.PruneInterval, logger, func(loopCtx context.Context) error {
		pruneCtx, cancel := context.WithTimeout(loopCtx, 30*time.Second)
		defer cancel()
		if err := pruneOnce(pruneCtx, store, cfg, logger, "ticker"); err != nil {
			if logger != nil {
				logger.Error("db error", "event", "db error", "op", "prune_ticker", "error", err)
			}
			return err
		}
		return nil
	})
}

func startBackgroundPruneLoop(ctx context.Context, interval time.Duration, _ *slog.Logger, fn func(context.Context) error) func() {
	if interval <= 0 || fn == nil {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = fn(runCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func seedUser(ctx context.Context, store *db.Store, cfg config.ServeConfig, seed config.SeedUser, force bool) error {
	userID, err := store.UserIDByUsername(ctx, seed.Username)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("lookup seed user %q: %w", seed.Username, err)
		}
		userID, err = store.CreateUser(ctx, seed.Username, seed.PasswordHash)
		if err != nil {
			return fmt.Errorf("create seed user %q: %w", seed.Username, err)
		}
	} else if force {
		if err := store.SetUserPasswordHash(ctx, userID, seed.PasswordHash); err != nil {
			return fmt.Errorf("update seed user %q hash: %w", seed.Username, err)
		}
	}

	if _, _, err := store.EnsureAddressbook(ctx, userID, cfg.DefaultBookSlug, cfg.DefaultBookName); err != nil {
		return fmt.Errorf("ensure default addressbook for %q: %w", seed.Username, err)
	}
	return nil
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

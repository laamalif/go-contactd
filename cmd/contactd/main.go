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
	"regexp"
	"strings"
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	return runMain(args, currentEnvMap(), stdout, stderr)
}

func runMain(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go-contactd <subcommand>")
		return 2
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:], env, stderr)
	case "user":
		return runUser(args[1:], env, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, "go-contactd dev")
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown subcommand: %s\n", args[0])
		_, _ = fmt.Fprintln(stderr, "usage: go-contactd <subcommand>")
		return 2
	}
}

func runServe(args []string, env map[string]string, stderr io.Writer) int {
	rt, err := prepareServeRuntime(context.Background(), args, env, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "startup error: %v\n", err)
		return 2
	}
	defer func() { _ = rt.close() }()

	rt.logger.Info(
		"server starting",
		"event", "server starting",
		"listen", rt.cfg.ListenAddr,
		"db_path", rt.cfg.DBPath,
		"log_level", rt.cfg.LogLevel,
		"log_format", rt.cfg.LogFormat,
		"trust_proxy_headers", rt.cfg.TrustProxyHeaders,
	)

	srv := &http.Server{
		Addr:    rt.cfg.ListenAddr,
		Handler: rt.handler,
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveHTTPGracefully(sigCtx, srv, rt.logger)
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

func runUser(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go-contactd user <add|list|delete|passwd>")
		return 2
	}

	switch args[0] {
	case "add":
		return runUserAdd(args[1:], env, stdout, stderr)
	case "list":
		return runUserList(args[1:], env, stdout, stderr)
	case "delete":
		return runUserDelete(args[1:], env, stdout, stderr)
	case "passwd":
		return runUserPasswd(args[1:], env, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown user subcommand: %s\n", args[0])
		_, _ = fmt.Fprintln(stderr, "usage: go-contactd user <add|list|delete|passwd>")
		return 2
	}
}

func runUserAdd(args []string, env map[string]string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("user add")
	var (
		dbPath   = defaultCLIOpt(env["CONTACTD_DB_PATH"], "/data/contactd.sqlite")
		username string
		password string
		bookSlug = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_SLUG"], "contacts")
		bookName = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_NAME"], "Contacts")
	)
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&username, "username", "", "username")
	fs.StringVar(&password, "password", "", "password")
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
	if err := validateUsername(username); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if password == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: --password is required")
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
	fs := newCLIFlagSet("user list")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], "/data/contactd.sqlite")
	format := "table"
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
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
	fs := newCLIFlagSet("user delete")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], "/data/contactd.sqlite")
	var username string
	var id int64
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
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

func runUserPasswd(args []string, env map[string]string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("user passwd")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], "/data/contactd.sqlite")
	var username string
	var id int64
	var password string
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&username, "username", "", "username")
	fs.Int64Var(&id, "id", 0, "user id")
	fs.StringVar(&password, "password", "", "password")
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
	if password == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: --password is required")
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
		return fmt.Errorf("username must match %s", usernameRE.String())
	}
	switch username {
	case ".well-known", "healthz", "readyz":
		return fmt.Errorf("username %q is reserved", username)
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
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := startupStore(ctx, store, cfg, logger); err != nil {
		_ = store.Close()
		return nil, err
	}

	h := server.NewHandler(server.HandlerOptions{
		Logger:                 logger,
		ReadyCheck:             store.Ready,
		Backend:                contactcarddav.NewBackend(store),
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: cfg.EnableAddressbookColor,
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
	opts := &slog.HandlerOptions{Level: parseSlogLevel(level)}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(out, opts))
	default:
		return slog.New(slog.NewTextHandler(out, opts))
	}
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

	var prunedAge int64
	if cfg.ChangeRetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.ChangeRetentionDays)
		n, err := store.PruneCardChangesByAge(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("startup prune by age: %w", err)
		}
		prunedAge = n
	}

	var prunedMax int64
	if cfg.ChangeRetentionMaxRevisions > 0 {
		n, err := store.PruneCardChangesByMaxRevisions(ctx, cfg.ChangeRetentionMaxRevisions)
		if err != nil {
			return fmt.Errorf("startup prune by max revisions: %w", err)
		}
		prunedMax = n
	}
	logger.Info("changes pruned", "event", "changes pruned", "by_age", prunedAge, "by_max_revisions", prunedMax)

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

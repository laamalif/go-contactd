package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

func isHelpToken(s string) bool {
	return s == "-h" || s == "--help"
}

func RunServeNamed(prog string, args []string, env map[string]string, stderr io.Writer, printServeHelp func(io.Writer)) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		if printServeHelp != nil {
			printServeHelp(stderr)
		}
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

	logServerStarting(rt.logger, rt.cfg)

	srv := newHTTPServer(rt.cfg.ListenAddr, rt.handler)
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

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func newDeferredLogWriter(live io.Writer) *deferredLogWriter {
	return &deferredLogWriter{live: live}
}

func logServerStarting(logger *slog.Logger, cfg config.ServeConfig) {
	if logger == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(cfg.LogFormat), "json") {
		logger.Info(
			"server starting",
			"event", "server starting",
			"listen", cfg.ListenAddr,
			"db_path", cfg.DBPath,
			"base_url", cfg.BaseURL,
			"log_level", cfg.LogLevel,
			"log_format", cfg.LogFormat,
			"trust_proxy_headers", cfg.TrustProxyHeaders,
		)
		return
	}
	logger.Info(
		"server starting",
		"event", "server starting",
		"listen", cfg.ListenAddr,
		"db_path", cfg.DBPath,
		"base_url", cfg.BaseURL,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"trust_proxy_headers", cfg.TrustProxyHeaders,
	)
	for _, kv := range []struct {
		key string
		val any
	}{
		{"CONTACTD_LISTEN_ADDR", cfg.ListenAddr},
		{"CONTACTD_DB_PATH", cfg.DBPath},
		{"CONTACTD_BASE_URL", cfg.BaseURL},
		{"CONTACTD_LOG_LEVEL", cfg.LogLevel},
		{"CONTACTD_LOG_FORMAT", cfg.LogFormat},
		{"CONTACTD_TRUST_PROXY_HEADERS", cfg.TrustProxyHeaders},
		{"CONTACTD_REQUEST_MAX_BYTES", cfg.RequestMaxBytes},
		{"CONTACTD_VCARD_MAX_BYTES", cfg.VCardMaxBytes},
		{"CONTACTD_FORCE_SEED", cfg.ForceSeed},
		{"CONTACTD_DEFAULT_BOOK_SLUG", cfg.DefaultBookSlug},
		{"CONTACTD_DEFAULT_BOOK_NAME", cfg.DefaultBookName},
		{"CONTACTD_CHANGE_RETENTION_DAYS", cfg.ChangeRetentionDays},
		{"CONTACTD_CHANGE_RETENTION_MAX_REVISIONS", cfg.ChangeRetentionMaxRevisions},
		{"CONTACTD_PRUNE_INTERVAL", cfg.PruneInterval},
		{"CONTACTD_ENABLE_ADDRESSBOOK_COLOR", cfg.EnableAddressbookColor},
		{"CONTACTD_AUTH_MAX_CONCURRENCY", cfg.AuthMaxConcurrency},
		{"CONTACTD_AUTH_FAIL_DELAY", cfg.AuthFailDelay},
	} {
		logger.Info("config", "event", "config", kv.key, kv.val)
	}
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
	for {
		trimmed := strings.TrimPrefix(msg, "load config: ")
		trimmed = strings.TrimPrefix(trimmed, "parse serve flags: ")
		if trimmed == msg {
			break
		}
		msg = trimmed
	}
	if msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

func extractDBPathForFatal(args []string, env map[string]string) string {
	cfg, err := config.LoadServeConfig(args, env)
	if err != nil {
		// Fallback to the known default if even config parsing failed.
		return config.DefaultDBPath
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		return config.DefaultDBPath
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
	// Prefer a generic "cannot open" diagnosis if we cannot reliably determine a filesystem cause.
	if strings.Contains(msg, "(14)") || strings.Contains(msg, "unable to open database file") {
		return fmt.Errorf("cannot open database %s: sqlite cannot open database file", dbPath)
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
		Authenticate: wrapAuthenticateWithFailureDelay(cfg.AuthFailDelay, wrapAuthenticateWithConcurrencyCap(cfg.AuthMaxConcurrency, func(ctx context.Context, username, password string) (string, bool, error) {
			ok, _, err := store.AuthenticateUser(ctx, username, password)
			if err != nil {
				return "", false, err
			}
			if !ok {
				return "", false, nil
			}
			return username, true, nil
		})),
	})

	return &serveRuntime{
		cfg:     cfg,
		store:   store,
		handler: h,
		logger:  logger,
	}, nil
}

func wrapAuthenticateWithConcurrencyCap(limit int, next func(context.Context, string, string) (string, bool, error)) func(context.Context, string, string) (string, bool, error) {
	if limit <= 0 || next == nil {
		return next
	}
	sem := make(chan struct{}, limit)
	return func(ctx context.Context, username, password string) (string, bool, error) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
		defer func() { <-sem }()
		return next(ctx, username, password)
	}
}

func wrapAuthenticateWithFailureDelay(delay time.Duration, next func(context.Context, string, string) (string, bool, error)) func(context.Context, string, string) (string, bool, error) {
	if delay <= 0 || next == nil {
		return next
	}
	return func(ctx context.Context, username, password string) (string, bool, error) {
		user, ok, err := next(ctx, username, password)
		if err != nil || ok {
			return user, ok, err
		}
		timer := time.NewTimer(delay)
		defer func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}()
		select {
		case <-timer.C:
			return user, ok, err
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
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

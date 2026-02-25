package daemon

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/db"
	"golang.org/x/crypto/bcrypt"
)

type fakeServeServer struct {
	listenAndServeFn func() error
	shutdownFn       func(context.Context) error
}

func (f *fakeServeServer) ListenAndServe() error {
	return f.listenAndServeFn()
}

func (f *fakeServeServer) Shutdown(ctx context.Context) error {
	return f.shutdownFn(ctx)
}

func TestPrepareServeRuntime_InvalidSeedHashFails(t *testing.T) {
	t.Parallel()

	_, err := prepareServeRuntime(context.Background(), nil, map[string]string{
		"CONTACTD_DB_PATH": "/tmp/invalid-seed-hash.sqlite",
		"CONTACTD_USERS":   "alice:not-a-bcrypt-hash",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("prepareServeRuntime error = nil, want error")
	}
}

func TestPrepareServeRuntime_SeedsEmptyDBAndDefaultAddressbook(t *testing.T) {
	t.Parallel()

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")

	rt, err := prepareServeRuntime(context.Background(), nil, map[string]string{
		"CONTACTD_DB_PATH": dbPath,
		"CONTACTD_USERS":   "alice:" + string(hash),
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepareServeRuntime: %v", err)
	}
	defer func() { _ = rt.close() }()

	ok, _, err := rt.store.AuthenticateUser(context.Background(), "alice", "secret")
	if err != nil {
		t.Fatalf("AuthenticateUser: %v", err)
	}
	if !ok {
		t.Fatal("seeded user did not authenticate")
	}

	hasBook, err := rt.store.HasAddressbook(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("HasAddressbook: %v", err)
	}
	if !hasBook {
		t.Fatal("default addressbook not created")
	}
}

func TestPrepareServeRuntime_DoesNotOverwriteExistingUsersWithoutForceSeed(t *testing.T) {
	t.Parallel()

	hash1, err := bcrypt.GenerateFromPassword([]byte("pw1"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword #1: %v", err)
	}
	hash2, err := bcrypt.GenerateFromPassword([]byte("pw2"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword #2: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")

	rt1, err := prepareServeRuntime(context.Background(), nil, map[string]string{
		"CONTACTD_DB_PATH": dbPath,
		"CONTACTD_USERS":   "alice:" + string(hash1),
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepareServeRuntime first: %v", err)
	}
	if err := rt1.close(); err != nil {
		t.Fatalf("close first runtime: %v", err)
	}

	rt2, err := prepareServeRuntime(context.Background(), nil, map[string]string{
		"CONTACTD_DB_PATH": dbPath,
		"CONTACTD_USERS":   "alice:" + string(hash2),
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepareServeRuntime second: %v", err)
	}
	defer func() { _ = rt2.close() }()

	okOld, _, err := rt2.store.AuthenticateUser(context.Background(), "alice", "pw1")
	if err != nil {
		t.Fatalf("AuthenticateUser old password: %v", err)
	}
	if !okOld {
		t.Fatal("old password should still authenticate after restart without force-seed")
	}

	okNew, _, err := rt2.store.AuthenticateUser(context.Background(), "alice", "pw2")
	if err != nil {
		t.Fatalf("AuthenticateUser new password: %v", err)
	}
	if okNew {
		t.Fatal("new password should not authenticate without force-seed overwrite")
	}
}

func TestServeHTTPGracefully_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenResult := make(chan error, 1)
	shutdownCalled := make(chan context.Context, 1)
	srv := &fakeServeServer{
		listenAndServeFn: func() error {
			return <-listenResult
		},
		shutdownFn: func(ctx context.Context) error {
			shutdownCalled <- ctx
			listenResult <- http.ErrServerClosed
			return nil
		},
	}

	done := make(chan int, 1)
	go func() {
		done <- serveHTTPGracefully(runCtx, srv, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	}()

	cancel()

	var shutdownCtx context.Context
	select {
	case shutdownCtx = <-shutdownCalled:
	case <-time.After(2 * time.Second):
	}
	if shutdownCtx == nil {
		t.Fatal("Shutdown was not called after context cancel")
	}
	if _, ok := shutdownCtx.Deadline(); !ok {
		t.Fatal("Shutdown context missing deadline")
	}

	if got, want := <-done, 0; got != want {
		t.Fatalf("serveHTTPGracefully exit code = %d, want %d", got, want)
	}
}

func TestServeHTTPGracefully_ListenFailureReturns1(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("bind failed")
	shutdownCalled := false
	srv := &fakeServeServer{
		listenAndServeFn: func() error { return wantErr },
		shutdownFn: func(context.Context) error {
			shutdownCalled = true
			return nil
		},
	}

	got := serveHTTPGracefully(context.Background(), srv, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if got != 1 {
		t.Fatalf("serveHTTPGracefully listen failure exit code = %d, want 1", got)
	}
	if shutdownCalled {
		t.Fatal("Shutdown should not be called when listen fails immediately without cancellation")
	}
}

func TestNewHTTPServer_ConfiguresTimeouts(t *testing.T) {
	t.Parallel()

	srv := newHTTPServer(":8080", http.NewServeMux())

	if got, want := srv.ReadHeaderTimeout, 5*time.Second; got != want {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", got, want)
	}
	if got, want := srv.ReadTimeout, 30*time.Second; got != want {
		t.Fatalf("ReadTimeout = %v, want %v", got, want)
	}
	if got, want := srv.WriteTimeout, 120*time.Second; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}
	if got, want := srv.IdleTimeout, 120*time.Second; got != want {
		t.Fatalf("IdleTimeout = %v, want %v", got, want)
	}
}

func TestNewServeLogger_FormatAndLevel(t *testing.T) {
	t.Parallel()

	var jsonBuf bytes.Buffer
	jsonLogger := newServeLogger("json", "info", &jsonBuf)
	jsonLogger.Info("request", "event", "request", "path", "/health")
	if got := jsonBuf.String(); !strings.Contains(got, `"event":"request"`) {
		t.Fatalf("json logger output missing JSON fields: %q", got)
	}

	var textBuf bytes.Buffer
	textLogger := newServeLogger("text", "warn", &textBuf)
	textLogger.Info("request", "event", "request")
	if got := strings.TrimSpace(textBuf.String()); got != "" {
		t.Fatalf("warn-level text logger should suppress info logs, got %q", got)
	}
	textLogger.Warn("request", "event", "request")
	if got := textBuf.String(); !strings.Contains(got, "contactd[") || !strings.Contains(got, "warning: request") {
		t.Fatalf("text logger output not in daemon/syslog style: %q", got)
	}
	if strings.Contains(textBuf.String(), "event=") || strings.Contains(textBuf.String(), "level=") {
		t.Fatalf("text logger output should not use slog key=value text format: %q", textBuf.String())
	}
}

func TestServeHTTPGracefully_LogsShutdownAndStopped(t *testing.T) {
	t.Parallel()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs bytes.Buffer
	listenResult := make(chan error, 1)
	srv := &fakeServeServer{
		listenAndServeFn: func() error { return <-listenResult },
		shutdownFn: func(context.Context) error {
			listenResult <- http.ErrServerClosed
			return nil
		},
	}

	done := make(chan int, 1)
	go func() {
		done <- serveHTTPGracefully(runCtx, srv, newServeLogger("text", "info", &logs))
	}()
	cancel()

	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("serveHTTPGracefully exit code = %d, want 0", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveHTTPGracefully did not return")
	}

	out := logs.String()
	if !strings.Contains(out, "server shutdown") {
		t.Fatalf("logs missing server shutdown event: %q", out)
	}
	if !strings.Contains(out, "server stopped") {
		t.Fatalf("logs missing server stopped event: %q", out)
	}
}

func TestHumanizeDBOpenError_SQLiteCantOpen14_Normalized(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.db")
	err := humanizeDBOpenError(dbPath, errors.New(`apply pragma "PRAGMA foreign_keys = ON;": unable to open database file: out of memory (14)`))
	if got := err.Error(); got != fmt.Sprintf("cannot open database %s: sqlite cannot open database file", dbPath) {
		t.Fatalf("humanizeDBOpenError = %q", got)
	}
}

func TestHumanizeDBOpenError_NoSuchDirectory(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "missing", "contactd.sqlite")
	err := humanizeDBOpenError(dbPath, errors.New(`open db: unable to open database file`))
	want := fmt.Sprintf("cannot open database %s: no such file or directory", dbPath)
	if err == nil || err.Error() != want {
		t.Fatalf("humanizeDBOpenError missing-dir = %v, want %q", err, want)
	}
}

func TestHumanizeDBOpenError_PermissionDenied(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	blocked := filepath.Join(base, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatalf("MkdirAll blocked: %v", err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatalf("Chmod blocked: %v", err)
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }()

	dbPath := filepath.Join(blocked, "child", "contactd.sqlite")
	err := humanizeDBOpenError(dbPath, errors.New(`open db: unable to open database file`))
	if err == nil {
		t.Fatal("humanizeDBOpenError permission branch returned nil")
	}
	got := err.Error()
	if strings.Contains(got, "permission denied") {
		return
	}
	// Some environments (or elevated users) may bypass the permission failure and fall back to generic/no-such-dir handling.
	t.Skipf("permission branch not observable in this environment, got %q", got)
}

func TestHumanizeServeFatalError_StripsKnownPrefixesRegardlessOrder(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.db")
	baseErr := errors.New("flag provided but not defined: -bogus")

	gotA := humanizeServeFatalError(dbPath, fmt.Errorf("load config: parse serve flags: %w", baseErr))
	if gotA == nil || gotA.Error() != baseErr.Error() {
		t.Fatalf("strip prefixes (load->parse) = %v, want %q", gotA, baseErr.Error())
	}

	gotB := humanizeServeFatalError(dbPath, fmt.Errorf("parse serve flags: load config: %w", baseErr))
	if gotB == nil || gotB.Error() != baseErr.Error() {
		t.Fatalf("strip prefixes (parse->load) = %v, want %q", gotB, baseErr.Error())
	}
}

func TestExtractDBPathForFatal(t *testing.T) {
	t.Parallel()

	t.Run("parse_failure_falls_back_to_default", func(t *testing.T) {
		got := extractDBPathForFatal([]string{"--bogus"}, map[string]string{})
		if got != config.DefaultDBPath {
			t.Fatalf("extractDBPathForFatal(parse fail) = %q, want %q", got, config.DefaultDBPath)
		}
	})

	t.Run("configured_db_path", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "contactd.sqlite")
		got := extractDBPathForFatal(nil, map[string]string{"CONTACTD_DB_PATH": want})
		if got != want {
			t.Fatalf("extractDBPathForFatal(configured) = %q, want %q", got, want)
		}
	})

	t.Run("empty_env_value_keeps_default", func(t *testing.T) {
		got := extractDBPathForFatal(nil, map[string]string{"CONTACTD_DB_PATH": "   "})
		if got != config.DefaultDBPath {
			t.Fatalf("extractDBPathForFatal(empty env) = %q, want %q", got, config.DefaultDBPath)
		}
	})
}

func TestHumanizeServeFatalError_NilPassthrough(t *testing.T) {
	t.Parallel()

	if got := humanizeServeFatalError(filepath.Join(t.TempDir(), "contactd.sqlite"), nil); got != nil {
		t.Fatalf("humanizeServeFatalError(nil) = %v, want nil", got)
	}
}

func TestLogging_Format_TextAndJSON(t *testing.T) {
	t.Parallel()

	var textBuf bytes.Buffer
	newServeLogger("text", "info", &textBuf).Info("request", "event", "request")
	if got := textBuf.String(); !strings.Contains(got, "contactd[") || !strings.Contains(got, ": request") {
		t.Fatalf("text logger output not daemon/syslog style: %q", got)
	}
	if strings.Contains(textBuf.String(), "event=") {
		t.Fatalf("text logger should suppress duplicate event attr in text mode: %q", textBuf.String())
	}

	var jsonBuf bytes.Buffer
	newServeLogger("json", "info", &jsonBuf).Info("request", "event", "request")
	if got := jsonBuf.String(); !strings.Contains(got, `"event":"request"`) {
		t.Fatalf("json logger output missing json field: %q", got)
	}
}

func TestStartConfiguredPruneLoop_NilStoreIsNoop(t *testing.T) {
	t.Parallel()

	stop := startConfiguredPruneLoop(context.Background(), nil, config.ServeConfig{PruneInterval: time.Millisecond}, nil)
	stop()
}

func TestServeHTTPGracefully_ListenFailureLogsEvent(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	srv := &fakeServeServer{
		listenAndServeFn: func() error { return errors.New("bind failed") },
		shutdownFn:       func(context.Context) error { return nil },
	}

	code := serveHTTPGracefully(context.Background(), srv, newServeLogger("text", "info", &logs))
	if code != 1 {
		t.Fatalf("serveHTTPGracefully code = %d, want 1", code)
	}
	out := logs.String()
	if !strings.Contains(out, "error: listen failed") {
		t.Fatalf("logs missing listen failed event: %q", out)
	}
}

func TestPrepareServeRuntime_LogsDBErrorOnOpenFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	_, err := prepareServeRuntime(context.Background(), nil, map[string]string{
		"CONTACTD_DB_PATH": t.TempDir(), // opening a directory path as sqlite DB should fail
	}, &logs)
	if err == nil {
		t.Fatal("prepareServeRuntime error=nil, want open db error")
	}
	if got := logs.String(); !strings.Contains(got, "error: db error") {
		t.Fatalf("logs missing db error event: %q", got)
	}
}

func TestStartBackgroundPruneLoop_DisabledWhenIntervalZero(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	stop := startBackgroundPruneLoop(context.Background(), 0, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), func(context.Context) error {
		calls.Add(1)
		return nil
	})
	defer stop()

	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("prune loop calls = %d, want 0", got)
	}
}

func TestStartBackgroundPruneLoop_RunsOnTicker(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startBackgroundPruneLoop(ctx, 10*time.Millisecond, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), func(context.Context) error {
		calls.Add(1)
		return nil
	})
	defer stop()

	deadline := time.After(300 * time.Millisecond)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("background prune loop did not run")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestPruneOnce_AppliesRetentionConfig(t *testing.T) {
	t.Parallel()

	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "prune.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(context.Background(), "alice", "bcrypt")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: bookID,
		Href:          "a.vcf",
		UID:           "uid-a",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-a\nFN:A\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("PutCard: %v", err)
	}
	if err := store.ForceCardChangesTimestamp(context.Background(), bookID, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("ForceCardChangesTimestamp: %v", err)
	}

	var logBuf bytes.Buffer
	cfg := config.ServeConfig{
		ChangeRetentionDays:         1,
		ChangeRetentionMaxRevisions: 0,
	}
	if err := pruneOnce(context.Background(), store, cfg, slog.New(slog.NewTextHandler(&logBuf, nil)), "ticker"); err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	count, err := store.CardChangeCount(context.Background(), bookID)
	if err != nil {
		t.Fatalf("CardChangeCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("CardChangeCount after prune = %d, want 0", count)
	}
	if got := logBuf.String(); !strings.Contains(got, "event=\"changes pruned\"") {
		t.Fatalf("pruneOnce log missing changes pruned event: %q", got)
	}
}

func TestDeferredLogWriter_BuffersUntilActivateThenForwards(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	w := newDeferredLogWriter(&sink)

	if _, err := w.Write([]byte("startup-1\n")); err != nil {
		t.Fatalf("Write startup: %v", err)
	}
	if got := sink.String(); got != "" {
		t.Fatalf("sink before activate = %q, want empty", got)
	}

	if err := w.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got, want := sink.String(), "startup-1\n"; got != want {
		t.Fatalf("sink after activate = %q, want %q", got, want)
	}

	if _, err := w.Write([]byte("runtime-1\n")); err != nil {
		t.Fatalf("Write runtime: %v", err)
	}
	if got, want := sink.String(), "startup-1\nruntime-1\n"; got != want {
		t.Fatalf("sink after runtime write = %q, want %q", got, want)
	}

	if err := w.Activate(); err != nil {
		t.Fatalf("second Activate: %v", err)
	}
	if got, want := sink.String(), "startup-1\nruntime-1\n"; got != want {
		t.Fatalf("sink after second activate = %q, want no duplicates", got)
	}
}

func TestSeedUser_ForceTrueUpdatesPasswordHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "seed-force.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "old-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	oldHash, err := bcrypt.GenerateFromPassword([]byte("oldpw"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword old: %v", err)
	}
	if err := store.SetUserPasswordHash(ctx, userID, string(oldHash)); err != nil {
		t.Fatalf("SetUserPasswordHash old: %v", err)
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte("newpw"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword new: %v", err)
	}
	cfg := config.ServeConfig{
		DefaultBookSlug: "contacts",
		DefaultBookName: "Contacts",
	}
	if _, _, err := store.EnsureAddressbook(ctx, userID, cfg.DefaultBookSlug, cfg.DefaultBookName); err != nil {
		t.Fatalf("EnsureAddressbook preseed: %v", err)
	}

	if err := seedUser(ctx, store, cfg, config.SeedUser{Username: "alice", PasswordHash: string(newHash)}, true); err != nil {
		t.Fatalf("seedUser(force=true): %v", err)
	}

	gotID, err := store.UserIDByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("UserIDByUsername: %v", err)
	}
	if gotID != userID {
		t.Fatalf("user ID changed = %d, want %d", gotID, userID)
	}
	okOld, _, err := store.AuthenticateUser(ctx, "alice", "oldpw")
	if err != nil {
		t.Fatalf("AuthenticateUser oldpw: %v", err)
	}
	if okOld {
		t.Fatal("old password still authenticates after force reseed")
	}
	okNew, _, err := store.AuthenticateUser(ctx, "alice", "newpw")
	if err != nil {
		t.Fatalf("AuthenticateUser newpw: %v", err)
	}
	if !okNew {
		t.Fatal("new password does not authenticate after force reseed")
	}
}

func TestPrepareServeRuntime_SyncTokenContinuesAcrossRestart(t *testing.T) {
	t.Parallel()

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{
		"CONTACTD_DB_PATH": dbPath,
		"CONTACTD_USERS":   "alice:" + string(hash),
	}

	rt1, err := prepareServeRuntime(context.Background(), nil, env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepareServeRuntime first: %v", err)
	}
	if err := putCardForTest(rt1.handler, "/alice/contacts/a.vcf", "uid-a", "Alice A"); err != nil {
		_ = rt1.close()
		t.Fatalf("put first card: %v", err)
	}
	token1, body1, err := syncReportForTest(rt1.handler, "")
	if err != nil {
		_ = rt1.close()
		t.Fatalf("initial sync report: %v body=%q", err, body1)
	}
	if token1 == "" {
		_ = rt1.close()
		t.Fatalf("initial sync token empty body=%q", body1)
	}
	if err := rt1.close(); err != nil {
		t.Fatalf("close first runtime: %v", err)
	}

	rt2, err := prepareServeRuntime(context.Background(), nil, env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepareServeRuntime second: %v", err)
	}
	defer func() { _ = rt2.close() }()

	if err := putCardForTest(rt2.handler, "/alice/contacts/b.vcf", "uid-b", "Bob B"); err != nil {
		t.Fatalf("put second card after restart: %v", err)
	}

	token2, body2, err := syncReportForTest(rt2.handler, token1)
	if err != nil {
		t.Fatalf("delta sync after restart: %v body=%q", err, body2)
	}
	if token2 == "" {
		t.Fatalf("delta sync token empty body=%q", body2)
	}
	if strings.Contains(body2, "valid-sync-token") {
		t.Fatalf("delta sync after restart returned invalid token error body=%q", body2)
	}
	if !strings.Contains(body2, "/alice/contacts/b.vcf") {
		t.Fatalf("delta sync after restart missing new href body=%q", body2)
	}
}

func putCardForTest(h http.Handler, path, uid, fn string) error {
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(testVCard(uid, fn)))
	req.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusNoContent {
		return errors.New("unexpected PUT status " + rr.Result().Status + " body=" + rr.Body.String())
	}
	return nil
}

func syncReportForTest(h http.Handler, token string) (string, string, error) {
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusMultiStatus {
		return "", body, errors.New("unexpected REPORT status " + rr.Result().Status)
	}
	var doc struct {
		SyncToken string `xml:"sync-token"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		return "", body, err
	}
	return strings.TrimSpace(doc.SyncToken), body, nil
}

func testVCard(uid, fn string) string {
	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nEND:VCARD\r\n"
}

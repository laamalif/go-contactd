package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestNewServeLogger_FormatAndLevel(t *testing.T) {
	t.Parallel()

	var jsonBuf bytes.Buffer
	jsonLogger := newServeLogger("json", "info", &jsonBuf)
	jsonLogger.Info("request", "event", "request", "path", "/healthz")
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

func TestRunMain_Version_NoDaemonAccessLogs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version"}, map[string]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runMain(version) code = %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "event=") || strings.Contains(stderr.String(), "\"event\"") {
		t.Fatalf("version command wrote daemon-style logs to stderr: %q", stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got == "" {
		t.Fatalf("version command stdout empty")
	}
}

func TestRunMain_DaemonRootHelp(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"--help"},
		{"-h"},
		{"help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMain(args, map[string]string{}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMain(%v) code = %d, want 0 stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: go-contactd [flags]") {
				t.Fatalf("stdout missing root usage: %q", out)
			}
			if !strings.Contains(out, "--listen-addr") || !strings.Contains(out, "--db-path") || !strings.Contains(out, "contactctl user") {
				t.Fatalf("stdout missing daemon/help details: %q", out)
			}
			for _, forbidden := range []string{"--listen addr", "--bind addr", "--addr addr", "--db path"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("stdout should not advertise daemon alias %q: %q", forbidden, out)
				}
			}
			for _, wantEnv := range []string{
				"CONTACTD_LISTEN_ADDR",
				"PORT",
				"CONTACTD_DB_PATH",
				"CONTACTD_BASE_URL",
				"CONTACTD_LOG_LEVEL",
				"CONTACTD_LOG_FORMAT",
				"CONTACTD_TRUST_PROXY_HEADERS",
				"CONTACTD_REQUEST_MAX_BYTES",
				"CONTACTD_VCARD_MAX_BYTES",
				"CONTACTD_FORCE_SEED",
				"CONTACTD_USERS",
				"CONTACTD_USER_*",
				"CONTACTD_DEFAULT_BOOK_SLUG",
				"CONTACTD_DEFAULT_BOOK_NAME",
				"CONTACTD_CHANGE_RETENTION_DAYS",
				"CONTACTD_CHANGE_RETENTION_MAX_REVISIONS",
				"CONTACTD_PRUNE_INTERVAL",
				"CONTACTD_ENABLE_ADDRESSBOOK_COLOR",
			} {
				if !strings.Contains(out, wantEnv) {
					t.Fatalf("stdout missing environment var %q: %q", wantEnv, out)
				}
			}
			if got := stderr.String(); got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
		})
	}
}

func TestRunMain_ContactctlUserHelpFlagsAndSubcommand(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"user", "--help"},
		{"user", "-h"},
		{"user", "help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput("contactctl", args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMainProgramWithInput(contactctl,%v) code = %d, want 0 stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: contactctl user <add|list|delete|passwd>") {
				t.Fatalf("stdout missing user usage: %q", out)
			}
			if !strings.Contains(out, "password-stdin") {
				t.Fatalf("stdout missing password-stdin hint: %q", out)
			}
			if got := stderr.String(); got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
		})
	}
}

func TestRunMain_ServeHelpFlagPrintsHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"serve", "--help"}, {"serve", "-h"}} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runMain(args, map[string]string{}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMain(%v) code = %d, want 0; stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: go-contactd [flags]") {
				t.Fatalf("stdout missing serve usage: %q", out)
			}
			if !strings.Contains(out, "--listen-addr") || !strings.Contains(out, "--port") || !strings.Contains(out, "--db-path") {
				t.Fatalf("stdout missing expected serve flags: %q", out)
			}
			if strings.Contains(out, "--listen addr") || strings.Contains(out, "--db path") {
				t.Fatalf("stdout should not advertise daemon aliases: %q", out)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunMain_NoArgsDefaultsToServe(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain(nil, map[string]string{"CONTACTD_DB_PATH": t.TempDir()}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain(no args) code = %d, want 2 startup failure from serve path", code)
	}
	if got := stderr.String(); !strings.Contains(got, "go-contactd:") {
		t.Fatalf("stderr = %q, want daemon-style fatal error prefix", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunMain_FlagArgsDispatchToServe(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-d", t.TempDir()}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain(-d <dir>) code = %d, want 2 startup failure from serve path", code)
	}
	if got := stderr.String(); !strings.Contains(got, "go-contactd:") {
		t.Fatalf("stderr = %q, want daemon-style fatal error prefix", got)
	}
}

func TestRunMain_StartupFailure_NoDuplicateStructuredLog(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-d", t.TempDir()}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain startup failure code = %d, want 2", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "go-contactd: cannot open database ") {
		t.Fatalf("stderr = %q, want daemon-style open db error", out)
	}
	if strings.Contains(out, `event="db error"`) || strings.Contains(out, `level=ERROR`) {
		t.Fatalf("stderr should not include structured startup log duplicate, got %q", out)
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

func TestRunMain_ContactctlHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"--help"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("contactctl --help code = %d, want 0 stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "usage: contactctl user <add|list|delete|passwd>") {
		t.Fatalf("contactctl help stdout = %q, want user usage", got)
	}
}

func TestLogging_CLI_NoDaemonAccessLogs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version"}, map[string]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runMain(version) code = %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "event=") || strings.Contains(stderr.String(), "\"event\"") {
		t.Fatalf("version command wrote daemon-style logs to stderr: %q", stderr.String())
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

func TestRunMain_Version_FormatTextAndJSON(t *testing.T) {
	t.Parallel()

	origVersion, origCommit, origBuildDate := version, commit, buildDate
	version, commit, buildDate = "v0.1.0", "abc1234", "2026-02-24"
	defer func() {
		version, commit, buildDate = origVersion, origCommit, origBuildDate
	}()

	var textOut, textErr bytes.Buffer
	code := runMain([]string{"version", "--format", "text"}, map[string]string{}, &textOut, &textErr)
	if code != 0 {
		t.Fatalf("version --format text code = %d, want 0 stderr=%q", code, textErr.String())
	}
	if got := textOut.String(); !strings.Contains(got, "go-contactd v0.1.0") || !strings.Contains(got, "commit abc1234") || !strings.Contains(got, "built 2026-02-24") {
		t.Fatalf("version text output missing metadata: %q", got)
	}

	var jsonOut, jsonErr bytes.Buffer
	code = runMain([]string{"version", "--format", "json"}, map[string]string{}, &jsonOut, &jsonErr)
	if code != 0 {
		t.Fatalf("version --format json code = %d, want 0 stderr=%q", code, jsonErr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &doc); err != nil {
		t.Fatalf("json.Unmarshal version output: %v; out=%q", err, jsonOut.String())
	}
	if got, want := doc["version"], "v0.1.0"; got != want {
		t.Fatalf("version json field = %#v, want %q", got, want)
	}
	if got, want := doc["commit"], "abc1234"; got != want {
		t.Fatalf("commit json field = %#v, want %q", got, want)
	}
	if got, want := doc["build_date"], "2026-02-24"; got != want {
		t.Fatalf("build_date json field = %#v, want %q", got, want)
	}
}

func TestRunMain_Version_InvalidFormatReturns2(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version", "--format", "yaml"}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("version invalid format code = %d, want 2", code)
	}
	if got := stderr.String(); !strings.Contains(got, "invalid --format") {
		t.Fatalf("stderr missing invalid format error: %q", got)
	}
}

func TestRunMainProgram_Version_UsesProgramNameInTextOutput(t *testing.T) {
	origVersion, origCommit, origBuildDate := version, commit, buildDate
	version, commit, buildDate = "v0.1.0", "abc1234", "2026-02-24"
	defer func() {
		version, commit, buildDate = origVersion, origCommit, origBuildDate
	}()

	tests := []struct {
		name string
		prog string
		args []string
		want string
	}{
		{name: "daemon_short_flag", prog: "contactd", args: []string{"-V"}, want: "contactd v0.1.0"},
		{name: "admin_short_flag", prog: "contactctl", args: []string{"-V"}, want: "contactctl v0.1.0"},
		{name: "daemon_version_subcommand", prog: "contactd", args: []string{"version"}, want: "contactd v0.1.0"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput(tt.prog, tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMainProgramWithInput(%q,%v) code = %d, want 0 stderr=%q", tt.prog, tt.args, code, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, tt.want) {
				t.Fatalf("stdout = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestRunMainProgram_DaemonUnknownCommand_IsDaemonStyle(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactd", []string{"server"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "contactd: unknown command: server") {
		t.Fatalf("stderr = %q, want daemon-style unknown command", errOut)
	}
	if strings.Contains(errOut, "compat alias") {
		t.Fatalf("stderr = %q, want no compat alias hint", errOut)
	}
}

func TestRunMainProgram_AdminUnknownCommand_HasProgramPrefix(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"wat"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "contactctl: unknown command: wat") {
		t.Fatalf("stderr = %q, want contactctl-prefixed unknown command", got)
	}
}

func TestRunMainProgram_AdminUserUnknownSubcommand_HasProgramPrefix(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"user", "wat"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "contactctl: unknown user subcommand: wat") {
		t.Fatalf("stderr = %q, want contactctl-prefixed user subcommand error", got)
	}
}

func TestRunMainProgram_DaemonFlagErrors_AreFlattenedForUsers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		notWant []string
	}{
		{
			name:    "unknown_flag",
			args:    []string{"--bogus"},
			want:    "flag provided but not defined",
			notWant: []string{"load config:", "parse serve flags:"},
		},
		{
			name:    "missing_arg",
			args:    []string{"-d"},
			want:    "flag needs an argument: -d",
			notWant: []string{"load config:", "parse serve flags:"},
		},
		{
			name:    "invalid_port_range",
			args:    []string{"-p", "0"},
			want:    "--port must be",
			notWant: []string{"load config:", "parse serve flags:"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput("contactd", tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			got := stderr.String()
			if !strings.Contains(got, "contactd: ") {
				t.Fatalf("stderr = %q, want daemon prefix", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("stderr = %q, want substring %q", got, tt.want)
			}
			for _, s := range tt.notWant {
				if strings.Contains(got, s) {
					t.Fatalf("stderr = %q, should not contain %q", got, s)
				}
			}
		})
	}
}

func TestRunMainProgram_VersionHelp_UsesProgramName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		prog string
		args []string
		want string
	}{
		{prog: "contactd", args: []string{"version", "--help"}, want: "usage: contactd version [--format text|json]"},
		{prog: "contactctl", args: []string{"version", "--help"}, want: "usage: contactctl version [--format text|json]"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.prog, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput(tt.prog, tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, want 0 stderr=%q", code, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, tt.want) {
				t.Fatalf("stdout = %q, want %q", got, tt.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
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

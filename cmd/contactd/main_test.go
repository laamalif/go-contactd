package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

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

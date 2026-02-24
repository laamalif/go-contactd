package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

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

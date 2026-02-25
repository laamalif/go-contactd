package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticateUser_MissingUserStillPerformsBcryptCompare(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if _, err := store.CreateUser(ctx, "alice", string(hash)); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	orig := bcryptCompareHashAndPassword
	t.Cleanup(func() { bcryptCompareHashAndPassword = orig })

	calls := 0
	bcryptCompareHashAndPassword = func(hashedPassword, password []byte) error {
		calls++
		return bcrypt.ErrMismatchedHashAndPassword
	}

	ok, id, err := store.AuthenticateUser(ctx, "nosuch", "wrong")
	if err != nil {
		t.Fatalf("AuthenticateUser missing: %v", err)
	}
	if ok || id != 0 {
		t.Fatalf("AuthenticateUser missing = (%v,%d), want (false,0)", ok, id)
	}
	if calls != 1 {
		t.Fatalf("bcrypt compare calls = %d, want 1", calls)
	}
}

func TestAuthenticateUser_MissingUserBcryptErrorPropagates(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	orig := bcryptCompareHashAndPassword
	t.Cleanup(func() { bcryptCompareHashAndPassword = orig })

	wantErr := errors.New("bcrypt boom")
	bcryptCompareHashAndPassword = func(hashedPassword, password []byte) error {
		return wantErr
	}

	ok, id, err := store.AuthenticateUser(ctx, "nosuch", "wrong")
	if err == nil {
		t.Fatal("AuthenticateUser missing err = nil, want error")
	}
	if ok || id != 0 {
		t.Fatalf("AuthenticateUser missing = (%v,%d), want (false,0)", ok, id)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

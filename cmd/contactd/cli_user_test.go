package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laamalif/go-contactd/internal/db"
)

func TestCLI_UserAddAndListAndPasswdAndDelete(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{"CONTACTD_DB_PATH": dbPath}

	code, stdout, stderr := runCLI(t, []string{"user", "add", "--username", "alice", "--password", "pw1"}, env)
	if code != 0 {
		t.Fatalf("user add code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "alice") {
		t.Fatalf("user add stdout = %q, want username", stdout)
	}

	code, stdout, stderr = runCLI(t, []string{"user", "list", "--format", "table"}, env)
	if code != 0 {
		t.Fatalf("user list code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "USERNAME") {
		t.Fatalf("user list stdout = %q, want headers", stdout)
	}
	if !strings.Contains(stdout, "alice") {
		t.Fatalf("user list stdout = %q, want alice row", stdout)
	}

	code, stdout, stderr = runCLI(t, []string{"user", "passwd", "--username", "alice", "--password", "pw2"}, env)
	if code != 0 {
		t.Fatalf("user passwd code = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout == "" {
		t.Fatal("user passwd stdout empty")
	}

	store := openStoreForCLIAssert(t, dbPath)
	defer func() { _ = store.Close() }()
	okOld, _, err := store.AuthenticateUser(context.Background(), "alice", "pw1")
	if err != nil {
		t.Fatalf("AuthenticateUser old: %v", err)
	}
	if okOld {
		t.Fatal("old password still authenticates after passwd")
	}
	okNew, _, err := store.AuthenticateUser(context.Background(), "alice", "pw2")
	if err != nil {
		t.Fatalf("AuthenticateUser new: %v", err)
	}
	if !okNew {
		t.Fatal("new password does not authenticate after passwd")
	}

	code, stdout, stderr = runCLI(t, []string{"user", "delete", "--username", "alice"}, env)
	if code != 0 {
		t.Fatalf("user delete code = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout == "" {
		t.Fatal("user delete stdout empty")
	}

	ok, _, err := store.AuthenticateUser(context.Background(), "alice", "pw2")
	if err != nil {
		t.Fatalf("AuthenticateUser after delete: %v", err)
	}
	if ok {
		t.Fatal("deleted user still authenticates")
	}
}

func TestCLI_UserAdd_InvalidUsernameRejected(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{"CONTACTD_DB_PATH": dbPath}

	code, _, stderr := runCLI(t, []string{"user", "add", "--username", "Alice!", "--password", "pw"}, env)
	if code != 2 {
		t.Fatalf("user add invalid username code = %d, want 2", code)
	}
	if !strings.Contains(strings.ToLower(stderr), "username") {
		t.Fatalf("stderr = %q, want username validation error", stderr)
	}
}

func TestCLI_UserDelete_NotFoundExitCode3(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{"CONTACTD_DB_PATH": dbPath}

	code, _, stderr := runCLI(t, []string{"user", "delete", "--username", "missing"}, env)
	if code != 3 {
		t.Fatalf("user delete missing code = %d, want 3", code)
	}
	if !strings.Contains(strings.ToLower(stderr), "not found") {
		t.Fatalf("stderr = %q, want not found", stderr)
	}
}

func TestCLI_UserAddAndPasswd_PasswordStdin(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{"CONTACTD_DB_PATH": dbPath}

	code, stdout, stderr := runCLIWithInput(t, []string{"user", "add", "--username", "alice", "--password-stdin"}, env, "pw1\n")
	if code != 0 {
		t.Fatalf("user add --password-stdin code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "alice") {
		t.Fatalf("user add stdout = %q, want username", stdout)
	}

	code, stdout, stderr = runCLIWithInput(t, []string{"user", "passwd", "--username", "alice", "--password-stdin"}, env, "pw2\n")
	if code != 0 {
		t.Fatalf("user passwd --password-stdin code = %d, want 0; stderr=%q", code, stderr)
	}
	if stdout == "" {
		t.Fatal("user passwd stdout empty")
	}

	store := openStoreForCLIAssert(t, dbPath)
	defer func() { _ = store.Close() }()
	okNew, _, err := store.AuthenticateUser(context.Background(), "alice", "pw2")
	if err != nil {
		t.Fatalf("AuthenticateUser new: %v", err)
	}
	if !okNew {
		t.Fatal("new password from stdin does not authenticate")
	}
}

func TestCLI_UserAdd_PasswordFlagsConflictRejected(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{"CONTACTD_DB_PATH": dbPath}

	code, _, stderr := runCLIWithInput(t, []string{"user", "add", "--username", "alice", "--password", "pw", "--password-stdin"}, env, "pw\n")
	if code != 2 {
		t.Fatalf("user add conflicting password flags code = %d, want 2", code)
	}
	if !strings.Contains(strings.ToLower(stderr), "exactly one") {
		t.Fatalf("stderr = %q, want xor validation error", stderr)
	}
}

func TestCLI_UserList_DBAlias(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	env := map[string]string{}

	code, _, stderr := runCLI(t, []string{"user", "add", "--db", dbPath, "--username", "alice", "--password", "pw1"}, env)
	if code != 0 {
		t.Fatalf("user add --db code = %d, want 0; stderr=%q", code, stderr)
	}

	code, stdout, stderr := runCLI(t, []string{"user", "list", "--db", dbPath, "--format", "json"}, env)
	if code != 0 {
		t.Fatalf("user list --db code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, `"username":"alice"`) {
		t.Fatalf("user list --db stdout = %q, want alice", stdout)
	}
}

func runCLI(t *testing.T, args []string, env map[string]string) (code int, stdout string, stderr string) {
	t.Helper()
	return runCLIWithInput(t, args, env, "")
}

func runCLIWithInput(t *testing.T, args []string, env map[string]string, stdin string) (code int, stdout string, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = runMainWithInput(args, env, bytes.NewBufferString(stdin), &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

func openStoreForCLIAssert(t *testing.T, path string) *db.Store {
	t.Helper()
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store
}

package ctl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laamalif/go-contactd/internal/db"
)

func TestRunCLI_ExportConcat_WritesStoredVCardBytes(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--book", "contacts",
		"--format", "concat",
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}

	want := "BEGIN:VCARD\r\nFN:Alpha\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nFN:Beta\r\nEND:VCARD\r\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout=%q want %q", got, want)
	}
}

func TestRunCLI_ExportDir_WritesVCardFiles(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)
	outDir := filepath.Join(t.TempDir(), "out")

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--book", "contacts",
		"--format", "dir",
		"--out", outDir,
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "cards=2") {
		t.Fatalf("stdout=%q want cards summary", got)
	}

	gotA, err := os.ReadFile(filepath.Join(outDir, "a.vcf"))
	if err != nil {
		t.Fatalf("ReadFile a.vcf: %v", err)
	}
	if string(gotA) != "BEGIN:VCARD\r\nFN:Alpha\r\nEND:VCARD\r\n" {
		t.Fatalf("a.vcf=%q", string(gotA))
	}
	gotB, err := os.ReadFile(filepath.Join(outDir, "b.vcf"))
	if err != nil {
		t.Fatalf("ReadFile b.vcf: %v", err)
	}
	if string(gotB) != "BEGIN:VCARD\r\nFN:Beta\r\nEND:VCARD\r\n" {
		t.Fatalf("b.vcf=%q", string(gotB))
	}
}

func seedExportTestDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "contactd.sqlite")
	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(context.Background(), "alice", "test-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	abID, _, err := store.EnsureAddressbook(context.Background(), userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("EnsureAddressbook: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: abID,
		Href:          "b.vcf",
		UID:           "uid-b",
		VCard:         []byte("BEGIN:VCARD\r\nFN:Beta\r\nEND:VCARD\r\n"),
	}); err != nil {
		t.Fatalf("PutCard b: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: abID,
		Href:          "a.vcf",
		UID:           "uid-a",
		VCard:         []byte("BEGIN:VCARD\r\nFN:Alpha\r\nEND:VCARD\r\n"),
	}); err != nil {
		t.Fatalf("PutCard a: %v", err)
	}

	return dbPath
}

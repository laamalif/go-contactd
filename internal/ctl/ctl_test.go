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

func TestRunCLI_ImportDir_ImportsVCFFiles(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "b.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-b\r\nFN:Beta\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile b.vcf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alpha\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile a.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"--book", "contacts",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "created=2") {
		t.Fatalf("stdout=%q want created summary", got)
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = store.Close() }()
	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	cards, err := store.ListCards(context.Background(), ab.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards len=%d want 2", len(cards))
	}
	if cards[0].Href != "a.vcf" || cards[1].Href != "b.vcf" {
		t.Fatalf("hrefs=%q,%q want a.vcf,b.vcf", cards[0].Href, cards[1].Href)
	}
}

func TestRunCLI_ImportConcatFile_UsesUIDForHref(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcFile := filepath.Join(t.TempDir(), "contacts.vcf")
	raw := "" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-z\r\nFN:Zed\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alpha\r\nEND:VCARD\r\n"
	if err := os.WriteFile(srcFile, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile contacts.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcFile,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "created=2") {
		t.Fatalf("stdout=%q want created summary", got)
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = store.Close() }()
	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	cards, err := store.ListCards(context.Background(), ab.ID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards len=%d want 2", len(cards))
	}
	if cards[0].Href != "uid-a.vcf" || cards[1].Href != "uid-z.vcf" {
		t.Fatalf("hrefs=%q,%q want uid-a.vcf,uid-z.vcf", cards[0].Href, cards[1].Href)
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

func seedEmptyImportTestDB(t *testing.T) string {
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
	if _, _, err := store.EnsureAddressbook(context.Background(), userID, "contacts", "Contacts"); err != nil {
		t.Fatalf("EnsureAddressbook: %v", err)
	}
	return dbPath
}

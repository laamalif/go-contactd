package ctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/db"
)

func TestOpenImportRegularFile_RejectsFIFO(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	p := filepath.Join(tmp, "in.fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	f, _, err := openImportRegularFile(p)
	if err == nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("openImportRegularFile error=nil, want non-regular rejection")
	}
	if got := err.Error(); !strings.Contains(got, "non-regular") {
		t.Fatalf("err=%q want non-regular message", got)
	}
}

func TestOpenImportRegularFileAtSnapshot_RejectsChangedFile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	p := filepath.Join(tmp, "a.vcf")
	if err := os.WriteFile(p, []byte("BEGIN:VCARD\r\nUID:a\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile initial: %v", err)
	}
	snap, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("Lstat snapshot: %v", err)
	}
	// Deterministic content change: change size so the snapshot check must fail.
	if err := os.WriteFile(p, []byte("BEGIN:VCARD\r\nUID:aaaaaa\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile changed: %v", err)
	}

	f, _, err := openImportRegularFileAtSnapshot(p, snap)
	if err == nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("openImportRegularFileAtSnapshot error=nil, want changed file rejection")
	}
	if got := err.Error(); !strings.Contains(got, "changed") {
		t.Fatalf("err=%q want changed message", got)
	}
}

func TestVerifyImportFileSnapshotUnchanged_RejectsInPlaceContentMutation(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	p := filepath.Join(tmp, "a.vcf")
	orig := []byte("BEGIN:VCARD\r\nUID:uid-a\r\nFN:Alpha\r\nEND:VCARD\r\n")
	if err := os.WriteFile(p, orig, 0o600); err != nil {
		t.Fatalf("WriteFile initial: %v", err)
	}
	snap, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("Lstat snapshot: %v", err)
	}
	wantDigest := sha256.Sum256(orig)

	// Same size payload, then restore mtime to simulate metadata-preserving tamper.
	mut := []byte("BEGIN:VCARD\r\nUID:uid-a\r\nFN:Omega\r\nEND:VCARD\r\n")
	if len(mut) != len(orig) {
		t.Fatalf("mut len=%d want %d", len(mut), len(orig))
	}
	if err := os.WriteFile(p, mut, 0o600); err != nil {
		t.Fatalf("WriteFile mutated: %v", err)
	}
	if err := os.Chtimes(p, snap.ModTime(), snap.ModTime()); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Some filesystems round mtimes; keep test deterministic by ensuring we don't rely on granularity.
	time.Sleep(10 * time.Millisecond)

	err = verifyImportFileSnapshotUnchanged(p, snap, wantDigest, int(config.DefaultVCardMaxBytes))
	if err == nil {
		t.Fatal("verifyImportFileSnapshotUnchanged err=nil, want content-changed error")
	}
	if got := err.Error(); !strings.Contains(got, "content changed") {
		t.Fatalf("err=%q want content changed message", got)
	}
}

func TestRunCLI_ImportHelp_DryRunWarnsAdvisoryUnderConcurrency(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{"import", "--help"}, nil, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "--dry-run") {
		t.Fatalf("stdout=%q want --dry-run help", got)
	}
	if !strings.Contains(strings.ToLower(got), "advisory") || !strings.Contains(strings.ToLower(got), "concurrent") {
		t.Fatalf("stdout=%q want advisory/concurrent dry-run warning", got)
	}
}

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

func TestRunCLI_ExportConcat_NormalizesBoundaryBetweenCards(t *testing.T) {
	t.Parallel()

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
	// No trailing CRLF on either card to reproduce invalid concat seam.
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: abID,
		Href:          "a.vcf",
		UID:           "uid-a",
		VCard:         []byte("BEGIN:VCARD\r\nFN:Alpha\r\nEND:VCARD"),
	}); err != nil {
		t.Fatalf("PutCard a: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: abID,
		Href:          "b.vcf",
		UID:           "uid-b",
		VCard:         []byte("BEGIN:VCARD\r\nFN:Beta\r\nEND:VCARD"),
	}); err != nil {
		t.Fatalf("PutCard b: %v", err)
	}

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
	got := stdout.String()
	if strings.Contains(got, "END:VCARDBEGIN:VCARD") {
		t.Fatalf("invalid concat seam present: %q", got)
	}
	if !strings.Contains(got, "END:VCARD\r\nBEGIN:VCARD") {
		t.Fatalf("missing normalized concat seam: %q", got)
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

func TestRunCLI_ExportDir_DryRun_DoesNotWriteFiles(t *testing.T) {
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
		"--dry-run",
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "dry-run:") || !strings.Contains(got, "cards=2") {
		t.Fatalf("stdout=%q want dry-run summary", got)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("outDir stat err=%v want not exists", err)
	}
}

func TestRunCLI_Export_AddressbookNotFoundReturns3(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--book", "missing",
		"--format", "dir",
		"--out", filepath.Join(t.TempDir(), "out"),
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 3 {
		t.Fatalf("code=%d want 3 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := strings.TrimSpace(stderr.String()); got != "not found" {
		t.Fatalf("stderr=%q want %q", got, "not found")
	}
}

func TestRunCLI_ExportDir_WriteFailureReturns1(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)
	outPath := filepath.Join(t.TempDir(), "out-as-file")
	if err := os.WriteFile(outPath, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatalf("WriteFile out-as-file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--format", "dir",
		"--out", outPath,
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "io error:") {
		t.Fatalf("stderr=%q want io error", got)
	}
}

func TestRunCLI_ExportDir_RejectsSymlinkDestinationFile(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("MkdirAll outDir: %v", err)
	}
	target := filepath.Join(tmp, "outside.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(outDir, "a.vcf")); err != nil {
		t.Fatalf("Symlink a.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--book", "contacts",
		"--format", "dir",
		"--out", outDir,
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "io error:") {
		t.Fatalf("stderr=%q want io error", got)
	}
	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(gotTarget) != "keep" {
		t.Fatalf("target=%q want unchanged", string(gotTarget))
	}
}

func TestRunCLI_ExportDir_DoesNotClobberHardlinkTarget(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("MkdirAll outDir: %v", err)
	}
	target := filepath.Join(tmp, "target.txt")
	orig := []byte("DO NOT OVERWRITE\n")
	if err := os.WriteFile(target, orig, 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.Link(target, filepath.Join(outDir, "a.vcf")); err != nil {
		t.Fatalf("Link a.vcf: %v", err)
	}

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
		t.Fatalf("code=%d want 0 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}

	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(gotTarget) != string(orig) {
		t.Fatalf("target overwritten: got=%q want=%q", string(gotTarget), string(orig))
	}

	gotExport, err := os.ReadFile(filepath.Join(outDir, "a.vcf"))
	if err != nil {
		t.Fatalf("ReadFile out/a.vcf: %v", err)
	}
	if !strings.Contains(string(gotExport), "BEGIN:VCARD") {
		t.Fatalf("out/a.vcf=%q want vcard bytes", string(gotExport))
	}
}

func TestRunCLI_ExportConcat_RejectsSymlinkOutPath(t *testing.T) {
	t.Parallel()

	dbPath := seedExportTestDB(t)
	tmp := t.TempDir()
	target := filepath.Join(tmp, "outside.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	outPath := filepath.Join(tmp, "out.vcf")
	if err := os.Symlink(target, outPath); err != nil {
		t.Fatalf("Symlink out.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"export",
		"--username", "alice",
		"--book", "contacts",
		"--format", "concat",
		"--out", outPath,
		"-d", dbPath,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "io error:") {
		t.Fatalf("stderr=%q want io error", got)
	}
	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(gotTarget) != "keep" {
		t.Fatalf("target=%q want unchanged", string(gotTarget))
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

func TestRunCLI_ImportDir_RejectsSymlinkSourceFile(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll srcDir: %v", err)
	}
	external := filepath.Join(tmp, "external-secret.vcf")
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-ext\r\nFN:External Secret Via Symlink\r\nEND:VCARD\r\n")
	if err := os.WriteFile(external, raw, 0o600); err != nil {
		t.Fatalf("WriteFile external: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(srcDir, "link.vcf")); err != nil {
		t.Fatalf("Symlink link.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") {
		t.Fatalf("stderr=%q want import error", got)
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
	if len(cards) != 0 {
		t.Fatalf("cards len=%d want 0", len(cards))
	}
}

func TestRunCLI_ImportDir_DryRun_DoesNotWriteDB(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alpha\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile a.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"--dry-run",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 0 {
		t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "dry-run:") || !strings.Contains(got, "created=1") {
		t.Fatalf("stdout=%q want dry-run summary", got)
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
	if len(cards) != 0 {
		t.Fatalf("cards len=%d want 0 (dry-run)", len(cards))
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

func TestRunCLI_ImportConcatFile_InvalidUIDHrefReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcFile := filepath.Join(t.TempDir(), "contacts.vcf")
	raw := "" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:bad/uid\r\nFN:Bad UID\r\nEND:VCARD\r\n"
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
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "invalid card href") {
		t.Fatalf("stderr=%q want invalid href import error", got)
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
	if len(cards) != 0 {
		t.Fatalf("cards len=%d want 0", len(cards))
	}
}

func TestRunCLI_ImportConcatFile_RejectsTotalInputOverCap(t *testing.T) {
	t.Parallel()

	oldCap := importConcatMaxBytesDefault
	importConcatMaxBytesDefault = 128
	defer func() { importConcatMaxBytesDefault = oldCap }()

	dbPath := seedEmptyImportTestDB(t)
	srcFile := filepath.Join(t.TempDir(), "contacts.vcf")
	if err := os.WriteFile(srcFile, bytes.Repeat([]byte("X"), 256), 0o600); err != nil {
		t.Fatalf("WriteFile contacts.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcFile,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import file too large") {
		t.Fatalf("stderr=%q want total input cap error", got)
	}
}

func TestRunCLI_ImportConcatFile_OversizeVCardReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcFile := filepath.Join(t.TempDir(), "contacts.vcf")
	oversize := int(config.DefaultVCardMaxBytes) + 1024
	raw := "" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-big\r\nFN:Big\r\nNOTE:" + strings.Repeat("A", oversize) + "\r\nEND:VCARD\r\n"
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
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "vcard too large") {
		t.Fatalf("stderr=%q want oversize import error", got)
	}
}

func TestRunCLI_ImportConcatFile_HonorsEnvVCardMaxBytes(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcFile := filepath.Join(t.TempDir(), "contacts.vcf")
	raw := "" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-small-limit\r\nFN:Small Limit\r\nNOTE:" + strings.Repeat("A", 256) + "\r\nEND:VCARD\r\n"
	if err := os.WriteFile(srcFile, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile contacts.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcFile,
	}, map[string]string{
		"CONTACTD_VCARD_MAX_BYTES": "128",
	}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "vcard too large") {
		t.Fatalf("stderr=%q want oversize import error from env limit", got)
	}
}

func TestRunCLI_ImportDir_InvalidVCardReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "bad.vcf"), []byte("not a vcard"), 0o600); err != nil {
		t.Fatalf("WriteFile bad.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "decode import file bad.vcf") {
		t.Fatalf("stderr=%q want import decode error", got)
	}
}

func TestRunCLI_ImportDir_TrailingGarbageAfterVCardReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	raw := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alice\r\nEND:VCARD\r\nGARBAGE\r\n"
	if err := os.WriteFile(filepath.Join(srcDir, "a.vcf"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile a.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "decode import file a.vcf") {
		t.Fatalf("stderr=%q want decode error", got)
	}
}

func TestRunCLI_ImportDir_MultiCardSingleFileReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	raw := "" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alice\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-b\r\nFN:Bob\r\nEND:VCARD\r\n"
	if err := os.WriteFile(filepath.Join(srcDir, "multi.vcf"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile multi.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "decode import file multi.vcf") {
		t.Fatalf("stderr=%q want decode error", got)
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
	if len(cards) != 0 {
		t.Fatalf("cards len=%d want 0", len(cards))
	}
}

func TestRunCLI_ImportDir_ErrorIsAtomic_NoPartialCommit(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alice\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile a.vcf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "b.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Missing UID\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile b.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "missing UID") {
		t.Fatalf("stderr=%q want missing UID error", got)
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
	if len(cards) != 0 {
		t.Fatalf("cards len=%d want 0 after failed import", len(cards))
	}
}

func TestRunCLI_ImportDir_UIDConflictReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open seed conflict: %v", err)
	}
	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		_ = store.Close()
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: ab.ID,
		Href:          "existing.vcf",
		UID:           "uid-conflict",
		VCard:         []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-conflict\r\nFN:Existing\r\nEND:VCARD\r\n"),
	}); err != nil {
		_ = store.Close()
		t.Fatalf("PutCard existing: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "new.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-conflict\r\nFN:New\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile new.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("code=%d want 1 stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "put card new.vcf") {
		t.Fatalf("stderr=%q want import put-card error", got)
	}
}

func TestRunCLI_ImportDir_DryRun_UIDConflictReturnsError(t *testing.T) {
	t.Parallel()

	dbPath := seedEmptyImportTestDB(t)
	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open seed conflict: %v", err)
	}
	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		_ = store.Close()
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: ab.ID,
		Href:          "existing.vcf",
		UID:           "uid-conflict",
		VCard:         []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-conflict\r\nFN:Existing\r\nEND:VCARD\r\n"),
	}); err != nil {
		_ = store.Close()
		t.Fatalf("PutCard existing: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	srcDir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "new.vcf"), []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-conflict\r\nFN:New\r\nEND:VCARD\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile new.vcf: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := RunCLI("contactctl", []string{
		"import",
		"--username", "alice",
		"--dry-run",
		"-d", dbPath,
		srcDir,
	}, map[string]string{}, strings.NewReader(""), &stdout, &stderr, nil)
	if code != 1 {
		t.Fatalf("dry-run code=%d want 1 stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "import error:") || !strings.Contains(got, "put card new.vcf") {
		t.Fatalf("stderr=%q want import put-card error", got)
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

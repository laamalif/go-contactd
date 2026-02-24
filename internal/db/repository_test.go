package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/laamalif/go-contactd/internal/db"
)

func TestOpen_AppliesSQLitePragmasAndMigrations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "contactd.sqlite")

	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	mode, err := store.PragmaString(ctx, "journal_mode")
	if err != nil {
		t.Fatalf("PragmaString(journal_mode): %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	timeout, err := store.PragmaInt(ctx, "busy_timeout")
	if err != nil {
		t.Fatalf("PragmaInt(busy_timeout): %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}

	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready returned error: %v", err)
	}
}

func TestComputeETag_DeterministicFromCanonicalBytes(t *testing.T) {
	t.Parallel()

	a := db.CanonicalizeVCard([]byte("BEGIN:VCARD\nVERSION:3.0\nFN:A\nEND:VCARD\n"))
	b := db.CanonicalizeVCard([]byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:A\r\nEND:VCARD\r\n"))

	gotA := db.ComputeETagHex(a)
	gotB := db.ComputeETagHex(b)
	if gotA == "" {
		t.Fatal("ComputeETagHex returned empty string")
	}
	if gotA != gotB {
		t.Fatalf("etag mismatch: %q vs %q", gotA, gotB)
	}
}

func TestStore_UIDUniquePerAddressbook_AndHrefUpsert(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "a.vcf",
		UID:           "uid-1",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-1\nFN:A\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("first PutCard: %v", err)
	}

	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "b.vcf",
		UID:           "uid-1",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-1\nFN:B\nEND:VCARD\n"),
	}); err == nil {
		t.Fatal("duplicate UID PutCard error = nil, want error")
	}

	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "a.vcf",
		UID:           "uid-2",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-2\nFN:C\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("href upsert PutCard returned error: %v", err)
	}

	count, err := store.CardCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("CardCount after href upsert = %d, want 1", count)
	}
}

func TestStore_PutCard_AtomicRevisionAndJournalRollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	rev0, err := store.AddressbookRevision(ctx, bookID)
	if err != nil {
		t.Fatalf("AddressbookRevision initial: %v", err)
	}
	if rev0 != 0 {
		t.Fatalf("initial revision = %d, want 0", rev0)
	}

	store.SetTestHooks(db.TestHooks{
		BeforeCardChangeInsert: func() error { return errors.New("boom") },
	})
	_, err = store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "x.vcf",
		UID:           "uid-x",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-x\nFN:X\nEND:VCARD\n"),
	})
	if err == nil {
		t.Fatal("PutCard error = nil, want injected failure")
	}

	rev1, err := store.AddressbookRevision(ctx, bookID)
	if err != nil {
		t.Fatalf("AddressbookRevision after rollback: %v", err)
	}
	if rev1 != 0 {
		t.Fatalf("revision after rollback = %d, want 0", rev1)
	}

	count, err := store.CardCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardCount after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("CardCount after rollback = %d, want 0", count)
	}

	changeCount, err := store.CardChangeCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardChangeCount after rollback: %v", err)
	}
	if changeCount != 0 {
		t.Fatalf("CardChangeCount after rollback = %d, want 0", changeCount)
	}
}

func TestStore_DeleteCard_AppendsDeletedChangeAndBumpsRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "x.vcf",
		UID:           "uid-x",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-x\nFN:X\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("PutCard: %v", err)
	}

	if err := store.DeleteCard(ctx, bookID, "x.vcf"); err != nil {
		t.Fatalf("DeleteCard: %v", err)
	}

	rev, err := store.AddressbookRevision(ctx, bookID)
	if err != nil {
		t.Fatalf("AddressbookRevision: %v", err)
	}
	if rev != 2 {
		t.Fatalf("revision after put+delete = %d, want 2", rev)
	}

	count, err := store.CardCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("CardCount after delete = %d, want 0", count)
	}

	last, err := store.LastCardChange(ctx, bookID)
	if err != nil {
		t.Fatalf("LastCardChange: %v", err)
	}
	if !last.Deleted {
		t.Fatalf("last change deleted = false, want true")
	}
	if last.ETagHex == "" {
		t.Fatalf("last change etag empty, want previous card etag")
	}
	if last.Href != "x.vcf" {
		t.Fatalf("last change href = %q, want x.vcf", last.Href)
	}
	if last.Revision != 2 {
		t.Fatalf("last change revision = %d, want 2", last.Revision)
	}
}

func TestStore_PruneCardChangesByAge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "x.vcf",
		UID:           "uid-x",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-x\nFN:X\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("PutCard: %v", err)
	}

	// Age the existing journal row so prune can remove it.
	if err := store.ForceCardChangesTimestamp(ctx, bookID, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("ForceCardChangesTimestamp: %v", err)
	}

	pruned, err := store.PruneCardChangesByAge(ctx, time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PruneCardChangesByAge: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}

	count, err := store.CardChangeCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardChangeCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("CardChangeCount after prune = %d, want 0", count)
	}
}

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

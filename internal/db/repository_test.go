package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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

func TestStore_DeleteCard_MissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	err = store.DeleteCard(ctx, bookID, "missing.vcf")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("DeleteCard missing err = %v, want db.ErrNotFound", err)
	}
}

func TestStore_DeleteCardConditional_StaleETagReturnsErrPreconditionFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	putRes, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "x.vcf",
		UID:           "uid-x",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-x\nFN:X\nEND:VCARD\n"),
	})
	if err != nil {
		t.Fatalf("PutCard: %v", err)
	}
	if _, err := store.PutCard(ctx, db.PutCardInput{
		AddressbookID: bookID,
		Href:          "x.vcf",
		UID:           "uid-x",
		VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-x\nFN:X2\nEND:VCARD\n"),
	}); err != nil {
		t.Fatalf("PutCard update: %v", err)
	}

	err = store.DeleteCardConditional(ctx, bookID, "x.vcf", putRes.ETagHex)
	if !errors.Is(err, db.ErrPreconditionFailed) {
		t.Fatalf("DeleteCardConditional err = %v, want db.ErrPreconditionFailed", err)
	}

	count, err := store.CardCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("CardCount after stale conditional delete = %d, want 1", count)
	}
}

func TestStore_LastCardChange_EmptyAddressbookReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	_, err = store.LastCardChange(ctx, bookID)
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("LastCardChange empty err = %v, want db.ErrNotFound", err)
	}
}

func TestStore_PruneCardChangesByAge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

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

func TestStore_PruneCardChangesByMaxRevisions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	for i := 0; i < 5; i++ {
		href := "x.vcf"
		uid := "uid-x"
		if i%2 == 1 {
			href = "y.vcf"
			uid = "uid-y"
		}
		if _, err := store.PutCard(ctx, db.PutCardInput{
			AddressbookID: bookID,
			Href:          href,
			UID:           uid,
			VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:" + uid + "\nFN:X\nEND:VCARD\n"),
		}); err != nil {
			t.Fatalf("PutCard #%d: %v", i+1, err)
		}
	}

	pruned, err := store.PruneCardChangesByMaxRevisions(ctx, 2)
	if err != nil {
		t.Fatalf("PruneCardChangesByMaxRevisions: %v", err)
	}
	if pruned != 3 {
		t.Fatalf("pruned = %d, want 3", pruned)
	}

	revs, err := store.CardChangeRevisions(ctx, bookID)
	if err != nil {
		t.Fatalf("CardChangeRevisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("len(revs) = %d, want 2", len(revs))
	}
	if revs[0] != 4 || revs[1] != 5 {
		t.Fatalf("revisions = %v, want [4 5]", revs)
	}
}

func TestStore_ConcurrentWrites_BeginImmediate_NoLockError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	defer func() { _ = store.Close() }()

	userID, err := store.CreateUser(ctx, "alice", "bcrypt-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bookID, err := store.CreateAddressbook(ctx, userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}

	const workers = 8
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.PutCard(ctx, db.PutCardInput{
				AddressbookID: bookID,
				Href:          "c" + strconv.Itoa(i) + ".vcf",
				UID:           "uid-" + strconv.Itoa(i),
				VCard:         []byte("BEGIN:VCARD\nVERSION:3.0\nUID:uid-" + strconv.Itoa(i) + "\nFN:C\nEND:VCARD\n"),
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err == nil {
			continue
		}
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			t.Fatalf("unexpected lock error: %v", err)
		}
		t.Fatalf("PutCard concurrent error: %v", err)
	}

	count, err := store.CardCount(ctx, bookID)
	if err != nil {
		t.Fatalf("CardCount: %v", err)
	}
	if count != workers {
		t.Fatalf("CardCount = %d, want %d", count, workers)
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

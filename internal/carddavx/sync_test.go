package carddavx_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/carddav"
	contactcarddav "github.com/laamalif/go-contactd/internal/carddav"
	"github.com/laamalif/go-contactd/internal/carddavx"
	"github.com/laamalif/go-contactd/internal/db"
)

func TestSyncToken_FormatParseRoundTrip(t *testing.T) {
	t.Parallel()

	token := carddavx.FormatSyncToken(42, 105)
	if token != "urn:contactd:sync:42:105" {
		t.Fatalf("FormatSyncToken = %q", token)
	}

	parsed, err := carddavx.ParseSyncToken(token)
	if err != nil {
		t.Fatalf("ParseSyncToken: %v", err)
	}
	if parsed.AddressbookID != 42 || parsed.Revision != 105 {
		t.Fatalf("ParseSyncToken parsed = %+v", parsed)
	}
}

func TestSyncToken_ParseRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, tc := range []string{
		"",
		"urn:contactd:sync",
		"urn:contactd:sync:x:1",
		"urn:contactd:sync:1:x",
		"urn:contactd:sync:1",
		"urn:other:sync:1:2",
	} {
		if _, err := carddavx.ParseSyncToken(tc); err == nil {
			t.Fatalf("ParseSyncToken(%q) error=nil, want error", tc)
		}
	}
}

func TestService_SyncCollection_EmptyTokenInitialSyncAndInvalidToken(t *testing.T) {
	t.Parallel()

	store := openSyncStore(t)
	defer func() { _ = store.Close() }()
	userID, err := store.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts"); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", sampleCard("uid-a", "Alice A"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject: %v", err)
	}

	svc := carddavx.NewSyncService(store)

	res, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection empty token: %v", err)
	}
	if res.SyncToken == "" {
		t.Fatal("SyncCollection empty token result missing sync token")
	}
	if len(res.Updated) != 1 || res.Updated[0].Href != "/alice/contacts/a.vcf" {
		t.Fatalf("initial sync updated = %+v, want one /alice/contacts/a.vcf", res.Updated)
	}

	_, err = svc.SyncCollection(context.Background(), "alice", "contacts", "urn:contactd:sync:999:1", 0)
	if err == nil {
		t.Fatal("SyncCollection invalid token error=nil, want error")
	}
	if !carddavx.IsInvalidSyncToken(err) {
		t.Fatalf("SyncCollection invalid token err=%v, want invalid-sync-token marker", err)
	}
}

func TestService_SyncCollection_DeltaLimitReturnsContinuationToken(t *testing.T) {
	t.Parallel()

	store := openSyncStore(t)
	defer func() { _ = store.Close() }()
	userID, err := store.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts"); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", sampleCard("uid-a", "Alice A"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject a: %v", err)
	}

	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}
	baseTok, err := carddavx.ParseSyncToken(baseline.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken baseline: %v", err)
	}

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", sampleCard("uid-b", "Bob B"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/c.vcf", sampleCard("uid-c", "Carol C"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject c: %v", err)
	}
	if err := backend.DeleteAddressObject(ctx, "/alice/contacts/b.vcf"); err != nil {
		t.Fatalf("DeleteAddressObject b: %v", err)
	}

	firstPage, err := svc.SyncCollection(context.Background(), "alice", "contacts", baseline.SyncToken, 2)
	if err != nil {
		t.Fatalf("SyncCollection page1: %v", err)
	}
	if len(firstPage.Updated)+len(firstPage.Deleted) != 2 {
		t.Fatalf("page1 item count = %d, want 2 (updated=%d deleted=%d)", len(firstPage.Updated)+len(firstPage.Deleted), len(firstPage.Updated), len(firstPage.Deleted))
	}
	firstTok, err := carddavx.ParseSyncToken(firstPage.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken page1: %v", err)
	}
	if firstTok.Revision != baseTok.Revision+2 {
		t.Fatalf("page1 token revision = %d, want %d", firstTok.Revision, baseTok.Revision+2)
	}

	secondPage, err := svc.SyncCollection(context.Background(), "alice", "contacts", firstPage.SyncToken, 2)
	if err != nil {
		t.Fatalf("SyncCollection page2: %v", err)
	}
	if len(secondPage.Updated)+len(secondPage.Deleted) != 1 {
		t.Fatalf("page2 item count = %d, want 1 (updated=%d deleted=%d)", len(secondPage.Updated)+len(secondPage.Deleted), len(secondPage.Updated), len(secondPage.Deleted))
	}
	if len(secondPage.Deleted) != 1 || secondPage.Deleted[0] != "/alice/contacts/b.vcf" {
		t.Fatalf("page2 deleted = %+v, want [/alice/contacts/b.vcf]", secondPage.Deleted)
	}
	secondTok, err := carddavx.ParseSyncToken(secondPage.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken page2: %v", err)
	}
	if secondTok.Revision != baseTok.Revision+3 {
		t.Fatalf("page2 token revision = %d, want %d", secondTok.Revision, baseTok.Revision+3)
	}
	if secondTok.Revision <= firstTok.Revision {
		t.Fatalf("tokens not monotonic: page1=%d page2=%d", firstTok.Revision, secondTok.Revision)
	}
}

func TestService_SyncCollection_StaleTokenAfterPruneIsInvalid(t *testing.T) {
	t.Parallel()

	store := openSyncStore(t)
	defer func() { _ = store.Close() }()
	userID, err := store.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	abID, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts")
	if err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", sampleCard("uid-a", "Alice A"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject a: %v", err)
	}

	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", sampleCard("uid-b", "Bob B"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/c.vcf", sampleCard("uid-c", "Carol C"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject c: %v", err)
	}
	if _, err := store.PruneCardChangesByMaxRevisions(context.Background(), 1); err != nil {
		t.Fatalf("PruneCardChangesByMaxRevisions: %v", err)
	}
	revs, err := store.CardChangeRevisions(context.Background(), abID)
	if err != nil {
		t.Fatalf("CardChangeRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("retained revisions = %+v, want 1 retained", revs)
	}

	_, err = svc.SyncCollection(context.Background(), "alice", "contacts", baseline.SyncToken, 0)
	if err == nil {
		t.Fatal("SyncCollection stale token error=nil, want invalid token")
	}
	if !carddavx.IsInvalidSyncToken(err) {
		t.Fatalf("SyncCollection stale token err=%v, want invalid-sync-token marker", err)
	}
}

func TestService_SyncCollection_DeltaCollapsesRepeatedUpdatesPerHref(t *testing.T) {
	t.Parallel()

	store := openSyncStore(t)
	defer func() { _ = store.Close() }()
	userID, err := store.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts"); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/d.vcf", sampleCard("uid-d", "Dora v1"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject d v1: %v", err)
	}

	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/d.vcf", sampleCard("uid-d", "Dora v2"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject d v2: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/d.vcf", sampleCard("uid-d", "Dora v3"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject d v3: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/d.vcf", sampleCard("uid-d", "Dora v4"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject d v4: %v", err)
	}

	res, err := svc.SyncCollection(context.Background(), "alice", "contacts", baseline.SyncToken, 0)
	if err != nil {
		t.Fatalf("SyncCollection delta: %v", err)
	}
	if got := len(res.Deleted); got != 0 {
		t.Fatalf("Deleted len = %d, want 0", got)
	}
	if got := len(res.Updated); got != 1 {
		t.Fatalf("Updated len = %d, want 1 (collapsed latest only)", got)
	}
	if got, want := res.Updated[0].Href, "/alice/contacts/d.vcf"; got != want {
		t.Fatalf("Updated[0].Href = %q, want %q", got, want)
	}
}

func TestService_SyncCollection_DeltaCollapsesUpdateThenDeleteToDelete(t *testing.T) {
	t.Parallel()

	store := openSyncStore(t)
	defer func() { _ = store.Close() }()
	userID, err := store.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, "contacts", "Contacts"); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/x.vcf", sampleCard("uid-x", "X v1"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject x v1: %v", err)
	}

	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/x.vcf", sampleCard("uid-x", "X v2"), &carddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject x v2: %v", err)
	}
	if err := backend.DeleteAddressObject(ctx, "/alice/contacts/x.vcf"); err != nil {
		t.Fatalf("DeleteAddressObject x: %v", err)
	}

	res, err := svc.SyncCollection(context.Background(), "alice", "contacts", baseline.SyncToken, 0)
	if err != nil {
		t.Fatalf("SyncCollection delta: %v", err)
	}
	if got := len(res.Updated); got != 0 {
		t.Fatalf("Updated len = %d, want 0", got)
	}
	if got := len(res.Deleted); got != 1 {
		t.Fatalf("Deleted len = %d, want 1 (collapsed delete)", got)
	}
	if got, want := res.Deleted[0], "/alice/contacts/x.vcf"; got != want {
		t.Fatalf("Deleted[0] = %q, want %q", got, want)
	}
}

func openSyncStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store
}

func sampleCard(uid, fn string) vcard.Card {
	c := make(vcard.Card)
	c.SetValue(vcard.FieldVersion, "3.0")
	c.SetValue(vcard.FieldUID, uid)
	c.SetValue(vcard.FieldFormattedName, fn)
	return c
}

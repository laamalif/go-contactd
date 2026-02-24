package carddav_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	contactcarddav "github.com/laamalif/go-contactd/internal/carddav"
	"github.com/laamalif/go-contactd/internal/db"
)

func TestBackend_CurrentUserPrincipalAndHomeSet(t *testing.T) {
	t.Parallel()

	store := openBackendStore(t)
	defer store.Close()
	seedUserAndBook(t, store, "alice", "contacts", "Contacts")

	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")

	principal, err := backend.CurrentUserPrincipal(ctx)
	if err != nil {
		t.Fatalf("CurrentUserPrincipal: %v", err)
	}
	if principal != "/alice/" {
		t.Fatalf("CurrentUserPrincipal = %q, want /alice/", principal)
	}

	home, err := backend.AddressBookHomeSetPath(ctx)
	if err != nil {
		t.Fatalf("AddressBookHomeSetPath: %v", err)
	}
	if home != "/alice/" {
		t.Fatalf("AddressBookHomeSetPath = %q, want /alice/", home)
	}
}

func TestBackend_PutGetDeleteAddressObject(t *testing.T) {
	t.Parallel()

	store := openBackendStore(t)
	defer store.Close()
	seedUserAndBook(t, store, "alice", "contacts", "Contacts")

	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	path := "/alice/contacts/a.vcf"

	created, err := backend.PutAddressObject(ctx, path, sampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{})
	if err != nil {
		t.Fatalf("PutAddressObject create: %v", err)
	}
	if created.ETag == "" {
		t.Fatal("PutAddressObject create returned empty ETag")
	}

	got, err := backend.GetAddressObject(ctx, path, nil)
	if err != nil {
		t.Fatalf("GetAddressObject: %v", err)
	}
	if got.Card.PreferredValue(vcard.FieldUID) != "uid-a" {
		t.Fatalf("UID = %q, want uid-a", got.Card.PreferredValue(vcard.FieldUID))
	}
	if got.Card.PreferredValue(vcard.FieldFormattedName) != "Alice A" {
		t.Fatalf("FN = %q, want Alice A", got.Card.PreferredValue(vcard.FieldFormattedName))
	}
	if got.ETag != created.ETag {
		t.Fatalf("Get ETag = %q, want %q", got.ETag, created.ETag)
	}

	if err := backend.DeleteAddressObject(ctx, path); err != nil {
		t.Fatalf("DeleteAddressObject: %v", err)
	}
	if _, err := backend.GetAddressObject(ctx, path, nil); err == nil {
		t.Fatal("GetAddressObject after delete error = nil, want error")
	}
}

func TestBackend_PutAddressObject_ConditionalMatches(t *testing.T) {
	t.Parallel()

	store := openBackendStore(t)
	defer store.Close()
	seedUserAndBook(t, store, "alice", "contacts", "Contacts")

	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	path := "/alice/contacts/a.vcf"

	created, err := backend.PutAddressObject(ctx, path, sampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{})
	if err != nil {
		t.Fatalf("PutAddressObject create: %v", err)
	}

	if _, err := backend.PutAddressObject(ctx, path, sampleCard("uid-a", "Alice B"), &gocarddav.PutAddressObjectOptions{
		IfNoneMatch: webdav.ConditionalMatch("*"),
	}); err == nil {
		t.Fatal("If-None-Match * on existing card error = nil, want error")
	}

	if _, err := backend.PutAddressObject(ctx, path, sampleCard("uid-a", "Alice C"), &gocarddav.PutAddressObjectOptions{
		IfMatch: webdav.ConditionalMatch(`"wrong-etag"`),
	}); err == nil {
		t.Fatal("If-Match mismatch error = nil, want error")
	}

	updated, err := backend.PutAddressObject(ctx, path, sampleCard("uid-a", "Alice D"), &gocarddav.PutAddressObjectOptions{
		IfMatch: webdav.ConditionalMatch(created.ETag),
	})
	if err != nil {
		t.Fatalf("If-Match correct update: %v", err)
	}
	if updated.ETag == created.ETag {
		t.Fatal("ETag did not change after card content update")
	}
}

func TestBackend_PutAddressObject_UIDConflict(t *testing.T) {
	t.Parallel()

	store := openBackendStore(t)
	defer store.Close()
	seedUserAndBook(t, store, "alice", "contacts", "Contacts")

	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", sampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject first: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", sampleCard("uid-a", "Alice B"), &gocarddav.PutAddressObjectOptions{}); err == nil {
		t.Fatal("PutAddressObject duplicate UID error = nil, want error")
	}
}

func TestBackend_QueryAddressObjects_NilQueryReturnsAll(t *testing.T) {
	t.Parallel()

	store := openBackendStore(t)
	defer store.Close()
	seedUserAndBook(t, store, "alice", "contacts", "Contacts")

	backend := contactcarddav.NewBackend(store)
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", sampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject a: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", sampleCard("uid-b", "Alice B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}

	got, err := backend.QueryAddressObjects(ctx, "/alice/contacts/", nil)
	if err != nil {
		t.Fatalf("QueryAddressObjects nil query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(QueryAddressObjects) = %d, want 2", len(got))
	}
}

func sampleCard(uid, fn string) vcard.Card {
	c := make(vcard.Card)
	c.SetValue(vcard.FieldVersion, "3.0")
	c.SetValue(vcard.FieldUID, uid)
	c.SetValue(vcard.FieldFormattedName, fn)
	return c
}

func openBackendStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store
}

func seedUserAndBook(t *testing.T, store *db.Store, username, slug, displayName string) {
	t.Helper()
	userID, err := store.CreateUser(context.Background(), username, "$2a$10$abcdefghijklmnopqrstuvwxyzABCDE1234567890abcde")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, slug, displayName); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
}

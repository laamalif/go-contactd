package server_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/emersion/go-vcard"
	contactcarddav "github.com/laamalif/go-contactd/internal/carddav"
	"github.com/laamalif/go-contactd/internal/db"
	"github.com/laamalif/go-contactd/internal/server"
)

func TestHandler_Healthz(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestHandler_WellKnownCardDAVRedirects(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{})
	req := httptest.NewRequest(http.MethodGet, "/.well-known/carddav", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusPermanentRedirect; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Location"), "/"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestHandler_ProtectedRouteRejectsUnauthenticated(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(context.Context, string, string) (string, bool, error) {
			t.Fatal("Authenticate should not be called without basic auth header")
			return "", false, nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/alice/", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("WWW-Authenticate header missing")
	}
}

func TestHandler_ProtectedRouteAcceptsValidBasicAuth(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/alice/", nil)
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d, want non-401", rr.Code)
	}
}

func TestHandler_CardPutGetDelete_WithBackend(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	putReq.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("PUT create status = %d, want %d", got, want)
	}
	etag := putRes.Header().Get("ETag")
	if etag == "" {
		t.Fatal("PUT create missing ETag header")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/alice/contacts/a.vcf", nil)
	getReq.SetBasicAuth("alice", "secret")
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, getReq)
	if got, want := getRes.Code, http.StatusOK; got != want {
		t.Fatalf("GET status = %d, want %d", got, want)
	}
	if ct := getRes.Header().Get("Content-Type"); ct != "text/vcard" {
		t.Fatalf("GET Content-Type = %q, want text/vcard", ct)
	}
	if got := getRes.Header().Get("ETag"); got != etag {
		t.Fatalf("GET ETag = %q, want %q", got, etag)
	}
	if body := getRes.Body.String(); !bytes.Contains([]byte(body), []byte("UID:uid-a")) {
		t.Fatalf("GET body missing UID, body=%q", body)
	}

	putReq2 := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice B")))
	putReq2.Header.Set("Content-Type", "text/vcard")
	putReq2.Header.Set("If-Match", etag)
	putReq2.SetBasicAuth("alice", "secret")
	putRes2 := httptest.NewRecorder()
	h.ServeHTTP(putRes2, putReq2)
	if got, want := putRes2.Code, http.StatusNoContent; got != want {
		t.Fatalf("PUT update status = %d, want %d", got, want)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/alice/contacts/a.vcf", nil)
	delReq.SetBasicAuth("alice", "secret")
	delRes := httptest.NewRecorder()
	h.ServeHTTP(delRes, delReq)
	if got, want := delRes.Code, http.StatusNoContent; got != want {
		t.Fatalf("DELETE status = %d, want %d", got, want)
	}
	if delRes.Body.Len() != 0 {
		t.Fatalf("DELETE body length = %d, want 0", delRes.Body.Len())
	}

	getReq2 := httptest.NewRequest(http.MethodGet, "/alice/contacts/a.vcf", nil)
	getReq2.SetBasicAuth("alice", "secret")
	getRes2 := httptest.NewRecorder()
	h.ServeHTTP(getRes2, getReq2)
	if got, want := getRes2.Code, http.StatusNotFound; got != want {
		t.Fatalf("GET after delete status = %d, want %d", got, want)
	}
}

func TestHandler_CardPut_RejectsMissingOrUnsupportedContentType(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return "alice", username == "alice" && password == "secret", nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusUnsupportedMediaType; got != want {
		t.Fatalf("missing Content-Type status = %d, want %d", got, want)
	}

	req2 := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	req2.Header.Set("Content-Type", "application/json")
	req2.SetBasicAuth("alice", "secret")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if got, want := rr2.Code, http.StatusUnsupportedMediaType; got != want {
		t.Fatalf("unsupported Content-Type status = %d, want %d", got, want)
	}
}

func openServerBackend(t *testing.T) (*db.Store, *contactcarddav.Backend) {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store, contactcarddav.NewBackend(store)
}

func seedServerUserBook(t *testing.T, store *db.Store, username, slug, name string) {
	t.Helper()
	userID, err := store.CreateUser(context.Background(), username, "bcrypt-hash-not-used-in-this-test")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAddressbook(context.Background(), userID, slug, name); err != nil {
		t.Fatalf("CreateAddressbook: %v", err)
	}
}

func vcardBody(uid, fn string) string {
	card := make(vcard.Card)
	card.SetValue(vcard.FieldVersion, "3.0")
	card.SetValue(vcard.FieldUID, uid)
	card.SetValue(vcard.FieldFormattedName, fn)
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		panic(err)
	}
	return buf.String()
}

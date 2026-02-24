package server_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	gocarddav "github.com/emersion/go-webdav/carddav"
	contactcarddav "github.com/laamalif/go-contactd/internal/carddav"
	"github.com/laamalif/go-contactd/internal/carddavx"
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

func TestHandler_CardPath_RejectsEncodedSlashInHref(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a%2Fb.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PUT encoded-slash href status = %d, want %d", got, want)
	}
}

func TestHandler_CardPath_RejectsTraversalSegment(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/../evil.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PUT traversal-segment status = %d, want %d", got, want)
	}
}

func TestHandler_CardPath_AllowsSafeFlatHrefWithDots(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/a..b.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	putReq.Header.Set("Content-Type", "text/vcard")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("PUT safe-flat href status = %d, want %d", got, want)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/alice/contacts/a..b.vcf", nil)
	getReq.SetBasicAuth("alice", "secret")
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, getReq)
	if got, want := getRes.Code, http.StatusOK; got != want {
		t.Fatalf("GET safe-flat href status = %d, want %d", got, want)
	}
}

func TestHandler_CardDelete_IfMatchEnforced(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", "Alice A")))
	putReq.Header.Set("Content-Type", "text/vcard")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("PUT create status = %d, want %d", got, want)
	}
	etag := putRes.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag on create")
	}

	delBad := httptest.NewRequest(http.MethodDelete, "/alice/contacts/a.vcf", nil)
	delBad.Header.Set("If-Match", `"wrong-etag"`)
	delBad.SetBasicAuth("alice", "secret")
	delBadRes := httptest.NewRecorder()
	h.ServeHTTP(delBadRes, delBad)
	if got, want := delBadRes.Code, http.StatusPreconditionFailed; got != want {
		t.Fatalf("DELETE wrong If-Match status = %d, want %d", got, want)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/alice/contacts/a.vcf", nil)
	getReq.SetBasicAuth("alice", "secret")
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, getReq)
	if got, want := getRes.Code, http.StatusOK; got != want {
		t.Fatalf("GET after failed delete status = %d, want %d", got, want)
	}

	delGood := httptest.NewRequest(http.MethodDelete, "/alice/contacts/a.vcf", nil)
	delGood.Header.Set("If-Match", etag)
	delGood.SetBasicAuth("alice", "secret")
	delGoodRes := httptest.NewRecorder()
	h.ServeHTTP(delGoodRes, delGood)
	if got, want := delGoodRes.Code, http.StatusNoContent; got != want {
		t.Fatalf("DELETE matching If-Match status = %d, want %d", got, want)
	}
}

func TestHandler_CardPut_UIDConflict_ReturnsCardDAVPreconditionXML(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	putA := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("same-uid", "Alice A")))
	putA.Header.Set("Content-Type", "text/vcard")
	putA.SetBasicAuth("alice", "secret")
	putARes := httptest.NewRecorder()
	h.ServeHTTP(putARes, putA)
	if got, want := putARes.Code, http.StatusCreated; got != want {
		t.Fatalf("PUT create status = %d, want %d", got, want)
	}

	putB := httptest.NewRequest(http.MethodPut, "/alice/contacts/b.vcf", bytes.NewBufferString(vcardBody("same-uid", "Alice B")))
	putB.Header.Set("Content-Type", "text/vcard")
	putB.SetBasicAuth("alice", "secret")
	putBRes := httptest.NewRecorder()
	h.ServeHTTP(putBRes, putB)

	if got, want := putBRes.Code, http.StatusConflict; got != want {
		t.Fatalf("PUT uid conflict status = %d, want %d", got, want)
	}
	if ct := putBRes.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("PUT uid conflict content-type = %q, want application/xml; charset=utf-8", ct)
	}
	body := putBRes.Body.String()
	if !strings.Contains(body, "no-uid-conflict") {
		t.Fatalf("PUT uid conflict body missing no-uid-conflict: %q", body)
	}
	if !strings.Contains(body, "urn:ietf:params:xml:ns:carddav") {
		t.Fatalf("PUT uid conflict body missing CardDAV namespace: %q", body)
	}
}

func TestHandler_CardPut_InvalidVCard_Returns400(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString("not-a-vcard"))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PUT invalid vcard status = %d, want %d", got, want)
	}
}

func TestHandler_CardPut_OversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:         backend,
		Sync:            carddavx.NewSyncService(store),
		RequestMaxBytes: 16,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(strings.Repeat("A", 64)))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("PUT oversize status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_PrincipalDepth0And1(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	if _, err := backend.PutAddressObject(contactcarddav.WithPrincipal(context.Background(), "alice"), "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject: %v", err)
	}
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	res0 := doPropfind(t, h, "/alice/", "0")
	if got, want := res0.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("principal depth0 status = %d, want %d", got, want)
	}
	if ct := res0.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("principal depth0 content-type = %q", ct)
	}
	body0 := res0.Body.String()
	if !strings.Contains(body0, "current-user-principal") || !strings.Contains(body0, "addressbook-home-set") {
		t.Fatalf("principal depth0 body missing principal props: %q", body0)
	}
	hrefs0 := mustCollectHrefs(t, body0)
	if len(hrefs0) != 1 || hrefs0[0] != "/alice/" {
		t.Fatalf("principal depth0 hrefs = %v, want [/alice/]", hrefs0)
	}

	res1 := doPropfind(t, h, "/alice/", "1")
	if got, want := res1.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("principal depth1 status = %d, want %d", got, want)
	}
	hrefs1 := mustCollectHrefs(t, res1.Body.String())
	if !containsString(hrefs1, "/alice/") || !containsString(hrefs1, "/alice/contacts/") {
		t.Fatalf("principal depth1 hrefs = %v, want principal + addressbook", hrefs1)
	}
	if containsString(hrefs1, "/alice/contacts/a.vcf") {
		t.Fatalf("principal depth1 hrefs leaked grandchild card: %v", hrefs1)
	}
}

func TestHandler_Propfind_AddressbookAndCardDepthHandling(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Alice B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject: %v", err)
	}
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	ab0 := doPropfind(t, h, "/alice/contacts/", "0")
	if got, want := ab0.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("addressbook depth0 status = %d, want %d", got, want)
	}
	hrefsAb0 := mustCollectHrefs(t, ab0.Body.String())
	if len(hrefsAb0) != 1 || hrefsAb0[0] != "/alice/contacts/" {
		t.Fatalf("addressbook depth0 hrefs = %v, want [/alice/contacts/]", hrefsAb0)
	}
	if !strings.Contains(ab0.Body.String(), "addressbook") {
		t.Fatalf("addressbook depth0 body missing addressbook resource type: %q", ab0.Body.String())
	}

	ab1 := doPropfind(t, h, "/alice/contacts/", "1")
	if got, want := ab1.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("addressbook depth1 status = %d, want %d", got, want)
	}
	hrefsAb1 := mustCollectHrefs(t, ab1.Body.String())
	if !containsString(hrefsAb1, "/alice/contacts/") || !containsString(hrefsAb1, "/alice/contacts/a.vcf") || !containsString(hrefsAb1, "/alice/contacts/b.vcf") {
		t.Fatalf("addressbook depth1 hrefs = %v, want collection + children", hrefsAb1)
	}

	card0 := doPropfind(t, h, "/alice/contacts/a.vcf", "0")
	if got, want := card0.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("card depth0 status = %d, want %d", got, want)
	}
	hrefsCard0 := mustCollectHrefs(t, card0.Body.String())
	if len(hrefsCard0) != 1 || hrefsCard0[0] != "/alice/contacts/a.vcf" {
		t.Fatalf("card depth0 hrefs = %v, want only card", hrefsCard0)
	}
	if !strings.Contains(card0.Body.String(), "getetag") {
		t.Fatalf("card depth0 body missing getetag: %q", card0.Body.String())
	}

	card1 := doPropfind(t, h, "/alice/contacts/a.vcf", "1")
	hrefsCard1 := mustCollectHrefs(t, card1.Body.String())
	if len(hrefsCard1) != 1 || hrefsCard1[0] != "/alice/contacts/a.vcf" {
		t.Fatalf("card depth1 hrefs = %v, want same as depth0", hrefsCard1)
	}
}

func TestHandler_Propfind_DepthInfinityRejected(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	rr := doPropfind(t, h, "/alice/contacts/", "infinity")
	if got, want := rr.Code, http.StatusForbidden; got != want {
		t.Fatalf("depth infinity status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_CardExplicitProps_UnknownReturns404Propstat(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject: %v", err)
	}
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest("PROPFIND", "/alice/contacts/a.vcf", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:getetag/>
    <D:displayname/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND explicit props status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "getetag") || !strings.Contains(body, "200 OK") {
		t.Fatalf("PROPFIND explicit props body missing 200/getetag: %q", body)
	}
	if !strings.Contains(body, "displayname") || !strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND explicit props body missing 404/displayname: %q", body)
	}
	// Explicit prop request should not inject extra unrelated properties beyond requested set.
	if strings.Contains(body, "current-user-principal") || strings.Contains(body, "addressbook-home-set") {
		t.Fatalf("PROPFIND explicit props body leaked unrelated props: %q", body)
	}
}

func TestHandler_Propfind_OversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:         backend,
		Sync:            carddavx.NewSyncService(store),
		RequestMaxBytes: 32,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	body := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/alice/contacts/a.vcf", bytes.NewBufferString(body))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("PROPFIND oversize status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_AddressbookExplicitExtensionProps(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/">
  <D:prop>
    <D:sync-token/>
    <CS:getctag/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND addressbook extension props status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sync-token") || !strings.Contains(body, "getctag") {
		t.Fatalf("PROPFIND addressbook extension props missing sync-token/getctag: %q", body)
	}
	if strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND addressbook extension props unexpectedly 404: %q", body)
	}
	syncToken, ctag := mustExtractAddressbookExtensionProps(t, body)
	if !strings.HasPrefix(syncToken, "urn:contactd:sync:") {
		t.Fatalf("sync-token = %q, want urn:contactd:sync:*", syncToken)
	}
	if _, err := strconv.ParseInt(ctag, 10, 64); err != nil {
		t.Fatalf("getctag = %q, want numeric revision string", ctag)
	}
}

func TestHandler_Propfind_AddressbookExtensionProps_MonotonicOnMutations(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	readProps := func() (string, int64) {
		t.Helper()
		req := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/">
  <D:prop>
    <D:sync-token/>
    <CS:getctag/>
  </D:prop>
</D:propfind>`))
		req.SetBasicAuth("alice", "secret")
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMultiStatus {
			t.Fatalf("PROPFIND addressbook extension props status = %d body=%q", rr.Code, rr.Body.String())
		}
		syncToken, ctag := mustExtractAddressbookExtensionProps(t, rr.Body.String())
		rev, err := strconv.ParseInt(ctag, 10, 64)
		if err != nil {
			t.Fatalf("ParseInt ctag %q: %v", ctag, err)
		}
		return syncToken, rev
	}

	sync0, ctag0 := readProps()
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject a: %v", err)
	}
	sync1, ctag1 := readProps()
	if err := backend.DeleteAddressObject(ctx, "/alice/contacts/a.vcf"); err != nil {
		t.Fatalf("DeleteAddressObject a: %v", err)
	}
	sync2, ctag2 := readProps()

	if !(ctag1 > ctag0 && ctag2 > ctag1) {
		t.Fatalf("getctag not monotonic: ctag0=%d ctag1=%d ctag2=%d", ctag0, ctag1, ctag2)
	}
	if sync0 == sync1 || sync1 == sync2 || sync0 == sync2 {
		t.Fatalf("sync-token not changing across mutations: %q %q %q", sync0, sync1, sync2)
	}
}

func TestHandler_Report_UnknownTypeReturns501(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Sync:    carddavx.NewSyncService(store),
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0"?>
<X:unknown-report xmlns:X="urn:example"></X:unknown-report>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusNotImplemented; got != want {
		t.Fatalf("REPORT unknown status = %d, want %d", got, want)
	}
}

func TestHandler_Report_OversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:         backend,
		Sync:            carddavx.NewSyncService(store),
		RequestMaxBytes: 32,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	body := `<?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:"><D:sync-token></D:sync-token><D:sync-level>1</D:sync-level></D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(body))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("REPORT oversize status = %d, want %d", got, want)
	}
}

func TestHandler_Report_AddressbookMultiget_ReturnsSubsetAnd404(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject a: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Alice B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject b: %v", err)
	}
	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            carddavx.NewSyncService(store),
	})

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:getetag/>
    <C:address-data/>
  </D:prop>
  <D:href>/alice/contacts/b.vcf</D:href>
  <D:href>/alice/contacts/missing.vcf</D:href>
</C:addressbook-multiget>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("REPORT multiget status = %d, want %d", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("REPORT multiget content-type = %q", ct)
	}

	type xmlResponse struct {
		Href     string `xml:"href"`
		Status   string `xml:"status"`
		PropStat []struct {
			Status string `xml:"status"`
			Prop   struct {
				GetETag     string `xml:"getetag"`
				AddressData string `xml:"address-data"`
			} `xml:"prop"`
		} `xml:"propstat"`
	}
	var doc struct {
		Responses []xmlResponse `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("xml.Unmarshal REPORT multiget: %v body=%q", err, rr.Body.String())
	}
	if len(doc.Responses) != 2 {
		t.Fatalf("len(responses) = %d, want 2", len(doc.Responses))
	}
	if doc.Responses[0].Href != "/alice/contacts/b.vcf" {
		t.Fatalf("response[0].href = %q, want /alice/contacts/b.vcf", doc.Responses[0].Href)
	}
	if len(doc.Responses[0].PropStat) == 0 || doc.Responses[0].PropStat[0].Prop.GetETag == "" {
		t.Fatalf("response[0] missing getetag: %+v", doc.Responses[0])
	}
	if !strings.Contains(doc.Responses[0].PropStat[0].Prop.AddressData, "UID:uid-b") {
		t.Fatalf("response[0] missing address-data payload: %+v", doc.Responses[0])
	}
	if doc.Responses[1].Href != "/alice/contacts/missing.vcf" {
		t.Fatalf("response[1].href = %q, want /alice/contacts/missing.vcf", doc.Responses[1].Href)
	}
	if !strings.Contains(doc.Responses[1].Status, "404") {
		t.Fatalf("response[1].status = %q, want 404", doc.Responses[1].Status)
	}
}

func TestHandler_Report_AddressbookQuery_ReturnsCardsWithAddressData(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject a: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Alice B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject b: %v", err)
	}
	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            carddavx.NewSyncService(store),
	})

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:getetag/>
    <C:address-data/>
  </D:prop>
</C:addressbook-query>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("REPORT query status = %d, want %d", got, want)
	}
	var doc struct {
		Responses []struct {
			Href     string `xml:"href"`
			PropStat []struct {
				Prop struct {
					GetETag     string `xml:"getetag"`
					AddressData string `xml:"address-data"`
				} `xml:"prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("xml.Unmarshal REPORT query: %v body=%q", err, rr.Body.String())
	}
	if len(doc.Responses) != 2 {
		t.Fatalf("len(responses) = %d, want 2", len(doc.Responses))
	}
	if !containsString([]string{doc.Responses[0].Href, doc.Responses[1].Href}, "/alice/contacts/a.vcf") {
		t.Fatalf("query responses missing a.vcf: %+v", doc.Responses)
	}
	if !containsString([]string{doc.Responses[0].Href, doc.Responses[1].Href}, "/alice/contacts/b.vcf") {
		t.Fatalf("query responses missing b.vcf: %+v", doc.Responses)
	}
	for i, resp := range doc.Responses {
		if len(resp.PropStat) == 0 || resp.PropStat[0].Prop.GetETag == "" {
			t.Fatalf("response[%d] missing getetag: %+v", i, resp)
		}
		if !strings.Contains(resp.PropStat[0].Prop.AddressData, "BEGIN:VCARD") {
			t.Fatalf("response[%d] missing address-data: %+v", i, resp)
		}
	}
}

func TestHandler_Report_SyncCollection_EmptyTokenReturnsSyncTokenAndItems(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject: %v", err)
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            carddavx.NewSyncService(store),
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token></D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("sync-collection empty token status = %d, want %d", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("sync-collection content-type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sync-token") || !strings.Contains(body, "urn:contactd:sync:") {
		t.Fatalf("sync-collection body missing sync-token: %q", body)
	}
	if !strings.Contains(body, "/alice/contacts/a.vcf") || !strings.Contains(body, "getetag") {
		t.Fatalf("sync-collection body missing item response: %q", body)
	}
}

func TestHandler_Report_SyncCollection_InvalidTokenReturns403ValidSyncTokenError(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            carddavx.NewSyncService(store),
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>urn:contactd:sync:999:1</D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusForbidden; got != want {
		t.Fatalf("sync-collection invalid token status = %d, want %d", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("sync-collection invalid token content-type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "valid-sync-token") {
		t.Fatalf("sync-collection invalid token body missing valid-sync-token: %q", body)
	}
	if strings.Contains(body, "<sync-token") {
		t.Fatalf("sync-collection invalid token body must not include sync-token: %q", body)
	}
}

func TestHandler_Report_SyncCollection_LimitUsesContinuationToken(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject a: %v", err)
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

	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Bob B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/c.vcf", mustSampleCard("uid-c", "Carol C"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject c: %v", err)
	}
	if err := backend.DeleteAddressObject(ctx, "/alice/contacts/b.vcf"); err != nil {
		t.Fatalf("DeleteAddressObject b: %v", err)
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            svc,
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + baseline.SyncToken + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:limit><D:nresults>2</D:nresults></D:limit>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("sync-collection limit status = %d, want %d", got, want)
	}
	var doc struct {
		SyncToken string     `xml:"sync-token"`
		Responses []struct{} `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("xml.Unmarshal sync-collection limit: %v body=%q", err, rr.Body.String())
	}
	if len(doc.Responses) != 2 {
		t.Fatalf("sync-collection limit responses = %d, want 2 body=%q", len(doc.Responses), rr.Body.String())
	}
	gotTok, err := carddavx.ParseSyncToken(doc.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken response: %v token=%q body=%q", err, doc.SyncToken, rr.Body.String())
	}
	if gotTok.Revision != baseTok.Revision+2 {
		t.Fatalf("sync-collection continuation token revision = %d, want %d body=%q", gotTok.Revision, baseTok.Revision+2, rr.Body.String())
	}
}

func TestHandler_Report_SyncCollection_StalePrunedTokenReturns403ValidSyncTokenError(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject a: %v", err)
	}
	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Bob B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/c.vcf", mustSampleCard("uid-c", "Carol C"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject c: %v", err)
	}
	if _, err := store.PruneCardChangesByMaxRevisions(context.Background(), 1); err != nil {
		t.Fatalf("PruneCardChangesByMaxRevisions: %v", err)
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            svc,
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + baseline.SyncToken + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusForbidden; got != want {
		t.Fatalf("sync-collection stale token status = %d, want %d", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("sync-collection stale token content-type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "valid-sync-token") {
		t.Fatalf("sync-collection stale token body missing valid-sync-token: %q", body)
	}
}

func TestHandler_Report_SyncCollection_Limit_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	ctx := contactcarddav.WithPrincipal(context.Background(), "alice")
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/a.vcf", mustSampleCard("uid-a", "Alice A"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("seed PutAddressObject a: %v", err)
	}
	svc := carddavx.NewSyncService(store)
	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/b.vcf", mustSampleCard("uid-b", "Bob B"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject b: %v", err)
	}
	if _, err := backend.PutAddressObject(ctx, "/alice/contacts/c.vcf", mustSampleCard("uid-c", "Carol C"), &gocarddav.PutAddressObjectOptions{}); err != nil {
		t.Fatalf("PutAddressObject c: %v", err)
	}
	if err := backend.DeleteAddressObject(ctx, "/alice/contacts/b.vcf"); err != nil {
		t.Fatalf("DeleteAddressObject b: %v", err)
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            svc,
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + baseline.SyncToken + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:limit><D:nresults>2</D:nresults></D:limit>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("sync-collection golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "sync_collection_limit_multistatus.xml")
}

func TestHandler_Report_SyncCollection_InvalidToken_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            carddavx.NewSyncService(store),
	})
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>urn:contactd:sync:999:1</D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusForbidden; got != want {
		t.Fatalf("sync-collection invalid golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "sync_collection_invalid_token_error.xml")
}

func openServerBackend(t *testing.T) (*db.Store, *contactcarddav.Backend) {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "contactd.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store, contactcarddav.NewBackend(store)
}

func newAuthedHandlerForTests(backend *contactcarddav.Backend) http.Handler {
	return server.NewHandler(server.HandlerOptions{
		Backend: backend,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})
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
	card := mustSampleCard(uid, fn)
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		panic(err)
	}
	return buf.String()
}

func mustSampleCard(uid, fn string) vcard.Card {
	card := make(vcard.Card)
	card.SetValue(vcard.FieldVersion, "3.0")
	card.SetValue(vcard.FieldUID, uid)
	card.SetValue(vcard.FieldFormattedName, fn)
	return card
}

func doPropfind(t *testing.T, h http.Handler, target, depth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PROPFIND", target, bytes.NewBufferString(`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func mustCollectHrefs(t *testing.T, body string) []string {
	t.Helper()
	var doc struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v; body=%q", err, body)
	}
	out := make([]string, 0, len(doc.Responses))
	for _, r := range doc.Responses {
		out = append(out, r.Href)
	}
	return out
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

var (
	reGoldenSyncToken = regexp.MustCompile(`urn:contactd:sync:\d+:\d+`)
	reGoldenETag      = regexp.MustCompile(`&#34;[0-9a-f]{64}&#34;`)
	rePropfindSyncTok = regexp.MustCompile(`<sync-token[^>]*>([^<]+)</sync-token>`)
	rePropfindCTag    = regexp.MustCompile(`<getctag[^>]*>([^<]+)</getctag>`)
)

func assertGoldenSyncXML(t *testing.T, gotBody, fixtureName string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", fixtureName))
	if err != nil {
		t.Fatalf("ReadFile fixture %s: %v", fixtureName, err)
	}
	gotNorm := normalizeSyncGolden(gotBody)
	wantNorm := normalizeSyncGolden(string(want))
	if gotNorm != wantNorm {
		t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", fixtureName, gotNorm, wantNorm)
	}
}

func normalizeSyncGolden(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	s = reGoldenSyncToken.ReplaceAllString(s, "{{SYNC_TOKEN}}")
	s = reGoldenETag.ReplaceAllString(s, "{{ETAG}}")
	return s
}

func mustExtractAddressbookExtensionProps(t *testing.T, body string) (syncToken, ctag string) {
	t.Helper()
	mTok := rePropfindSyncTok.FindStringSubmatch(body)
	if len(mTok) != 2 {
		t.Fatalf("missing sync-token prop in body: %q", body)
	}
	mCTag := rePropfindCTag.FindStringSubmatch(body)
	if len(mCTag) != 2 {
		t.Fatalf("missing getctag prop in body: %q", body)
	}
	return mTok[1], mCTag[1]
}

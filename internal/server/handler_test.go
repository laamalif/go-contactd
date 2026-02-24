package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
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

func TestDavx_Discovery_WellKnownRedirect(t *testing.T) {
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

func TestHandler_WellKnownCardDAVRedirect_UsesBaseURLWhenConfigured(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{BaseURL: "https://dav.example.com/prefix"})
	req := httptest.NewRequest(http.MethodGet, "/.well-known/carddav", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusPermanentRedirect; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Location"), "https://dav.example.com/prefix/"; got != want {
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

func TestHandler_AccessLog_JSON_OnePerRequestAndRequiredFields(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := server.NewHandler(server.HandlerOptions{Logger: logger})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "10.0.0.5:4242"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	lines := nonEmptyLines(logBuf.String())
	if got, want := len(lines), 1; got != want {
		t.Fatalf("access log lines = %d, want %d; logs=%q", got, want, logBuf.String())
	}
	entry := parseJSONLogLine(t, lines[0])
	if got, want := entry["event"], "request"; got != want {
		t.Fatalf("event = %#v, want %q", got, want)
	}
	if got, want := entry["method"], http.MethodGet; got != want {
		t.Fatalf("method = %#v, want %q", got, want)
	}
	if got, want := entry["path"], "/healthz"; got != want {
		t.Fatalf("path = %#v, want %q", got, want)
	}
	if got, want := int(entry["status"].(float64)), http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if _, ok := entry["dur_ms"]; !ok {
		t.Fatalf("dur_ms missing in log entry %#v", entry)
	}
	if _, ok := entry["req_bytes"]; !ok {
		t.Fatalf("req_bytes missing in log entry %#v", entry)
	}
	if _, ok := entry["resp_bytes"]; !ok {
		t.Fatalf("resp_bytes missing in log entry %#v", entry)
	}
	if got, want := entry["remote"], "10.0.0.5"; got != want {
		t.Fatalf("remote = %#v, want %q", got, want)
	}
	if got, want := entry["user"], ""; got != want {
		t.Fatalf("user = %#v, want empty", got)
	}
}

func TestHandler_AccessLog_NoAuthorizationLeak_And_UnauthenticatedUserEmpty(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := server.NewHandler(server.HandlerOptions{
		Logger: logger,
		Authenticate: func(context.Context, string, string) (string, bool, error) {
			return "", false, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/alice/", nil)
	req.SetBasicAuth("alice", "supersecret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	lines := nonEmptyLines(logBuf.String())
	if got, want := len(lines), 1; got != want {
		t.Fatalf("access log lines = %d, want %d; logs=%q", got, want, logBuf.String())
	}
	entry := parseJSONLogLine(t, lines[0])
	if got, want := int(entry["status"].(float64)), http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := entry["user"], ""; got != want {
		t.Fatalf("user = %#v, want empty", got)
	}
	logs := logBuf.String()
	if strings.Contains(logs, "Authorization") || strings.Contains(logs, "supersecret") || strings.Contains(logs, "YWxpY2U6c3VwZXJzZWNyZXQ=") {
		t.Fatalf("access log leaked sensitive auth material: %q", logs)
	}
}

func TestHandler_AccessLog_ProxyRemoteBehavior_TrustDisabledAndEnabled(t *testing.T) {
	t.Parallel()

	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "10.0.0.5:4242"
		req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.5")
		return req
	}

	var disabledBuf bytes.Buffer
	hDisabled := server.NewHandler(server.HandlerOptions{
		Logger: slog.New(slog.NewJSONHandler(&disabledBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	rrDisabled := httptest.NewRecorder()
	hDisabled.ServeHTTP(rrDisabled, makeReq())
	entryDisabled := parseJSONLogLine(t, nonEmptyLines(disabledBuf.String())[0])
	if got, want := entryDisabled["remote"], "10.0.0.5"; got != want {
		t.Fatalf("trust disabled remote = %#v, want %q", got, want)
	}

	var enabledBuf bytes.Buffer
	hEnabled := server.NewHandler(server.HandlerOptions{
		Logger:            slog.New(slog.NewJSONHandler(&enabledBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		TrustProxyHeaders: true,
	})
	rrEnabled := httptest.NewRecorder()
	hEnabled.ServeHTTP(rrEnabled, makeReq())
	entryEnabled := parseJSONLogLine(t, nonEmptyLines(enabledBuf.String())[0])
	if got, want := entryEnabled["remote"], "203.0.113.10"; got != want {
		t.Fatalf("trust enabled remote = %#v, want %q", got, want)
	}
}

func TestHandler_AccessLog_LevelFiltering_WarnSuppressesInfoAccessLogs(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := server.NewHandler(server.HandlerOptions{Logger: logger})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := strings.TrimSpace(logBuf.String()); got != "" {
		t.Fatalf("expected no access logs at warn level, got %q", got)
	}
}

func TestHandler_CardPutGetDelete_WithBackend(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

type putStatusFakeBackend struct{}

func (putStatusFakeBackend) CurrentUserPrincipal(context.Context) (string, error) {
	return "/alice/", nil
}
func (putStatusFakeBackend) AddressBookHomeSetPath(context.Context) (string, error) {
	return "/alice/", nil
}
func (putStatusFakeBackend) ListAddressBooks(context.Context) ([]gocarddav.AddressBook, error) {
	return nil, nil
}
func (putStatusFakeBackend) GetAddressBook(context.Context, string) (*gocarddav.AddressBook, error) {
	return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
}
func (putStatusFakeBackend) CreateAddressBook(context.Context, *gocarddav.AddressBook) error {
	return nil
}
func (putStatusFakeBackend) DeleteAddressBook(context.Context, string) error { return nil }
func (putStatusFakeBackend) GetAddressObject(context.Context, string, *gocarddav.AddressDataRequest) (*gocarddav.AddressObject, error) {
	return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
}
func (putStatusFakeBackend) ListAddressObjects(context.Context, string, *gocarddav.AddressDataRequest) ([]gocarddav.AddressObject, error) {
	return nil, nil
}
func (putStatusFakeBackend) QueryAddressObjects(context.Context, string, *gocarddav.AddressBookQuery) ([]gocarddav.AddressObject, error) {
	return nil, nil
}
func (putStatusFakeBackend) PutAddressObject(context.Context, string, vcard.Card, *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, error) {
	return nil, errors.New("legacy PutAddressObject should not be called when status-aware put is available")
}
func (putStatusFakeBackend) PutAddressObjectWithStatus(_ context.Context, p string, card vcard.Card, _ *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, bool, error) {
	return &gocarddav.AddressObject{Path: p, ETag: `"etag-updated"`, Card: card}, false, nil
}
func (putStatusFakeBackend) DeleteAddressObject(context.Context, string) error { return nil }

func TestHandler_CardPut_UsesBackendCreateStatusWhenAvailable(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{
		Backend: putStatusFakeBackend{},
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return "alice", username == "alice" && password == "secret", nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	body := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:u1\r\nFN:Alice\r\nEND:VCARD\r\n"
	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusNoContent; got != want {
		t.Fatalf("status = %d, want %d; body=%q", got, want, rr.Body.String())
	}
	if got := rr.Header().Get("ETag"); got != `"etag-updated"` {
		t.Fatalf("ETag = %q, want quoted etag", got)
	}
}

type putStatusCreatedFakeBackend struct{ putStatusFakeBackend }

func (putStatusCreatedFakeBackend) PutAddressObjectWithStatus(_ context.Context, p string, card vcard.Card, _ *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, bool, error) {
	return &gocarddav.AddressObject{Path: p, ETag: `"etag-created"`, Card: card}, true, nil
}

func TestHandler_CardPut_UsesBackendCreateStatusCreatedPathWhenAvailable(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{
		Backend: putStatusCreatedFakeBackend{},
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return "alice", username == "alice" && password == "secret", nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	body := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:u2\r\nFN:Alice Created\r\nEND:VCARD\r\n"
	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/b.vcf", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusCreated; got != want {
		t.Fatalf("status = %d, want %d; body=%q", got, want, rr.Body.String())
	}
	if got := rr.Header().Get("ETag"); got != `"etag-created"` {
		t.Fatalf("ETag = %q, want quoted created etag", got)
	}
}

func TestDavx_Options_AdvertisesDAV_1_3_Addressbook(t *testing.T) {
	t.Parallel()

	h := server.NewHandler(server.HandlerOptions{})
	req := httptest.NewRequest(http.MethodOptions, "/alice/contacts/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusNoContent; got != want {
		t.Fatalf("OPTIONS status = %d, want %d", got, want)
	}
	if got := rr.Header().Get("DAV"); got != "1, 3, addressbook" {
		t.Fatalf("DAV header = %q, want %q", got, "1, 3, addressbook")
	}
}

func TestDavx_Read_GetCard_IncludesContentTypeETagContentLength(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/g.vcf", bytes.NewBufferString(vcardBody("uid-g", "Getter")))
	putReq.Header.Set("Content-Type", "text/vcard")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("seed PUT status = %d, want %d", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/alice/contacts/g.vcf", nil)
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("GET status = %d, want %d", got, want)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/vcard" {
		t.Fatalf("Content-Type = %q, want text/vcard", got)
	}
	if got := rr.Header().Get("ETag"); got == "" {
		t.Fatal("ETag header missing")
	}
	if got := rr.Header().Get("Content-Length"); got == "" || got == "0" {
		t.Fatalf("Content-Length = %q, want non-empty/non-zero", got)
	}
}

func TestHandler_CardPropfind_IncludesShouldProps(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/p.vcf", bytes.NewBufferString(vcardBody("uid-p", "Prop Card")))
	putReq.Header.Set("Content-Type", "text/vcard")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("seed PUT status = %d, want %d", got, want)
	}

	reqBody := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/><D:resourcetype/><D:getcontenttype/><D:getcontentlength/><D:getlastmodified/></D:prop></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/alice/contacts/p.vcf", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND status = %d, want %d body=%q", got, want, rr.Body.String())
	}
	body := rr.Body.String()
	for _, needle := range []string{"getetag", "resourcetype", "getcontenttype", "getcontentlength", "getlastmodified"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("PROPFIND body missing %s: %q", needle, body)
		}
	}
	if strings.Contains(body, "HTTP/1.1 404 Not Found") && (strings.Contains(body, "getcontenttype") || strings.Contains(body, "getcontentlength") || strings.Contains(body, "getlastmodified")) {
		t.Fatalf("SHOULD props unexpectedly returned 404 propstat: %q", body)
	}
}

func TestHandler_Mkcol_ParsesBodyMetadata(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set>
    <D:prop>
      <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
      <D:displayname>Friends Book</D:displayname>
      <C:addressbook-description>Friends desc</C:addressbook-description>
    </D:prop>
  </D:set>
</D:mkcol>`
	req := httptest.NewRequest("MKCOL", "/alice/friends-meta/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusCreated; got != want {
		t.Fatalf("MKCOL status = %d, want %d body=%q", got, want, rr.Body.String())
	}

	propfindReq := httptest.NewRequest("PROPFIND", "/alice/friends-meta/", bytes.NewBufferString(
		`<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:displayname/><C:addressbook-description/></D:prop></D:propfind>`))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)
	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND status = %d, want %d body=%q", got, want, propfindRes.Body.String())
	}
	if body := propfindRes.Body.String(); !strings.Contains(body, "Friends Book") || !strings.Contains(body, "Friends desc") {
		t.Fatalf("PROPFIND missing MKCOL metadata body=%q", body)
	}
}

func TestHandler_AddressbookDelete_RoutesToBackend(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	req := httptest.NewRequest(http.MethodDelete, "/alice/contacts/", nil)
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusNoContent; got != want {
		t.Fatalf("DELETE addressbook status = %d, want %d body=%q", got, want, rr.Body.String())
	}

	has, err := store.HasAddressbook(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("HasAddressbook: %v", err)
	}
	if has {
		t.Fatal("addressbook still exists after DELETE")
	}
}

func TestHandler_CardPut_RejectsMissingOrUnsupportedContentType(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

func TestHandler_CrossUserAccess_Returns404(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Alice Contacts")
	seedServerUserBook(t, store, "bob", "contacts", "Bob Contacts")

	bobBook, err := store.GetAddressbookByUsernameSlug(context.Background(), "bob", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug bob/contacts: %v", err)
	}
	if _, err := store.PutCard(context.Background(), db.PutCardInput{
		AddressbookID: bobBook.ID,
		Href:          "b.vcf",
		UID:           "uid-bob",
		VCard:         []byte(vcardBody("uid-bob", "Bob B")),
	}); err != nil {
		t.Fatalf("seed bob card: %v", err)
	}

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

	getReq := httptest.NewRequest(http.MethodGet, "/bob/contacts/b.vcf", nil)
	getReq.SetBasicAuth("alice", "secret")
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, getReq)
	if got, want := getRes.Code, http.StatusNotFound; got != want {
		t.Fatalf("cross-user GET status = %d, want %d", got, want)
	}

	putReq := httptest.NewRequest(http.MethodPut, "/bob/contacts/new.vcf", bytes.NewBufferString(vcardBody("uid-new", "Nope")))
	putReq.Header.Set("Content-Type", "text/vcard")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusNotFound; got != want {
		t.Fatalf("cross-user PUT status = %d, want %d", got, want)
	}
}

func TestHandler_CardRoundTripFidelity_PreservesUnicodeAndPhotoPayload(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := newAuthedHandlerForTests(backend)
	const photo = "AAECAwQFBgcICQ=="
	body := strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:3.0",
		"UID:uid-fidelity",
		"FN:Zoë 🚀",
		"PHOTO;ENCODING=b;TYPE=JPEG:" + photo,
		"END:VCARD",
		"",
	}, "\r\n")

	putReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/fidelity.vcf", bytes.NewBufferString(body))
	putReq.Header.Set("Content-Type", "text/vcard; charset=utf-8")
	putReq.SetBasicAuth("alice", "secret")
	putRes := httptest.NewRecorder()
	h.ServeHTTP(putRes, putReq)
	if got, want := putRes.Code, http.StatusCreated; got != want {
		t.Fatalf("PUT status = %d, want %d body=%q", got, want, putRes.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/alice/contacts/fidelity.vcf", nil)
	getReq.SetBasicAuth("alice", "secret")
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, getReq)
	if got, want := getRes.Code, http.StatusOK; got != want {
		t.Fatalf("GET status = %d, want %d", got, want)
	}
	gotBody := getRes.Body.String()
	if !strings.Contains(gotBody, "FN:Zoë 🚀") {
		t.Fatalf("GET body missing unicode FN: %q", gotBody)
	}
	if !strings.Contains(gotBody, photo) {
		t.Fatalf("GET body missing photo payload: %q", gotBody)
	}
}

func TestHandler_CardPut_IfMatchRace_One204One412(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")

	h := newAuthedHandlerForTests(backend)

	seedReq := httptest.NewRequest(http.MethodPut, "/alice/contacts/race.vcf", bytes.NewBufferString(vcardBody("uid-race", "Base")))
	seedReq.Header.Set("Content-Type", "text/vcard")
	seedReq.SetBasicAuth("alice", "secret")
	seedRes := httptest.NewRecorder()
	h.ServeHTTP(seedRes, seedReq)
	if got, want := seedRes.Code, http.StatusCreated; got != want {
		t.Fatalf("seed PUT status = %d, want %d", got, want)
	}
	etag := seedRes.Header().Get("ETag")
	if etag == "" {
		t.Fatal("seed PUT missing ETag")
	}

	blockFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hookCalls atomic.Int32
	store.SetTestHooks(db.TestHooks{
		BeforeCardChangeInsert: func() error {
			if hookCalls.Add(1) == 1 {
				close(blockFirst)
				<-releaseFirst
			}
			return nil
		},
	})
	defer store.SetTestHooks(db.TestHooks{})

	type result struct {
		status int
		body   string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	runUpdate := func(name string) {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPut, "/alice/contacts/race.vcf", bytes.NewBufferString(vcardBody("uid-race", name)))
		req.Header.Set("Content-Type", "text/vcard")
		req.Header.Set("If-Match", etag)
		req.SetBasicAuth("alice", "secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		results <- result{status: rr.Code, body: rr.Body.String()}
	}

	wg.Add(2)
	go runUpdate("Alice A")
	<-blockFirst
	go runUpdate("Alice B")
	close(releaseFirst)
	wg.Wait()
	close(results)

	var count204, count412 int
	for res := range results {
		switch res.status {
		case http.StatusNoContent:
			count204++
		case http.StatusPreconditionFailed:
			count412++
		default:
			t.Fatalf("unexpected concurrent PUT status = %d body=%q", res.status, res.body)
		}
	}
	if count204 != 1 || count412 != 1 {
		t.Fatalf("concurrent PUT results = 204x%d/412x%d, want 1/1", count204, count412)
	}
}

func TestHandler_Mkcol_Create201_And_Existing405(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	reqCreate := httptest.NewRequest("MKCOL", "/alice/friends/", nil)
	reqCreate.SetBasicAuth("alice", "secret")
	rrCreate := httptest.NewRecorder()
	h.ServeHTTP(rrCreate, reqCreate)
	if got, want := rrCreate.Code, http.StatusCreated; got != want {
		t.Fatalf("MKCOL create status = %d, want %d body=%q", got, want, rrCreate.Body.String())
	}

	reqCreateAgain := httptest.NewRequest("MKCOL", "/alice/friends/", nil)
	reqCreateAgain.SetBasicAuth("alice", "secret")
	rrCreateAgain := httptest.NewRecorder()
	h.ServeHTTP(rrCreateAgain, reqCreateAgain)
	if got, want := rrCreateAgain.Code, http.StatusMethodNotAllowed; got != want {
		t.Fatalf("MKCOL existing status = %d, want %d body=%q", got, want, rrCreateAgain.Body.String())
	}

	hasBook, err := store.HasAddressbook(context.Background(), "alice", "friends")
	if err != nil {
		t.Fatalf("HasAddressbook friends: %v", err)
	}
	if !hasBook {
		t.Fatal("MKCOL did not persist addressbook")
	}
}

func TestHandler_Mkcol_InvalidTargetPath_Rejected(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	for _, target := range []string{"/", "/alice/"} {
		req := httptest.NewRequest("MKCOL", target, nil)
		req.SetBasicAuth("alice", "secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if got, want := rr.Code, http.StatusNotFound; got != want {
			t.Fatalf("MKCOL invalid target %q status = %d, want %d body=%q", target, got, want, rr.Body.String())
		}
	}
}

func TestHandler_CardPath_RejectsEncodedSlashInHref(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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

func TestHandler_CardDelete_MissingReturns404(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest(http.MethodDelete, "/alice/contacts/missing.vcf", nil)
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Fatalf("DELETE missing status = %d, want %d; body=%q", got, want, rr.Body.String())
	}
}

func TestHandler_CardPut_UIDConflict_ReturnsCardDAVPreconditionXML(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	if body, want := rr.Body.String(), http.StatusText(http.StatusRequestEntityTooLarge)+"\n"; body != want {
		t.Fatalf("PUT oversize body = %q, want %q", body, want)
	}
}

func TestHandler_CardPut_ExceedsVCardMaxButWithinRequestMax_Returns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:         backend,
		Sync:            carddavx.NewSyncService(store),
		RequestMaxBytes: 1024,
		VCardMaxBytes:   48,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString(vcardBody("uid-a", strings.Repeat("A", 128))))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("PUT vcard-max status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_PrincipalDepth0And1(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

func TestHandler_Propfind_RootDiscoveryDepth0_ReturnsCurrentUserPrincipal(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	reqBodyBytes, err := os.ReadFile(filepath.Join("testdata", "davx5_root_propfind.xml"))
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}
	req := httptest.NewRequest("PROPFIND", "/", bytes.NewBuffer(reqBodyBytes))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND / status=%d want %d body=%q", got, want, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<href>/</href>`,
		`current-user-principal`,
		`/alice/`,
		`addressbook-home-set`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PROPFIND / body missing %q: %q", want, body)
		}
	}
}

func TestHandler_Propfind_AddressbookAndCardDepthHandling(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	if body, want := rr.Body.String(), http.StatusText(http.StatusRequestEntityTooLarge)+"\n"; body != want {
		t.Fatalf("PROPFIND oversize body = %q, want %q", body, want)
	}
}

func TestHandler_Propfind_AddressbookPath_RejectsTraversalSegment(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest("PROPFIND", "/alice/../contacts/", bytes.NewBufferString(`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PROPFIND addressbook traversal status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_AddressbookPath_RejectsEncodedSlashInSegment(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest("PROPFIND", "/alice%2Ftenant/contacts/", bytes.NewBufferString(`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PROPFIND addressbook encoded slash status = %d, want %d", got, want)
	}
}

func TestHandler_Propfind_MalformedOversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/>` + strings.Repeat("X", 128)
	req := httptest.NewRequest("PROPFIND", "/alice/contacts/a.vcf", bytes.NewBufferString(body))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("PROPFIND malformed oversize status = %d, want %d", got, want)
	}
	if body, want := rr.Body.String(), http.StatusText(http.StatusRequestEntityTooLarge)+"\n"; body != want {
		t.Fatalf("PROPFIND malformed oversize body = %q, want %q", body, want)
	}
}

func TestHandler_Propfind_AddressbookExplicitExtensionProps(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

func TestHandler_Propfind_AddressbookSupportedAddressData_Explicit(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <C:supported-address-data/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND supported-address-data status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND supported-address-data unexpectedly 404: %q", body)
	}
	if !strings.Contains(body, "supported-address-data") || !strings.Contains(body, "address-data-type") {
		t.Fatalf("PROPFIND supported-address-data missing property/type element: %q", body)
	}
	if !strings.Contains(body, `content-type="text/vcard"`) || !strings.Contains(body, `version="3.0"`) {
		t.Fatalf("PROPFIND supported-address-data missing expected vcard type attrs: %q", body)
	}
}

func TestHandler_Propfind_AddressbookExplicitExtensionProps_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
		t.Fatalf("PROPFIND addressbook extension props golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "propfind_addressbook_extension_props_explicit.xml")
}

func TestHandler_Propfind_AddressbookSupportedReportSet_Explicit(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:supported-report-set/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND addressbook supported-report-set status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND addressbook supported-report-set unexpectedly 404: %q", body)
	}
	if !strings.Contains(body, "supported-report-set") {
		t.Fatalf("PROPFIND addressbook supported-report-set missing property: %q", body)
	}
	if !strings.Contains(body, "sync-collection") || !strings.Contains(body, "addressbook-multiget") || !strings.Contains(body, "addressbook-query") {
		t.Fatalf("PROPFIND addressbook supported-report-set missing reports: %q", body)
	}
}

func TestHandler_Propfind_AddressbookSupportedReportSet_Explicit_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:supported-report-set/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND addressbook supported-report-set golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "propfind_addressbook_supported_report_set_explicit.xml")
}

func TestHandler_Propfind_AddressbookAllPropExtensionProps_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	rr := doPropfind(t, h, "/alice/contacts/", "0")
	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND addressbook allprop golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "propfind_addressbook_allprop_extension_props.xml")
}

func TestHandler_Propfind_AddressbookExtensionProps_MonotonicOnMutations(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	if ctag1 <= ctag0 || ctag2 <= ctag1 {
		t.Fatalf("getctag not monotonic: ctag0=%d ctag1=%d ctag2=%d", ctag0, ctag1, ctag2)
	}
	if sync0 == sync1 || sync1 == sync2 || sync0 == sync2 {
		t.Fatalf("sync-token not changing across mutations: %q %q %q", sync0, sync1, sync2)
	}
}

func TestHandler_Report_UnknownTypeReturns501(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

func TestHandler_Proppatch_AddressbookUnsupportedProps_Returns207ForbiddenPropstat(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/" xmlns:X="urn:example">
  <D:set>
    <D:prop>
      <D:sync-token>ignored</D:sync-token>
      <X:foo>bar</X:foo>
    </D:prop>
  </D:set>
  <D:remove>
    <D:prop>
      <CS:getctag/>
    </D:prop>
  </D:remove>
</D:propertyupdate>`
	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH unsupported status = %d, want %d", got, want)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Fatalf("PROPPATCH unsupported content-type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/alice/contacts/") {
		t.Fatalf("PROPPATCH body missing href: %q", body)
	}
	if !strings.Contains(body, "403 Forbidden") {
		t.Fatalf("PROPPATCH body missing 403 propstat: %q", body)
	}
	if !strings.Contains(body, "sync-token") || !strings.Contains(body, "getctag") || !strings.Contains(body, "foo") {
		t.Fatalf("PROPPATCH body missing echoed unsupported props: %q", body)
	}
}

func TestHandler_Proppatch_AddressbookUnsupportedProps_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/" xmlns:X="urn:example">
  <D:set>
    <D:prop>
      <D:sync-token>ignored</D:sync-token>
      <X:foo>bar</X:foo>
    </D:prop>
  </D:set>
  <D:remove>
    <D:prop>
      <CS:getctag/>
    </D:prop>
  </D:remove>
</D:propertyupdate>`
	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH unsupported golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "proppatch_unsupported_multistatus.xml")
}

func TestHandler_Proppatch_AddressbookMetadata_MixedSupportedAndUnsupportedPropstats(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav" xmlns:X="urn:example">
  <D:set>
    <D:prop>
      <D:displayname>Team Contacts</D:displayname>
      <X:foo>bar</X:foo>
    </D:prop>
  </D:set>
  <D:remove>
    <D:prop>
      <C:addressbook-description/>
    </D:prop>
  </D:remove>
</D:propertyupdate>`
	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH mixed status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "200 OK") || !strings.Contains(body, "403 Forbidden") {
		t.Fatalf("PROPPATCH mixed body missing 200/403 propstats: %q", body)
	}
	if !strings.Contains(body, "displayname") || !strings.Contains(body, "addressbook-description") || !strings.Contains(body, "foo") {
		t.Fatalf("PROPPATCH mixed body missing expected props: %q", body)
	}

	propfindReq := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:displayname/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>`))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)
	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND after mixed PROPPATCH status = %d, want %d", got, want)
	}
	propfindBody := propfindRes.Body.String()
	if !strings.Contains(propfindBody, "Team Contacts") {
		t.Fatalf("PROPFIND after mixed PROPPATCH missing persisted displayname: %q", propfindBody)
	}
	if !strings.Contains(propfindBody, "addressbook-description") || !strings.Contains(propfindBody, "404 Not Found") {
		t.Fatalf("PROPFIND after mixed PROPPATCH missing cleared description 404 propstat: %q", propfindBody)
	}
}

func TestHandler_Proppatch_AddressbookColor_DisabledReturnsForbiddenPropstat(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:set>
    <D:prop>
      <INF:addressbook-color>#ff0000</INF:addressbook-color>
    </D:prop>
  </D:set>
</D:propertyupdate>`
	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH color disabled status = %d, want %d", got, want)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "addressbook-color") || !strings.Contains(body, "403 Forbidden") {
		t.Fatalf("PROPPATCH color disabled missing 403 propstat: %q", body)
	}
}

func TestHandler_Proppatch_AddressbookColor_EnabledPersistsAndPropfindExposes(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:                backend,
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: true,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	patchReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:set>
    <D:prop>
      <INF:addressbook-color>#ff0000ff</INF:addressbook-color>
    </D:prop>
  </D:set>
</D:propertyupdate>`))
	patchReq.SetBasicAuth("alice", "secret")
	patchReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	patchRes := httptest.NewRecorder()
	h.ServeHTTP(patchRes, patchReq)

	if got, want := patchRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH color enabled status = %d, want %d", got, want)
	}
	if body := patchRes.Body.String(); !strings.Contains(body, "addressbook-color") || !strings.Contains(body, "200 OK") {
		t.Fatalf("PROPPATCH color enabled body missing 200 propstat: %q", body)
	}

	propfindReq := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:prop>
    <INF:addressbook-color/>
  </D:prop>
</D:propfind>`))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)

	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND color enabled status = %d, want %d", got, want)
	}
	body := propfindRes.Body.String()
	if strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND color enabled unexpectedly 404: %q", body)
	}
	if !strings.Contains(body, "addressbook-color") || !strings.Contains(body, "#ff0000ff") {
		t.Fatalf("PROPFIND color enabled missing color value: %q", body)
	}
}

func TestHandler_Proppatch_AddressbookColor_Enabled_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:                backend,
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: true,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:set>
    <D:prop>
      <INF:addressbook-color>#ff0000ff</INF:addressbook-color>
    </D:prop>
  </D:set>
</D:propertyupdate>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH color golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "proppatch_addressbook_color_enabled.xml")
}

func TestHandler_Propfind_AddressbookColor_Enabled_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:                backend,
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: true,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	patchReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:set>
    <D:prop>
      <INF:addressbook-color>#ff0000ff</INF:addressbook-color>
    </D:prop>
  </D:set>
</D:propertyupdate>`))
	patchReq.SetBasicAuth("alice", "secret")
	patchReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	patchRes := httptest.NewRecorder()
	h.ServeHTTP(patchRes, patchReq)
	if got, want := patchRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH color setup status = %d, want %d", got, want)
	}

	req := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:prop>
    <INF:addressbook-color/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND color golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "propfind_addressbook_color_enabled_explicit.xml")
}

func TestHandler_Proppatch_AddressbookColor_Enabled_DuplicateOps_LastWins(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := server.NewHandler(server.HandlerOptions{
		Backend:                backend,
		Sync:                   carddavx.NewSyncService(store),
		EnableAddressbookColor: true,
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			if username == "alice" && password == "secret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		AttachPrincipal: contactcarddav.WithPrincipal,
	})

	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:set><D:prop><INF:addressbook-color>#ff0000ff</INF:addressbook-color></D:prop></D:set>
  <D:remove><D:prop><INF:addressbook-color/></D:prop></D:remove>
  <D:set><D:prop><INF:addressbook-color>#00ff00ff</INF:addressbook-color></D:prop></D:set>
</D:propertyupdate>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH color duplicate ops status = %d, want %d", got, want)
	}

	propfindReq := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:INF="http://inf-it.com/ns/ab/">
  <D:prop><INF:addressbook-color/></D:prop>
</D:propfind>`))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)
	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND after duplicate color ops status = %d, want %d", got, want)
	}
	if body := propfindRes.Body.String(); !strings.Contains(body, "#00ff00ff") {
		t.Fatalf("PROPFIND after duplicate color ops missing last value: %q", body)
	}
}

func TestHandler_Proppatch_InvalidXML_Returns400(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<D:propertyupdate xmlns:D="DAV:"><D:set>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PROPPATCH invalid xml status = %d, want %d", got, want)
	}
}

func TestHandler_Proppatch_AddressbookMetadata_PersistsAndPropfindExposes(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	proppatchBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set>
    <D:prop>
      <D:displayname>Team Contacts</D:displayname>
      <C:addressbook-description>Shared directory</C:addressbook-description>
    </D:prop>
  </D:set>
</D:propertyupdate>`
	proppatchReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(proppatchBody))
	proppatchReq.SetBasicAuth("alice", "secret")
	proppatchReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	proppatchRes := httptest.NewRecorder()
	h.ServeHTTP(proppatchRes, proppatchReq)

	if got, want := proppatchRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH metadata set status = %d, want %d", got, want)
	}
	if body := proppatchRes.Body.String(); !strings.Contains(body, "200 OK") {
		t.Fatalf("PROPPATCH metadata set body missing 200 propstat: %q", body)
	}

	propfindBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:displayname/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>`
	propfindReq := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(propfindBody))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)

	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND metadata props status = %d, want %d", got, want)
	}
	body := propfindRes.Body.String()
	if !strings.Contains(body, "displayname") || !strings.Contains(body, "Team Contacts") {
		t.Fatalf("PROPFIND metadata body missing displayname value: %q", body)
	}
	if !strings.Contains(body, "addressbook-description") || !strings.Contains(body, "Shared directory") {
		t.Fatalf("PROPFIND metadata body missing addressbook-description value: %q", body)
	}
	if !strings.Contains(body, "200 OK") {
		t.Fatalf("PROPFIND metadata body missing 200 propstat: %q", body)
	}
}

func TestHandler_Proppatch_AddressbookMetadata_RemoveClearsPropfindProps(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	setBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set>
    <D:prop>
      <D:displayname>Team Contacts</D:displayname>
      <C:addressbook-description>Shared directory</C:addressbook-description>
    </D:prop>
  </D:set>
</D:propertyupdate>`
	setReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(setBody))
	setReq.SetBasicAuth("alice", "secret")
	setReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	setRes := httptest.NewRecorder()
	h.ServeHTTP(setRes, setReq)
	if got, want := setRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH metadata seed status = %d, want %d", got, want)
	}

	removeBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:remove>
    <D:prop>
      <D:displayname/>
      <C:addressbook-description/>
    </D:prop>
  </D:remove>
</D:propertyupdate>`
	removeReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(removeBody))
	removeReq.SetBasicAuth("alice", "secret")
	removeReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	removeRes := httptest.NewRecorder()
	h.ServeHTTP(removeRes, removeReq)

	if got, want := removeRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH metadata remove status = %d, want %d", got, want)
	}
	if body := removeRes.Body.String(); !strings.Contains(body, "200 OK") {
		t.Fatalf("PROPPATCH metadata remove body missing 200 propstat: %q", body)
	}

	propfindBody := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:displayname/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>`
	propfindReq := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(propfindBody))
	propfindReq.SetBasicAuth("alice", "secret")
	propfindReq.Header.Set("Depth", "0")
	propfindReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	propfindRes := httptest.NewRecorder()
	h.ServeHTTP(propfindRes, propfindReq)

	if got, want := propfindRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND metadata props after remove status = %d, want %d", got, want)
	}
	body := propfindRes.Body.String()
	if !strings.Contains(body, "displayname") || !strings.Contains(body, "addressbook-description") {
		t.Fatalf("PROPFIND metadata props after remove missing prop names: %q", body)
	}
	if !strings.Contains(body, "404 Not Found") {
		t.Fatalf("PROPFIND metadata props after remove missing 404 propstat: %q", body)
	}
	if strings.Contains(body, "Team Contacts") || strings.Contains(body, "Shared directory") {
		t.Fatalf("PROPFIND metadata props after remove still contains prior values: %q", body)
	}
}

func TestHandler_Propfind_AddressbookMetadataProps_BodyMatchesGolden(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	patchReq := httptest.NewRequest("PROPPATCH", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set>
    <D:prop>
      <D:displayname>Team Contacts</D:displayname>
      <C:addressbook-description>Shared directory</C:addressbook-description>
    </D:prop>
  </D:set>
</D:propertyupdate>`))
	patchReq.SetBasicAuth("alice", "secret")
	patchReq.Header.Set("Content-Type", "application/xml; charset=utf-8")
	patchRes := httptest.NewRecorder()
	h.ServeHTTP(patchRes, patchReq)
	if got, want := patchRes.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPPATCH metadata golden setup status = %d, want %d", got, want)
	}

	req := httptest.NewRequest("PROPFIND", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:displayname/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("PROPFIND metadata golden status = %d, want %d", got, want)
	}
	assertGoldenSyncXML(t, rr.Body.String(), "propfind_addressbook_metadata_props_explicit.xml")
}

func TestHandler_Report_OversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	if body, want := rr.Body.String(), http.StatusText(http.StatusRequestEntityTooLarge)+"\n"; body != want {
		t.Fatalf("REPORT oversize body = %q, want %q", body, want)
	}
}

func TestHandler_Report_AddressbookPath_RejectsTraversalSegment(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest("REPORT", "/alice/../contacts/", bytes.NewBufferString(`<?xml version="1.0"?><D:sync-collection xmlns:D="DAV:"></D:sync-collection>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("REPORT addressbook traversal status = %d, want %d", got, want)
	}
}

func TestHandler_Report_AddressbookPath_RejectsEncodedSlashInSegment(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	req := httptest.NewRequest("REPORT", "/alice/contacts%2Fextra/", bytes.NewBufferString(`<?xml version="1.0"?><D:sync-collection xmlns:D="DAV:"></D:sync-collection>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("REPORT addressbook encoded slash status = %d, want %d", got, want)
	}
}

func TestHandler_Report_MalformedOversizeBodyReturns413(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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

	body := `<?xml version="1.0"?><D:sync-collection xmlns:D="DAV:"><D:sync-token>` + strings.Repeat("X", 128)
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(body))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
		t.Fatalf("REPORT malformed oversize status = %d, want %d", got, want)
	}
	if body, want := rr.Body.String(), http.StatusText(http.StatusRequestEntityTooLarge)+"\n"; body != want {
		t.Fatalf("REPORT malformed oversize body = %q, want %q", body, want)
	}
}

func TestHandler_Report_AddressbookMultiget_ReturnsSubsetAnd404(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
		SyncToken string `xml:"sync-token"`
		Responses []struct {
			Href   string `xml:"href"`
			Status string `xml:"status"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("xml.Unmarshal sync-collection limit: %v body=%q", err, rr.Body.String())
	}
	var (
		itemCount int
		has507    bool
	)
	for _, r := range doc.Responses {
		if r.Href == "/alice/contacts/" && strings.Contains(r.Status, "507") {
			has507 = true
			continue
		}
		itemCount++
	}
	if itemCount != 2 {
		t.Fatalf("sync-collection limit item responses = %d, want 2 body=%q", itemCount, rr.Body.String())
	}
	if !has507 {
		t.Fatalf("sync-collection limit missing self 507 response body=%q", rr.Body.String())
	}
	gotTok, err := carddavx.ParseSyncToken(doc.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken response: %v token=%q body=%q", err, doc.SyncToken, rr.Body.String())
	}
	if gotTok.Revision != baseTok.Revision+2 {
		t.Fatalf("sync-collection continuation token revision = %d, want %d body=%q", gotTok.Revision, baseTok.Revision+2, rr.Body.String())
	}
}

func TestDavx_Sync_PaginationBoundary_500Then501(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	svc := carddavx.NewSyncService(store)

	baseline, err := svc.SyncCollection(context.Background(), "alice", "contacts", "", 0)
	if err != nil {
		t.Fatalf("SyncCollection baseline: %v", err)
	}
	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	for i := 0; i < 501; i++ {
		uid := "uid-" + strconv.Itoa(i)
		href := "c" + strconv.Itoa(i) + ".vcf"
		if _, err := store.PutCard(context.Background(), db.PutCardInput{
			AddressbookID: ab.ID,
			Href:          href,
			UID:           uid,
			VCard:         []byte(vcardBody(uid, "Name "+strconv.Itoa(i))),
		}); err != nil {
			t.Fatalf("PutCard #%d: %v", i, err)
		}
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            svc,
	})

	first := doSyncReportWithLimit(t, h, baseline.SyncToken, 500)
	if got, want := first.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("first sync status = %d, want %d", got, want)
	}
	firstDoc := mustParseSyncMultiStatus(t, first.Body.Bytes())
	if got, want := countNonSelfSyncResponses(firstDoc, "/alice/contacts/"), 500; got != want {
		t.Fatalf("first sync item responses = %d, want %d", got, want)
	}
	if !hasSyncStatusForHref(firstDoc, "/alice/contacts/", http.StatusInsufficientStorage) {
		t.Fatalf("first sync missing self 507 response body=%q", first.Body.String())
	}
	firstTok, err := carddavx.ParseSyncToken(firstDoc.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken first: %v token=%q", err, firstDoc.SyncToken)
	}
	if got, want := firstTok.Revision, int64(500); got != want {
		t.Fatalf("first sync token revision = %d, want %d", got, want)
	}

	second := doSyncReportWithLimit(t, h, firstDoc.SyncToken, 500)
	if got, want := second.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("second sync status = %d, want %d", got, want)
	}
	secondDoc := mustParseSyncMultiStatus(t, second.Body.Bytes())
	if got, want := countNonSelfSyncResponses(secondDoc, "/alice/contacts/"), 1; got != want {
		t.Fatalf("second sync item responses = %d, want %d body=%q", got, want, second.Body.String())
	}
	if hasSyncStatusForHref(secondDoc, "/alice/contacts/", http.StatusInsufficientStorage) {
		t.Fatalf("second sync unexpectedly includes self 507 body=%q", second.Body.String())
	}
	secondTok, err := carddavx.ParseSyncToken(secondDoc.SyncToken)
	if err != nil {
		t.Fatalf("ParseSyncToken second: %v token=%q", err, secondDoc.SyncToken)
	}
	if got, want := secondTok.Revision, int64(501); got != want {
		t.Fatalf("second sync token revision = %d, want %d", got, want)
	}
}

func TestDavx_Sync_Pagination_TruncatedPageIncludes507SelfResponse(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	svc := carddavx.NewSyncService(store)

	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	for i := 0; i < 3; i++ {
		uid := "uid-trunc-" + strconv.Itoa(i)
		if _, err := store.PutCard(context.Background(), db.PutCardInput{
			AddressbookID: ab.ID,
			Href:          "t" + strconv.Itoa(i) + ".vcf",
			UID:           uid,
			VCard:         []byte(vcardBody(uid, "Trunc "+strconv.Itoa(i))),
		}); err != nil {
			t.Fatalf("PutCard #%d: %v", i, err)
		}
	}

	h := server.NewHandler(server.HandlerOptions{
		Authenticate: func(_ context.Context, username, password string) (string, bool, error) {
			return username, true, nil
		},
		Backend:         backend,
		AttachPrincipal: contactcarddav.WithPrincipal,
		Sync:            svc,
	})
	rr := doSyncReportWithLimit(t, h, carddavx.FormatSyncToken(ab.ID, 0), 2)
	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("sync status = %d, want %d", got, want)
	}
	doc := mustParseSyncMultiStatus(t, rr.Body.Bytes())
	if !hasSyncStatusForHref(doc, "/alice/contacts/", http.StatusInsufficientStorage) {
		t.Fatalf("truncated sync missing self 507 response body=%q", rr.Body.String())
	}
}

func TestHandler_Report_SyncCollection_DefaultServerLimit500(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	svc := carddavx.NewSyncService(store)

	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), "alice", "contacts")
	if err != nil {
		t.Fatalf("GetAddressbookByUsernameSlug: %v", err)
	}
	for i := 0; i < 501; i++ {
		uid := "uid-default-" + strconv.Itoa(i)
		if _, err := store.PutCard(context.Background(), db.PutCardInput{
			AddressbookID: ab.ID,
			Href:          "d" + strconv.Itoa(i) + ".vcf",
			UID:           uid,
			VCard:         []byte(vcardBody(uid, "Default "+strconv.Itoa(i))),
		}); err != nil {
			t.Fatalf("PutCard #%d: %v", i, err)
		}
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
  <D:sync-token>` + carddavx.FormatSyncToken(ab.ID, 0) + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got, want := rr.Code, http.StatusMultiStatus; got != want {
		t.Fatalf("sync-collection status = %d, want %d", got, want)
	}
	doc := mustParseSyncMultiStatus(t, rr.Body.Bytes())
	if got, want := countNonSelfSyncResponses(doc, "/alice/contacts/"), 500; got != want {
		t.Fatalf("default limit item responses = %d, want %d", got, want)
	}
	if !hasSyncStatusForHref(doc, "/alice/contacts/", http.StatusInsufficientStorage) {
		t.Fatalf("default-limit sync missing self 507 response body=%q", rr.Body.String())
	}
}

func TestHandler_Report_SyncCollection_StalePrunedTokenReturns403ValidSyncTokenError(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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
	defer func() { _ = store.Close() }()
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

func nonEmptyLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func parseJSONLogLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("json.Unmarshal log line: %v; line=%q", err, line)
	}
	return m
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

type syncMultiStatusDoc struct {
	SyncToken string `xml:"sync-token"`
	Responses []struct {
		Href   string `xml:"href"`
		Status string `xml:"status"`
	} `xml:"response"`
}

func doSyncReportWithLimit(t *testing.T, h http.Handler, token string, limit int) *httptest.ResponseRecorder {
	t.Helper()
	reqBody := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:limit><D:nresults>` + strconv.Itoa(limit) + `</D:nresults></D:limit>
</D:sync-collection>`
	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(reqBody))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func mustParseSyncMultiStatus(t *testing.T, body []byte) syncMultiStatusDoc {
	t.Helper()
	var doc syncMultiStatusDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("xml.Unmarshal sync multistatus: %v body=%q", err, string(body))
	}
	return doc
}

func hasSyncStatusForHref(doc syncMultiStatusDoc, href string, status int) bool {
	want := "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status)
	for _, r := range doc.Responses {
		if r.Href == href && strings.TrimSpace(r.Status) == want {
			return true
		}
	}
	return false
}

func countNonSelfSyncResponses(doc syncMultiStatusDoc, selfHref string) int {
	n := 0
	for _, r := range doc.Responses {
		if r.Href == selfHref {
			continue
		}
		n++
	}
	return n
}

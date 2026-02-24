package server_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	gocarddav "github.com/emersion/go-webdav/carddav"
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

func TestHandler_CardDelete_IfMatchEnforced(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

	req := httptest.NewRequest(http.MethodPut, "/alice/contacts/a.vcf", bytes.NewBufferString("not-a-vcard"))
	req.Header.Set("Content-Type", "text/vcard")
	req.SetBasicAuth("alice", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusBadRequest; got != want {
		t.Fatalf("PUT invalid vcard status = %d, want %d", got, want)
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
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

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

func TestHandler_Report_UnknownTypeReturns501(t *testing.T) {
	t.Parallel()

	store, backend := openServerBackend(t)
	defer store.Close()
	seedServerUserBook(t, store, "alice", "contacts", "Contacts")
	h := newAuthedHandlerForTests(backend)

	req := httptest.NewRequest("REPORT", "/alice/contacts/", bytes.NewBufferString(`<?xml version="1.0"?>
<D:sync-collection xmlns:D="DAV:"></D:sync-collection>`))
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got, want := rr.Code, http.StatusNotImplemented; got != want {
		t.Fatalf("REPORT unknown status = %d, want %d", got, want)
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
	h := newAuthedHandlerForTests(backend)

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
	h := newAuthedHandlerForTests(backend)

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

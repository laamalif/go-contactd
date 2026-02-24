package davxml

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

func TestMarshal_MultiStatusIncludesXMLHeaderAndDAVRoot(t *testing.T) {
	t.Parallel()

	b, err := Marshal(MultiStatus{
		Responses: []Response{{Href: "/alice/"}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	if !strings.HasPrefix(s, xml.Header) {
		t.Fatalf("Marshal missing xml header: %q", s)
	}
	if !strings.Contains(s, "<multistatus xmlns=\"DAV:\">") {
		t.Fatalf("Marshal missing DAV multistatus root: %q", s)
	}
	if !strings.Contains(s, "<href>/alice/</href>") {
		t.Fatalf("Marshal missing href: %q", s)
	}
}

func TestStatusLine(t *testing.T) {
	t.Parallel()

	if got, want := StatusLine(207), "HTTP/1.1 207 Multi-Status"; got != want {
		t.Fatalf("StatusLine(207) = %q, want %q", got, want)
	}
}

func TestCardDAVPrecondition_MarshalIncludesCardDAVNamespaceElement(t *testing.T) {
	t.Parallel()

	b, err := Marshal(CardDAVPrecondition("no-uid-conflict"))
	if err != nil {
		t.Fatalf("Marshal(CardDAVPrecondition): %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "<error xmlns=\"DAV:\"") {
		t.Fatalf("missing DAV error root: %q", s)
	}
	if !strings.Contains(s, `xmlns="urn:ietf:params:xml:ns:carddav"`) && !strings.Contains(s, `xmlns:ns1="urn:ietf:params:xml:ns:carddav"`) {
		t.Fatalf("missing carddav namespace decl: %q", s)
	}
	if !strings.Contains(s, "<no-uid-conflict xmlns=\"urn:ietf:params:xml:ns:carddav\"></no-uid-conflict>") &&
		!strings.Contains(s, "<no-uid-conflict xmlns=\"urn:ietf:params:xml:ns:carddav\"/>") &&
		!strings.Contains(s, "<ns1:no-uid-conflict></ns1:no-uid-conflict>") &&
		!strings.Contains(s, "<ns1:no-uid-conflict/>") {
		t.Fatalf("missing no-uid-conflict element: %q", s)
	}
}

func TestAddressbookSupportedReportSet_IncludesOptionalSyncFirst(t *testing.T) {
	t.Parallel()

	got := AddressbookSupportedReportSet(true)
	if got == nil {
		t.Fatal("AddressbookSupportedReportSet(true) = nil")
	}
	if len(got.Reports) != 3 {
		t.Fatalf("len(Reports) = %d, want 3", len(got.Reports))
	}
	if got.Reports[0].Report.SyncCollection == nil {
		t.Fatalf("first report is not sync-collection: %#v", got.Reports[0])
	}
}

func TestCardDAVSupportedAddressData_EmitsTypes(t *testing.T) {
	t.Parallel()

	sad := CardDAVSupportedAddressData([]struct {
		ContentType string
		Version     string
	}{
		{ContentType: "text/vcard", Version: "3.0"},
	})
	if sad == nil || len(sad.Types) != 1 {
		t.Fatalf("supported address data types = %#v, want 1 entry", sad)
	}
	if sad.Types[0].ContentType != "text/vcard" || sad.Types[0].Version != "3.0" {
		t.Fatalf("address data type = %#v, want text/vcard 3.0", sad.Types[0])
	}

	var buf bytes.Buffer
	if err := xml.NewEncoder(&buf).Encode(sad); err != nil {
		t.Fatalf("xml encode sad: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "address-data-type") {
		t.Fatalf("xml output missing address-data-type: %q", got)
	}
}

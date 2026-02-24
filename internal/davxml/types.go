package davxml

import (
	"encoding/xml"
	"fmt"
	"net/http"
)

const (
	NamespaceDAV     = "DAV:"
	NamespaceCardDAV = "urn:ietf:params:xml:ns:carddav"
	NamespaceCS      = "http://calendarserver.org/ns/"
	NamespaceINF     = "http://inf-it.com/ns/ab/"
)

type MultiStatus struct {
	XMLName   xml.Name   `xml:"DAV: multistatus"`
	Responses []Response `xml:"response,omitempty"`
	SyncToken string     `xml:"sync-token,omitempty"`
}

type Response struct {
	Href      string     `xml:"href"`
	Status    string     `xml:"status,omitempty"`
	PropStats []PropStat `xml:"propstat,omitempty"`
}

type PropStat struct {
	Prop   any    `xml:"prop"`
	Status string `xml:"status"`
}

type Href struct {
	Href string `xml:"DAV: href"`
}

type ResourceType struct {
	Collection  *struct{} `xml:"DAV: collection,omitempty"`
	Principal   *struct{} `xml:"DAV: principal,omitempty"`
	Addressbook *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook,omitempty"`
}

type SupportedReportSet struct {
	Reports []SupportedReport `xml:"DAV: supported-report,omitempty"`
}

type SupportedReport struct {
	Report Report `xml:"DAV: report"`
}

type Report struct {
	SyncCollection   *struct{} `xml:"DAV: sync-collection,omitempty"`
	AddressbookMulti *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook-multiget,omitempty"`
	AddressbookQuery *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook-query,omitempty"`
}

type Prop struct {
	CurrentUserPrincipal *Href               `xml:"DAV: current-user-principal,omitempty"`
	PrincipalURL         *Href               `xml:"DAV: principal-URL,omitempty"`
	AddressbookHomeSet   *Href               `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set,omitempty"`
	ResourceType         *ResourceType       `xml:"DAV: resourcetype,omitempty"`
	SupportedReportSet   *SupportedReportSet `xml:"DAV: supported-report-set,omitempty"`
	DisplayName          string              `xml:"DAV: displayname,omitempty"`
	AddressbookDesc      string              `xml:"urn:ietf:params:xml:ns:carddav addressbook-description,omitempty"`
	AddressbookColor     string              `xml:"http://inf-it.com/ns/ab/ addressbook-color,omitempty"`
	SyncToken            string              `xml:"DAV: sync-token,omitempty"`
	GetCTag              string              `xml:"http://calendarserver.org/ns/ getctag,omitempty"`
	GetETag              string              `xml:"DAV: getetag,omitempty"`
	AddressData          string              `xml:"urn:ietf:params:xml:ns:carddav address-data,omitempty"`
	Extra                []RawProp           `xml:",any,omitempty"`
}

type RawProp struct {
	XMLName xml.Name
}

type Error struct {
	XMLName        xml.Name  `xml:"DAV: error"`
	ValidSyncToken *struct{} `xml:"valid-sync-token,omitempty"`
	Extra          []RawProp `xml:",any,omitempty"`
}

func Marshal(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

func StatusLine(code int) string {
	return "HTTP/1.1 " + itoa(code) + " " + httpStatusText(code)
}

func PropStatOK(prop Prop) PropStat {
	return PropStatStatus(prop, 200)
}

func PropStatStatus(prop Prop, code int) PropStat {
	return PropStat{
		Prop:   prop,
		Status: StatusLine(code),
	}
}

func DAVCollection() *struct{}      { return &struct{}{} }
func DAVPrincipal() *struct{}       { return &struct{}{} }
func CardDAVAddressbook() *struct{} { return &struct{}{} }
func CardDAVPrecondition(name string) Error {
	return Error{
		Extra: []RawProp{{XMLName: xml.Name{Space: NamespaceCardDAV, Local: name}}},
	}
}

func AddressbookSupportedReportSet(includeSync bool) *SupportedReportSet {
	out := &SupportedReportSet{
		Reports: []SupportedReport{
			{Report: Report{AddressbookMulti: &struct{}{}}},
			{Report: Report{AddressbookQuery: &struct{}{}}},
		},
	}
	if includeSync {
		out.Reports = append([]SupportedReport{{Report: Report{SyncCollection: &struct{}{}}}}, out.Reports...)
	}
	return out
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func httpStatusText(code int) string {
	return http.StatusText(code)
}

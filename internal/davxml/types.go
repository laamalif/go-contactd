package davxml

import (
	"encoding/xml"
)

const (
	NamespaceDAV     = "DAV:"
	NamespaceCardDAV = "urn:ietf:params:xml:ns:carddav"
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

type Error struct {
	XMLName        xml.Name  `xml:"DAV: error"`
	ValidSyncToken *struct{} `xml:"valid-sync-token,omitempty"`
}

func Marshal(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

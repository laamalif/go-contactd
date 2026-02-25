package server

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	"github.com/laamalif/go-contactd/internal/carddavx"
	"github.com/laamalif/go-contactd/internal/davxml"
)

type HandlerOptions struct {
	ReadyCheck             func(context.Context) error
	Logger                 *slog.Logger
	Authenticate           func(context.Context, string, string) (string, bool, error)
	AttachPrincipal        func(context.Context, string) context.Context
	Backend                gocarddav.Backend
	Sync                   *carddavx.SyncService
	EnableAddressbookColor bool
	TrustProxyHeaders      bool
	BaseURL                string
	RequestMaxBytes        int64
	VCardMaxBytes          int64
	ReportMaxResponseBytes int64
}

const syncServerPageLimit = 500
const maxAuthorizationHeaderBytes = 8192
const maxReportMultigetHrefs = 1000
const defaultReportMaxResponseBytes = 16 << 20 // 16 MiB

func NewHandler(opts HandlerOptions) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.RequestMaxBytes <= 0 {
		opts.RequestMaxBytes = 1 << 20 // 1 MiB
	}
	if opts.VCardMaxBytes <= 0 || opts.VCardMaxBytes > opts.RequestMaxBytes {
		opts.VCardMaxBytes = opts.RequestMaxBytes
	}
	if opts.ReportMaxResponseBytes <= 0 {
		opts.ReportMaxResponseBytes = defaultReportMaxResponseBytes
	}
	return &handler{opts: opts}
}

type handler struct {
	opts HandlerOptions
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status    int
	respBytes int64
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.respBytes += int64(n)
	return n, err
}

func (w *loggingResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

type requestUserContextKey struct{}

type addressbookMetadataUpdater interface {
	UpdateAddressBookMetadata(ctx context.Context, p string, displayname, description, color *string) error
}

type addressbookColorReader interface {
	GetAddressBookColor(ctx context.Context, p string) (string, error)
}

type addressObjectPutWithStatus interface {
	PutAddressObjectWithStatus(ctx context.Context, p string, card vcard.Card, opts *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, bool, error)
}

type addressObjectGetWithRaw interface {
	GetAddressObjectWithRaw(ctx context.Context, p string, req *gocarddav.AddressDataRequest) (*gocarddav.AddressObject, []byte, error)
}

type addressObjectDeleteWithCurrentETag interface {
	DeleteAddressObjectWithCurrentETag(ctx context.Context, p string, currentETag string) error
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &loggingResponseWriter{ResponseWriter: w}
	r = h.serveHTTP(lw, r)
	h.logAccess(r, lw, time.Since(start))
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) *http.Request {
	switch r.URL.Path {
	case "/.well-known/carddav":
		http.Redirect(w, r, h.wellKnownRedirectTarget(), http.StatusPermanentRedirect)
		return r
	case "/health":
		h.serveHealth(w, r)
		return r
	}
	if h.opts.Authenticate != nil && !isPublicPath(r.URL.Path) {
		var ok bool
		r, ok = h.requireBasicAuth(w, r)
		if !ok {
			return r
		}
	}
	if err := validateRequestPathPayload(r.URL); err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return r
	}

	switch {
	case r.Method == "PROPFIND" && h.opts.Backend != nil:
		h.handlePropfind(w, r)
		return r
	case r.Method == "PROPPATCH" && h.opts.Backend != nil:
		h.handleProppatch(w, r)
		return r
	case r.Method == "REPORT" && h.opts.Backend != nil:
		h.handleReport(w, r)
		return r
	case r.Method == "MKCOL" && h.opts.Backend != nil:
		h.handleMkcol(w, r)
		return r
	case r.Method == http.MethodDelete && h.opts.Backend != nil && classifyDAVPath(r.URL.Path) == davResourceAddressbook:
		h.handleAddressbookDelete(w, r)
		return r
	case h.opts.Backend != nil && isCardPath(r.URL.Path):
		h.serveCardPath(w, r)
		return r
	case r.Method == http.MethodOptions:
		w.Header().Set("DAV", "1, 3, addressbook")
		w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, REPORT, MKCOL, PROPPATCH")
		w.WriteHeader(http.StatusNoContent)
		return r
	default:
		http.NotFound(w, r)
		return r
	}
}

func (h *handler) wellKnownRedirectTarget() string {
	if strings.TrimSpace(h.opts.BaseURL) == "" {
		return "/"
	}
	return strings.TrimRight(strings.TrimSpace(h.opts.BaseURL), "/") + "/"
}

func isPublicPath(p string) bool {
	switch p {
	case "/health", "/.well-known/carddav":
		return true
	default:
		return false
	}
}

func (h *handler) requireBasicAuth(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if values := r.Header.Values("Authorization"); len(values) > 1 {
		http.Error(w, "invalid authorization header", http.StatusBadRequest)
		return r, false
	} else if len(values) == 1 && strings.Contains(values[0], ",") {
		// Basic auth base64 payloads do not contain commas; reject combined header forms.
		http.Error(w, "invalid authorization header", http.StatusBadRequest)
		return r, false
	} else if len(values) == 1 && len(values[0]) > maxAuthorizationHeaderBytes {
		http.Error(w, "authorization header too large", http.StatusRequestHeaderFieldsTooLarge)
		return r, false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		writeBasicChallenge(w)
		return r, false
	}
	principal, authed, err := h.opts.Authenticate(r.Context(), username, password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return r, false
	}
	if !authed {
		writeBasicChallenge(w)
		return r, false
	}
	if principal != "" {
		ctx := r.Context()
		if h.opts.AttachPrincipal != nil {
			ctx = h.opts.AttachPrincipal(ctx, principal)
		}
		ctx = context.WithValue(ctx, requestUserContextKey{}, principal)
		r = r.WithContext(ctx)
	}
	return r, true
}

func (h *handler) logAccess(r *http.Request, w *loggingResponseWriter, dur time.Duration) {
	if r == nil || w == nil {
		return
	}
	reqBytes := r.ContentLength
	if reqBytes < 0 {
		reqBytes = 0
	}
	user, _ := r.Context().Value(requestUserContextKey{}).(string)
	h.opts.Logger.Info(
		"request",
		"event", "request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", w.statusCode(),
		"dur_ms", dur.Milliseconds(),
		"user", user,
		"req_bytes", reqBytes,
		"resp_bytes", w.respBytes,
		"remote", requestRemoteForLog(r, h.opts.TrustProxyHeaders),
	)
}

func requestRemoteForLog(r *http.Request, trustProxy bool) string {
	if r == nil {
		return ""
	}
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for _, part := range parts {
				if v := strings.TrimSpace(part); v != "" {
					return v
				}
			}
		}
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
			return xrip
		}
	}
	if r.RemoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if host == "" {
		return r.RemoteAddr
	}
	return host
}

func writeBasicChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="contactd"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (h *handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	if h.opts.ReadyCheck != nil {
		if err := h.opts.ReadyCheck(r.Context()); err != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *handler) serveCardPath(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleCardGet(w, r)
	case http.MethodPut:
		h.handleCardPut(w, r)
	case http.MethodDelete:
		h.handleCardDelete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *handler) handlePropfind(w http.ResponseWriter, r *http.Request) {
	depth, ok := parsePropfindDepth(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	reqSpec, err := parsePropfindRequest(r.Body)
	if err != nil {
		_ = writeInvalidBodyOrTooLarge(w, err, "invalid xml")
		return
	}

	ms, err := h.buildPropfindMultiStatus(r.Context(), r.URL.Path, depth, reqSpec)
	if err != nil {
		writeBackendError(w, err)
		return
	}

	body, err := davxml.Marshal(ms)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

type propfindRequest struct {
	AllProp bool
	Props   []xml.Name
}

func parsePropfindRequest(body io.Reader) (propfindRequest, error) {
	if body == nil {
		return propfindRequest{AllProp: true}, nil
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return propfindRequest{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return propfindRequest{AllProp: true}, nil
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	req := propfindRequest{}
	var (
		rootSeen bool
		depth    int
		inProp   bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return propfindRequest{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !rootSeen {
				rootSeen = true
				if t.Name.Local != "propfind" {
					return propfindRequest{}, fmt.Errorf("unexpected root %q", t.Name.Local)
				}
				depth = 1
				continue
			}
			switch {
			case depth == 1 && t.Name.Local == "allprop":
				req.AllProp = true
				if err := dec.Skip(); err != nil {
					return propfindRequest{}, err
				}
			case depth == 1 && t.Name.Local == "prop":
				inProp = true
				depth++
			case inProp && depth == 2:
				req.Props = append(req.Props, t.Name)
				if err := dec.Skip(); err != nil {
					return propfindRequest{}, err
				}
			default:
				depth++
			}
		case xml.EndElement:
			if inProp && t.Name.Local == "prop" {
				inProp = false
			}
			if depth > 0 {
				depth--
			}
		}
	}
	if !rootSeen {
		return propfindRequest{AllProp: true}, nil
	}
	if !req.AllProp && len(req.Props) == 0 {
		req.AllProp = true
	}
	return req, nil
}

type proppatchRequest struct {
	Ops []proppatchOp
}

type proppatchOp struct {
	Name   xml.Name
	Remove bool
	Value  string
}

func parseProppatchRequest(body io.Reader) (proppatchRequest, error) {
	if body == nil {
		return proppatchRequest{}, fmt.Errorf("empty body")
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return proppatchRequest{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return proppatchRequest{}, fmt.Errorf("empty body")
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	req := proppatchRequest{}
	var (
		rootSeen bool
		depth    int
		inProp   bool
		mode     string
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return proppatchRequest{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !rootSeen {
				rootSeen = true
				if !matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "propertyupdate"}) {
					return proppatchRequest{}, fmt.Errorf("unexpected root %q", t.Name.Local)
				}
				depth = 1
				continue
			}
			switch {
			case depth == 1 && t.Name.Space == davxml.NamespaceDAV && (t.Name.Local == "set" || t.Name.Local == "remove"):
				mode = t.Name.Local
				depth++
			case depth == 1:
				return proppatchRequest{}, fmt.Errorf("invalid proppatch structure")
			case depth == 2 && mode != "" && t.Name.Space == davxml.NamespaceDAV && t.Name.Local == "prop":
				inProp = true
				depth++
			case depth == 2 && mode != "":
				return proppatchRequest{}, fmt.Errorf("invalid proppatch structure")
			case inProp && depth == 3:
				op := proppatchOp{
					Name:   t.Name,
					Remove: mode == "remove",
				}
				if mode == "set" {
					if err := dec.DecodeElement(&op.Value, &t); err != nil {
						return proppatchRequest{}, err
					}
				} else {
					if err := dec.Skip(); err != nil {
						return proppatchRequest{}, err
					}
				}
				req.Ops = append(req.Ops, op)
			default:
				depth++
			}
		case xml.EndElement:
			if inProp && t.Name.Space == davxml.NamespaceDAV && t.Name.Local == "prop" {
				inProp = false
			}
			if depth == 2 && t.Name.Space == davxml.NamespaceDAV && (t.Name.Local == "set" || t.Name.Local == "remove") {
				mode = ""
			}
			if depth > 0 {
				depth--
			}
		}
	}
	if !rootSeen {
		return proppatchRequest{}, fmt.Errorf("empty body")
	}
	if len(req.Ops) == 0 {
		return proppatchRequest{}, fmt.Errorf("no properties")
	}
	if err := validateSingleXMLDocument(raw); err != nil {
		return proppatchRequest{}, err
	}
	return req, nil
}

func (h *handler) handleReport(w http.ResponseWriter, r *http.Request) {
	if classifyDAVPath(r.URL.Path) != davResourceAddressbook {
		http.NotFound(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	req, err := parseReportRequest(r.Body)
	if err != nil {
		if errors.Is(err, errUnknownReportType) {
			http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
			return
		}
		_ = writeInvalidBodyOrTooLarge(w, err, "invalid xml")
		return
	}

	switch req.Kind {
	case reportKindAddressbookMultiGet:
		h.handleAddressbookMultiGet(w, r, req.Hrefs)
		return
	case reportKindAddressbookQuery:
		h.handleAddressbookQuery(w, r)
		return
	case reportKindSyncCollection:
		limit := syncServerPageLimit
		if req.LimitNResults > 0 {
			limit = req.LimitNResults
			if limit > syncServerPageLimit {
				limit = syncServerPageLimit
			}
		}
		h.handleSyncCollection(w, r, req.SyncToken, limit)
		return
	default:
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
}

type reportKind int

const (
	reportKindUnknown reportKind = iota
	reportKindAddressbookMultiGet
	reportKindAddressbookQuery
	reportKindSyncCollection
)

type reportRequest struct {
	Kind          reportKind
	Hrefs         []string
	SyncToken     string
	LimitNResults int
}

var errUnknownReportType = errors.New("unknown report type")

func parseReportRequest(body io.Reader) (reportRequest, error) {
	if body == nil {
		return reportRequest{}, fmt.Errorf("empty body")
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return reportRequest{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return reportRequest{}, fmt.Errorf("empty body")
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var req reportRequest
	rootSeen := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return reportRequest{}, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !rootSeen {
			rootSeen = true
			switch {
			case matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-multiget"}):
				req.Kind = reportKindAddressbookMultiGet
			case matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-query"}):
				req.Kind = reportKindAddressbookQuery
			case matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "sync-collection"}):
				req.Kind = reportKindSyncCollection
			default:
				return reportRequest{}, errUnknownReportType
			}
			continue
		}

		switch req.Kind {
		case reportKindAddressbookMultiGet:
			if matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "href"}) {
				var href string
				if err := dec.DecodeElement(&href, &start); err != nil {
					return reportRequest{}, err
				}
				req.Hrefs = append(req.Hrefs, strings.TrimSpace(href))
			}
		case reportKindSyncCollection:
			switch {
			case matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "sync-token"}):
				var token string
				if err := dec.DecodeElement(&token, &start); err != nil {
					return reportRequest{}, err
				}
				req.SyncToken = strings.TrimSpace(token)
			case matchXMLName(start.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "limit"}):
				n, err := parseDAVLimitNResults(dec, start)
				if err != nil {
					return reportRequest{}, err
				}
				req.LimitNResults = n
			}
		}
	}
	if !rootSeen {
		return reportRequest{}, fmt.Errorf("empty body")
	}
	if err := validateSingleXMLDocument(raw); err != nil {
		return reportRequest{}, err
	}
	return req, nil
}

func validateSingleXMLDocument(raw []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var (
		rootSeen   bool
		rootClosed bool
		depth      int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if rootSeen && rootClosed {
					return fmt.Errorf("trailing xml content")
				}
				rootSeen = true
			}
			depth++
		case xml.EndElement:
			if depth > 0 {
				depth--
				if depth == 0 && rootSeen {
					rootClosed = true
				}
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace([]byte(t))) > 0 {
				return fmt.Errorf("trailing xml content")
			}
		}
	}
	return nil
}

func parseDAVLimitNResults(dec *xml.Decoder, start xml.StartElement) (int, error) {
	depth := 1
	nresults := 0
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return 0, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 1 && matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "nresults"}) {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return 0, err
				}
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				n, err := strconv.Atoi(v)
				if err != nil {
					return 0, err
				}
				nresults = n
				continue
			}
			depth++
		case xml.EndElement:
			if matchXMLName(t.Name, start.Name) {
				depth--
				continue
			}
			if depth > 0 {
				depth--
			}
		}
	}
	return nresults, nil
}

func (h *handler) handleMkcol(w http.ResponseWriter, r *http.Request) {
	if classifyDAVPath(r.URL.Path) != davResourceAddressbook {
		http.NotFound(w, r)
		return
	}
	user, slug, ok := parseAddressbookPath(r.URL.Path)
	if !ok || user == "" || slug == "" {
		http.NotFound(w, r)
		return
	}
	ab := &gocarddav.AddressBook{
		Path: r.URL.Path,
		Name: slug,
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	mreq, err := parseMkcolRequest(r.Body)
	if err != nil {
		_ = writeInvalidBodyOrTooLarge(w, err, "invalid xml")
		return
	}
	if mreq.ResourceTypePresent && (!mreq.HasCollection || !mreq.HasAddressbook) {
		http.Error(w, "invalid mkcol resourcetype", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(mreq.DisplayName) != "" {
		ab.Name = strings.TrimSpace(mreq.DisplayName)
	}
	if strings.TrimSpace(mreq.Description) != "" {
		ab.Description = strings.TrimSpace(mreq.Description)
	}
	if err := h.opts.Backend.CreateAddressBook(r.Context(), ab); err != nil {
		writeBackendError(w, err)
		return
	}
	if (strings.TrimSpace(mreq.DisplayName) != "" || strings.TrimSpace(mreq.Description) != "") && h.opts.Backend != nil {
		if updater, ok := h.opts.Backend.(addressbookMetadataUpdater); ok {
			var display, desc *string
			if strings.TrimSpace(mreq.DisplayName) != "" {
				v := strings.TrimSpace(mreq.DisplayName)
				display = &v
			}
			if strings.TrimSpace(mreq.Description) != "" {
				v := strings.TrimSpace(mreq.Description)
				desc = &v
			}
			if err := updater.UpdateAddressBookMetadata(r.Context(), r.URL.Path, display, desc, nil); err != nil {
				writeBackendError(w, err)
				return
			}
		}
	}
	w.WriteHeader(http.StatusCreated)
}

type mkcolRequest struct {
	DisplayName         string
	Description         string
	ResourceTypePresent bool
	HasCollection       bool
	HasAddressbook      bool
}

func parseMkcolRequest(body io.Reader) (mkcolRequest, error) {
	if body == nil {
		return mkcolRequest{}, nil
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return mkcolRequest{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return mkcolRequest{}, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var (
		req       mkcolRequest
		rootSeen  bool
		inResType bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return mkcolRequest{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !rootSeen {
				rootSeen = true
				if t.Name.Local != "mkcol" {
					return mkcolRequest{}, fmt.Errorf("unexpected root %q", t.Name.Local)
				}
				continue
			}
			switch {
			case matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "displayname"}):
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return mkcolRequest{}, err
				}
				req.DisplayName = v
			case matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-description"}):
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return mkcolRequest{}, err
				}
				req.Description = v
			case matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
				req.ResourceTypePresent = true
				inResType = true
			case inResType && matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "collection"}):
				req.HasCollection = true
				if err := dec.Skip(); err != nil {
					return mkcolRequest{}, err
				}
			case inResType && matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook"}):
				req.HasAddressbook = true
				if err := dec.Skip(); err != nil {
					return mkcolRequest{}, err
				}
			}
		case xml.EndElement:
			if matchXMLName(t.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}) {
				inResType = false
			}
		}
	}
	return req, nil
}

func (h *handler) handleProppatch(w http.ResponseWriter, r *http.Request) {
	if classifyDAVPath(r.URL.Path) != davResourceAddressbook {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	req, err := parseProppatchRequest(r.Body)
	if err != nil {
		_ = writeInvalidBodyOrTooLarge(w, err, "invalid xml")
		return
	}

	var (
		supportedPropNames []davxml.RawProp
		unsupported        []davxml.RawProp
		displayname        *string
		description        *string
		color              *string
	)
	for _, op := range req.Ops {
		switch {
		case matchXMLName(op.Name, xml.Name{Space: davxml.NamespaceDAV, Local: "displayname"}):
			supportedPropNames = append(supportedPropNames, davxml.RawProp{XMLName: op.Name})
			if op.Remove {
				v := ""
				displayname = &v
			} else {
				v := op.Value
				displayname = &v
			}
		case matchXMLName(op.Name, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-description"}):
			supportedPropNames = append(supportedPropNames, davxml.RawProp{XMLName: op.Name})
			if op.Remove {
				v := ""
				description = &v
			} else {
				v := op.Value
				description = &v
			}
		case matchXMLName(op.Name, xml.Name{Space: davxml.NamespaceINF, Local: "addressbook-color"}) && h.opts.EnableAddressbookColor:
			supportedPropNames = append(supportedPropNames, davxml.RawProp{XMLName: op.Name})
			if op.Remove {
				v := ""
				color = &v
			} else {
				v := op.Value
				color = &v
			}
		default:
			unsupported = append(unsupported, davxml.RawProp{XMLName: op.Name})
		}
	}
	if (displayname != nil || description != nil || color != nil) && h.opts.Backend != nil {
		updater, ok := h.opts.Backend.(addressbookMetadataUpdater)
		if !ok {
			unsupported = append(append([]davxml.RawProp(nil), supportedPropNames...), unsupported...)
			supportedPropNames = nil
		} else if err := updater.UpdateAddressBookMetadata(r.Context(), r.URL.Path, displayname, description, color); err != nil {
			writeBackendError(w, err)
			return
		}
	}
	ms := davxml.MultiStatus{
		Responses: []davxml.Response{{
			Href: r.URL.Path,
		}},
	}
	if len(supportedPropNames) > 0 {
		ms.Responses[0].PropStats = append(ms.Responses[0].PropStats, davxml.PropStatStatus(davxml.Prop{Extra: supportedPropNames}, http.StatusOK))
	}
	if len(unsupported) > 0 {
		ms.Responses[0].PropStats = append(ms.Responses[0].PropStats, davxml.PropStatStatus(davxml.Prop{Extra: unsupported}, http.StatusForbidden))
	}
	body, err := davxml.Marshal(ms)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

func (h *handler) handleSyncCollection(w http.ResponseWriter, r *http.Request, rawToken string, limit int) {
	if h.opts.Sync == nil {
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
	user, slug, ok := parseAddressbookPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if principal, _ := r.Context().Value(requestUserContextKey{}).(string); principal != "" && principal != user {
		http.NotFound(w, r)
		return
	}
	res, err := h.opts.Sync.SyncCollection(r.Context(), user, slug, strings.TrimSpace(rawToken), limit)
	if err != nil {
		if carddavx.IsInvalidSyncToken(err) {
			body, mErr := davxml.Marshal(davxml.Error{ValidSyncToken: &struct{}{}})
			if mErr != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(body)
			return
		}
		writeBackendError(w, err)
		return
	}

	ms := davxml.MultiStatus{SyncToken: res.SyncToken}
	for _, u := range res.Updated {
		ms.Responses = append(ms.Responses, davxml.Response{
			Href: u.Href,
			PropStats: []davxml.PropStat{
				davxml.PropStatOK(davxml.Prop{GetETag: u.ETag}),
			},
		})
	}
	for _, d := range res.Deleted {
		ms.Responses = append(ms.Responses, davxml.Response{
			Href:   d,
			Status: davxml.StatusLine(http.StatusNotFound),
		})
	}
	if res.Truncated {
		ms.Responses = append(ms.Responses, davxml.Response{
			Href:   r.URL.Path,
			Status: davxml.StatusLine(http.StatusInsufficientStorage),
		})
	}
	body, err := davxml.Marshal(ms)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

func (h *handler) handleAddressbookDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.opts.Backend.DeleteAddressBook(r.Context(), r.URL.Path); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleAddressbookMultiGet(w http.ResponseWriter, r *http.Request, hrefs []string) {
	if len(hrefs) > maxReportMultigetHrefs {
		http.Error(w, "too many hrefs", http.StatusBadRequest)
		return
	}
	targetUser, targetSlug, ok := parseAddressbookPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := h.opts.Backend.GetAddressBook(r.Context(), r.URL.Path); err != nil {
		writeBackendError(w, err)
		return
	}

	responses := make([]davxml.Response, 0, len(hrefs))
	seen := make(map[string]struct{}, len(hrefs))
	for _, href := range hrefs {
		user, slug, name, ok := parseCardPath(href)
		if !ok || user != targetUser || slug != targetSlug {
			responses = append(responses, davxml.Response{
				Href:   href,
				Status: davxml.StatusLine(http.StatusNotFound),
			})
			continue
		}
		canonicalHref := "/" + user + "/" + slug + "/" + name
		if _, dup := seen[canonicalHref]; dup {
			continue
		}
		seen[canonicalHref] = struct{}{}

		var (
			ao  *gocarddav.AddressObject
			raw []byte
			err error
		)
		if be, ok := h.opts.Backend.(addressObjectGetWithRaw); ok {
			ao, raw, err = be.GetAddressObjectWithRaw(r.Context(), canonicalHref, nil)
		} else {
			ao, err = h.opts.Backend.GetAddressObject(r.Context(), canonicalHref, nil)
		}
		if err != nil {
			if status, ok := httpStatusFromError(err); ok && status == http.StatusNotFound {
				responses = append(responses, davxml.Response{
					Href:   canonicalHref,
					Status: davxml.StatusLine(http.StatusNotFound),
				})
				continue
			}
			writeBackendError(w, err)
			return
		}

		resp, err := reportCardResponse(*ao, raw)
		if err != nil {
			http.Error(w, "invalid vcard", http.StatusInternalServerError)
			return
		}
		resp.Href = canonicalHref
		responses = append(responses, resp)
	}

	h.writeReportMultiStatus(w, responses)
}

func (h *handler) handleAddressbookQuery(w http.ResponseWriter, r *http.Request) {
	aos, err := h.opts.Backend.QueryAddressObjects(r.Context(), r.URL.Path, nil)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	responses := make([]davxml.Response, 0, len(aos))
	for _, ao := range aos {
		var raw []byte
		if be, ok := h.opts.Backend.(addressObjectGetWithRaw); ok {
			_, gotRaw, rawErr := be.GetAddressObjectWithRaw(r.Context(), ao.Path, nil)
			if rawErr != nil {
				writeBackendError(w, rawErr)
				return
			}
			raw = gotRaw
		}
		resp, err := reportCardResponse(ao, raw)
		if err != nil {
			http.Error(w, "invalid vcard", http.StatusInternalServerError)
			return
		}
		responses = append(responses, resp)
	}
	h.writeReportMultiStatus(w, responses)
}

func parsePropfindDepth(w http.ResponseWriter, r *http.Request) (int, bool) {
	depth := strings.TrimSpace(r.Header.Get("Depth"))
	switch depth {
	case "", "0":
		return 0, true
	case "1":
		return 1, true
	case "infinity":
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return 0, false
	default:
		http.Error(w, "bad depth", http.StatusBadRequest)
		return 0, false
	}
}

func (h *handler) buildPropfindMultiStatus(ctx context.Context, p string, depth int, req propfindRequest) (davxml.MultiStatus, error) {
	if strings.TrimSpace(p) == "/" {
		return h.propfindRoot(ctx, depth, req)
	}
	switch classifyDAVPath(p) {
	case davResourcePrincipal:
		return h.propfindPrincipal(ctx, p, depth, req)
	case davResourceAddressbook:
		return h.propfindAddressbook(ctx, p, depth, req)
	case davResourceCard:
		return h.propfindCard(ctx, p, req)
	default:
		return davxml.MultiStatus{}, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("resource not found"))
	}
}

func (h *handler) propfindRoot(ctx context.Context, depth int, req propfindRequest) (davxml.MultiStatus, error) {
	principal, err := h.opts.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	responses := []davxml.Response{rootPropfindResponse("/", principal, req)}
	if depth == 1 {
		pms, err := h.propfindPrincipal(ctx, principal, 0, req)
		if err != nil {
			return davxml.MultiStatus{}, err
		}
		responses = append(responses, pms.Responses...)
	}
	return davxml.MultiStatus{Responses: responses}, nil
}

type davResourceKind int

const (
	davResourceUnknown davResourceKind = iota
	davResourcePrincipal
	davResourceAddressbook
	davResourceCard
)

func classifyDAVPath(p string) davResourceKind {
	if strings.TrimSpace(p) == "/" {
		return davResourceUnknown
	}
	clean := path.Clean("/" + strings.TrimSpace(p))
	if clean == "/" {
		return davResourceUnknown
	}
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	hasTrailingSlash := strings.HasSuffix(p, "/")
	switch len(parts) {
	case 1:
		if hasTrailingSlash {
			return davResourcePrincipal
		}
	case 2:
		if hasTrailingSlash {
			return davResourceAddressbook
		}
	case 3:
		return davResourceCard
	}
	return davResourceUnknown
}

func (h *handler) propfindPrincipal(ctx context.Context, reqPath string, depth int, req propfindRequest) (davxml.MultiStatus, error) {
	principal, err := h.opts.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	if principal != reqPath {
		return davxml.MultiStatus{}, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("principal not found"))
	}

	responses := []davxml.Response{principalPropfindResponse(principal, req)}
	if depth == 1 {
		books, err := h.opts.Backend.ListAddressBooks(ctx)
		if err != nil {
			return davxml.MultiStatus{}, err
		}
		for _, ab := range books {
			resp, err := h.addressbookPropfindResponse(ctx, ab, req)
			if err != nil {
				return davxml.MultiStatus{}, err
			}
			responses = append(responses, resp)
		}
	}
	return davxml.MultiStatus{Responses: responses}, nil
}

func (h *handler) propfindAddressbook(ctx context.Context, reqPath string, depth int, req propfindRequest) (davxml.MultiStatus, error) {
	ab, err := h.opts.Backend.GetAddressBook(ctx, reqPath)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	resp, err := h.addressbookPropfindResponse(ctx, *ab, req)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	responses := []davxml.Response{resp}
	if depth == 1 {
		aos, err := h.opts.Backend.ListAddressObjects(ctx, reqPath, nil)
		if err != nil {
			return davxml.MultiStatus{}, err
		}
		for _, ao := range aos {
			responses = append(responses, cardPropfindResponse(ao, req))
		}
	}
	return davxml.MultiStatus{Responses: responses}, nil
}

func (h *handler) propfindCard(ctx context.Context, reqPath string, req propfindRequest) (davxml.MultiStatus, error) {
	ao, err := h.opts.Backend.GetAddressObject(ctx, reqPath, nil)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	return davxml.MultiStatus{Responses: []davxml.Response{cardPropfindResponse(*ao, req)}}, nil
}

func principalPropfindResponse(href string, req propfindRequest) davxml.Response {
	okProp := davxml.Prop{}
	var unknown []davxml.RawProp
	for _, p := range expandPropfindRequestedProps(req, principalDefaultPropNames()) {
		switch {
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "current-user-principal"}):
			okProp.CurrentUserPrincipal = &davxml.Href{Href: href}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "principal-URL"}):
			okProp.PrincipalURL = &davxml.Href{Href: href}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-home-set"}):
			okProp.AddressbookHomeSet = &davxml.Href{Href: href}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
			okProp.ResourceType = &davxml.ResourceType{
				Collection: davxml.DAVCollection(),
				Principal:  davxml.DAVPrincipal(),
			}
		default:
			unknown = append(unknown, davxml.RawProp{XMLName: p})
		}
	}
	return davxml.Response{Href: href, PropStats: buildPropstats(okProp, unknown)}
}

func rootPropfindResponse(href, principal string, req propfindRequest) davxml.Response {
	okProp := davxml.Prop{}
	var unknown []davxml.RawProp
	for _, p := range expandPropfindRequestedProps(req, rootDefaultPropNames()) {
		switch {
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "current-user-principal"}):
			okProp.CurrentUserPrincipal = &davxml.Href{Href: principal}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-home-set"}):
			okProp.AddressbookHomeSet = &davxml.Href{Href: principal}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
			okProp.ResourceType = &davxml.ResourceType{Collection: davxml.DAVCollection()}
		default:
			unknown = append(unknown, davxml.RawProp{XMLName: p})
		}
	}
	return davxml.Response{Href: href, PropStats: buildPropstats(okProp, unknown)}
}

func (h *handler) addressbookPropfindResponse(ctx context.Context, ab gocarddav.AddressBook, req propfindRequest) (davxml.Response, error) {
	okProp := davxml.Prop{}
	var unknown []davxml.RawProp
	var collectionState *carddavx.CollectionState
	var colorValue *string
	requested := expandPropfindRequestedProps(req, addressbookDefaultPropNames(h.opts.Sync != nil, h.opts.EnableAddressbookColor))
	for _, p := range requested {
		switch {
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
			okProp.ResourceType = &davxml.ResourceType{
				Collection:  davxml.DAVCollection(),
				Addressbook: davxml.CardDAVAddressbook(),
			}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "supported-report-set"}):
			okProp.SupportedReportSet = davxml.AddressbookSupportedReportSet(h.opts.Sync != nil)
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceCardDAV, Local: "supported-address-data"}):
			if len(ab.SupportedAddressData) == 0 {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			sad := &davxml.SupportedAddressData{}
			for _, t := range ab.SupportedAddressData {
				sad.Types = append(sad.Types, davxml.AddressDataType{
					ContentType: t.ContentType,
					Version:     t.Version,
				})
			}
			okProp.SupportedAddressData = sad
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "displayname"}):
			if ab.Name == "" {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			okProp.DisplayName = ab.Name
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceCardDAV, Local: "addressbook-description"}):
			if ab.Description == "" {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			okProp.AddressbookDesc = ab.Description
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceINF, Local: "addressbook-color"}):
			if !h.opts.EnableAddressbookColor {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			if colorValue == nil {
				reader, ok := h.opts.Backend.(addressbookColorReader)
				if !ok {
					unknown = append(unknown, davxml.RawProp{XMLName: p})
					continue
				}
				v, err := reader.GetAddressBookColor(ctx, ab.Path)
				if err != nil {
					return davxml.Response{}, err
				}
				colorValue = &v
			}
			if colorValue == nil || *colorValue == "" {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			okProp.AddressbookColor = *colorValue
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "sync-token"}):
			if h.opts.Sync == nil {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			state, err := h.addressbookCollectionState(ctx, ab.Path)
			if err != nil {
				return davxml.Response{}, err
			}
			collectionState = state
			okProp.SyncToken = state.SyncToken
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceCS, Local: "getctag"}):
			if h.opts.Sync == nil {
				unknown = append(unknown, davxml.RawProp{XMLName: p})
				continue
			}
			if collectionState == nil {
				state, err := h.addressbookCollectionState(ctx, ab.Path)
				if err != nil {
					return davxml.Response{}, err
				}
				collectionState = state
			}
			okProp.GetCTag = collectionState.CTag
		default:
			unknown = append(unknown, davxml.RawProp{XMLName: p})
		}
	}
	return davxml.Response{Href: ab.Path, PropStats: buildPropstats(okProp, unknown)}, nil
}

func cardPropfindResponse(ao gocarddav.AddressObject, req propfindRequest) davxml.Response {
	okProp := davxml.Prop{}
	var unknown []davxml.RawProp
	for _, p := range expandPropfindRequestedProps(req, cardDefaultPropNames()) {
		switch {
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
			okProp.ResourceType = &davxml.ResourceType{}
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "getetag"}):
			okProp.GetETag = ao.ETag
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "getcontenttype"}):
			okProp.GetContentType = "text/vcard"
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "getcontentlength"}):
			okProp.GetContentLength = ao.ContentLength
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "getlastmodified"}):
			if !ao.ModTime.IsZero() {
				okProp.GetLastModified = ao.ModTime.UTC().Format(http.TimeFormat)
			}
		default:
			unknown = append(unknown, davxml.RawProp{XMLName: p})
		}
	}
	return davxml.Response{Href: ao.Path, PropStats: buildPropstats(okProp, unknown)}
}

func buildPropstats(okProp davxml.Prop, unknown []davxml.RawProp) []davxml.PropStat {
	var out []davxml.PropStat
	if hasAnyProp(okProp) {
		out = append(out, davxml.PropStatOK(okProp))
	}
	if len(unknown) > 0 {
		out = append(out, davxml.PropStatStatus(davxml.Prop{Extra: unknown}, http.StatusNotFound))
	}
	return out
}

func hasAnyProp(p davxml.Prop) bool {
	return p.CurrentUserPrincipal != nil ||
		p.PrincipalURL != nil ||
		p.AddressbookHomeSet != nil ||
		p.ResourceType != nil ||
		p.SupportedReportSet != nil ||
		p.SupportedAddressData != nil ||
		p.DisplayName != "" ||
		p.AddressbookDesc != "" ||
		p.AddressbookColor != "" ||
		p.SyncToken != "" ||
		p.GetCTag != "" ||
		p.GetETag != "" ||
		p.GetContentType != "" ||
		p.GetContentLength != 0 ||
		p.GetLastModified != "" ||
		p.AddressData != "" ||
		len(p.Extra) > 0
}

func expandPropfindRequestedProps(req propfindRequest, defaults []xml.Name) []xml.Name {
	if req.AllProp || len(req.Props) == 0 {
		return defaults
	}
	return req.Props
}

func cardDefaultPropNames() []xml.Name {
	return []xml.Name{
		{Space: davxml.NamespaceDAV, Local: "resourcetype"},
		{Space: davxml.NamespaceDAV, Local: "getetag"},
		{Space: davxml.NamespaceDAV, Local: "getcontenttype"},
		{Space: davxml.NamespaceDAV, Local: "getcontentlength"},
		{Space: davxml.NamespaceDAV, Local: "getlastmodified"},
	}
}

func addressbookDefaultPropNames(includeExtensions, includeColor bool) []xml.Name {
	out := []xml.Name{
		{Space: davxml.NamespaceDAV, Local: "resourcetype"},
		{Space: davxml.NamespaceDAV, Local: "supported-report-set"},
		{Space: davxml.NamespaceCardDAV, Local: "supported-address-data"},
	}
	if includeExtensions {
		out = append(out,
			xml.Name{Space: davxml.NamespaceDAV, Local: "sync-token"},
			xml.Name{Space: davxml.NamespaceCS, Local: "getctag"},
		)
	}
	if includeColor {
		out = append(out, xml.Name{Space: davxml.NamespaceINF, Local: "addressbook-color"})
	}
	return out
}

func principalDefaultPropNames() []xml.Name {
	return []xml.Name{
		{Space: davxml.NamespaceDAV, Local: "current-user-principal"},
		{Space: davxml.NamespaceDAV, Local: "principal-URL"},
		{Space: davxml.NamespaceCardDAV, Local: "addressbook-home-set"},
		{Space: davxml.NamespaceDAV, Local: "resourcetype"},
	}
}

func rootDefaultPropNames() []xml.Name {
	return []xml.Name{
		{Space: davxml.NamespaceDAV, Local: "resourcetype"},
		{Space: davxml.NamespaceDAV, Local: "current-user-principal"},
		{Space: davxml.NamespaceCardDAV, Local: "addressbook-home-set"},
	}
}

func matchXMLName(a, b xml.Name) bool {
	return a.Space == b.Space && a.Local == b.Local
}

func (h *handler) addressbookCollectionState(ctx context.Context, abPath string) (*carddavx.CollectionState, error) {
	if h.opts.Sync == nil {
		return nil, fmt.Errorf("sync service unavailable")
	}
	user, slug, ok := parseAddressbookPath(abPath)
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("addressbook not found"))
	}
	state, err := h.opts.Sync.CollectionState(ctx, user, slug)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func (h *handler) handleCardGet(w http.ResponseWriter, r *http.Request) {
	var (
		ao  *gocarddav.AddressObject
		raw []byte
		err error
	)
	if be, ok := h.opts.Backend.(addressObjectGetWithRaw); ok {
		ao, raw, err = be.GetAddressObjectWithRaw(r.Context(), r.URL.Path, nil)
	} else {
		ao, err = h.opts.Backend.GetAddressObject(r.Context(), r.URL.Path, nil)
	}
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if raw == nil {
		var buf bytes.Buffer
		if err := vcard.NewEncoder(&buf).Encode(ao.Card); err != nil {
			http.Error(w, "invalid vcard", http.StatusInternalServerError)
			return
		}
		raw = buf.Bytes()
	}
	w.Header().Set("Content-Type", "text/vcard")
	if ao.ETag != "" {
		w.Header().Set("ETag", ao.ETag)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *handler) handleCardPut(w http.ResponseWriter, r *http.Request) {
	if !isVCardContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		_ = writeInvalidBodyOrTooLarge(w, err, "invalid vcard")
		return
	}
	if int64(len(raw)) > h.opts.VCardMaxBytes {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	card, err := vcard.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil {
		http.Error(w, "invalid vcard", http.StatusBadRequest)
		return
	}
	putOpts := &gocarddav.PutAddressObjectOptions{
		IfMatch:     webdav.ConditionalMatch(r.Header.Get("If-Match")),
		IfNoneMatch: webdav.ConditionalMatch(r.Header.Get("If-None-Match")),
	}

	var (
		ao      *gocarddav.AddressObject
		created bool
	)
	if be, ok := h.opts.Backend.(addressObjectPutWithStatus); ok {
		ao, created, err = be.PutAddressObjectWithStatus(r.Context(), r.URL.Path, card, putOpts)
	} else {
		var existed bool
		if _, getErr := h.opts.Backend.GetAddressObject(r.Context(), r.URL.Path, nil); getErr == nil {
			existed = true
		} else if status, ok := httpStatusFromError(getErr); !ok || status != http.StatusNotFound {
			writeBackendError(w, getErr)
			return
		}
		ao, err = h.opts.Backend.PutAddressObject(r.Context(), r.URL.Path, card, putOpts)
		created = !existed
	}
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if ao != nil && ao.ETag != "" {
		w.Header().Set("ETag", ao.ETag)
	}
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleCardDelete(w http.ResponseWriter, r *http.Request) {
	var (
		hasMatchedCurrentETag bool
		matchedCurrentETag    string
	)
	if ifMatch := webdav.ConditionalMatch(r.Header.Get("If-Match")); ifMatch.IsSet() {
		ao, err := h.opts.Backend.GetAddressObject(r.Context(), r.URL.Path, nil)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		currentETag, err := webdav.ConditionalMatch(ao.ETag).ETag()
		if err != nil {
			http.Error(w, "invalid ETag", http.StatusInternalServerError)
			return
		}
		ok, err := ifMatch.MatchETag(currentETag)
		if err != nil {
			http.Error(w, "bad If-Match", http.StatusBadRequest)
			return
		}
		if !ok {
			http.Error(w, http.StatusText(http.StatusPreconditionFailed), http.StatusPreconditionFailed)
			return
		}
		hasMatchedCurrentETag = true
		matchedCurrentETag = currentETag
	}
	var err error
	if hasMatchedCurrentETag {
		if be, ok := h.opts.Backend.(addressObjectDeleteWithCurrentETag); ok {
			err = be.DeleteAddressObjectWithCurrentETag(r.Context(), r.URL.Path, matchedCurrentETag)
		} else {
			err = h.opts.Backend.DeleteAddressObject(r.Context(), r.URL.Path)
		}
	} else {
		err = h.opts.Backend.DeleteAddressObject(r.Context(), r.URL.Path)
	}
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isCardPath(p string) bool {
	_, _, _, ok := parseCardPath(p)
	return ok
}

func parseAddressbookPath(p string) (user, slug string, ok bool) {
	clean := path.Clean("/" + strings.TrimSpace(p))
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if !strings.HasSuffix(p, "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseCardPath(p string) (user, slug, href string, ok bool) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", false
	}
	pathValue := u.Path
	if pathValue == "" {
		pathValue = raw
	}
	clean := path.Clean("/" + strings.TrimPrefix(pathValue, "/"))
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	for _, part := range parts {
		if hasASCIIControl(part) {
			return "", "", "", false
		}
	}
	if strings.HasSuffix(pathValue, "/") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func hasASCIIControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func isMaxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func writeInvalidBodyOrTooLarge(w http.ResponseWriter, err error, badRequestMsg string) bool {
	if err == nil {
		return false
	}
	if isMaxBytesError(err) {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return true
	}
	http.Error(w, badRequestMsg, http.StatusBadRequest)
	return true
}

func validateRequestPathPayload(u *url.URL) error {
	if u == nil {
		return nil
	}
	escaped := u.EscapedPath()
	if escaped == "" {
		escaped = u.Path
	}
	for _, seg := range strings.Split(escaped, "/") {
		if seg == "" {
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return fmt.Errorf("bad path escape: %w", err)
		}
		if decoded == "." || decoded == ".." {
			return fmt.Errorf("path traversal segment")
		}
		if strings.Contains(decoded, "/") || strings.Contains(decoded, `\`) {
			return fmt.Errorf("path separator in segment")
		}
		if hasASCIIControl(decoded) {
			return fmt.Errorf("control character in segment")
		}
	}
	return nil
}

func isVCardContentType(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(v)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "text/vcard")
}

func writeBackendError(w http.ResponseWriter, err error) {
	if err == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if status, ok := httpStatusFromError(err); ok && status == http.StatusConflict && strings.Contains(err.Error(), "no-uid-conflict") {
		body, mErr := davxml.Marshal(davxml.CardDAVPrecondition("no-uid-conflict"))
		if mErr == nil {
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write(body)
			return
		}
	}
	if status, ok := httpStatusFromError(err); ok {
		// Keep DELETE 204 body contract by only using this on error paths.
		http.Error(w, http.StatusText(status), status)
		return
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeDAVMultiStatus(w http.ResponseWriter, responses []davxml.Response) {
	body, err := davxml.Marshal(davxml.MultiStatus{Responses: responses})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

func (h *handler) writeReportMultiStatus(w http.ResponseWriter, responses []davxml.Response) {
	if max := h.opts.ReportMaxResponseBytes; max > 0 {
		if estimateDAVMultiStatusBytes(responses) > max {
			http.Error(w, http.StatusText(http.StatusInsufficientStorage), http.StatusInsufficientStorage)
			return
		}
	}
	writeDAVMultiStatus(w, responses)
}

func estimateDAVMultiStatusBytes(responses []davxml.Response) int64 {
	// Conservative upper-bound-ish estimate to avoid building large marshaled XML blobs in memory.
	var n int64 = 256 // XML prolog + multistatus wrapper
	for _, resp := range responses {
		n += 256 // response wrapper / propstat overhead
		n += int64(len(resp.Href) + len(resp.Status))
		for _, ps := range resp.PropStats {
			n += 256 // propstat/status wrapper overhead
			n += int64(len(ps.Status))
			switch prop := ps.Prop.(type) {
			case davxml.Prop:
				n += estimateDAVPropBytes(prop)
			case *davxml.Prop:
				if prop != nil {
					n += estimateDAVPropBytes(*prop)
				}
			default:
				n += int64(len(fmt.Sprintf("%v", ps.Prop)))
			}
		}
	}
	return n
}

func estimateDAVPropBytes(p davxml.Prop) int64 {
	n := int64(0)
	n += int64(len(p.GetETag))
	n += int64(len(p.AddressData))
	n += int64(len(p.SyncToken))
	n += int64(len(p.GetContentType))
	n += int64(len(p.GetLastModified))
	n += int64(len(p.DisplayName))
	n += int64(len(p.AddressbookDesc))
	n += int64(len(p.AddressbookColor))
	n += int64(len(p.Extra) * 64)
	for _, extra := range p.Extra {
		n += int64(len(extra.XMLName.Space) + len(extra.XMLName.Local))
	}
	return n
}

func reportCardResponse(ao gocarddav.AddressObject, raw []byte) (davxml.Response, error) {
	if raw == nil {
		var buf bytes.Buffer
		if err := vcard.NewEncoder(&buf).Encode(ao.Card); err != nil {
			return davxml.Response{}, err
		}
		raw = buf.Bytes()
	}
	return davxml.Response{
		Href: ao.Path,
		PropStats: []davxml.PropStat{
			davxml.PropStatOK(davxml.Prop{
				GetETag:     ao.ETag,
				AddressData: string(raw),
			}),
		},
	}, nil
}

func httpStatusFromError(err error) (int, bool) {
	seen := map[error]struct{}{}
	for err != nil {
		if _, ok := seen[err]; ok {
			break
		}
		seen[err] = struct{}{}

		v := reflect.ValueOf(err)
		if v.IsValid() {
			if v.Kind() == reflect.Pointer && !v.IsNil() {
				elem := v.Elem()
				if elem.IsValid() && elem.Kind() == reflect.Struct {
					f := elem.FieldByName("Code")
					if f.IsValid() && f.CanInt() {
						code := int(f.Int())
						if code >= 100 && code <= 599 {
							return code, true
						}
					}
				}
			}
		}

		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return 0, false
}

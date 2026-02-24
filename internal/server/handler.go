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
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"

	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	"github.com/laamalif/go-contactd/internal/carddavx"
	"github.com/laamalif/go-contactd/internal/davxml"
)

type HandlerOptions struct {
	ReadyCheck      func(context.Context) error
	Logger          *slog.Logger
	Authenticate    func(context.Context, string, string) (string, bool, error)
	AttachPrincipal func(context.Context, string) context.Context
	Backend         gocarddav.Backend
	Sync            *carddavx.SyncService
	RequestMaxBytes int64
	VCardMaxBytes   int64
}

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
	return &handler{opts: opts}
}

type handler struct {
	opts HandlerOptions
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/carddav":
		http.Redirect(w, r, "/", http.StatusPermanentRedirect)
		return
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	case "/readyz":
		h.serveReadyz(w, r)
		return
	}
	if err := validateRequestPathPayload(r.URL); err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if h.opts.Authenticate != nil && !isPublicPath(r.URL.Path) {
		var ok bool
		r, ok = h.requireBasicAuth(w, r)
		if !ok {
			return
		}
	}

	switch {
	case r.Method == "PROPFIND" && h.opts.Backend != nil:
		h.handlePropfind(w, r)
		return
	case r.Method == "REPORT" && h.opts.Backend != nil:
		h.handleReport(w, r)
		return
	case h.opts.Backend != nil && isCardPath(r.URL.Path):
		h.serveCardPath(w, r)
		return
	case r.Method == http.MethodOptions:
		w.Header().Set("DAV", "1, 3, addressbook")
		w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, REPORT, MKCOL, PROPPATCH")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		http.NotFound(w, r)
		return
	}
}

func isPublicPath(p string) bool {
	switch p {
	case "/healthz", "/readyz", "/.well-known/carddav":
		return true
	default:
		return false
	}
}

func (h *handler) requireBasicAuth(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
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
	if h.opts.AttachPrincipal != nil && principal != "" {
		r = r.WithContext(h.opts.AttachPrincipal(r.Context(), principal))
	}
	return r, true
}

func writeBasicChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="contactd"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (h *handler) serveReadyz(w http.ResponseWriter, r *http.Request) {
	if h.opts.ReadyCheck != nil {
		if err := h.opts.ReadyCheck(r.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
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
		if isMaxBytesError(err) {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid xml", http.StatusBadRequest)
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

func (h *handler) handleReport(w http.ResponseWriter, r *http.Request) {
	if classifyDAVPath(r.URL.Path) != davResourceAddressbook {
		http.NotFound(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	var envelope struct {
		XMLName   xml.Name
		Hrefs     []string `xml:"href"`
		SyncToken string   `xml:"sync-token"`
		Limit     *struct {
			NResults int `xml:"nresults"`
		} `xml:"limit"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&envelope); err != nil {
		if isMaxBytesError(err) {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid xml", http.StatusBadRequest)
		return
	}

	switch envelope.XMLName.Local {
	case "addressbook-multiget":
		h.handleAddressbookMultiGet(w, r, envelope.Hrefs)
		return
	case "addressbook-query":
		h.handleAddressbookQuery(w, r)
		return
	case "sync-collection":
		limit := 0
		if envelope.Limit != nil && envelope.Limit.NResults > 0 {
			limit = envelope.Limit.NResults
		}
		h.handleSyncCollection(w, r, envelope.SyncToken, limit)
		return
	default:
		http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
		return
	}
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
	body, err := davxml.Marshal(ms)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(body)
}

func (h *handler) handleAddressbookMultiGet(w http.ResponseWriter, r *http.Request, hrefs []string) {
	responses := make([]davxml.Response, 0, len(hrefs))
	for _, href := range hrefs {
		ao, err := h.opts.Backend.GetAddressObject(r.Context(), href, nil)
		if err != nil {
			if status, ok := httpStatusFromError(err); ok && status == http.StatusNotFound {
				responses = append(responses, davxml.Response{
					Href:   href,
					Status: davxml.StatusLine(http.StatusNotFound),
				})
				continue
			}
			writeBackendError(w, err)
			return
		}

		resp, err := reportCardResponse(*ao)
		if err != nil {
			http.Error(w, "invalid vcard", http.StatusInternalServerError)
			return
		}
		responses = append(responses, resp)
	}

	writeDAVMultiStatus(w, responses)
}

func (h *handler) handleAddressbookQuery(w http.ResponseWriter, r *http.Request) {
	aos, err := h.opts.Backend.QueryAddressObjects(r.Context(), r.URL.Path, nil)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	responses := make([]davxml.Response, 0, len(aos))
	for _, ao := range aos {
		resp, err := reportCardResponse(ao)
		if err != nil {
			http.Error(w, "invalid vcard", http.StatusInternalServerError)
			return
		}
		responses = append(responses, resp)
	}
	writeDAVMultiStatus(w, responses)
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

func (h *handler) addressbookPropfindResponse(ctx context.Context, ab gocarddav.AddressBook, req propfindRequest) (davxml.Response, error) {
	okProp := davxml.Prop{}
	var unknown []davxml.RawProp
	var collectionState *carddavx.CollectionState
	requested := expandPropfindRequestedProps(req, addressbookDefaultPropNames(h.opts.Sync != nil))
	for _, p := range requested {
		switch {
		case matchXMLName(p, xml.Name{Space: davxml.NamespaceDAV, Local: "resourcetype"}):
			okProp.ResourceType = &davxml.ResourceType{
				Collection:  davxml.DAVCollection(),
				Addressbook: davxml.CardDAVAddressbook(),
			}
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
		p.SyncToken != "" ||
		p.GetCTag != "" ||
		p.GetETag != "" ||
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
	}
}

func addressbookDefaultPropNames(includeExtensions bool) []xml.Name {
	out := []xml.Name{
		{Space: davxml.NamespaceDAV, Local: "resourcetype"},
	}
	if includeExtensions {
		out = append(out,
			xml.Name{Space: davxml.NamespaceDAV, Local: "sync-token"},
			xml.Name{Space: davxml.NamespaceCS, Local: "getctag"},
		)
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
	ao, err := h.opts.Backend.GetAddressObject(r.Context(), r.URL.Path, nil)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(ao.Card); err != nil {
		http.Error(w, "invalid vcard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/vcard")
	if ao.ETag != "" {
		w.Header().Set("ETag", ao.ETag)
	}
	w.Header().Set("Content-Length", strconvItoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (h *handler) handleCardPut(w http.ResponseWriter, r *http.Request) {
	if !isVCardContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	var existed bool
	if _, err := h.opts.Backend.GetAddressObject(r.Context(), r.URL.Path, nil); err == nil {
		existed = true
	} else if status, ok := httpStatusFromError(err); !ok || status != http.StatusNotFound {
		writeBackendError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.opts.RequestMaxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid vcard", http.StatusBadRequest)
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
	ao, err := h.opts.Backend.PutAddressObject(r.Context(), r.URL.Path, card, &gocarddav.PutAddressObjectOptions{
		IfMatch:     webdav.ConditionalMatch(r.Header.Get("If-Match")),
		IfNoneMatch: webdav.ConditionalMatch(r.Header.Get("If-None-Match")),
	})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if ao != nil && ao.ETag != "" {
		w.Header().Set("ETag", ao.ETag)
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) handleCardDelete(w http.ResponseWriter, r *http.Request) {
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
	}
	if err := h.opts.Backend.DeleteAddressObject(r.Context(), r.URL.Path); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isCardPath(p string) bool {
	clean := path.Clean("/" + strings.TrimSpace(p))
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
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

func isMaxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
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

func reportCardResponse(ao gocarddav.AddressObject) (davxml.Response, error) {
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(ao.Card); err != nil {
		return davxml.Response{}, err
	}
	return davxml.Response{
		Href: ao.Path,
		PropStats: []davxml.PropStat{
			davxml.PropStatOK(davxml.Prop{
				GetETag:     ao.ETag,
				AddressData: buf.String(),
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

func strconvItoa(n int) string {
	// Tiny local helper avoids pulling in strconv for a single callsite in tests.
	return fmt.Sprintf("%d", n)
}

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"reflect"
	"strings"

	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	"github.com/laamalif/go-contactd/internal/davxml"
)

type HandlerOptions struct {
	ReadyCheck      func(context.Context) error
	Logger          *slog.Logger
	Authenticate    func(context.Context, string, string) (string, bool, error)
	AttachPrincipal func(context.Context, string) context.Context
	Backend         gocarddav.Backend
}

func NewHandler(opts HandlerOptions) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &handler{opts: opts}
}

type handler struct {
	opts HandlerOptions
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/carddav":
		http.Redirect(w, r, "/", http.StatusPermanentRedirect)
		return
	case r.URL.Path == "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	case r.URL.Path == "/readyz":
		h.serveReadyz(w, r)
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

	ms, err := h.buildPropfindMultiStatus(r.Context(), r.URL.Path, depth)
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

func (h *handler) buildPropfindMultiStatus(ctx context.Context, p string, depth int) (davxml.MultiStatus, error) {
	switch classifyDAVPath(p) {
	case davResourcePrincipal:
		return h.propfindPrincipal(ctx, p, depth)
	case davResourceAddressbook:
		return h.propfindAddressbook(ctx, p, depth)
	case davResourceCard:
		return h.propfindCard(ctx, p)
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

func (h *handler) propfindPrincipal(ctx context.Context, reqPath string, depth int) (davxml.MultiStatus, error) {
	principal, err := h.opts.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	if principal != reqPath {
		return davxml.MultiStatus{}, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("principal not found"))
	}

	responses := []davxml.Response{principalPropfindResponse(principal)}
	if depth == 1 {
		books, err := h.opts.Backend.ListAddressBooks(ctx)
		if err != nil {
			return davxml.MultiStatus{}, err
		}
		for _, ab := range books {
			responses = append(responses, addressbookPropfindResponse(ab))
		}
	}
	return davxml.MultiStatus{Responses: responses}, nil
}

func (h *handler) propfindAddressbook(ctx context.Context, reqPath string, depth int) (davxml.MultiStatus, error) {
	ab, err := h.opts.Backend.GetAddressBook(ctx, reqPath)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	responses := []davxml.Response{addressbookPropfindResponse(*ab)}
	if depth == 1 {
		aos, err := h.opts.Backend.ListAddressObjects(ctx, reqPath, nil)
		if err != nil {
			return davxml.MultiStatus{}, err
		}
		for _, ao := range aos {
			responses = append(responses, cardPropfindResponse(ao))
		}
	}
	return davxml.MultiStatus{Responses: responses}, nil
}

func (h *handler) propfindCard(ctx context.Context, reqPath string) (davxml.MultiStatus, error) {
	ao, err := h.opts.Backend.GetAddressObject(ctx, reqPath, nil)
	if err != nil {
		return davxml.MultiStatus{}, err
	}
	return davxml.MultiStatus{Responses: []davxml.Response{cardPropfindResponse(*ao)}}, nil
}

func principalPropfindResponse(href string) davxml.Response {
	return davxml.Response{
		Href: href,
		PropStats: []davxml.PropStat{
			davxml.PropStatOK(davxml.Prop{
				CurrentUserPrincipal: &davxml.Href{Href: href},
				PrincipalURL:         &davxml.Href{Href: href},
				AddressbookHomeSet:   &davxml.Href{Href: href},
				ResourceType: &davxml.ResourceType{
					Collection: davxml.DAVCollection(),
					Principal:  davxml.DAVPrincipal(),
				},
			}),
		},
	}
}

func addressbookPropfindResponse(ab gocarddav.AddressBook) davxml.Response {
	return davxml.Response{
		Href: ab.Path,
		PropStats: []davxml.PropStat{
			davxml.PropStatOK(davxml.Prop{
				ResourceType: &davxml.ResourceType{
					Collection:  davxml.DAVCollection(),
					Addressbook: davxml.CardDAVAddressbook(),
				},
			}),
		},
	}
}

func cardPropfindResponse(ao gocarddav.AddressObject) davxml.Response {
	return davxml.Response{
		Href: ao.Path,
		PropStats: []davxml.PropStat{
			davxml.PropStatOK(davxml.Prop{
				ResourceType: &davxml.ResourceType{},
				GetETag:      ao.ETag,
			}),
		},
	}
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

	card, err := vcard.NewDecoder(r.Body).Decode()
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
	if status, ok := httpStatusFromError(err); ok {
		// Keep DELETE 204 body contract by only using this on error paths.
		http.Error(w, http.StatusText(status), status)
		return
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
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

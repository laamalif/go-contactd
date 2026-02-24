package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
)

type HandlerOptions struct {
	ReadyCheck   func(context.Context) error
	Logger       *slog.Logger
	Authenticate func(context.Context, string, string) (string, bool, error)
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
		if !h.requireBasicAuth(w, r) {
			return
		}
	}

	switch {
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

func (h *handler) requireBasicAuth(w http.ResponseWriter, r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		writeBasicChallenge(w)
		return false
	}
	_, authed, err := h.opts.Authenticate(r.Context(), username, password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !authed {
		writeBasicChallenge(w)
		return false
	}
	return true
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

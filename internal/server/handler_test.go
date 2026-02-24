package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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

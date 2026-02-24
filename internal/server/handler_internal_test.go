package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteInvalidBodyOrTooLarge(t *testing.T) {
	t.Parallel()

	t.Run("max-bytes maps to 413", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		handled := writeInvalidBodyOrTooLarge(rr, &http.MaxBytesError{Limit: 8}, "invalid xml")
		if !handled {
			t.Fatal("handled=false, want true")
		}
		if got, want := rr.Code, http.StatusRequestEntityTooLarge; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})

	t.Run("other error maps to provided bad request", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		handled := writeInvalidBodyOrTooLarge(rr, errors.New("bad xml"), "invalid xml")
		if !handled {
			t.Fatal("handled=false, want true")
		}
		if got, want := rr.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if body := rr.Body.String(); body != "invalid xml\n" {
			t.Fatalf("body = %q, want %q", body, "invalid xml\n")
		}
	})
}

func TestRequestRemoteForLog_DirectSocketReturnsHostOnly(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "10.0.0.5:4242"

	if got, want := requestRemoteForLog(req, false), "10.0.0.5"; got != want {
		t.Fatalf("requestRemoteForLog(trust=false) = %q, want %q", got, want)
	}
}

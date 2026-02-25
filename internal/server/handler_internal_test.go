package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.0.0.5:4242"

	if got, want := requestRemoteForLog(req, false), "10.0.0.5"; got != want {
		t.Fatalf("requestRemoteForLog(trust=false) = %q, want %q", got, want)
	}
}

func TestRequestRemoteForLog_TrustProxyUsesRightmostValidXFFHop(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.0.0.5:4242"
	req.Header.Set("X-Forwarded-For", "198.51.100.66, 203.0.113.9")

	if got, want := requestRemoteForLog(req, true), "203.0.113.9"; got != want {
		t.Fatalf("requestRemoteForLog(trust=true) = %q, want %q", got, want)
	}
}

func TestRequestRemoteForLog_TrustProxyOversizeHeadersFallbackToSocketRemote(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "10.0.0.5:4242"
	req.Header.Set("X-Forwarded-For", strings.Repeat("1", 5000))
	req.Header.Set("X-Real-IP", strings.Repeat("2", 5000))

	if got, want := requestRemoteForLog(req, true), "10.0.0.5"; got != want {
		t.Fatalf("requestRemoteForLog(trust=true oversize xff) = %q, want %q", got, want)
	}
}

func TestParseCardPath_RejectsControlChars(t *testing.T) {
	t.Parallel()

	for _, tc := range []string{
		"/alice/contacts/card%00.vcf",
		"/alice/contacts/card%09.vcf",
		"/alice/contacts/card%0A.vcf",
		"/alice/contacts/card%0D.vcf",
		"/alice/contacts/card%7F.vcf",
	} {
		if _, _, _, ok := parseCardPath(tc); ok {
			t.Fatalf("parseCardPath(%q) ok=true, want false", tc)
		}
	}
}

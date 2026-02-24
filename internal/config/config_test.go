package config_test

import (
	"strings"
	"testing"

	"github.com/laamalif/go-contactd/internal/config"
)

func TestLoadServeConfig_PriorityAndPortFallback(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadServeConfig([]string{"--listen-addr", ":9999"}, map[string]string{
		"CONTACTD_LISTEN_ADDR": ":7777",
		"PORT":                 "8085",
		"CONTACTD_DB_PATH":     "/tmp/from-env.sqlite",
	})
	if err != nil {
		t.Fatalf("LoadServeConfig returned error: %v", err)
	}

	if got, want := cfg.ListenAddr, ":9999"; got != want {
		t.Fatalf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.DBPath, "/tmp/from-env.sqlite"; got != want {
		t.Fatalf("DBPath = %q, want %q", got, want)
	}

	cfg, err = config.LoadServeConfig(nil, map[string]string{"PORT": "8086"})
	if err != nil {
		t.Fatalf("LoadServeConfig PORT fallback returned error: %v", err)
	}
	if got, want := cfg.ListenAddr, ":8086"; got != want {
		t.Fatalf("ListenAddr from PORT = %q, want %q", got, want)
	}
}

func TestLoadServeConfig_InvalidUserSeedFails(t *testing.T) {
	t.Parallel()

	_, err := config.LoadServeConfig(nil, map[string]string{
		"CONTACTD_USERS": "alice-no-colon",
	})
	if err == nil {
		t.Fatal("LoadServeConfig error = nil, want error")
	}
	if !strings.Contains(err.Error(), "CONTACTD_USERS") {
		t.Fatalf("error %q does not mention CONTACTD_USERS", err)
	}
}

func TestLoadServeConfig_VCardMaxBytesMustNotExceedRequestMaxBytes(t *testing.T) {
	t.Parallel()

	_, err := config.LoadServeConfig(nil, map[string]string{
		"CONTACTD_REQUEST_MAX_BYTES": "64",
		"CONTACTD_VCARD_MAX_BYTES":   "128",
	})
	if err == nil {
		t.Fatal("LoadServeConfig error=nil, want validation error")
	}
	if !strings.Contains(err.Error(), "vcard max bytes") {
		t.Fatalf("error %q does not mention vcard max bytes", err)
	}
}

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

func TestLoadServeConfig_DefaultDBPath_IsBSDFriendly(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadServeConfig(nil, map[string]string{})
	if err != nil {
		t.Fatalf("LoadServeConfig returned error: %v", err)
	}
	if got, want := cfg.DBPath, "/var/db/contactd.db"; got != want {
		t.Fatalf("DBPath default = %q, want %q", got, want)
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

func TestLoadServeConfig_EnableAddressbookColorFlag_Priority(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadServeConfig([]string{"--enable-addressbook-color=false"}, map[string]string{
		"CONTACTD_ENABLE_ADDRESSBOOK_COLOR": "true",
	})
	if err != nil {
		t.Fatalf("LoadServeConfig returned error: %v", err)
	}
	if cfg.EnableAddressbookColor {
		t.Fatalf("EnableAddressbookColor = %v, want false (flag overrides env)", cfg.EnableAddressbookColor)
	}

	cfg, err = config.LoadServeConfig(nil, map[string]string{
		"CONTACTD_ENABLE_ADDRESSBOOK_COLOR": "true",
	})
	if err != nil {
		t.Fatalf("LoadServeConfig env returned error: %v", err)
	}
	if !cfg.EnableAddressbookColor {
		t.Fatalf("EnableAddressbookColor = %v, want true from env", cfg.EnableAddressbookColor)
	}
}

func TestLoadServeConfig_InvalidLogLevelRejected(t *testing.T) {
	t.Parallel()

	_, err := config.LoadServeConfig(nil, map[string]string{
		"CONTACTD_LOG_LEVEL": "verbose",
	})
	if err == nil {
		t.Fatal("LoadServeConfig error=nil, want validation error")
	}
	if got := err.Error(); !strings.Contains(got, "invalid log level") {
		t.Fatalf("error = %q, want invalid log level", got)
	}
}

func TestLoadServeConfig_BaseURLAndPruneInterval_ParseAndPriority(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadServeConfig([]string{"--base-url", "https://flag.example/pfx", "--prune-interval", "12h"}, map[string]string{
		"CONTACTD_BASE_URL":       "https://env.example/base",
		"CONTACTD_PRUNE_INTERVAL": "48h",
	})
	if err != nil {
		t.Fatalf("LoadServeConfig returned error: %v", err)
	}
	if got, want := cfg.BaseURL, "https://flag.example/pfx"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.PruneInterval.String(), "12h0m0s"; got != want {
		t.Fatalf("PruneInterval = %q, want %q", got, want)
	}
}

func TestLoadServeConfig_ServeAliasesAndPortConvenience(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want func(t *testing.T, cfg config.ServeConfig)
	}{
		{
			name: "short l",
			args: []string{"-l", ":7069"},
			want: func(t *testing.T, cfg config.ServeConfig) {
				t.Helper()
				if got, want := cfg.ListenAddr, ":7069"; got != want {
					t.Fatalf("ListenAddr=%q want %q", got, want)
				}
			},
		},
		{
			name: "short d",
			args: []string{"-d", "/tmp/short.sqlite"},
			want: func(t *testing.T, cfg config.ServeConfig) {
				t.Helper()
				if got, want := cfg.DBPath, "/tmp/short.sqlite"; got != want {
					t.Fatalf("DBPath=%q want %q", got, want)
				}
			},
		},
		{
			name: "short p",
			args: []string{"-p", "9091"},
			want: func(t *testing.T, cfg config.ServeConfig) {
				t.Helper()
				if got, want := cfg.ListenAddr, ":9091"; got != want {
					t.Fatalf("ListenAddr=%q want %q", got, want)
				}
			},
		},
		{
			name: "port convenience",
			args: []string{"--port", "9090"},
			want: func(t *testing.T, cfg config.ServeConfig) {
				t.Helper()
				if got, want := cfg.ListenAddr, ":9090"; got != want {
					t.Fatalf("ListenAddr=%q want %q", got, want)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.LoadServeConfig(tc.args, map[string]string{})
			if err != nil {
				t.Fatalf("LoadServeConfig(%v) error: %v", tc.args, err)
			}
			tc.want(t, cfg)
		})
	}
}

func TestLoadServeConfig_LegacyLongAliasesRejected(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"--listen", ":7070"},
		{"--bind", ":7070"},
		{"--addr", ":7070"},
		{"--db", "/tmp/x.sqlite"},
		{"--url", "https://example.test"},
		{"--level", "debug"},
		{"--trust-proxy"},
	} {
		if _, err := config.LoadServeConfig(args, map[string]string{}); err == nil {
			t.Fatalf("LoadServeConfig(%v) error=nil, want unknown flag", args)
		}
	}
}

func TestLoadServeConfig_PortAndListenAddrConflictRejected(t *testing.T) {
	t.Parallel()

	_, err := config.LoadServeConfig([]string{"--port", "9090", "--listen-addr", ":8080"}, map[string]string{})
	if err == nil {
		t.Fatal("LoadServeConfig conflict error=nil, want error")
	}
	if got := err.Error(); !strings.Contains(got, "--port") || !strings.Contains(got, "--listen-addr") {
		t.Fatalf("conflict error=%q, want port/listen-addr mention", got)
	}
}

func TestLoadServeConfig_ShortPortAndListenConflictRejected(t *testing.T) {
	t.Parallel()

	_, err := config.LoadServeConfig([]string{"-p", "9090", "-l", ":8080"}, map[string]string{})
	if err == nil {
		t.Fatal("LoadServeConfig short-flag conflict error=nil, want error")
	}
}

func TestLoadServeConfig_InvalidPortRejected(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"--port", "0"},
		{"--port", "65536"},
		{"-p", "0"},
		{"-p", "70000"},
	} {
		if _, err := config.LoadServeConfig(args, map[string]string{}); err == nil {
			t.Fatalf("LoadServeConfig(%v) error=nil, want invalid port", args)
		}
	}
}

func TestLoadServeConfig_InvalidBaseURLAndPruneIntervalRejected(t *testing.T) {
	t.Parallel()

	if _, err := config.LoadServeConfig(nil, map[string]string{"CONTACTD_BASE_URL": "/relative"}); err == nil {
		t.Fatal("LoadServeConfig invalid base url error=nil, want error")
	}
	if _, err := config.LoadServeConfig(nil, map[string]string{"CONTACTD_PRUNE_INTERVAL": "-1s"}); err == nil {
		t.Fatal("LoadServeConfig invalid prune interval error=nil, want error")
	}
}

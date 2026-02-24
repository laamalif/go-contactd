package config

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultListenAddr      = ":8080"
	defaultDBPath          = "/data/contactd.sqlite"
	defaultLogLevel        = "info"
	defaultLogFormat       = "text"
	defaultRequestMaxBytes = int64(1 << 20) // 1 MiB
)

type SeedUser struct {
	Username     string
	PasswordHash string
}

type ServeConfig struct {
	ListenAddr        string
	DBPath            string
	LogLevel          string
	LogFormat         string
	RequestMaxBytes   int64
	TrustProxyHeaders bool
	ForceSeed         bool
	Users             []SeedUser
}

func LoadServeConfig(args []string, env map[string]string) (ServeConfig, error) {
	cfg := ServeConfig{
		ListenAddr:      defaultListenAddr,
		DBPath:          defaultDBPath,
		LogLevel:        defaultLogLevel,
		LogFormat:       defaultLogFormat,
		RequestMaxBytes: defaultRequestMaxBytes,
	}

	applyEnv(&cfg, env)
	if err := parseFlags(&cfg, args); err != nil {
		return ServeConfig{}, err
	}
	users, err := ParseSeedUsers(env)
	if err != nil {
		return ServeConfig{}, err
	}
	cfg.Users = users
	if err := validateServeConfig(cfg); err != nil {
		return ServeConfig{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *ServeConfig, env map[string]string) {
	if v, ok := env["CONTACTD_LISTEN_ADDR"]; ok && strings.TrimSpace(v) != "" {
		cfg.ListenAddr = v
	} else if v, ok := env["PORT"]; ok && strings.TrimSpace(v) != "" {
		cfg.ListenAddr = ":" + strings.TrimSpace(v)
	}

	if v, ok := env["CONTACTD_DB_PATH"]; ok && strings.TrimSpace(v) != "" {
		cfg.DBPath = v
	}
	if v, ok := env["CONTACTD_LOG_LEVEL"]; ok && strings.TrimSpace(v) != "" {
		cfg.LogLevel = v
	}
	if v, ok := env["CONTACTD_LOG_FORMAT"]; ok && strings.TrimSpace(v) != "" {
		cfg.LogFormat = v
	}
	if v, ok := env["CONTACTD_REQUEST_MAX_BYTES"]; ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			cfg.RequestMaxBytes = n
		}
	}
	if v, ok := env["CONTACTD_TRUST_PROXY_HEADERS"]; ok && strings.TrimSpace(v) != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.TrustProxyHeaders = b
		}
	}
	if v, ok := env["CONTACTD_FORCE_SEED"]; ok && strings.TrimSpace(v) != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.ForceSeed = b
		}
	}
}

func parseFlags(cfg *ServeConfig, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "listen address")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "sqlite database path")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (text|json)")
	fs.Int64Var(&cfg.RequestMaxBytes, "request-max-bytes", cfg.RequestMaxBytes, "max request body bytes")
	fs.BoolVar(&cfg.TrustProxyHeaders, "trust-proxy-headers", cfg.TrustProxyHeaders, "trust X-Forwarded-* headers")
	fs.BoolVar(&cfg.ForceSeed, "force-seed", cfg.ForceSeed, "re-apply env seed even if DB has users")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}
	return nil
}

func validateServeConfig(cfg ServeConfig) error {
	if cfg.ListenAddr == "" {
		return fmt.Errorf("listen address is empty")
	}
	if cfg.DBPath == "" {
		return fmt.Errorf("db path is empty")
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q", cfg.LogFormat)
	}
	if cfg.RequestMaxBytes <= 0 {
		return fmt.Errorf("request max bytes must be > 0")
	}

	return nil
}

func ParseSeedUsers(env map[string]string) ([]SeedUser, error) {
	var users []SeedUser
	if raw, ok := env["CONTACTD_USERS"]; ok && strings.TrimSpace(raw) != "" {
		parsed, err := parseSeedList(raw, "CONTACTD_USERS")
		if err != nil {
			return nil, err
		}
		users = append(users, parsed...)
	}

	var keys []string
	for k := range env {
		if strings.HasPrefix(k, "CONTACTD_USER_") && strings.TrimSpace(env[k]) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		parsed, err := parseSeedList(env[k], k)
		if err != nil {
			return nil, err
		}
		users = append(users, parsed...)
	}

	return users, nil
}

func parseSeedList(raw, source string) ([]SeedUser, error) {
	parts := strings.Split(raw, ",")
	out := make([]SeedUser, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		pair := strings.SplitN(entry, ":", 2)
		if len(pair) != 2 || strings.TrimSpace(pair[0]) == "" || strings.TrimSpace(pair[1]) == "" {
			return nil, fmt.Errorf("invalid %s entry %q (want username:bcryptHash)", source, entry)
		}
		out = append(out, SeedUser{
			Username:     strings.TrimSpace(pair[0]),
			PasswordHash: strings.TrimSpace(pair[1]),
		})
	}
	return out, nil
}

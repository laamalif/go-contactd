package config

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr      = ":8080"
	defaultDBPath          = "/var/db/contactd.db"
	defaultLogLevel        = "info"
	defaultLogFormat       = "text"
	defaultRequestMaxBytes = int64(1 << 20) // 1 MiB
	defaultVCardMaxBytes   = int64(1 << 20) // 1 MiB
	defaultBookSlug        = "contacts"
	defaultBookName        = "Contacts"
	defaultRetentionDays   = 180
	defaultPruneInterval   = 24 * time.Hour
)

const DefaultDBPath = defaultDBPath

type SeedUser struct {
	Username     string
	PasswordHash string
}

type ServeConfig struct {
	ListenAddr                  string
	BaseURL                     string
	DBPath                      string
	LogLevel                    string
	LogFormat                   string
	RequestMaxBytes             int64
	VCardMaxBytes               int64
	TrustProxyHeaders           bool
	ForceSeed                   bool
	DefaultBookSlug             string
	DefaultBookName             string
	ChangeRetentionDays         int
	ChangeRetentionMaxRevisions int64
	PruneInterval               time.Duration
	EnableAddressbookColor      bool
	Users                       []SeedUser
}

func LoadServeConfig(args []string, env map[string]string) (ServeConfig, error) {
	cfg := ServeConfig{
		ListenAddr:          defaultListenAddr,
		DBPath:              defaultDBPath,
		LogLevel:            defaultLogLevel,
		LogFormat:           defaultLogFormat,
		RequestMaxBytes:     defaultRequestMaxBytes,
		VCardMaxBytes:       defaultVCardMaxBytes,
		DefaultBookSlug:     defaultBookSlug,
		DefaultBookName:     defaultBookName,
		ChangeRetentionDays: defaultRetentionDays,
		PruneInterval:       defaultPruneInterval,
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
	if v, ok := env["CONTACTD_BASE_URL"]; ok && strings.TrimSpace(v) != "" {
		cfg.BaseURL = strings.TrimSpace(v)
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
	if v, ok := env["CONTACTD_VCARD_MAX_BYTES"]; ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			cfg.VCardMaxBytes = n
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
	if v, ok := env["CONTACTD_DEFAULT_BOOK_SLUG"]; ok && strings.TrimSpace(v) != "" {
		cfg.DefaultBookSlug = strings.TrimSpace(v)
	}
	if v, ok := env["CONTACTD_DEFAULT_BOOK_NAME"]; ok && strings.TrimSpace(v) != "" {
		cfg.DefaultBookName = strings.TrimSpace(v)
	}
	if v, ok := env["CONTACTD_CHANGE_RETENTION_DAYS"]; ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.ChangeRetentionDays = n
		}
	}
	if v, ok := env["CONTACTD_CHANGE_RETENTION_MAX_REVISIONS"]; ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			cfg.ChangeRetentionMaxRevisions = n
		}
	}
	if v, ok := env["CONTACTD_PRUNE_INTERVAL"]; ok && strings.TrimSpace(v) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			cfg.PruneInterval = d
		}
	}
	if v, ok := env["CONTACTD_ENABLE_ADDRESSBOOK_COLOR"]; ok && strings.TrimSpace(v) != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.EnableAddressbookColor = b
		}
	}
}

func parseFlags(cfg *ServeConfig, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var port int

	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "listen address")
	fs.StringVar(&cfg.ListenAddr, "l", cfg.ListenAddr, "alias for --listen-addr")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "alias for --listen-addr")
	fs.StringVar(&cfg.ListenAddr, "bind", cfg.ListenAddr, "alias for --listen-addr")
	fs.StringVar(&cfg.ListenAddr, "addr", cfg.ListenAddr, "alias for --listen-addr")
	fs.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "base URL for absolute redirects (optional)")
	fs.StringVar(&cfg.BaseURL, "url", cfg.BaseURL, "alias for --base-url")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "sqlite database path")
	fs.StringVar(&cfg.DBPath, "d", cfg.DBPath, "alias for --db-path")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "alias for --db-path")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level")
	fs.StringVar(&cfg.LogLevel, "level", cfg.LogLevel, "alias for --log-level")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (text|json)")
	fs.Int64Var(&cfg.RequestMaxBytes, "request-max-bytes", cfg.RequestMaxBytes, "max request body bytes")
	fs.Int64Var(&cfg.VCardMaxBytes, "vcard-max-bytes", cfg.VCardMaxBytes, "max persisted vCard bytes")
	fs.BoolVar(&cfg.TrustProxyHeaders, "trust-proxy-headers", cfg.TrustProxyHeaders, "trust X-Forwarded-* headers")
	fs.BoolVar(&cfg.TrustProxyHeaders, "trust-proxy", cfg.TrustProxyHeaders, "alias for --trust-proxy-headers")
	fs.BoolVar(&cfg.ForceSeed, "force-seed", cfg.ForceSeed, "re-apply env seed even if DB has users")
	fs.StringVar(&cfg.DefaultBookSlug, "default-book-slug", cfg.DefaultBookSlug, "default addressbook slug")
	fs.StringVar(&cfg.DefaultBookName, "default-book-name", cfg.DefaultBookName, "default addressbook display name")
	fs.IntVar(&cfg.ChangeRetentionDays, "change-retention-days", cfg.ChangeRetentionDays, "retain card_changes for this many days")
	fs.Int64Var(&cfg.ChangeRetentionMaxRevisions, "change-retention-max-revisions", cfg.ChangeRetentionMaxRevisions, "keep latest N revisions per addressbook (0 disables)")
	fs.DurationVar(&cfg.PruneInterval, "prune-interval", cfg.PruneInterval, "background prune interval (0 disables)")
	fs.BoolVar(&cfg.EnableAddressbookColor, "enable-addressbook-color", cfg.EnableAddressbookColor, "enable INF:addressbook-color PROPPATCH/PROPFIND support")
	fs.IntVar(&port, "port", 0, "convenience: listen on :PORT (cannot combine with --listen-addr)")
	fs.IntVar(&port, "p", 0, "alias for --port")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}

	var listenSet, portSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen-addr", "l", "listen", "bind", "addr":
			listenSet = true
		case "port", "p":
			portSet = true
		}
	})
	if portSet && listenSet {
		return fmt.Errorf("cannot use --port/-p together with --listen-addr/-l (or its aliases)")
	}
	if portSet {
		if port < 1 || port > 65535 {
			return fmt.Errorf("--port must be between 1 and 65535")
		}
		cfg.ListenAddr = ":" + strconv.Itoa(port)
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
	if strings.TrimSpace(cfg.BaseURL) != "" {
		u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
		if err != nil || !u.IsAbs() || strings.TrimSpace(u.Host) == "" {
			return fmt.Errorf("invalid base url %q", cfg.BaseURL)
		}
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q", cfg.LogFormat)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q", cfg.LogLevel)
	}
	if cfg.RequestMaxBytes <= 0 {
		return fmt.Errorf("request max bytes must be > 0")
	}
	if cfg.VCardMaxBytes <= 0 {
		return fmt.Errorf("vcard max bytes must be > 0")
	}
	if cfg.VCardMaxBytes > cfg.RequestMaxBytes {
		return fmt.Errorf("vcard max bytes must be <= request max bytes")
	}
	if strings.TrimSpace(cfg.DefaultBookSlug) == "" {
		return fmt.Errorf("default book slug is empty")
	}
	if strings.TrimSpace(cfg.DefaultBookName) == "" {
		return fmt.Errorf("default book name is empty")
	}
	if cfg.ChangeRetentionDays < 0 {
		return fmt.Errorf("change retention days must be >= 0")
	}
	if cfg.ChangeRetentionMaxRevisions < 0 {
		return fmt.Errorf("change retention max revisions must be >= 0")
	}
	if cfg.PruneInterval < 0 {
		return fmt.Errorf("prune interval must be >= 0")
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

# go-contactd

Minimal, container-ready CardDAV server for DAVx5 using Go + SQLite (WAL mode).

## Status

- Storage backend: SQLite only (`modernc.org/sqlite`)
- Auth: Basic Auth (DB-backed at runtime; env seed is bootstrap-only)
- Protocol: CardDAV core flows, sync-collection, PROPFIND/PROPPATCH extensions
- Deployment: HTTP service behind reverse proxy (TLS terminated upstream)

## Quick Start (Docker)

Build and run directly:

```bash
docker build -t go-contactd .
docker run --rm \
  -p 8080:8080 \
  -e PORT=8080 \
  -e CONTACTD_DB_PATH=/data/contactd.sqlite \
  -v contactd-data:/data \
  go-contactd
```

Health checks:

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

## Quick Start (Compose)

```bash
cp .env.example .env
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
```

Compose assets:

- `deploy/docker-compose.yml`
- `deploy/README.md`
- `.env.example`

## CLI Reference

Root command:

```bash
go-contactd <subcommand>
```

Subcommands:

- `serve [flags]` start HTTP server
- `user add --username --password [--db-path --default-book-slug --default-book-name]`
- `user list [--db-path] [--format table|json]`
- `user delete (--username | --id) [--db-path]`
- `user passwd (--username | --id) --password [--db-path]`
- `version` prints version text (currently `go-contactd dev`)

Common exit codes:

- `0` success
- `1` internal/db/runtime error
- `2` usage/startup validation error
- `3` not found (user delete/passwd targets)

## Serve Config (Env / Flags)

Config precedence is: flags > env vars > defaults.

Core runtime:

| Env var | Flag | Default | Notes |
|---|---|---:|---|
| `CONTACTD_DB_PATH` | `--db-path` | `/data/contactd.sqlite` | Required in most deployments |
| `CONTACTD_LISTEN_ADDR` | `--listen-addr` | `:8080` | Overrides `PORT` |
| `PORT` | n/a | unset | Used only if `CONTACTD_LISTEN_ADDR` is unset |
| `CONTACTD_BASE_URL` | `--base-url` | inferred/empty | Used for absolute redirects (e.g. `/.well-known/carddav`); DAV `href`s stay root-relative |
| `CONTACTD_REQUEST_MAX_BYTES` | `--request-max-bytes` | `1048576` | XML/vCard request body limit |
| `CONTACTD_VCARD_MAX_BYTES` | `--vcard-max-bytes` | `1048576` | Persisted vCard size cap; must be `<=` request max |

Logging / proxy:

| Env var | Flag | Default | Notes |
|---|---|---:|---|
| `CONTACTD_LOG_LEVEL` | `--log-level` | `info` | Runtime `slog` level (`debug|info|warn|error`) |
| `CONTACTD_LOG_FORMAT` | `--log-format` | `text` | Runtime `slog` format (`text|json`) |
| `CONTACTD_TRUST_PROXY_HEADERS` | `--trust-proxy-headers` | `false` | Use `X-Forwarded-*` for access-log remote only behind trusted proxy boundary |

Bootstrap / maintenance:

| Env var | Flag | Default | Notes |
|---|---|---:|---|
| `CONTACTD_FORCE_SEED` | `--force-seed` | `false` | Re-apply env user seed even if DB has users |
| `CONTACTD_DEFAULT_BOOK_SLUG` | `--default-book-slug` | `contacts` | Seeded/default addressbook slug |
| `CONTACTD_DEFAULT_BOOK_NAME` | `--default-book-name` | `Contacts` | Seeded/default addressbook display name |
| `CONTACTD_CHANGE_RETENTION_DAYS` | `--change-retention-days` | `180` | Startup journal prune by age (`0` disables age prune) |
| `CONTACTD_CHANGE_RETENTION_MAX_REVISIONS` | `--change-retention-max-revisions` | `0` | Startup journal prune by latest N revisions (`0` disables) |
| `CONTACTD_PRUNE_INTERVAL` | `--prune-interval` | `24h` | Background journal prune ticker interval (`0` disables ticker) |
| `CONTACTD_ENABLE_ADDRESSBOOK_COLOR` | `--enable-addressbook-color` | `false` | Enables `INF:addressbook-color` PROPPATCH/PROPFIND support |

User seed (bootstrap-only):

- `CONTACTD_USERS=username:bcryptHash[,username:bcryptHash...]`
- `CONTACTD_USER_<NAME>=username:bcryptHash[, ...]` (multiple vars accepted)

Seed behavior:

- Applied only when DB is empty, unless `--force-seed` / `CONTACTD_FORCE_SEED=true`
- DB remains the source of truth at runtime

## Health and Readiness

- `GET /healthz`: liveness only (no DB query)
- `GET /readyz`: readiness includes SQLite roundtrip (`SELECT 1`)

`/readyz` should fail if the DB is unavailable, unreadable, or corrupted.

## Logging and Redaction Policy

- Logs are emitted to `stderr`
- CLI subcommands keep `stdout` deterministic/script-friendly
- Never log:
  - `Authorization` header values
  - raw vCard payloads
  - password material / bcrypt seeds

Current runtime logs are intentionally minimal (startup/listen/shutdown/error paths). Access-log formatting described in `AGENTS.md` is a future hardening item.

## Reverse Proxy Guidance

- Run `go-contactd` on internal HTTP only
- Terminate TLS at nginx/Caddy/Traefik/HAProxy/ingress
- Set public hostname/path at the proxy layer
- Only set `CONTACTD_TRUST_PROXY_HEADERS=true` behind a trusted proxy boundary

## DAVx5 Setup (Basic Auth)

1. Create a user:

   ```bash
   go run ./cmd/contactd user add --db-path /path/to/contactd.sqlite --username alice --password 'secret'
   ```

2. Start the server (direct or via Docker/compose).
3. In DAVx5, add account using:
   - Base URL: `https://your-host.example/`
   - Username: `alice`
   - Password: configured password
4. DAVx5 discovery will use `/.well-known/carddav` and standard CardDAV discovery properties.

## Backup and Restore (SQLite / WAL)

SQLite runs in WAL mode. For reliable file-level backups:

- Prefer stopping the service first, then copy the DB files together:
  - `contactd.sqlite`
  - `contactd.sqlite-wal` (if present)
  - `contactd.sqlite-shm` (if present)

Example (service stopped):

```bash
cp /data/contactd.sqlite* /backup/contactd/
```

Restore:

1. Stop the service.
2. Restore the DB files to the configured DB path directory.
3. Ensure permissions are correct (see below).
4. Start the service and verify `/readyz`.

Note: after restores/migrations that alter internal lineage, old sync tokens may become invalid. Clients should receive `valid-sync-token` errors and perform a fresh sync.

## File Permissions (DB Path / Volume)

Contacts are sensitive PII. Use restrictive permissions on the DB directory:

- Recommended directory mode: `0700` or `0750`
- Ensure the runtime UID can read/write the DB path

For the provided distroless container image, the runtime user is non-root (typically UID `65532`).

## Bare-Metal and BSD Builds

Native build:

```bash
go build -o go-contactd ./cmd/contactd
```

Cross-compile examples:

```bash
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -o go-contactd-freebsd-amd64 ./cmd/contactd
GOOS=openbsd GOARCH=amd64 CGO_ENABLED=0 go build -o go-contactd-openbsd-amd64 ./cmd/contactd
```

Run directly:

```bash
CONTACTD_DB_PATH=/var/lib/contactd/contactd.sqlite \
CONTACTD_LISTEN_ADDR=127.0.0.1:8080 \
./go-contactd serve
```

## Service Examples

### systemd (Linux)

Example unit (`/etc/systemd/system/go-contactd.service`):

```ini
[Unit]
Description=go-contactd CardDAV server
After=network.target

[Service]
Type=simple
User=contactd
Group=contactd
Environment=CONTACTD_DB_PATH=/var/lib/contactd/contactd.sqlite
Environment=CONTACTD_LISTEN_ADDR=127.0.0.1:8080
ExecStart=/usr/local/bin/go-contactd serve
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
```

### FreeBSD rc.d (example wrapper)

Minimal rc.d script example (`/usr/local/etc/rc.d/contactd`):

```sh
#!/bin/sh
# PROVIDE: contactd
# REQUIRE: LOGIN
# KEYWORD: shutdown

. /etc/rc.subr

name=contactd
rcvar=contactd_enable
: ${contactd_enable:=NO}
: ${contactd_user:=www}
: ${contactd_group:=www}
: ${contactd_flags:="serve"}
: ${contactd_env:="CONTACTD_DB_PATH=/var/db/contactd/contactd.sqlite CONTACTD_LISTEN_ADDR=127.0.0.1:8080"}

command=/usr/local/bin/go-contactd
command_args="${contactd_flags}"
start_cmd="contactd_start"

contactd_start() {
  /usr/sbin/daemon -u "${contactd_user}" -o /var/log/contactd.log -t "${name}" env ${contactd_env} ${command} ${command_args}
}

load_rc_config $name
run_rc_command "$1"
```

### OpenBSD rcctl (example)

If installed as `/usr/local/bin/go-contactd`, run behind `rcctl` with a small wrapper script that exports env vars, for example `/usr/local/bin/contactd-wrapper`:

```sh
#!/bin/sh
export CONTACTD_DB_PATH=/var/contactd/contactd.sqlite
export CONTACTD_LISTEN_ADDR=127.0.0.1:8080
exec /usr/local/bin/go-contactd serve
```

Then:

```sh
chmod 0755 /usr/local/bin/contactd-wrapper
rcctl set contactd command /usr/local/bin/contactd-wrapper
rcctl set contactd user _contactd
rcctl enable contactd
rcctl start contactd
```

## Signals

- `SIGINT`: graceful shutdown
- `SIGTERM`: graceful shutdown
- `SIGHUP`: ignored/no-op for MVP (no hot reload)

## Developer Verification

```bash
gofmt -w .
golangci-lint run   # if installed
go vet ./...
go test ./...
bash deploy/smoke-native.sh
# Docker-enabled hosts:
# CONTACTD_HOST_PORT=18080 bash deploy/smoke-docker.sh
```

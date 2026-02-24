# go-contactd

Minimal CardDAV server for DAVx5 using Go and SQLite (WAL).

## Status

- SQLite only (`modernc.org/sqlite`)
- Basic Auth (DB-backed at runtime; env seed is bootstrap-only)
- CardDAV core flows + `sync-collection` + `PROPFIND`/`PROPPATCH` extensions
- HTTP service behind reverse proxy (TLS terminated upstream)

## Upstream Libraries

- `github.com/emersion/go-vcard` (vCard parsing/encoding)
- `github.com/emersion/go-webdav` (WebDAV/CardDAV protocol support)

Both projects are by Simon Ser (`emersion`).

## Quick Start (Docker)

Build and run directly:

```bash
docker build -t contactd .
docker run --rm \
  -p 8080:8080 \
  -e PORT=8080 \
  -e CONTACTD_DB_PATH=/data/contactd.sqlite \
  -v contactd-data:/data \
  contactd
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

Primary binaries / modes:

```bash
contactd [flags]
contactctl user <add|list|delete|passwd>
contactctl export [flags]
contactctl import [flags] <file-or-dir>
```

`contactd` is the daemon (`sshd/httpd` style). `contactctl` is the admin utility.

Daemon (`contactd`) examples:

- `contactd` start server using env/defaults
- `contactd -l :8080` listen on explicit address
- `contactd -p 8080` convenience port shorthand (`:8080`)
- `contactd -d /var/db/contactd.db` select DB path
- `contactd -V` print version and exit
- `contactd -h` print daemon help

Admin (`contactctl`) examples:

- `contactctl user add --username alice --password-stdin [-d /path/to/db]`
- `contactctl user list [-d /path/to/db] [--format table|json]`
- `contactctl user delete (--username | --id) [-d /path/to/db]`
- `contactctl user passwd (--username | --id) (--password | --password-stdin) [-d /path/to/db]`
- `contactctl export --username alice --format dir --out ./backup [-d /path/to/db]`
- `contactctl export --username alice --format concat [-d /path/to/db] > alice.vcf`
- `contactctl import --username bob [-d /path/to/db] ./backup`
- `contactctl import --username bob [-d /path/to/db] ./contacts.vcf`
- `contactctl export --dry-run ...` / `contactctl import --dry-run ...` (validate + summarize only)
- `contactctl -V` print version and exit

## Serve Config (Env / Flags)

Config precedence is: flags > env vars > defaults.

Common daemon aliases:

- `-l` = `--listen-addr`
- `-p` = `--port`
- `-d` = `--db-path`

Core runtime:

| Env var | Flag | Default | Notes |
|---|---|---:|---|
| `CONTACTD_DB_PATH` | `--db-path` | `/var/db/contactd.db` | Override to `/data/contactd.sqlite` in container deployments |
| `CONTACTD_LISTEN_ADDR` | `--listen-addr` | `:8080` | Overrides `PORT` |
| `PORT` | n/a | unset | Used only if `CONTACTD_LISTEN_ADDR` is unset |
| `CONTACTD_BASE_URL` | `--base-url` | inferred/empty | Used for absolute redirects (e.g. `/.well-known/carddav`); DAV `href`s stay root-relative |
| `CONTACTD_REQUEST_MAX_BYTES` | `--request-max-bytes` | `10485760` | XML/vCard request body limit |
| `CONTACTD_VCARD_MAX_BYTES` | `--vcard-max-bytes` | `10485760` | Persisted vCard size cap; must be `<=` request max |

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

## Reverse Proxy Guidance

- Run `contactd` on internal HTTP only
- Terminate TLS at nginx/Caddy/Traefik/HAProxy/ingress
- Set public hostname/path at the proxy layer
- Only set `CONTACTD_TRUST_PROXY_HEADERS=true` behind a trusted proxy boundary

## File Permissions (DB Path / Volume)

Use a writable DB directory with restrictive permissions (for example `0700`).

## Build

```bash
make build-static    # local binaries: contactd, contactctl
make release         # release artifacts (linux/freebsd/openbsd)
```

## Service Examples

### systemd (Linux)

Example unit (`/etc/systemd/system/contactd.service`):

```ini
[Unit]
Description=contactd CardDAV server
After=network.target

[Service]
Type=simple
User=contactd
Group=contactd
Environment=CONTACTD_DB_PATH=/var/lib/contactd/contactd.sqlite
Environment=CONTACTD_LISTEN_ADDR=127.0.0.1:8080
ExecStart=/usr/local/bin/contactd
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
: ${contactd_flags:=""}
: ${contactd_env:="CONTACTD_DB_PATH=/var/db/contactd/contactd.sqlite CONTACTD_LISTEN_ADDR=127.0.0.1:8080"}

command=/usr/local/bin/contactd
command_args="${contactd_flags}"
start_cmd="contactd_start"

contactd_start() {
  /usr/sbin/daemon -u "${contactd_user}" -o /var/log/contactd.log -t "${name}" env ${contactd_env} ${command} ${command_args}
}

load_rc_config $name
run_rc_command "$1"
```

### OpenBSD rcctl (example)

If installed as `/usr/local/bin/contactd`, run behind `rcctl` with a small wrapper script that exports env vars, for example `/usr/local/bin/contactd-wrapper`:

```sh
#!/bin/sh
export CONTACTD_DB_PATH=/var/contactd/contactd.sqlite
export CONTACTD_LISTEN_ADDR=127.0.0.1:8080
exec /usr/local/bin/contactd
```

Then:

```sh
chmod 0755 /usr/local/bin/contactd-wrapper
rcctl set contactd command /usr/local/bin/contactd-wrapper
rcctl set contactd user _contactd
rcctl enable contactd
rcctl start contactd
```

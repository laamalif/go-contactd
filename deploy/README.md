# Docker Compose Quick Start

## Start

1. Copy the example env file:

   `cp .env.example .env`

2. (Optional) Add `CONTACTD_USERS` with a bcrypt hash for bootstrap seeding.

3. Start the service:

   `docker compose --env-file .env -f deploy/docker-compose.yml up -d --build`

The container runs `go-contactd serve` and persists SQLite data under `/data` (named volume `contactd-data`).

## Check health

- Liveness: `curl http://127.0.0.1:${CONTACTD_HOST_PORT:-8080}/healthz`
- Readiness: `curl http://127.0.0.1:${CONTACTD_HOST_PORT:-8080}/readyz`

The image uses a distroless runtime, so no shell-based in-container healthcheck command is configured.

## Reverse proxy notes

- The service speaks HTTP internally.
- TLS/public hostname should be terminated at a reverse proxy or ingress.
- Only set `CONTACTD_TRUST_PROXY_HEADERS=true` behind a trusted proxy boundary.

## Host bind mount alternative (permissions)

If you prefer a host directory instead of the named volume, replace the volume with a bind mount and ensure the directory is writable by the container runtime UID (distroless nonroot, typically `65532`). Recommended mode is `0700` or `0750`.

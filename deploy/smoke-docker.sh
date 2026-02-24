#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${ROOT_DIR}/deploy/docker-compose.yml"
TMP_DIR="$(mktemp -d)"
ENV_FILE="${TMP_DIR}/smoke.env"
RESP_FILE="${TMP_DIR}/resp.txt"
PROJECT_NAME="contactdsmoke"
HOST_PORT="${CONTACTD_HOST_PORT:-18080}"
BASE_URL="http://127.0.0.1:${HOST_PORT}"

cleanup() {
  docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" down -v >/dev/null 2>&1 || true
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

log() {
  printf '[smoke-docker] %s\n' "$*"
}

fail() {
  printf '[smoke-docker] ERROR: %s\n' "$*" >&2
  docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" ps >&2 || true
  docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" logs --tail=200 >&2 || true
  exit 1
}

wait_for_200() {
  local path="$1"
  local i code
  for i in $(seq 1 80); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE_URL}${path}" 2>/dev/null || true)"
    if [[ "${code}" == "200" ]]; then
      return 0
    fi
    sleep 0.25
  done
  fail "timed out waiting for 200 on ${path}"
}

http_capture() {
  local method="$1"
  local url="$2"
  shift 2
  curl -sS -i -X "${method}" "$@" "${url}" | tee "${RESP_FILE}"
}

expect_status() {
  local want="$1"
  local got
  got="$(head -n1 "${RESP_FILE}" | awk '{print $2}')"
  [[ "${got}" == "${want}" ]] || fail "expected HTTP ${want}, got ${got}; response=$(cat "${RESP_FILE}")"
}

assert_resp_contains() {
  local needle="$1"
  grep -Fq "${needle}" "${RESP_FILE}" || fail "response missing ${needle}: $(cat "${RESP_FILE}")"
}

extract_sync_token() {
  sed -n 's:.*<sync-token[^>]*>\([^<]*\)</sync-token>.*:\1:p' "${RESP_FILE}" | head -n1
}

require_cmds() {
  local c
  for c in docker curl; do
    command -v "${c}" >/dev/null 2>&1 || fail "missing required command: ${c}"
  done
}

ensure_env_file() {
  local seed_hash='$2a$10$SjCHOcrRXmmBDto.JKxWyugHwHNnqNFUVLTsiEVC1sZflfJCDBx5q' # password: secret
  local seed_hash_compose_escaped
  seed_hash_compose_escaped="${seed_hash//$/\$\$}"
  cat >"${ENV_FILE}" <<EOF
CONTACTD_HOST_PORT=${HOST_PORT}
CONTACTD_LOG_LEVEL=info
CONTACTD_LOG_FORMAT=text
CONTACTD_TRUST_PROXY_HEADERS=false
CONTACTD_REQUEST_MAX_BYTES=1048576
CONTACTD_VCARD_MAX_BYTES=1048576
CONTACTD_CHANGE_RETENTION_DAYS=180
CONTACTD_CHANGE_RETENTION_MAX_REVISIONS=0
CONTACTD_ENABLE_ADDRESSBOOK_COLOR=false
CONTACTD_USERS=alice:${seed_hash_compose_escaped}
CONTACTD_DEFAULT_BOOK_SLUG=contacts
CONTACTD_DEFAULT_BOOK_NAME=Contacts
EOF
}

CARD_A=$'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alice A\r\nEND:VCARD\r\n'
CARD_B=$'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-b\r\nFN:Bob B\r\nEND:VCARD\r\n'
SYNC_EMPTY='<?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:"><D:sync-token></D:sync-token><D:sync-level>1</D:sync-level></D:sync-collection>'

require_cmds
ensure_env_file

log "starting compose stack"
docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" up -d --build

log "waiting for health/readiness"
wait_for_200 "/healthz"
wait_for_200 "/readyz"

log "principal discovery"
http_capture PROPFIND "${BASE_URL}/alice/" \
  -u alice:secret \
  -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary '<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>' >/dev/null
expect_status 207
assert_resp_contains "current-user-principal"
assert_resp_contains "addressbook-home-set"

log "PUT card a"
http_capture PUT "${BASE_URL}/alice/contacts/a.vcf" \
  -u alice:secret \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary "${CARD_A}" >/dev/null
expect_status 201
if ! grep -Eiq '^Etag:|^ETag:' "${RESP_FILE}"; then
  fail "missing ETag header in PUT response: $(cat "${RESP_FILE}")"
fi

log "GET card a"
http_capture GET "${BASE_URL}/alice/contacts/a.vcf" -u alice:secret >/dev/null
expect_status 200
assert_resp_contains "UID:uid-a"

log "initial sync"
http_capture REPORT "${BASE_URL}/alice/contacts/" \
  -u alice:secret \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${SYNC_EMPTY}" >/dev/null
expect_status 207
assert_resp_contains "/alice/contacts/a.vcf"
token1="$(extract_sync_token)"
[[ -n "${token1}" ]] || fail "missing initial sync token"
log "captured token: ${token1}"

log "restarting container"
docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" restart contactd
wait_for_200 "/healthz"
wait_for_200 "/readyz"

log "PUT card b after restart"
http_capture PUT "${BASE_URL}/alice/contacts/b.vcf" \
  -u alice:secret \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary "${CARD_B}" >/dev/null
expect_status 201

log "delta sync with pre-restart token"
sync_delta="<?xml version=\"1.0\" encoding=\"utf-8\"?><D:sync-collection xmlns:D=\"DAV:\"><D:sync-token>${token1}</D:sync-token><D:sync-level>1</D:sync-level></D:sync-collection>"
http_capture REPORT "${BASE_URL}/alice/contacts/" \
  -u alice:secret \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${sync_delta}" >/dev/null
expect_status 207
assert_resp_contains "/alice/contacts/b.vcf"
if grep -Fq "valid-sync-token" "${RESP_FILE}"; then
  fail "delta sync returned valid-sync-token error unexpectedly: $(cat "${RESP_FILE}")"
fi

log "container smoke + E2E flow passed"

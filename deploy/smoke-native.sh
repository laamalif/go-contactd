#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
PORT="${CONTACTD_SMOKE_PORT:-18080}"
LISTEN_ADDR="127.0.0.1:${PORT}"
BASE_URL="http://${LISTEN_ADDR}"
BIN_PATH="${TMP_DIR}/contactd"
ADMIN_BIN_PATH="${TMP_DIR}/contactctl"
DB_PATH="${TMP_DIR}/contactd.sqlite"
SERVER_LOG="${TMP_DIR}/server.log"
SERVER_PID=""
IMPORT_CONFLICT_DIR="${TMP_DIR}/import-conflict"

cleanup() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill -TERM "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

log() {
  printf '[smoke] %s\n' "$*"
}

fail() {
  printf '[smoke] ERROR: %s\n' "$*" >&2
  if [[ -f "${SERVER_LOG}" ]]; then
    printf '\n[smoke] server log:\n' >&2
    sed -n '1,240p' "${SERVER_LOG}" >&2 || true
  fi
  exit 1
}

wait_for_200() {
  local path="$1"
  local i
  for i in $(seq 1 50); do
    if code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE_URL}${path}" 2>/dev/null || true)"; then
      if [[ "${code}" == "200" ]]; then
        return 0
      fi
    fi
    sleep 0.1
  done
  fail "timed out waiting for 200 on ${path}"
}

start_server() {
  : >"${SERVER_LOG}"
  CONTACTD_DB_PATH="${DB_PATH}" \
  CONTACTD_LISTEN_ADDR="${LISTEN_ADDR}" \
  "${BIN_PATH}" >"${TMP_DIR}/server.stdout" 2>"${SERVER_LOG}" &
  SERVER_PID=$!
  wait_for_200 "/health"
}

stop_server() {
  if [[ -z "${SERVER_PID}" ]]; then
    return 0
  fi
  kill -TERM "${SERVER_PID}" || true
  wait "${SERVER_PID}" || true
  SERVER_PID=""
}

expect_status() {
  local want="$1"
  local raw="$2"
  local got
  got="$(printf '%s' "${raw}" | head -n1 | awk '{print $2}')"
  [[ "${got}" == "${want}" ]] || fail "expected HTTP ${want}, got ${got}; response: ${raw}"
}

assert_contains() {
  local needle="$1"
  local haystack="$2"
  [[ "${haystack}" == *"${needle}"* ]] || fail "response missing ${needle}: ${haystack}"
}

assert_not_contains() {
  local needle="$1"
  local haystack="$2"
  [[ "${haystack}" != *"${needle}"* ]] || fail "response unexpectedly contained ${needle}: ${haystack}"
}

extract_sync_token() {
  sed -n 's:.*<sync-token[^>]*>\([^<]*\)</sync-token>.*:\1:p' | head -n1
}

CARD_A=$'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Alice A\r\nEND:VCARD\r\n'
CARD_B=$'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-b\r\nFN:Bob B\r\nEND:VCARD\r\n'
SYNC_EMPTY='<?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:"><D:sync-token></D:sync-token><D:sync-level>1</D:sync-level></D:sync-collection>'

log "building native contactd binary"
(cd "${ROOT_DIR}" && go build -o "${BIN_PATH}" ./cmd/contactd)
ln -sf "${BIN_PATH}" "${ADMIN_BIN_PATH}"

log "starting server"
start_server

log "creating user via CLI"
"${ADMIN_BIN_PATH}" user add -d "${DB_PATH}" --username alice --password secret >/dev/null

log "creating second user for cross-tenant checks"
"${ADMIN_BIN_PATH}" user add -d "${DB_PATH}" --username bob --password secret >/dev/null

log "checking well-known redirect"
resp="$(curl -sS -i "${BASE_URL}/.well-known/carddav")"
expect_status 308 "${resp}"
assert_contains $'Location: /' "${resp}"

log "checking principal discovery"
resp="$(curl -sS -i -u alice:secret -X PROPFIND \
  -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary '<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>' \
  "${BASE_URL}/alice/")"
expect_status 207 "${resp}"
assert_contains "current-user-principal" "${resp}"
assert_contains "addressbook-home-set" "${resp}"

log "creating first card"
resp="$(curl -sS -i -u alice:secret -X PUT \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary "${CARD_A}" \
  "${BASE_URL}/alice/contacts/a.vcf")"
expect_status 201 "${resp}"
if [[ "${resp}" != *"ETag:"* && "${resp}" != *"Etag:"* ]]; then
  fail "PUT create response missing ETag header: ${resp}"
fi

log "reading card"
resp="$(curl -sS -i -u alice:secret "${BASE_URL}/alice/contacts/a.vcf")"
expect_status 200 "${resp}"
assert_contains "UID:uid-a" "${resp}"

log "initial sync-collection"
resp="$(curl -sS -i -u alice:secret -X REPORT \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${SYNC_EMPTY}" \
  "${BASE_URL}/alice/contacts/")"
expect_status 207 "${resp}"
assert_contains "/alice/contacts/a.vcf" "${resp}"
token1="$(printf '%s' "${resp}" | extract_sync_token)"
[[ -n "${token1}" ]] || fail "initial sync token missing"
log "captured token: ${token1}"

log "cross-tenant sync-collection must be 404"
resp="$(curl -sS -i -u bob:secret -X PUT \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-bob\r\nFN:Bob User\r\nEND:VCARD\r\n' \
  "${BASE_URL}/bob/contacts/bob.vcf")"
expect_status 201 "${resp}"
resp="$(curl -sS -i -u alice:secret -X REPORT \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${SYNC_EMPTY}" \
  "${BASE_URL}/bob/contacts/")"
expect_status 404 "${resp}"
assert_not_contains "/bob/contacts/bob.vcf" "${resp}"

log "contactctl import --dry-run must fail on UID conflict"
mkdir -p "${IMPORT_CONFLICT_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Conflict A\r\nEND:VCARD\r\n' > "${IMPORT_CONFLICT_DIR}/conflict.vcf"
set +e
dry_out="$("${ADMIN_BIN_PATH}" import --username alice --dry-run -d "${DB_PATH}" "${IMPORT_CONFLICT_DIR}" 2>&1)"
dry_code=$?
set -e
if [[ "${dry_code}" -eq 0 ]]; then
  fail "dry-run import unexpectedly succeeded on UID conflict: ${dry_out}"
fi
assert_contains "import error:" "${dry_out}"

log "restarting server"
stop_server
start_server

log "creating second card after restart"
resp="$(curl -sS -i -u alice:secret -X PUT \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary "${CARD_B}" \
  "${BASE_URL}/alice/contacts/b.vcf")"
expect_status 201 "${resp}"

log "delta sync with pre-restart token"
sync_delta="<?xml version=\"1.0\" encoding=\"utf-8\"?><D:sync-collection xmlns:D=\"DAV:\"><D:sync-token>${token1}</D:sync-token><D:sync-level>1</D:sync-level></D:sync-collection>"
resp="$(curl -sS -i -u alice:secret -X REPORT \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${sync_delta}" \
  "${BASE_URL}/alice/contacts/")"
expect_status 207 "${resp}"
assert_contains "/alice/contacts/b.vcf" "${resp}"
if [[ "${resp}" == *"valid-sync-token"* ]]; then
  fail "delta sync returned invalid token unexpectedly: ${resp}"
fi

log "native smoke + E2E flow passed"

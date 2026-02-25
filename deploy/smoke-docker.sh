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
CONFLICT_SRC_DIR="${TMP_DIR}/import-conflict"
MALFORMED_SRC_DIR="${TMP_DIR}/import-malformed"
MULTICARD_SRC_DIR="${TMP_DIR}/import-multicard"
ATOMIC_SRC_DIR="${TMP_DIR}/import-atomic"
BAD_UID_CONCAT_FILE="${TMP_DIR}/import-bad-uid.vcf"
OVERSIZE_CONCAT_FILE="${TMP_DIR}/import-oversize.vcf"

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

assert_resp_not_contains() {
  local needle="$1"
  if grep -Fq "${needle}" "${RESP_FILE}"; then
    fail "response unexpectedly contained ${needle}: $(cat "${RESP_FILE}")"
  fi
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

compose_exec_contactctl() {
  docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" exec -T contactd /contactctl "$@"
}

compose_container_id() {
  docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" ps -q contactd
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
CONTACTD_REQUEST_MAX_BYTES=10485760
CONTACTD_VCARD_MAX_BYTES=10485760
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

log "waiting for health"
wait_for_200 "/health"

log "creating second user for cross-tenant checks"
compose_exec_contactctl user add --username bob --password secret -d /data/contactd.sqlite >/dev/null

log "duplicate Authorization headers must be rejected"
http_capture GET "${BASE_URL}/alice/" \
  -H 'Authorization: Basic YWxpY2U6YmFk' \
  -H 'Authorization: Basic Ym9iOnNlY3JldA==' >/dev/null
expect_status 400
assert_resp_contains "invalid authorization header"

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

log "cross-tenant sync-collection must be 404"
http_capture PUT "${BASE_URL}/bob/contacts/bob.vcf" \
  -u bob:secret \
  -H 'Content-Type: text/vcard; charset=utf-8' \
  --data-binary $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-bob\r\nFN:Bob User\r\nEND:VCARD\r\n' >/dev/null
expect_status 201
http_capture REPORT "${BASE_URL}/bob/contacts/" \
  -u alice:secret \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "${SYNC_EMPTY}" >/dev/null
expect_status 404
assert_resp_not_contains "/bob/contacts/bob.vcf"

log "contactctl import --dry-run must fail on UID conflict"
mkdir -p "${CONFLICT_SRC_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-a\r\nFN:Conflict A\r\nEND:VCARD\r\n' > "${CONFLICT_SRC_DIR}/conflict.vcf"
CID="$(compose_container_id)"
[[ -n "${CID}" ]] || fail "could not resolve contactd container id"
docker cp "${CONFLICT_SRC_DIR}" "${CID}:/tmp/import-conflict" >/dev/null
set +e
dry_out="$(compose_exec_contactctl import --username alice --dry-run -d /data/contactd.sqlite /tmp/import-conflict 2>&1)"
dry_code=$?
set -e
if [[ "${dry_code}" -eq 0 ]]; then
  fail "dry-run import unexpectedly succeeded on UID conflict: ${dry_out}"
fi
printf '%s' "${dry_out}" | grep -Fq "import error:" || fail "dry-run import conflict missing import error: ${dry_out}"

log "contactctl import must reject trailing garbage in directory .vcf file"
mkdir -p "${MALFORMED_SRC_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-garbage\r\nFN:Garbage\r\nEND:VCARD\r\nGARBAGE\r\n' > "${MALFORMED_SRC_DIR}/bad.vcf"
docker cp "${MALFORMED_SRC_DIR}" "${CID}:/tmp/import-malformed" >/dev/null
set +e
out="$(compose_exec_contactctl import --username bob -d /data/contactd.sqlite /tmp/import-malformed 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "directory import unexpectedly accepted trailing garbage payload: ${out}"
fi
printf '%s' "${out}" | grep -Fq "import error:" || fail "missing import error for trailing-garbage payload: ${out}"

log "contactctl import must reject multi-card single file in directory mode"
mkdir -p "${MULTICARD_SRC_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-m1\r\nFN:M1\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-m2\r\nFN:M2\r\nEND:VCARD\r\n' > "${MULTICARD_SRC_DIR}/multi.vcf"
docker cp "${MULTICARD_SRC_DIR}" "${CID}:/tmp/import-multicard" >/dev/null
set +e
out="$(compose_exec_contactctl import --username bob -d /data/contactd.sqlite /tmp/import-multicard 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "directory import unexpectedly accepted multi-card single file: ${out}"
fi
printf '%s' "${out}" | grep -Fq "import error:" || fail "missing import error for multi-card single file: ${out}"

log "contactctl import concat must reject UID-derived invalid href"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:bad/uid\r\nFN:Bad UID\r\nEND:VCARD\r\n' > "${BAD_UID_CONCAT_FILE}"
docker cp "${BAD_UID_CONCAT_FILE}" "${CID}:/tmp/import-bad-uid.vcf" >/dev/null
set +e
out="$(compose_exec_contactctl import --username bob -d /data/contactd.sqlite /tmp/import-bad-uid.vcf 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "concat import unexpectedly accepted invalid UID-derived href: ${out}"
fi
printf '%s' "${out}" | grep -Fq "invalid card href" || fail "missing invalid href error: ${out}"

log "contactctl import concat must reject oversized vCard payload"
{
  printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-oversize\r\nFN:Big\r\nNOTE:'
  head -c 10490000 /dev/zero | tr '\0' 'A'
  printf '\r\nEND:VCARD\r\n'
} > "${OVERSIZE_CONCAT_FILE}"
docker cp "${OVERSIZE_CONCAT_FILE}" "${CID}:/tmp/import-oversize.vcf" >/dev/null
set +e
out="$(compose_exec_contactctl import --username bob -d /data/contactd.sqlite /tmp/import-oversize.vcf 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "concat import unexpectedly accepted oversized vCard: ${out}"
fi
printf '%s' "${out}" | grep -Fq "vcard too large" || fail "missing oversize error: ${out}"

log "contactctl import failure must be atomic (no partial commit)"
compose_exec_contactctl user add --username charlie --password secret -d /data/contactd.sqlite >/dev/null
mkdir -p "${ATOMIC_SRC_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-ok\r\nFN:OK\r\nEND:VCARD\r\n' > "${ATOMIC_SRC_DIR}/a.vcf"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Missing UID\r\nEND:VCARD\r\n' > "${ATOMIC_SRC_DIR}/b.vcf"
docker cp "${ATOMIC_SRC_DIR}" "${CID}:/tmp/import-atomic" >/dev/null
set +e
out="$(compose_exec_contactctl import --username charlie -d /data/contactd.sqlite /tmp/import-atomic 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "import unexpectedly succeeded for atomicity test: ${out}"
fi
atomic_export="$(compose_exec_contactctl export --username charlie --format concat -d /data/contactd.sqlite 2>&1)"
if [[ -n "${atomic_export}" ]]; then
  fail "failed import left partial cards persisted (concat export not empty): ${atomic_export}"
fi

log "restarting container"
docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" restart contactd
wait_for_200 "/health"

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

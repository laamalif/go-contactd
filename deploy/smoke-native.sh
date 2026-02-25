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
IMPORT_MALFORMED_DIR="${TMP_DIR}/import-malformed"
IMPORT_MULTICARD_DIR="${TMP_DIR}/import-multicard"
IMPORT_ATOMIC_DIR="${TMP_DIR}/import-atomic"
IMPORT_BAD_UID_FILE="${TMP_DIR}/import-bad-uid.vcf"
IMPORT_OVERSIZE_FILE="${TMP_DIR}/import-oversize.vcf"
EXPORT_VERIFY_DIR="${TMP_DIR}/export-verify"
IMPORT_SEAM_DIR="${TMP_DIR}/import-seam"
EXPORT_SEAM_FILE="${TMP_DIR}/export-seam.vcf"

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

log "duplicate Authorization headers must be rejected"
resp="$(curl -sS -i \
  -H 'Authorization: Basic YWxpY2U6YmFk' \
  -H 'Authorization: Basic Ym9iOnNlY3JldA==' \
  "${BASE_URL}/alice/")"
expect_status 400 "${resp}"
assert_contains "invalid authorization header" "${resp}"

log "oversized Authorization header must be rejected"
AUTH_BIG="Basic $(head -c 9000 /dev/zero | tr '\0' 'A')"
resp="$(curl -sS -i \
  -H "Authorization: ${AUTH_BIG}" \
  "${BASE_URL}/alice/")"
expect_status 431 "${resp}"
assert_contains "authorization header too large" "${resp}"

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

log "multiget duplicate hrefs must be deduped"
resp="$(curl -sS -i -u alice:secret -X REPORT \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary '<?xml version="1.0" encoding="utf-8"?><C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:href>/alice/contacts/a.vcf</D:href><D:href>/alice/contacts/a.vcf</D:href></C:addressbook-multiget>' \
  "${BASE_URL}/alice/contacts/")"
expect_status 207 "${resp}"
if [[ "$(printf '%s' "${resp}" | grep -o '<href>/alice/contacts/a.vcf</href>' | wc -l | tr -d ' ')" != "1" ]]; then
  fail "duplicate multiget href was not deduped: ${resp}"
fi

log "multiget too many hrefs must be rejected"
MULTIGET_HREFS=''
for _ in $(seq 1 1001); do
  MULTIGET_HREFS="${MULTIGET_HREFS}<D:href>/alice/contacts/a.vcf</D:href>"
done
resp="$(curl -sS -i -u alice:secret -X REPORT \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data-binary "<?xml version=\"1.0\" encoding=\"utf-8\"?><C:addressbook-multiget xmlns:D=\"DAV:\" xmlns:C=\"urn:ietf:params:xml:ns:carddav\">${MULTIGET_HREFS}</C:addressbook-multiget>" \
  "${BASE_URL}/alice/contacts/")"
expect_status 400 "${resp}"
assert_contains "too many hrefs" "${resp}"

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

log "contactctl import must reject trailing garbage in directory .vcf file"
mkdir -p "${IMPORT_MALFORMED_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-garbage\r\nFN:Garbage\r\nEND:VCARD\r\nGARBAGE\r\n' > "${IMPORT_MALFORMED_DIR}/bad.vcf"
set +e
out="$("${ADMIN_BIN_PATH}" import --username bob -d "${DB_PATH}" "${IMPORT_MALFORMED_DIR}" 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "directory import unexpectedly accepted trailing garbage payload: ${out}"
fi
assert_contains "import error:" "${out}"

log "contactctl import must reject multi-card single file in directory mode"
mkdir -p "${IMPORT_MULTICARD_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-m1\r\nFN:M1\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-m2\r\nFN:M2\r\nEND:VCARD\r\n' > "${IMPORT_MULTICARD_DIR}/multi.vcf"
set +e
out="$("${ADMIN_BIN_PATH}" import --username bob -d "${DB_PATH}" "${IMPORT_MULTICARD_DIR}" 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "directory import unexpectedly accepted multi-card single file: ${out}"
fi
assert_contains "import error:" "${out}"

log "contactctl import concat must reject UID-derived invalid href"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:bad/uid\r\nFN:Bad UID\r\nEND:VCARD\r\n' > "${IMPORT_BAD_UID_FILE}"
set +e
out="$("${ADMIN_BIN_PATH}" import --username bob -d "${DB_PATH}" "${IMPORT_BAD_UID_FILE}" 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "concat import unexpectedly accepted invalid UID-derived href: ${out}"
fi
assert_contains "invalid card href" "${out}"

log "contactctl import concat must reject oversized vCard payload"
{
  printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-oversize\r\nFN:Big\r\nNOTE:'
  head -c 10490000 /dev/zero | tr '\0' 'A'
  printf '\r\nEND:VCARD\r\n'
} > "${IMPORT_OVERSIZE_FILE}"
set +e
out="$("${ADMIN_BIN_PATH}" import --username bob -d "${DB_PATH}" "${IMPORT_OVERSIZE_FILE}" 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "concat import unexpectedly accepted oversized vCard: ${out}"
fi
assert_contains "vcard too large" "${out}"

log "contactctl import failure must be atomic (no partial commit)"
"${ADMIN_BIN_PATH}" user add -d "${DB_PATH}" --username charlie --password secret >/dev/null
mkdir -p "${IMPORT_ATOMIC_DIR}"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-ok\r\nFN:OK\r\nEND:VCARD\r\n' > "${IMPORT_ATOMIC_DIR}/a.vcf"
printf '%s' $'BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Missing UID\r\nEND:VCARD\r\n' > "${IMPORT_ATOMIC_DIR}/b.vcf"
set +e
out="$("${ADMIN_BIN_PATH}" import --username charlie -d "${DB_PATH}" "${IMPORT_ATOMIC_DIR}" 2>&1)"
code=$?
set -e
if [[ "${code}" -eq 0 ]]; then
  fail "import unexpectedly succeeded for atomicity test: ${out}"
fi
rm -rf "${EXPORT_VERIFY_DIR}"
"${ADMIN_BIN_PATH}" export --username charlie --format dir --out "${EXPORT_VERIFY_DIR}" -d "${DB_PATH}" >/dev/null
vcf_count="$(find "${EXPORT_VERIFY_DIR}" -type f -name '*.vcf' | wc -l | tr -d ' ')"
[[ "${vcf_count}" == "0" ]] || fail "failed import left partial cards persisted (vcf_count=${vcf_count})"

log "contactctl export concat must normalize card seams for re-import"
"${ADMIN_BIN_PATH}" user add -d "${DB_PATH}" --username dora --password secret >/dev/null
mkdir -p "${IMPORT_SEAM_DIR}"
printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-seam-a\r\nFN:Seam A\r\nEND:VCARD' > "${IMPORT_SEAM_DIR}/a.vcf"
printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-seam-b\r\nFN:Seam B\r\nEND:VCARD' > "${IMPORT_SEAM_DIR}/b.vcf"
"${ADMIN_BIN_PATH}" import --username dora -d "${DB_PATH}" "${IMPORT_SEAM_DIR}" >/dev/null
"${ADMIN_BIN_PATH}" export --username dora --format concat --out "${EXPORT_SEAM_FILE}" -d "${DB_PATH}" >/dev/null
if grep -Fq 'END:VCARDBEGIN:VCARD' "${EXPORT_SEAM_FILE}"; then
  fail "concat export produced invalid seam"
fi
"${ADMIN_BIN_PATH}" user add -d "${DB_PATH}" --username erin --password secret >/dev/null
"${ADMIN_BIN_PATH}" import --username erin -d "${DB_PATH}" "${EXPORT_SEAM_FILE}" >/dev/null

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

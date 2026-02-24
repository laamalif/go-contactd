#!/usr/bin/env bash
#
# davx5-sim.sh — Simulate a realistic DAVx5 sync session against go-contactd.
#
# Reproduces the actual HTTP request sequence that DAVx5 (vCard 3.0 mode) sends
# during account setup, initial sync, contact CRUD, and incremental sync.
# Derived from DAVx5 OSE source: DavResourceFinder.kt, ContactsSyncManager.kt,
# SyncManager.kt, HomeSetRefresher.kt.
#
# Usage:
#   ./davx5-sim.sh                    # builds, starts server, runs full flow
#   CONTACTD_SMOKE_PORT=19090 ./davx5-sim.sh
#   DAVX5_SIM_STRICT_PLAN=1 ./davx5-sim.sh   # fail on known fallback gaps
#
# Exit codes:
#   0 — all steps passed
#   1 — a step failed (details on stderr)
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
PORT="${CONTACTD_SMOKE_PORT:-18081}"
LISTEN_ADDR="127.0.0.1:${PORT}"
BASE="http://${LISTEN_ADDR}"
SERVER_BASE_URL="${CONTACTD_BASE_URL:-}"
STRICT_PLAN="${DAVX5_SIM_STRICT_PLAN:-0}"
CURL_CONNECT_TIMEOUT="${DAVX5_SIM_CURL_CONNECT_TIMEOUT:-2}"
CURL_MAX_TIME="${DAVX5_SIM_CURL_MAX_TIME:-10}"
BIN="${TMP_DIR}/go-contactd"
ADMIN_BIN="${TMP_DIR}/contactctl"
DB="${TMP_DIR}/contactd.sqlite"
LOG="${TMP_DIR}/server.log"
PID=""
CURL_ERR=""
CURL_EXIT="0"

# ── helpers ──────────────────────────────────────────────────────────────────

cleanup() {
  if [[ -n "${PID}" ]] && kill -0 "${PID}" 2>/dev/null; then
    kill -TERM "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

step()  { printf '\n\033[1;34m» %s\033[0m\n' "$*"; }
ok()    { printf '  \033[32m✓ %s\033[0m\n' "$*"; }
fail()  {
  printf '  \033[31m✗ %s\033[0m\n' "$*" >&2
  if [[ -n "${CURL_ERR}" ]]; then
    printf '\n--- curl stderr ---\n%s\n' "${CURL_ERR}" >&2
  fi
  if [[ -f "${LOG}" ]]; then
    printf '\n--- server log (last 60 lines) ---\n' >&2
    tail -n60 "${LOG}" >&2 || true
  fi
  exit 1
}

# Run curl and capture status + headers + body into variables.
# Usage: dav METHOD path [curl-opts...]
#   Sets: HTTP_STATUS, HTTP_HEADERS, HTTP_BODY
dav() {
  local method="$1" path="$2"; shift 2
  local tmp_hdr="${TMP_DIR}/hdr.$$"
  local tmp_err="${TMP_DIR}/err.$$"
  CURL_ERR=""
  CURL_EXIT="0"
  if HTTP_BODY="$(curl -sS -X "${method}" \
    --connect-timeout "${CURL_CONNECT_TIMEOUT}" \
    --max-time "${CURL_MAX_TIME}" \
    -u alice:secret \
    -D "${tmp_hdr}" \
    "$@" \
    "${BASE}${path}" 2>"${tmp_err}")"; then
    CURL_EXIT="0"
  else
    CURL_EXIT="$?"
  fi
  CURL_ERR="$(cat "${tmp_err}" 2>/dev/null || true)"
  HTTP_HEADERS="$(cat "${tmp_hdr}" 2>/dev/null)" || true
  HTTP_STATUS="$(head -n1 "${tmp_hdr}" 2>/dev/null | awk '{print $2}')" || true
  rm -f "${tmp_hdr}"
  rm -f "${tmp_err}"
}

# Assert HTTP status equals expected.
expect() {
  local want="$1" label="${2:-}"
  [[ "${HTTP_STATUS}" == "${want}" ]] || \
    fail "expected ${want}, got ${HTTP_STATUS}${label:+ (${label})}; body: ${HTTP_BODY}"
  ok "${label:-status ${want}}"
}

# Assert body contains needle.
body_has() {
  local needle="$1" label="${2:-contains ${1}}"
  [[ "${HTTP_BODY}" == *"${needle}"* ]] || \
    fail "${label}: body missing '${needle}': ${HTTP_BODY}"
  ok "${label}"
}

# Assert body does NOT contain needle.
body_lacks() {
  local needle="$1" label="${2:-lacks ${1}}"
  [[ "${HTTP_BODY}" != *"${needle}"* ]] || \
    fail "${label}: body unexpectedly contains '${needle}'"
  ok "${label}"
}

# Assert header line present (case-insensitive key match).
header_has() {
  local key="$1"
  local label="${2:-header ${key}}"
  if ! echo "${HTTP_HEADERS}" | grep -qi "^${key}:"; then
    fail "${label}: header '${key}' missing; headers: ${HTTP_HEADERS}"
  fi
  ok "${label}"
}

header_value() {
  local key="$1"
  echo "${HTTP_HEADERS}" | awk -v k="$(echo "${key}" | tr '[:upper:]' '[:lower:]')" '
    BEGIN { IGNORECASE=1 }
    {
      line=$0
      sub(/\r$/, "", line)
      split(line, parts, ":")
      hk=tolower(parts[1])
      if (hk == k) {
        sub(/^[^:]*:[[:space:]]*/, "", line)
        print line
        exit
      }
    }'
}

header_equals() {
  local key="$1"
  local want="$2"
  local label="${3:-header ${key} == ${want}}"
  local got
  got="$(header_value "${key}")"
  [[ "${got}" == "${want}" ]] || fail "${label}: got '${got}', want '${want}'"
  ok "${label}"
}

# Extract first <D:sync-token> from body.
extract_sync_token() {
  echo "${HTTP_BODY}" | sed -n 's:.*<[^>]*sync-token[^>]*>\([^<]*\)</[^>]*sync-token>.*:\1:p' | head -n1
}

# Extract ETag header value.
extract_etag_header() {
  echo "${HTTP_HEADERS}" | grep -i '^etag:' | head -n1 | sed 's/^[^:]*: *//;s/\r$//'
}

# Count occurrences of a string in body.
count_in_body() {
  echo "${HTTP_BODY}" | grep -o "$1" | wc -l | tr -d ' '
}

wait_ready() {
  for _ in $(seq 1 50); do
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/readyz" 2>/dev/null || true)"
    [[ "${code}" == "200" ]] && return 0
    sleep 0.1
  done
  fail "server did not become ready"
}

start_server() {
  : >"${LOG}"
  CONTACTD_DB_PATH="${DB}" CONTACTD_LISTEN_ADDR="${LISTEN_ADDR}" CONTACTD_BASE_URL="${SERVER_BASE_URL}" \
    "${BIN}" >"${TMP_DIR}/stdout.log" 2>"${LOG}" &
  PID=$!
  wait_ready
}

stop_server() {
  [[ -z "${PID}" ]] && return 0
  kill -TERM "${PID}" || true
  wait "${PID}" 2>/dev/null || true
  PID=""
}

# ── vCards ───────────────────────────────────────────────────────────────────

VCARD_ALICE=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-alice-contact\r\nFN:Alice Contact\r\nN:Contact;Alice;;;\r\nEMAIL:alice@example.com\r\nPRODID:-//DAVx5-sim//EN\r\nEND:VCARD\r\n')
VCARD_BOB=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-bob-contact\r\nFN:Bob Contact\r\nN:Contact;Bob;;;\r\nEMAIL:bob@example.com\r\nPRODID:-//DAVx5-sim//EN\r\nEND:VCARD\r\n')
VCARD_CAROL=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-carol-contact\r\nFN:Carol Contact\r\nN:Contact;Carol;;;\r\nTEL:+1-555-0199\r\nPRODID:-//DAVx5-sim//EN\r\nEND:VCARD\r\n')
VCARD_BOB_UPDATED=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-bob-contact\r\nFN:Bob Updated\r\nN:Updated;Bob;;;\r\nEMAIL:bob-new@example.com\r\nPRODID:-//DAVx5-sim//EN\r\nEND:VCARD\r\n')

# ── XML payloads (match DAVx5 wire format) ───────────────────────────────────

# DavResourceFinder.kt — initial PROPFIND on base URL
PROPFIND_DISCOVERY='<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:resourcetype/>
    <D:displayname/>
    <D:current-user-principal/>
    <C:addressbook-home-set/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>'

# HomeSetRefresher.kt — PROPFIND on home-set (Depth: 1)
PROPFIND_HOMESET='<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:current-user-privilege-set/>
    <D:displayname/>
    <D:owner/>
    <D:resourcetype/>
    <C:addressbook-description/>
  </D:prop>
</D:propfind>'

# ContactsSyncManager.kt — PROPFIND for sync capabilities (Depth: 0)
PROPFIND_CAPABILITIES='<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"
            xmlns:C="urn:ietf:params:xml:ns:carddav"
            xmlns:CS="http://calendarserver.org/ns/">
  <D:prop>
    <C:max-resource-size/>
    <C:supported-address-data/>
    <D:supported-report-set/>
    <CS:getctag/>
    <D:sync-token/>
  </D:prop>
</D:propfind>'

# Sync-collection: initial (empty token)
SYNC_INITIAL='<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token/>
  <D:sync-level>1</D:sync-level>
  <D:prop>
    <D:getetag/>
  </D:prop>
</D:sync-collection>'

# Multiget template (hrefs filled in at runtime)
multiget_xml() {
  local hrefs=""
  for h in "$@"; do
    hrefs="${hrefs}  <D:href>${h}</D:href>
"
  done
  cat <<XMLEOF
<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:getetag/>
    <C:address-data/>
  </D:prop>
${hrefs}</C:addressbook-multiget>
XMLEOF
}

# Sync-collection with token
sync_delta_xml() {
  cat <<XMLEOF
<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>${1}</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:prop>
    <D:getetag/>
  </D:prop>
</D:sync-collection>
XMLEOF
}

# ── build & start ────────────────────────────────────────────────────────────

step "Building go-contactd"
  (cd "${ROOT_DIR}" && go build -o "${BIN}" ./cmd/contactd)
  ln -sf "${BIN}" "${ADMIN_BIN}"
ok "binary built"

step "Starting server"
start_server
ok "server ready"

step "Creating test user via CLI"
printf '%s\n' "secret" | "${ADMIN_BIN}" user add -d "${DB}" --username alice --password-stdin >/dev/null
ok "user alice created"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 1: DAVx5 Account Setup (DavResourceFinder.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 1: Account Discovery"

# 1a. Well-known redirect
dav GET "/.well-known/carddav"
expect 308 "well-known redirect"
body_lacks "error" "no error in redirect body"
if [[ -n "${SERVER_BASE_URL}" ]]; then
  header_equals "Location" "${SERVER_BASE_URL%/}/" "well-known Location uses CONTACTD_BASE_URL"
else
  header_equals "Location" "/" "well-known Location is root-relative"
fi

# 1b. PROPFIND on root — server currently returns 404 (root discovery not
#     implemented; PLAN.md says it SHOULD return current-user-principal).
#     DAVx5 falls back to the user-supplied base URL or principal URL.
dav PROPFIND "/" \
  -H "Depth: 0" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_DISCOVERY}"
# Accept either 207 (plan-compliant) or 404 (current behavior).
if [[ "${HTTP_STATUS}" == "207" ]]; then
  ok "root PROPFIND returns 207 (plan-compliant)"
  body_has "current-user-principal" "root returns current-user-principal"
elif [[ "${HTTP_STATUS}" == "404" ]]; then
  if [[ "${STRICT_PLAN}" == "1" ]]; then
    fail "root PROPFIND returned 404 (strict mode requires 207)"
  fi
  ok "root PROPFIND returns 404 (known gap — DAVx5 falls back to principal URL)"
else
  fail "root PROPFIND unexpected status ${HTTP_STATUS}"
fi

# 1c. PROPFIND on principal (Depth: 0) — discovery properties
dav PROPFIND "/alice/" \
  -H "Depth: 0" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_DISCOVERY}"
expect 207 "principal PROPFIND Depth:0"
body_has "current-user-principal" "principal has current-user-principal"
body_has "addressbook-home-set" "principal has addressbook-home-set"
body_has "/alice/" "home-set href is /alice/"

# 1d. OPTIONS on principal (service capability check)
dav OPTIONS "/alice/"
expect 204 "OPTIONS on principal"
header_has "DAV" "DAV header present"
header_has "Allow" "Allow header present"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 2: Address Book Discovery (HomeSetRefresher.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 2: Address Book Discovery"

# 2a. PROPFIND on home-set (Depth: 1) — list address books
dav PROPFIND "/alice/" \
  -H "Depth: 1" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_HOMESET}"
expect 207 "home-set PROPFIND Depth:1"
body_has "/alice/contacts/" "contacts addressbook listed"
body_has "addressbook" "resourcetype includes addressbook"

# Verify response count: should have principal + at least one addressbook
# Server may use <D:response> or <response xmlns="DAV:">, so match both patterns.
n_responses="$(echo "${HTTP_BODY}" | grep -co '<[^/]*response>' | head -n1 || echo 0)"
[[ "${n_responses}" -ge 2 ]] || fail "Depth:1 should return >= 2 responses, got ${n_responses}"
ok "Depth:1 returns ${n_responses} responses (principal + addressbooks)"

# 2b. Verify Depth: infinity is rejected
dav PROPFIND "/alice/" \
  -H "Depth: infinity" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_HOMESET}"
expect 403 "Depth:infinity rejected"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 3: Sync Capability Detection (ContactsSyncManager.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 3: Sync Capabilities"

# 3a. PROPFIND on addressbook (Depth: 0) for sync properties
dav PROPFIND "/alice/contacts/" \
  -H "Depth: 0" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_CAPABILITIES}"
expect 207 "addressbook capabilities PROPFIND"
body_has "sync-token" "has sync-token property"
body_has "getctag" "has getctag property"
body_has "supported-report-set" "has supported-report-set"
body_has "sync-collection" "supported-report-set includes sync-collection"
body_has "addressbook-multiget" "supported-report-set includes multiget"
body_has "supported-address-data" "has supported-address-data"

# Save initial CTag/sync-token for later comparison
INITIAL_BODY="${HTTP_BODY}"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 4: Initial Sync (SyncManager.kt — COLLECTION_SYNC algorithm)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 4: Initial Sync (empty collection)"

# 4a. sync-collection with empty token
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${SYNC_INITIAL}"
expect 207 "initial sync-collection"
SYNC_TOKEN_0="$(extract_sync_token)"
[[ -n "${SYNC_TOKEN_0}" ]] || fail "no sync-token in initial sync response"
ok "got initial sync-token: ${SYNC_TOKEN_0}"

# Empty collection — should have token but no card responses
body_lacks ".vcf" "no cards in empty collection sync"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 5: Contact Upload — DAVx5 writeback (SyncManager.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 5: Contact Creation (PUT with If-None-Match: *)"

# 5a. Create alice-contact.vcf (DAVx5 sends If-None-Match: * for new contacts)
dav PUT "/alice/contacts/alice-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_ALICE}"
expect 201 "PUT create alice-contact"
ETAG_ALICE="$(extract_etag_header)"
header_has "ETag" "PUT response includes ETag"
[[ -n "${ETAG_ALICE}" ]] || fail "ETag header empty after PUT create"
ok "alice ETag: ${ETAG_ALICE}"

# 5b. Verify If-None-Match: * rejects overwrite of existing resource
dav PUT "/alice/contacts/alice-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_ALICE}"
expect 412 "If-None-Match:* on existing → 412"

# 5c. Create bob-contact.vcf
dav PUT "/alice/contacts/bob-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_BOB}"
expect 201 "PUT create bob-contact"
ETAG_BOB="$(extract_etag_header)"

# 5d. Create carol-contact.vcf
dav PUT "/alice/contacts/carol-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_CAROL}"
expect 201 "PUT create carol-contact"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 6: Incremental Sync — detect new contacts
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 6: Incremental Sync (3 new contacts)"

# 6a. CTag must have changed since Phase 3
dav PROPFIND "/alice/contacts/" \
  -H "Depth: 0" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_CAPABILITIES}"
expect 207 "capabilities after PUT"
# CTag should differ from initial (empty collection)
[[ "${HTTP_BODY}" != "${INITIAL_BODY}" ]] || fail "CTag did not change after PUTs"
ok "CTag changed after contact creation"

# 6b. sync-collection with token from initial sync
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "${SYNC_TOKEN_0}")"
expect 207 "delta sync after 3 PUTs"
body_has "/alice/contacts/alice-contact.vcf" "delta includes alice-contact"
body_has "/alice/contacts/bob-contact.vcf" "delta includes bob-contact"
body_has "/alice/contacts/carol-contact.vcf" "delta includes carol-contact"
body_has "getetag" "delta responses include getetag"
body_lacks "valid-sync-token" "no invalid-token error"
SYNC_TOKEN_1="$(extract_sync_token)"
[[ -n "${SYNC_TOKEN_1}" ]] || fail "missing sync-token in delta response"
ok "new sync-token: ${SYNC_TOKEN_1}"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 7: Multiget — download contact data (ContactsSyncManager.kt)
#   DAVx5 batches max 10 hrefs per multiget.
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 7: Multiget (batch download)"

# 7a. Multiget all 3 contacts
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(multiget_xml \
    "/alice/contacts/alice-contact.vcf" \
    "/alice/contacts/bob-contact.vcf" \
    "/alice/contacts/carol-contact.vcf")"
expect 207 "multiget 3 contacts"
body_has "address-data" "multiget includes address-data"
body_has "UID:uid-alice-contact" "multiget has alice vCard"
body_has "UID:uid-bob-contact" "multiget has bob vCard"
body_has "UID:uid-carol-contact" "multiget has carol vCard"
body_has "getetag" "multiget has getetag"

# 7b. Multiget subset (only 1 of 3)
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(multiget_xml "/alice/contacts/carol-contact.vcf")"
expect 207 "multiget subset"
body_has "UID:uid-carol-contact" "subset has carol"
body_lacks "UID:uid-alice-contact" "subset omits alice"
body_lacks "UID:uid-bob-contact" "subset omits bob"

# 7c. Multiget with nonexistent href
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(multiget_xml \
    "/alice/contacts/alice-contact.vcf" \
    "/alice/contacts/does-not-exist.vcf")"
expect 207 "multiget with missing href"
body_has "/alice/contacts/does-not-exist.vcf" "missing href has response entry"
body_has "404" "missing href returns 404 status"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 8: Contact Update — conditional PUT (SyncManager.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 8: Contact Update (PUT with If-Match)"

# 8a. Update bob with correct ETag
dav PUT "/alice/contacts/bob-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-Match: ${ETAG_BOB}" \
  --data-binary "${VCARD_BOB_UPDATED}"
expect 204 "PUT update bob (correct ETag)"
header_has "ETag" "update response has ETag"

# 8b. Stale ETag update must fail
dav PUT "/alice/contacts/bob-contact.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-Match: ${ETAG_BOB}" \
  --data-binary "${VCARD_BOB_UPDATED}"
expect 412 "PUT update with stale ETag → 412"

# 8c. Verify updated content via GET
dav GET "/alice/contacts/bob-contact.vcf"
expect 200 "GET updated bob"
body_has "FN:Bob Updated" "bob FN updated"
body_has "bob-new@example.com" "bob email updated"
header_has "Content-Type" "GET has Content-Type"
header_has "ETag" "GET has ETag"
header_has "Content-Length" "GET has Content-Length"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 9: Contact Deletion (SyncManager.kt)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 9: Contact Deletion (DELETE with If-Match)"

# Capture carol's current ETag first
dav GET "/alice/contacts/carol-contact.vcf"
expect 200 "GET carol for ETag"
ETAG_CAROL="$(extract_etag_header)"

# 9a. DELETE carol with correct ETag (DAVx5 always sends If-Match on delete)
dav DELETE "/alice/contacts/carol-contact.vcf" \
  -H "If-Match: ${ETAG_CAROL}"
expect 204 "DELETE carol"
[[ -z "${HTTP_BODY}" ]] || [[ "${HTTP_BODY}" =~ ^[[:space:]]*$ ]] || \
  fail "DELETE 204 should have empty body, got: ${HTTP_BODY}"
ok "DELETE body is empty"

# 9b. Confirm carol is gone
dav GET "/alice/contacts/carol-contact.vcf"
expect 404 "carol gone after DELETE"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 10: Incremental Sync — detect update + delete
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 10: Delta Sync (update + delete since last token)"

dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "${SYNC_TOKEN_1}")"
expect 207 "delta sync after update+delete"

# bob-contact.vcf should appear as updated (200 + getetag)
body_has "/alice/contacts/bob-contact.vcf" "delta has updated bob"

# carol-contact.vcf should appear as deleted (404)
body_has "/alice/contacts/carol-contact.vcf" "delta has deleted carol"
body_has "404" "delta has 404 status for carol"

body_lacks "valid-sync-token" "token still valid"
SYNC_TOKEN_2="$(extract_sync_token)"
[[ -n "${SYNC_TOKEN_2}" ]] || fail "missing sync-token"
ok "sync-token: ${SYNC_TOKEN_2}"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 11: Empty Delta — no changes since last sync
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 11: Empty Delta Sync (no changes)"

dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "${SYNC_TOKEN_2}")"
expect 207 "empty delta sync"
body_lacks ".vcf" "no card entries in empty delta"
SYNC_TOKEN_3="$(extract_sync_token)"
[[ -n "${SYNC_TOKEN_3}" ]] || fail "missing sync-token in empty delta"
ok "empty delta returns token: ${SYNC_TOKEN_3}"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 12: Invalid Sync Token — server returns valid-sync-token error
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 12: Invalid Sync Token"

dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "urn:contactd:sync:999:999")"
expect 403 "invalid token → 403"
body_has "valid-sync-token" "error body has valid-sync-token"
body_lacks "sync-token>urn:" "error body has no sync-token element"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 13: UID Conflict
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 13: UID Conflict Detection"

# Try creating a new card with alice's UID on a different href
VCARD_CONFLICT=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-alice-contact\r\nFN:Duplicate\r\nEND:VCARD\r\n')
dav PUT "/alice/contacts/conflict.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_CONFLICT}"
expect 409 "UID conflict → 409"
body_has "no-uid-conflict" "error has no-uid-conflict precondition"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 14: Content-Type Enforcement
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 14: Content-Type Enforcement"

dav PUT "/alice/contacts/bad.vcf" \
  -H "Content-Type: application/json" \
  --data-binary '{"name":"bad"}'
expect 415 "wrong Content-Type → 415"

dav PUT "/alice/contacts/bad.vcf" \
  --data-binary "${VCARD_ALICE}"
expect 415 "missing Content-Type → 415"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 15: Cross-User Isolation (IDOR)
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 15: Cross-User Isolation"

# Create a second user
printf '%s\n' "evil" | "${ADMIN_BIN}" user add -d "${DB}" --username mallory --password-stdin >/dev/null
ok "user mallory created"

# alice tries to read mallory's contacts → 404 (not 403)
dav GET "/mallory/contacts/alice-contact.vcf"
expect 404 "cross-user GET → 404"

# alice tries to write to mallory's collection → 404
dav PUT "/mallory/contacts/injected.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_ALICE}"
expect 404 "cross-user PUT → 404"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 16: Server Restart — token continuity
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 16: Server Restart + Token Continuity"

stop_server
start_server
ok "server restarted"

# Verify last sync token still works after restart
dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "${SYNC_TOKEN_3}")"
expect 207 "post-restart delta sync"
body_lacks "valid-sync-token" "token valid across restart"
ok "sync-token survives restart"

# Create a new card after restart and verify delta sync picks it up
VCARD_DAVE=$(printf 'BEGIN:VCARD\r\nVERSION:3.0\r\nUID:uid-dave\r\nFN:Dave Post-Restart\r\nEND:VCARD\r\n')
dav PUT "/alice/contacts/dave.vcf" \
  -H "Content-Type: text/vcard; charset=utf-8" \
  -H "If-None-Match: *" \
  --data-binary "${VCARD_DAVE}"
expect 201 "PUT after restart"

dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "$(sync_delta_xml "${SYNC_TOKEN_3}")"
expect 207 "delta after restart + new card"
body_has "/alice/contacts/dave.vcf" "new card appears in delta"
body_lacks "valid-sync-token" "token still valid"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 17: MKCOL — create additional addressbook
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 17: MKCOL"

dav MKCOL "/alice/work/"
expect 201 "MKCOL create /alice/work/"

dav MKCOL "/alice/work/"
expect 405 "MKCOL existing → 405"

# Verify new addressbook appears in Depth:1 listing
dav PROPFIND "/alice/" \
  -H "Depth: 1" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPFIND_HOMESET}"
expect 207 "home-set after MKCOL"
body_has "/alice/work/" "new addressbook listed"
body_has "/alice/contacts/" "original addressbook still listed"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 18: PROPPATCH — addressbook metadata
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 18: PROPPATCH Metadata"

PROPPATCH_XML='<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set>
    <D:prop>
      <D:displayname>Work Contacts</D:displayname>
      <C:addressbook-description>Professional contacts</C:addressbook-description>
    </D:prop>
  </D:set>
</D:propertyupdate>'

dav PROPPATCH "/alice/work/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary "${PROPPATCH_XML}"
expect 207 "PROPPATCH set metadata"
body_has "200" "PROPPATCH 200 OK in propstat"

# Verify metadata persisted via PROPFIND
dav PROPFIND "/alice/work/" \
  -H "Depth: 0" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary '<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:displayname/><C:addressbook-description/></D:prop></D:propfind>'
expect 207 "PROPFIND after PROPPATCH"
body_has "Work Contacts" "displayname persisted"
body_has "Professional contacts" "description persisted"

# ═════════════════════════════════════════════════════════════════════════════
# PHASE 19: Unknown REPORT type
# ═════════════════════════════════════════════════════════════════════════════

step "Phase 19: Unknown REPORT Type"

dav REPORT "/alice/contacts/" \
  -H "Content-Type: application/xml; charset=utf-8" \
  --data-binary '<?xml version="1.0"?><D:calendar-query xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:calendar-query>'
expect 501 "unknown REPORT → 501"

# ═════════════════════════════════════════════════════════════════════════════
# Summary
# ═════════════════════════════════════════════════════════════════════════════

printf '\n\033[1;32m══════════════════════════════════════════\033[0m\n'
printf '\033[1;32m  All 19 phases passed.\033[0m\n'
printf '\033[1;32m══════════════════════════════════════════\033[0m\n'

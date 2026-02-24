# FIXME (Validated From `TODO`)

This file is a deduplicated, validated list of real open issues from `TODO`.
Items already fixed are listed at the bottom so they can be removed from `TODO`.

## Open Issues

### FIXME-000 (P2) `Store.PragmaString` / `Store.PragmaInt` SQL injection via PRAGMA name interpolation (internal API hardening)

- Status: validated (code inspection; TODO includes working stacked-statement repros)
- Impact:
  - `db.Store` exposes helper methods that interpolate `name` directly into `PRAGMA %s;` SQL.
  - Callers passing untrusted names can execute stacked statements (e.g. create/drop tables) under the DB connection.
  - Current risk is primarily internal/maintenance-facing because these helpers are not exposed on the HTTP path, but the primitive is real.
- Affected code:
  - `internal/db/store.go` (`PragmaString`)
  - `internal/db/store.go` (`PragmaInt`)
- Root cause:
  - `fmt.Sprintf("PRAGMA %s;", name)` with no identifier validation/allowlist.
- Suggested fix:
  - Do not accept arbitrary PRAGMA names.
  - Replace with:
    - a small allowlist of supported pragma names (`busy_timeout`, `journal_mode`, etc.), or
    - dedicated methods per pragma needed by tests/ops.
  - If a generic helper is retained, strictly validate `name` against an identifier regex and reject `;`, whitespace, quotes, comments.
- Tests to add:
  - injection payload with `;` is rejected for both `PragmaString` and `PragmaInt`
  - valid simple pragma names still work

### FIXME-001 (P2) `contactctl import` ignores configured vCard size limit

- Status: validated (code inspection)
- Impact: `contactctl import` always enforces `config.DefaultVCardMaxBytes` instead of the configured runtime limit (`CONTACTD_VCARD_MAX_BYTES`), so operator-configured size policy is not honored by admin imports.
- Affected code:
  - `internal/ctl/ctl.go` (`runImport`)
  - current code hardcodes `vcardMaxBytes := int(config.DefaultVCardMaxBytes)`
- Suggested fix:
  - Decide config source for `contactctl import` (env only, or env + flag).
  - Parse and validate `CONTACTD_VCARD_MAX_BYTES` (and optionally `CONTACTD_REQUEST_MAX_BYTES` parity) for `contactctl`.
  - Use configured value instead of `config.DefaultVCardMaxBytes`.
- Tests to add:
  - `contactctl import` with `CONTACTD_VCARD_MAX_BYTES=1024` rejects a >1KiB card (dir and concat paths).

### FIXME-002 (P1) `REPORT addressbook-multiget` is not bound to request-target ownership/collection

- Status: validated (code inspection; TODO contains multiple duplicate repro variants)
- Impact:
  - A `REPORT` to `/alice/contacts/` can request hrefs from another collection under the same principal (including traversal-normalized hrefs) and receive data from that other collection.
  - A `REPORT` sent to a cross-tenant or nonexistent target collection path (for example `/bob/contacts/` or `/doesnotexist/x/`) can still return caller-owned card data if the body `href`s point at caller-owned resources.
  - This also combines with traversal-style body hrefs (for example `/alice/contacts/../private/p.vcf`) to bypass collection boundaries while returning normalized leaked hrefs.
- Affected code:
  - `internal/server/handler.go` (`handleAddressbookMultiGet`)
  - `internal/carddav/backend.go` (`parseCardPathForPrincipal` normalizes path and validates only principal ownership)
- Root cause:
  - `handleReport` dispatches `addressbook-multiget` based on path shape (`classifyDAVPath`) and report local-name.
  - `handleAddressbookMultiGet` trusts each requested `href` and fetches it directly via backend, without enforcing:
    - request-target collection ownership/principal binding (`r.URL.Path`)
    - fetched-card membership in the same request-target addressbook
- Suggested fix:
  - Parse and validate the request-target collection (`user`, `slug`) once.
  - Enforce authenticated principal ownership of the REPORT target collection before processing body `href`s (return `404` on mismatch, consistent with `addressbook-query` / `sync-collection`).
  - For each `href`, require exact membership in the same collection (same user + same slug).
  - Treat mismatches as `404` per-item response (not global error).
- Tests to add:
  - Cross-tenant REPORT target (`/bob/...`) as `alice` returns `404` (no body processing leak).
  - Nonexistent REPORT target returns `404` (no body processing leak).
  - Cross-collection href returns `404` prop response in multiget.
  - Traversal-style href (`../private/...`) returns `404` and does not leak normalized target.
  - Malformed-namespace multiget (`X:addressbook-multiget`) does not bypass the above checks (after `FIXME-003`).

### FIXME-003 (P2) `REPORT` namespace confusion (local-name-only dispatch and field parsing)

- Status: validated (code inspection)
- Impact: Non-DAV/CardDAV XML roots and children can drive report behavior (`sync-collection`, `addressbook-multiget`, `limit`, `sync-token`) because dispatch/parsing relies on local names.
- Affected code:
  - `internal/server/handler.go` (`handleReport`)
  - `xml` envelope uses local-name tags (`xml:"href"`, `xml:"sync-token"`, `xml:"limit"`, `xml:"nresults"`)
  - dispatch uses `envelope.XMLName.Local`
- Suggested fix:
  - Enforce root namespace for supported REPORTs:
    - `DAV:` for `sync-collection`
    - CardDAV namespace for `addressbook-multiget` / `addressbook-query`
  - Parse child elements with namespace-aware structs or custom decoder logic.
  - Reject namespace-mismatched payloads with `400`.
- Tests to add:
  - Non-DAV `X:sync-collection` rejected.
  - Non-DAV `X:limit` / `X:sync-token` do not affect sync behavior.
  - Non-CardDAV `X:addressbook-multiget` rejected.

### FIXME-004 (P2) `PROPPATCH` namespace confusion and malformed mixed-namespace structure acceptance

- Status: validated (code inspection)
- Impact:
  - Non-DAV root `propertyupdate` is accepted.
  - Mixed-namespace structure (e.g. `X:set`) can still produce operations because the parser only partially enforces DAV structure and uses depth-based parsing.
- Affected code:
  - `internal/server/handler.go` (`parseProppatchRequest`)
- Root cause:
  - Root checks only `Local == "propertyupdate"`.
  - Parser can enter `inProp` and append ops even when `mode` is not a valid DAV `set/remove`.
- Suggested fix:
  - Require `DAV:` root namespace.
  - Require DAV `set`/`remove` structure before accepting props.
  - Reject malformed structures instead of silently producing ops.
- Tests to add:
  - Non-DAV root rejected.
  - `X:set` / mixed namespace structure rejected and no metadata mutation occurs.

### FIXME-005 (P2) `REPORT`/`PROPPATCH` accept trailing XML content (multi-root / trailing garbage)

- Status: validated (code inspection)
- Impact: Valid first XML document can be processed even when extra top-level content follows.
- Affected code:
  - `internal/server/handler.go` (`handleReport`) uses one `Decode` call without EOF/trailing-content check
  - `internal/server/handler.go` (`parseProppatchRequest`) token loop does not enforce a single top-level document root
- Suggested fix:
  - Add strict trailing-content checks after parse (ignoring only whitespace/comments).
  - Reject extra top-level elements with `400`.
- Tests to add:
  - valid REPORT + trailing `<X:evil/>` rejected.
  - valid PROPPATCH + trailing `<X:evil/>` rejected and state unchanged.

### FIXME-006 (P2) Control characters in path segments are accepted

- Status: validated (code inspection)
- Impact: Encoded control bytes in path segments can be used to create and operate on collections (`MKCOL` / `PROPPATCH` / `REPORT`), producing hard-to-handle or unsafe path names.
- Affected code:
  - `internal/server/handler.go` (`validateRequestPathPayload`)
- Root cause:
  - Validation rejects separators/traversal, but not ASCII control characters.
- Suggested fix:
  - Reject decoded path segments containing control bytes (`< 0x20` or `0x7f`).
- Tests to add:
  - `%00`, `%09`, `%0A`, `%0D`, `%7F` path segment payloads rejected with `400`.

### FIXME-007 (P2) `addressbook-multiget` has no href count cap or dedupe (response amplification / N-query behavior)

- Status: validated (code inspection)
- Impact: Large multiget requests with duplicate hrefs can cause large duplicate responses and expensive repeated backend lookups.
- Affected code:
  - `internal/server/handler.go` (`handleAddressbookMultiGet`)
- Root cause:
  - Loops directly over request `hrefs` with no dedupe and no maximum count.
- Suggested fix:
  - Add a configurable or hardcoded cap on href count.
  - Dedupe hrefs before backend fetch (preserve requested order only if required).
- Tests to add:
  - Duplicate hrefs are deduped (or capped) with predictable result behavior.
  - Large href count rejected or truncated per defined policy.

### FIXME-008 (P1) `sync-collection` token can fail to advance under concurrent writes (token-window race)

- Status: validated as plausible by code inspection (TODO includes reproducible evidence; repro not re-run in this review)
- Impact: Server may return changes while reusing the same sync token under concurrency, causing duplicate delta replay / sync thrash.
- Affected code:
  - `internal/carddavx/sync.go` (`SyncCollection`)
- Suspected root cause:
  - Split reads (`GetAddressbookByUsernameSlug` then `ListCardChangesSince`) without a consistent snapshot.
  - Token computation starts from `ab.Revision` and can be lowered to observed rows, but race windows can produce stale/unchanged response token despite returned changes.
- Suggested fix:
  - Compute sync result against a consistent snapshot (transaction/read view), or derive token from observed max revision robustly.
  - Add a deterministic concurrency harness regression in-tree.
- Tests to add:
  - Deterministic race test proving returned delta always advances token when any change item is returned.

### FIXME-009 (P1) Username enumeration timing side-channel in Basic Auth

- Status: validated (code inspection)
- Impact: Missing usernames return much faster than existing usernames with wrong password, allowing enumeration via timing.
- Affected code:
  - `internal/db/store.go` (`AuthenticateUser`)
- Root cause:
  - `sql.ErrNoRows` returns immediately without bcrypt work.
  - Existing user path performs bcrypt compare.
- Revalidated evidence (from TODO):
  - `alice:wrong` median ~`0.080839s`
  - `nosuchuser:wrong` median ~`0.000567s`
  - gap ~`142x` while both return `401`
- Suggested fix:
  - Always perform a bcrypt compare using a fixed dummy hash on missing-user path.
  - Keep response body/status/challenge behavior identical.
- Tests to add:
  - Functional behavior remains identical for missing vs wrong-password (timing test can be optional / benchmark-style).

### FIXME-010 (P2) Unauthenticated path-validation oracle (`400` before `401`)

- Status: validated (code inspection)
- Impact: Invalid encoded-path payloads get `400` without auth challenge, while valid protected paths get `401` with challenge. This leaks path-validation behavior pre-auth.
- Affected code:
  - `internal/server/handler.go` (`serveHTTP`)
- Root cause:
  - `validateRequestPathPayload` runs before `requireBasicAuth`.
- Revalidated evidence (from TODO):
  - no-auth valid protected path -> `401` + `WWW-Authenticate`
  - no-auth invalid encoded path payload (e.g. `%2F`) -> `400` without challenge
- Suggested fix:
  - Run auth challenge before path validation for protected routes, or normalize unauthenticated failures to the same `401` + challenge behavior.
- Tests to add:
  - No-auth invalid protected path returns the same auth challenge behavior as a valid protected path.

## Already Fixed (remove from `TODO`)

These findings were verified fixed in the current tree and should be deleted from `TODO`:

- Stale `If-Match` DELETE race (fixed in `f34f3ad`)
- Strong ETag mismatch vs GET bytes (fixed in `2a18321`)
- Import partial commit / non-atomic batch failure (fixed in `84d8c9d`)
- Directory import trailing garbage / multi-card single-file acceptance (fixed in `b46f873`)
- `contactctl import` directory symlink read escape (fixed locally, pending commit)
- `contactctl export` symlink output clobber (`dir` and `concat --out`) (fixed locally, pending commit)

## Notes

- `TODO` currently contains several duplicates of the same multiget / namespace / control-character issues.
- The "attribute-heavy XML" note is an observation, not a validated bug by itself.

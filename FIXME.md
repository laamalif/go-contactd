# FIXME (Validated From `TODO`)

This file is a deduplicated, validated list of real open issues from `TODO`.
Items already fixed are listed at the bottom so they can be removed from `TODO`.

## Open Issues

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

### FIXME-016 (P1) No auth throttling/lockout/rate-limit enables practical password spraying after enumeration

- Status: validated operationally (TODO includes attack-chain repro); code inspection confirms no throttling/lockout in auth path
- Impact:
  - Attackers can combine `FIXME-009` (username enumeration) with unlimited online password attempts.
  - Legitimate authentication remains available after large failed-attempt bursts, enabling ongoing spraying without server-enforced delay/lockout.
  - Credential-validity oracles (`401` vs non-`401`) work across methods and nonexistent paths, making spray verification cheap and flexible.
- Affected code:
  - `internal/server/handler.go` (`requireBasicAuth`)
  - `internal/db/store.go` (`AuthenticateUser`)
  - `internal/daemon/daemon.go` (no auth middleware throttling/rate-limit wiring)
- Root cause:
  - No per-IP / per-username / global failed-auth throttling, no lockout window, no backoff.
- Revalidated evidence (from TODO):
  - After `400` failed attempts for `alice`, legitimate auth still succeeded immediately.
  - Chained demo: enumerate top usernames, then spray candidate passwords until a success indicator (`non-401`) is observed.
  - Fast stuffing chain demo: exact top-3 users enumerated, then password spray found a valid credential in ~`0.387s` for `18` attempts.
  - Specific spray-chain evidence:
    - `enum_top3=alice,bob,charlie`
    - `spray_hit user=alice pass=welcome1 code=404`
    - `spray_elapsed_s=0.387 attempts=18 hits=1`
  - Credential-validity oracle works on arbitrary nonexistent protected paths:
    - valid creds on nonexistent path -> `404`
    - invalid creds on nonexistent path -> `401`
    - attacker does not need a known existing resource path to verify sprayed credentials
  - Oracle also works across methods (example `OPTIONS /not-real`: valid creds -> `204`, invalid creds -> `401`)
- Suggested fix:
  - Add optional auth throttling / rate-limiting in the HTTP auth path (per-IP and/or per-username dimensions).
  - Consider randomized small delay or token-bucket controls.
  - Keep default behavior/operator UX in mind (document tradeoffs and trusted-proxy interactions).
- Tests to add:
  - Repeated failed auth attempts trigger throttle behavior (without breaking valid auth semantics)
  - Throttle keying behavior documented and tested (direct remote vs trusted proxy mode)

### FIXME-019 (P1) `contactctl import` still lacks full bounded-read and content-TOCTOU hardening for untrusted file inputs (local CLI DoS / tamper risk)

- Status: partially fixed (`f03d63b`); remaining risk validated by code inspection and TODO repro evidence
- Impact:
  - Directory import can also ingest tampered content that was not present at initial directory snapshot (regular-file content swap race).
  - This is a local/admin-path availability issue, not a remote HTTP issue.
- Affected code:
  - `internal/ctl/ctl.go` (`importFromDir`)
  - `internal/ctl/ctl.go` (`importFromConcatFile`)
- Root cause:
  - Directory mode still enumerates names and later opens by path; content can change between directory snapshot and later open/read (regular-file content TOCTOU), even though file-type/symlink checks are improved.
- Partial fixes already applied:
  - `89d03cf`: rejects symlink/non-regular dir import entries via path checks
  - `f03d63b`: opens import files via stable handle, revalidates opened descriptor type, rejects non-regular concat sources, and bounds dir-mode file reads before full load
  - `3af97ee`: adds total-input cap for concat import sources (default `64 MiB`) and bounded decode reader
- Revalidated evidence (from TODO):
  - Regular-file swap race changed imported content while import still exited success (`race_benign=0`, `race_evil=1`), demonstrating content TOCTOU.
- Suggested fix:
  - Optional stronger dir-mode hardening:
    - use directory FD + openat-style workflow (or equivalent) to reduce name-to-content TOCTOU surface further
    - hash/mtime/inode revalidation strategy if snapshot consistency is desired
- Tests to add:
  - oversized regular `.vcf` in dir mode is rejected without reading entire file into memory (best-effort behavioral test)
  - dir import regular-file content swap cannot alter imported bytes after snapshot/open (best-effort deterministic harness)
  - concat import from non-regular source is rejected (already covered; keep regression)
  - concat import total-input cap is enforced on large regular files (already covered; keep regression)

### FIXME-024 (P1) Large full-sync pagination can still fail after prune when continuation cursor cache is skipped

- Status: validated by code inspection and user repro evidence
- Impact:
  - `sync-collection` with empty token and `nresults` can return a truncated first page, then reject the server-issued continuation token on page 2 after journal prune.
  - New/reset clients syncing large addressbooks can fail mid-bootstrap under normal prune operation and may churn into full-resync retries.
- Affected code:
  - `internal/carddavx/sync.go` (`buildPagedSyncResult`, `SyncCollection`)
  - `internal/db/store.go` (`ListCurrentCardSyncStates`)
- Root cause:
  - Full-sync pagination relies on the in-memory continuation cursor cache when `len(remaining) <= syncCursorMaxItems`.
  - For large addressbooks (`remaining > syncCursorMaxItems`), no cursor is stored and page-2 falls back to journal delta lookup (`ListCardChangesSince`).
  - After prune (especially post-prune full-sync states with revision `0`), that fallback can hit the stale-token path and reject the just-issued continuation token.
- Revalidated evidence (from user repro):
  - Failing case: `n=10060`, prune all `card_changes`, empty-token sync with `limit=50`
    - page1 `truncated=true`, token `urn:contactd:sync:1:0`
    - page2 with that token -> `invalid sync token: stale token`
  - Control case: `n=10030` (below cache threshold), page2 succeeds
- Suggested fix:
  - Preserve continuation state for full-sync pagination even when remaining items exceed `syncCursorMaxItems` (chunked cursor storage / paged cursor references / alternate full-sync continuation path).
  - Avoid falling back to journal delta semantics for a full-sync continuation token issued from a truncated empty-token response.
- Tests to add:
  - deterministic service test with >`syncCursorMaxItems` full-sync items, prune between pages, page2 continuation still succeeds
  - HTTP-level regression for `REPORT sync-collection` empty-token pagination > cache threshold (optional; may be heavy)

### FIXME-026 (P1) `sync-collection` continuation cursor cache has no hard size cap/eviction and can grow unbounded under authenticated load

- Status: validated by code inspection and user repro evidence
- Impact:
  - Authenticated clients can create many distinct paginated sync cursors, causing unbounded in-memory growth (`s.page` map and cached items) until TTL expiry.
  - This can become a memory-DoS vector even after multiget fan-out hardening.
- Affected code:
  - `internal/carddavx/sync.go` (`putCursor`, `takeCursorPage`, `SyncService.page`)
- Root cause:
  - Cursor entries expire by TTL only; there is no global entry cap, item cap across all cursors, or eviction policy.
  - `syncCursorMaxItems` limits items per cursor, not total cache memory.
- Revalidated evidence (from user repro):
  - Crafted token requests drove cache to `cursor_entries=1000`, `total_cached_items=1000000`
- Suggested fix:
  - Add global cursor cache limits (entry count and/or total cached items) with eviction (e.g. oldest-first / nearest-expiry).
  - Consider refusing to cache new cursors when limits are exceeded and return non-paginated fallback behavior safely.
- Tests to add:
  - deterministic cache growth test enforces max entries/items and eviction behavior
  - pagination still works for recent cursors after eviction pressure

### FIXME-027 (P1) REPORT multistatus responses are fully marshaled in memory (authenticated response-amplification memory DoS)

- Status: validated by code inspection and user repro evidence
- Impact:
  - A single authenticated `REPORT addressbook-query` / `addressbook-multiget` can force large in-memory response construction, significantly exceeding request size and causing high heap spikes.
  - This can degrade or crash the daemon under repeated large-card queries even after href-count limits.
- Affected code:
  - `internal/server/handler.go` (`handleAddressbookMultiGet`, `handleAddressbookQuery`, `writeDAVMultiStatus`)
- Root cause:
  - Handler builds the full `[]davxml.Response`, then `writeDAVMultiStatus` calls `davxml.Marshal(davxml.MultiStatus{...})`, materializing the entire XML body as a single `[]byte` before writing.
  - `address-data` payloads can be large, so response amplification multiplies memory use.
- Revalidated evidence (from user repro):
  - Single `REPORT addressbook-query` over `120` cards (~`280KB` each) returned ~`34MB` response body
  - observed heap delta ~`65MB` during one request
- Suggested fix:
  - Add a response-size cap for REPORT paths (especially when returning `address-data`) and fail fast when estimated/actual output exceeds limit.
  - Longer-term: stream multistatus XML instead of marshalling whole body at once.
  - Consider paging/query limits for `addressbook-query` and `multiget` response payload volume.
- Tests to add:
  - deterministic REPORT payload-size cap test (query/multiget returns error when response would exceed cap)
  - regression that normal small REPORT responses still pass
  - optional benchmark/pprof guard for large `address-data` responses

## Already Fixed (remove from `TODO`)

These findings were verified fixed in the current tree and should be deleted from `TODO`:

- Stale `If-Match` DELETE race (fixed in `f34f3ad`)
- Strong ETag mismatch vs GET bytes (fixed in `2a18321`)
- Import partial commit / non-atomic batch failure (fixed in `84d8c9d`)
- Directory import trailing garbage / multi-card single-file acceptance (fixed in `b46f873`)
- `contactctl import` directory symlink read escape (fixed in `89d03cf`)
- `contactctl export` symlink output clobber (`dir` and `concat --out`) (fixed in `89d03cf`)
- Basic Auth missing-user timing parity / dummy bcrypt compare (fixed in `1e7a4c5`)
- Duplicate `Authorization` header ambiguity (duplicate/comma-combined headers rejected) (fixed in `9f6e261`)
- Oversized Basic `Authorization` header rejection (`431`) (fixed in `3d076c5`)
- HTTP server timeout defaults (`ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`) (fixed in `cb4f34e`)
- Unauthenticated protected-path auth challenge before path validation (`401` vs `400` oracle) (fixed in `9e00528`)
- Control-character rejection in path segments (fixed in `b4c80fd`)
- Strict trailing XML content rejection for `REPORT` / `PROPPATCH` (fixed in `0b2c940`)
- `Store.PragmaString` / `Store.PragmaInt` PRAGMA-name validation (fixed in `48d0b25`)
- `contactctl import` honors `CONTACTD_VCARD_MAX_BYTES` (fixed in `7344b19`)
- `contactctl export --format concat` seam normalization (fixed in `9a12b6c`)
- `sync-collection` token non-advancing race under concurrent writes (fixed in `ae7d895`)
- `contactctl export --format dir` hardlink/TOCTOU clobber in attacker-controlled output directory (fixed in `1315505`)
- `contactctl import --dry-run` advisory/non-snapshot semantics documented in help (fixed in `cdd08dd`)
- `REPORT address-data` bytes now use raw vCard bytes to match advertised `getetag` (fixed in `fcbb843`)
- `sync-collection` delta per-href collapse (duplicates / contradictory states) (fixed in `d97e4c8`)
- Full `sync-collection` bootstrap includes live cards after journal prune (fixed in `213697e`)
- `sync-collection` continuation pages remain valid across prune (fixed in `fe65dde`)
- `REPORT` XML namespace enforcement (fixed in `bfb28e8`)
- `REPORT addressbook-multiget` target ownership/collection binding (fixed in `509b5db`)
- `addressbook-multiget` href cap + dedupe (fixed in `bc694e9`)

## Notes

- `TODO` currently contains several duplicates of the same multiget / namespace / control-character issues.
- The "attribute-heavy XML" note is an observation, not a validated bug by itself.

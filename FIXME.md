# FIXME (Validated From `TODO`)

This file is a deduplicated, validated list of real open issues from `TODO`.
Items already fixed are listed at the bottom so they can be removed from `TODO`.

## Open Issues

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

## Already Fixed (remove from `TODO`)

These findings were verified fixed in the current tree and should be deleted from `TODO`:

- Stale `If-Match` DELETE race (fixed in `f34f3ad`)
- Strong ETag mismatch vs GET bytes (fixed in `2a18321`)
- Import partial commit / non-atomic batch failure (fixed in `84d8c9d`)
- Directory import trailing garbage / multi-card single-file acceptance (fixed in `b46f873`)
- `PROPPATCH` namespace confusion / mixed-namespace structure acceptance (fixed in `56c045f`)
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
- REPORT multistatus response-size cap for query/multiget (fixes authenticated memory amplification path) (fixed in `ff8bb26`)
- `sync-collection` cursor cache global size caps/eviction (fixed in `96f5637`)
- `sync-collection` delta per-href collapse (duplicates / contradictory states) (fixed in `d97e4c8`)
- Full `sync-collection` bootstrap includes live cards after journal prune (fixed in `213697e`)
- `sync-collection` continuation pages remain valid across prune (fixed in `fe65dde`)
- Large full-sync pagination continuation remains cached beyond legacy threshold (fixed in `cdfbb45`)
- `REPORT` XML namespace enforcement (fixed in `bfb28e8`)
- `REPORT addressbook-multiget` target ownership/collection binding (fixed in `509b5db`)
- `addressbook-multiget` href cap + dedupe (fixed in `bc694e9`)

## Notes

- `TODO` currently contains several duplicates of the same multiget / namespace / control-character issues.
- The "attribute-heavy XML" note is an observation, not a validated bug by itself.

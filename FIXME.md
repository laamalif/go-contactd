# FIXME (Validated From `TODO`)

This file is a deduplicated, validated list of real open issues from `TODO`.
Items already fixed are listed at the bottom so they can be removed from `TODO`.

## Open Issues

### FIXME-002 (P1) `REPORT addressbook-multiget` is not bound to request-target ownership/collection

- Status: validated (code inspection; TODO contains multiple duplicate repro variants)
- Impact:
  - A `REPORT` to `/alice/contacts/` can request hrefs from another collection under the same principal (including traversal-normalized hrefs) and receive data from that other collection.
  - A `REPORT` sent to a cross-tenant or nonexistent target collection path (for example `/bob/contacts/` or `/doesnotexist/x/`) can still return caller-owned card data if the body `href`s point at caller-owned resources.
  - This also combines with traversal-style body hrefs (for example `/alice/contacts/../private/p.vcf`) to bypass collection boundaries while returning normalized leaked hrefs.
  - When combined with `FIXME-003` (namespace confusion), malformed-namespace multiget payloads can still exfiltrate same-principal cross-collection data and bypass target-path intent.
  - Method-level authorization behavior is inconsistent on the same target path (e.g. `addressbook-query` correctly returns `404`, while `addressbook-multiget` can return `207` + caller-owned data).
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
  - Relative hrefs in multiget request body are either rejected or normalized and still collection-bound (no bypass).

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
- Revalidated evidence (from TODO):
  - `X:addressbook-multiget` + `Y:href` payloads are accepted (`207`) and can drive multiget behavior.
  - Combined with `FIXME-002`, malformed-namespace payloads can return private same-principal cards from unrelated or invalid REPORT targets.
  - Namespace-less `<addressbook-multiget><href>...` payloads also drive the same behavior.

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

### FIXME-007 (P1) `addressbook-multiget` has no href count cap or dedupe (response amplification / DoS risk)

- Status: validated (code inspection)
- Impact: Large multiget requests with duplicate hrefs can cause large duplicate responses and expensive repeated backend lookups.
  - Strong authenticated amplification is possible (small request -> large response) and can drive CPU/DB/serialization load.
  - Handler builds the full multistatus response in memory before writing, increasing memory pressure risk under large href fan-out.
  - Under concurrent fan-out bursts, the daemon can be terminated (observed exit `137`, likely OOM kill), causing service outage.
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
- Revalidated evidence (from TODO):
  - Example amplification: `req_bytes=1915` -> `resp_bytes≈10.5MB` with duplicated hrefs and a large card.
  - Example large fan-out: `120,000` hrefs produced `207` with `resp_bytes≈53.5MB` and long processing time.
  - Concurrent fan-out test (3 requests x 12,000 duplicate hrefs on ~131 KiB card) caused daemon process death (`exit_status=137`) and `curl` empty replies.
  - Repeated campaign result: only 2 concurrent requests with 12,000 duplicate hrefs on a ~131 KiB card reproduced daemon death (`alive=0`, `exit=137`) and `curl` empty replies.

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

### FIXME-023 (P1) Background prune can invalidate freshly issued `sync-collection` continuation tokens mid-pagination

- Status: validated by TODO repro evidence; code inspection confirms mechanism in stale-token gap detection
- Impact:
  - A client can receive a continuation token from page 1 and then immediately get `403 valid-sync-token` on page 2 during normal operation if pruning runs between requests.
  - This causes pagination churn/full-resync loops despite correct client behavior.
- Affected code:
  - `internal/carddavx/sync.go` (`SyncCollection`, delta path gap detection)
  - `internal/daemon/daemon.go` (background prune ticker)
  - `internal/db/store.go` (prune by max revisions / age)
- Root cause:
  - Gap detection intentionally rejects tokens when intermediate revisions are missing.
  - Background pruning can remove those intermediate revisions after the server has already issued a continuation token.
- Revalidated evidence (from TODO):
  - With `--change-retention-max-revisions 3 --prune-interval 1s`:
    - page 1 returned `page1_token=urn:contactd:sync:1:12`
    - prune reduced journal to `post_prune_changes=3`
    - page 2 with that token returned `403` + `valid-sync-token`
- Suggested fix:
  - Prevent pruning from invalidating freshly issued pagination tokens within a practical sync window, or
  - move pagination to a stable snapshot/watermark model, or
  - document aggressive-prune behavior and recommend retention settings that exceed sync page completion windows (minimum mitigation).
- Tests to add:
  - page-1 continuation token remains valid across a prune tick (for chosen mitigation), or
  - explicit regression/docs test capturing current behavior if kept by design

### FIXME-019 (P1) `contactctl import` lacks bounded reads and stable file handles for untrusted file inputs (local CLI DoS / TOCTOU hang risk)

- Status: validated by code inspection (current-tree follow-up after symlink hardening)
- Impact:
  - `contactctl import` can consume excessive memory or hang on hostile local inputs because size checks occur after full reads (dir mode) or after streaming decode work (concat mode).
  - Directory import is vulnerable to file-type/content TOCTOU races (e.g. regular file swapped to FIFO after enumeration/check), which can hang the import run.
  - Directory import can also ingest tampered content that was not present at initial directory snapshot (regular-file content swap race).
  - This is a local/admin-path availability issue, not a remote HTTP issue.
- Affected code:
  - `internal/ctl/ctl.go` (`importFromDir`)
  - `internal/ctl/ctl.go` (`importFromConcatFile`)
- Root cause:
  - Directory mode reads entire file with `os.ReadFile(...)` before `validateImportedVCardSize(...)`.
  - Directory mode enumerates names and later re-opens by path; file type/content can change between checks and reads (no stable handle workflow).
  - Concat mode streams from `os.Open(...)` into `vcard.NewDecoder(...)` with no `io.LimitedReader` / byte cap for the input stream.
  - Symlink/non-regular rejection in dir mode (fixed in `89d03cf`) prevents `/dev/zero`-via-symlink there, but does not solve bounded-read behavior in general.
- Revalidated evidence (from TODO):
  - FIFO-swap race on a later file caused import hang/timeout (`import_rc=124`) after progress, demonstrating path-type TOCTOU on reopen.
  - Regular-file swap race changed imported content while import still exited success (`race_benign=0`, `race_evil=1`), demonstrating content TOCTOU.
- Suggested fix:
  - Directory mode:
    - open files via a stable handle, `fstat` the opened descriptor, and reject non-regular files on the descriptor
    - enforce size cap before full read (for regular files) and/or read via bounded reader
  - Concat mode: wrap input with a bounded reader (or enforce a total input cap / per-card cap during decode).
  - Consider rejecting direct non-regular concat import sources as well (similar to dir-mode source checks).
- Tests to add:
  - oversized regular `.vcf` in dir mode is rejected without reading entire file into memory (best-effort behavioral test)
  - dir import FIFO/file-type swap race does not hang (or is rejected deterministically)
  - dir import regular-file content swap cannot alter imported bytes after snapshot/open (best-effort deterministic harness)
  - concat import from non-regular source is rejected (or bounded) deterministically

### FIXME-020 (P1) `contactctl export --format dir` is vulnerable to hardlink/TOCTOU clobber in attacker-controlled output directory

- Status: validated operationally (TODO includes reproducible hardlink race); current symlink fix does not cover hardlinks/replace races
- Impact:
  - An attacker with write access to the export directory can race and plant a hardlink at a predictable export filename, causing export to overwrite an external file via shared inode.
  - An attacker can also replace a checked destination path with a FIFO between check and write, causing export to block/hang on write.
  - This is a local filesystem integrity risk for admin exports into untrusted directories.
- Affected code:
  - `internal/ctl/ctl.go` (`writeDirExport`)
- Root cause:
  - Export writes directly to `out/<href>.vcf` with `os.WriteFile(...)` (in-place truncate/write).
  - Current check rejects symlinks/non-regular files, but a hardlink is a regular file and passes the check.
  - There is also a TOCTOU window between path check and `WriteFile`.
- Revalidated evidence (from TODO):
  - Predictable last-filename race planted `out/zzzzz.vcf -> target.txt` (hardlink) before final write.
  - Export succeeded and external target was overwritten (`target_head=BEGIN:VCARD`, `target_nlink=2`).
  - FIFO replacement race on a predictable later filename caused export hang/timeout (`export_rc=124`), leaving a FIFO in the output dir.
- Suggested fix:
  - Avoid in-place writes to final path in directory export.
  - Write to a temp file in the export directory and `rename` into place (rename replaces the directory entry, avoiding hardlink-target clobber).
  - Revalidate destination path semantics around rename if overwrite behavior is retained.
  - Consider an option to refuse overwriting existing files entirely (safer backup mode).
- Tests to add:
  - hardlink destination race/clobber regression (best-effort deterministic harness or unit-level helper test)
  - FIFO replacement race does not hang export (best-effort deterministic harness)
  - normal dir export still works with temp+rename strategy

### FIXME-022 (P2) `contactctl import --dry-run` is non-snapshot and can diverge materially from immediate real import under concurrent writers

- Status: validated conceptually and by TODO repro evidence; current-tree root cause corrected
- Impact:
  - `--dry-run` create/update counts (or success/failure expectation) can be wrong when the target addressbook changes concurrently between dry-run classification and the subsequent real import run.
  - Operators may treat dry-run as a reliable preview, but it is advisory only under concurrency.
- Affected code:
  - `internal/ctl/ctl.go` (`applyImportedBatch`, dry-run path; `putOrClassifyImportedCard`)
- Root cause (current tree):
  - Dry-run classification uses live point-in-time reads (`GetCard`, `ListCards`) without snapshot isolation/locking across the batch.
  - Real import is batch-atomic (`PutCardsAtomic`) now, so prior reports citing per-file commits are stale; divergence is due to concurrent mutation between runs, not partial writes by import itself.
- Revalidated evidence (from TODO):
  - Dry-run predicted `created=12001 updated=0`; concurrent writer changed outcome to either:
    - import failure (`UNIQUE ... uid`) while external concurrent write persisted, or
    - count drift (`created=12000 updated=1`) without import failure.
- Suggested fix:
  - At minimum, document `--dry-run` as advisory and non-snapshot under concurrent writers.
  - Optional hardening:
    - add a snapshot/lock mode for dry-run + import planning, or
    - compare and report drift risk (best effort) before commit.
- Tests to add:
  - regression demonstrating dry-run drift under concurrent writer (if deterministic harness is maintainable)
  - documentation/help test clarifying advisory semantics (if wording is codified)

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
- `Store.PragmaString` / `Store.PragmaInt` PRAGMA-name validation (fixed in `48d0b25`)
- `contactctl import` honors `CONTACTD_VCARD_MAX_BYTES` (fixed in `7344b19`)
- `contactctl export --format concat` seam normalization (fixed in `9a12b6c`)
- `sync-collection` delta per-href collapse (duplicates / contradictory states) (fixed in `d97e4c8`)
- Full `sync-collection` bootstrap includes live cards after journal prune (fixed in `213697e`)

## Notes

- `TODO` currently contains several duplicates of the same multiget / namespace / control-character issues.
- The "attribute-heavy XML" note is an observation, not a validated bug by itself.

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
- Unauthenticated protected-path auth challenge before path validation (`401` vs `400` oracle) (fixed in `9e00528`)
- Control-character rejection in path segments (fixed in `b4c80fd`)
- Strict trailing XML content rejection for `REPORT` / `PROPPATCH` (fixed in `0b2c940`)
- `Store.PragmaString` / `Store.PragmaInt` PRAGMA-name validation (fixed in `48d0b25`)
- `contactctl import` honors `CONTACTD_VCARD_MAX_BYTES` (fixed in `7344b19`)
- `contactctl export --format concat` seam normalization (fixed in `9a12b6c`)
- `sync-collection` token non-advancing race under concurrent writes (fixed in `ae7d895`)
- `sync-collection` delta per-href collapse (duplicates / contradictory states) (fixed in `d97e4c8`)
- Full `sync-collection` bootstrap includes live cards after journal prune (fixed in `213697e`)
- `sync-collection` continuation pages remain valid across prune (fixed in `fe65dde`)
- `REPORT` XML namespace enforcement (fixed in `bfb28e8`)
- `REPORT addressbook-multiget` target ownership/collection binding (fixed in `509b5db`)
- `addressbook-multiget` href cap + dedupe (fixed in `bc694e9`)

## Notes

- `TODO` currently contains several duplicates of the same multiget / namespace / control-character issues.
- The "attribute-heavy XML" note is an observation, not a validated bug by itself.

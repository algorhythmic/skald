# Codex session coverage and resource verification

Implemented and deployed on 2026-10-22 Pacific time, using three Sol
implementation agents, cross-review and integration checks. The deployment began
at 2026-10-23 01:12:19 UTC. Native sources were read only; personal transcript
bodies were neither printed nor copied into fixtures.

## Changes

- The selected capture corpus supports up to 4,096 enrolled streams. Detailed
  source status stays bounded at 256 rows, with explicit omission counts. Missing
  native files retain their registrations and history. Discovery capacity errors
  remain visible through later probe limits and per-file rejections.
- Native Codex fork metadata accepts only explicitly evidenced, ordered ancestor
  IDs. The child retains its conversation, project and version; copied headers
  keep inherited metadata separately. A distinct fork adapter emits parent
  relations even when resuming an old checkpoint after the first header. The
  prefix remains bounded to 256 records / 4 MiB and 16 ancestor edges.
- Schema 7 adds a namespace covering index. Scoped version counts use indexed
  lookups; complete historical namespace scope permits SQLite's direct count.
  Every call checks scope against all persisted namespaces, including retired
  history. No mutable counters or public record/checkpoint/delivery schema change
  was introduced.
- Migration checks disk headroom and the configured database page budget before
  creating the index. Capacity failures roll back without changing schema 6;
  a verified recovery archive is retained before migration.
- The TUI can load 41 bounded session pages per refresh, enough for the expanded
  capture corpus, and retains cursors for additional archived history.

## Verification

`make check smoke tui-smoke discovery-smoke mcp-smoke` passed, including Go tests,
schema and manifest checks, vet and compiled Unix-socket/stdio acceptance.
`go test -race ./...` passed. Archive and capture race tests passed again after
the final migration and recovery safeguards. Documentation links and
`git diff --check` passed. Socket smoke checks ran outside the sandbox because
the sandbox prohibits binding temporary Unix sockets.

Synthetic tests cover expanded capacity and restart, stable identities after
source loss, bounded diagnostics, atomic overflow rejection, fork ancestry and
conflicts, old-checkpoint recovery, namespace retirement/redaction/restore,
query plans, recovery-copy preservation and failed migration retry.

The reproducible count benchmark uses 187,373 record versions, 100,000 artifacts
and four namespaces. Run `go test ./internal/archive -run '^$' -bench
'^BenchmarkScopedVersionCount$' -benchtime=1s -count=1` with the repository Go
toolchain and writable build/module caches.

| Query scope | Previous path | New path |
| --- | ---: | ---: |
| One namespace | 157.7 ms | 80.6 ms |
| All four namespaces | 659.8 ms | 16.6 ms |

A read-only native validation identified all 353 Codex files present at that
check and parsed 90,037 records. Of those files, 349 completed and four stopped
at `record_too_large`; none failed native identity verification. All committed
regressions use synthetic data.

## Live deployment and coverage

The installed and running daemon executable has SHA-256
`45bbd1812b3f8aa10960f4df4e61eed6e8922d60d425213fa00d3e4869a8ea0d`.
The original archive instance is unchanged. The pre-deployment archive had
187,909 record versions in 251 sessions. Final verification found 280,488 record
versions in 615 sessions, schema 7, zero queued deliveries and an empty daemon
error log.

As of 01:19:12 UTC, every one of the 357 current Codex rollout IDs, including
all 189 Odin rollout IDs, was represented in the archive. All historical
backfilling had finished. Four Codex streams retain captured prefixes and remain
visibly blocked at oversized records; 126 enrolled sources remain unavailable.
These states are not silently treated as complete captures.

The same four selected roots and their existing namespaces/cutoffs remain:

| Root profile | Enrolled | Configured capacity |
| --- | ---: | ---: |
| Development Claude | 37 | 128 |
| Codex | 473 | 768 |
| Devin | 14 | 32 |
| Odin Claude, including children | 91 | 256 |

The archive page limit increased from 4 GiB to 8 GiB to accommodate backfill and
future capture; this limit does not reserve that space or cap recovery copies.
Raw reads remain disabled.

Odin's dedicated Claude profile now grants its existing pilot namespace and the
configured Codex namespace to Skald MCP. A fresh connection using the saved
configuration read both scopes successfully and rejected an ungranted namespace.
Existing MCP processes retain their startup grants until reconnected; active
Odin sessions were not restarted to reload this configuration.

Private recovery and verification artifacts are under
`~/.local/share/skald/deployment-backups/session-coverage-20261023T011219Z/`.
They include the prior binary/configurations, deployment metadata, coverage and
MCP verification, and `resource-verification.json`. The verified
`pre-schema-7-*.sqlite` copy is in the main data directory. The prior executable
requires its matching schema-6 recovery archive for rollback.

## Resource sample after backfill

A 70.19-second sample beginning at 01:19:29 UTC covered periodic verification
and heartbeat work. It captured 99 new record versions; this was ongoing live
collection, not a zero-change idle benchmark. The process set had four idle MCP
servers and the daemon, with no TUI process present. The earlier 40-second
baseline had a TUI, two MCP servers and only 251 enrolled streams, so the two
samples are not a controlled measurement of the code change alone.

| Daemon metric | Earlier sample | After backfill |
| --- | ---: | ---: |
| CPU, percentage of one core | 46.1% | 27.5% |
| Logical reads | 391.6 MB/s | 89.5 MB/s |
| Physical reads | 0 | 0 |
| Accounted storage writes | 0.532 MB/s | 1.461 MB/s |
| Resident RAM at sample end | 50.5 MiB | 58.7 MiB |

After-backfill sampled peak daemon RSS was 62.8 MiB. Startup/backfill lifetime
high-water RSS was 587.8 MiB, and swap usage was zero. All Skald processes
together used 114.1 MiB RSS, or 84.7 MiB proportional memory accounting for shared
pages. Virtual address space is not resident physical allocation.

Allocated data-directory storage was 9.15 GiB: 3.28 GiB for the live archive,
5.70 GiB for migration recovery copies, plus WAL, spool and deployment files.
The new recovery copy accounts for about 2.02 GiB of that retained storage.
Older backups were retained. The queue was empty even though the spool database
retained allocated pages and receipt metadata.

CPU and page-cache reads remain substantial, and accounted writes increased
with the larger capture corpus and ongoing ingestion. These results establish
the coverage and query improvements; they do not claim the remaining background
resource issue is resolved.

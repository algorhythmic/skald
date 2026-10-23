# Background load, diagnostics and recovery compression — 2026-10-22

Three Sol agents implemented checkpoint snapshots, grouped heartbeats/empty-poll
suppression and compressed recovery files. The coordinating agent integrated
aggregate diagnostics and the official OpenTelemetry metrics SDK. A Sol review
checked the combined changes and fixes for hard-linked live database aliases
and namespace-scoped heartbeat failures.

## Result and behavior

- A scan bulk-loads archive and spool checkpoints in two SQL statements instead
  of the previous five lookups per enrolled source. It refreshes every pass and
  invalidates a source before producer/sink writes. Destination reconciliation,
  pending delivery and external checkpoint changes remain authoritative.
- Unchanged due health timestamps commit together in one transaction. Health
  transitions still persist immediately. Failure rolls the whole heartbeat
  transaction back, retries later and reports only affected configured sources.
- Repeated empty capture envelopes skip durable delivery only after confirmed
  acceptance with no pending receipt. Initial checkpoints, activity/metadata
  changes, gaps and redaction checks remain effective, including hot Devin WALs.
- `metrics` exposes fixed aggregate counters/timings and process CPU, resident
  memory, Go heap/goroutines and logical/storage I/O over the private admin
  socket. `profile` produces bounded CPU/heap pprof files when explicitly enabled.
  Neither is exposed as an MCP tool. MCP remains a read adapter; native log
  collection and the capture spool do not require MCP.
- OTLP/HTTP metrics use the official SDK only when an explicit endpoint is
  configured. Export defaults off, including with only OTEL endpoint environment
  variables. The live configuration enables private profiling and leaves export
  off because no collector endpoint is configured. Exported metrics have no
  transcript text, source paths, native IDs or namespace labels.
- `backup-compress` creates zstd plus a versioned manifest, independently
  decompresses and verifies exact bytes, SQLite integrity, foreign keys and
  original content digests. It preserves schema and retains the original file.
  Ordinary `restore` accepts this pair. No automatic retention policy is enabled.

See [setup](ARCHIVE-SETUP.md#metrics-and-profiling) for configuration and commands.

## Measurements

Matched synthetic idle runs used 615 sources, the same Btrfs work directory,
70-second samples and no new records or change events. Before used the previously
installed binary; after used the new implementation.

| Measurement | Before | After |
| --- | ---: | ---: |
| CPU, percent of one core | 11.357% | 2.214% |
| Logical reads | 97.8 KB/s | 2.58 KB/s |
| Logical writes | 777.9 KB/s | 2.61 KB/s |
| Reported storage writes | 2.919 MB/s | 0.00878 MB/s |

CPU decreased about 80.5%; storage writes decreased about 99.7%. A focused
615-source checkpoint lookup benchmark measured 121.55 ms before and 15.45 ms
after, with allocations falling from 91,157 to 42,539. This measures the lookup
path, not end-to-end live throughput.

Live 70-second daemon-only samples excluded compression and profiling:

| Measurement | Before | After |
| --- | ---: | ---: |
| CPU, percent of one core | 31.809% | 29.342% |
| Logical reads | 104.0 MB/s | 123.2 MB/s |
| Logical writes | 0.460 MB/s | 0.199 MB/s |
| Reported storage writes | 2.169 MB/s | 0.611 MB/s |
| End-of-sample RSS | 46.79 MiB | 42.26 MiB |
| Peak sampled RSS | 65.81 MiB | 71.60 MiB |
| New record versions | 94 | 59 |

Physical reads were zero in both samples; these logical reads came from cache.
Live write throughput was about 72% lower. These are observations under different
active transcript workloads, so they do not establish a controlled CPU speedup.
The four blocked streams, 14 streams with capture gaps and 126 unavailable
sources remained visible. These source diagnostics predate this batch.

A separate 30-second live CPU profile sampled 5.69 CPU-seconds (19.0% of one
core). SHA256 consumed 37.6% of sampled CPU; `sessioncapture.Read` prefix hashing
accounted for 37.4% cumulatively and Devin canonical dumping for 36.2%.
The heap profile sampled 9.21 MiB of in-use Go allocations immediately after CPU
profiling; it includes profiler allocations and is not equivalent to process RSS
or a forced-GC steady-state heap. Remaining load calls for a measured native
continuation/dump strategy that retains rewrite detection, rather than increasing
polling delay without evidence.

## Deployment and verification

### Recovery storage

All four older raw backups have verified compressed copies; their original
schemas and every byte are preserved. Compression used one zstd thread and ran
after the resource comparison. Byte sizes below are apparent file lengths.

| Recovery copy | Raw bytes | Compressed bytes |
| --- | ---: | ---: |
| pre-schema-3 | 586,145,792 | 127,268,248 |
| pre-schema-4 | 586,289,152 | 127,286,772 |
| pre-schema-5 | 1,263,681,536 | 196,862,107 |
| pre-schema-6 | 1,509,888,000 | 224,262,316 |
| Total | 3,946,004,480 | 675,679,443 |

This represents an 82.9% reduction for those backup representations, or about
3.05 GiB net savings after retiring their raw originals. The latest
`pre-schema-7-d8fadfe4d762481188b474614dc299a3.sqlite` remains uncompressed at
2,172,215,296 bytes (2.02 GiB) for rapid rollback. The live database, native
transcripts and spool were not compressed or removed.

Automatic approval review rejected retirement of the four older raw originals
without explicit permission. All originals remain; their compressed copies
currently add about 644 MiB. Explicit approval has been requested for the final
retirement step. The private `backup-compression.json` records both digests,
schema identities, sizes and retention state for each copy.

At the post-compression snapshot, the data directory allocated 10,519,101,440
bytes (9.80 GiB): live database 3,596,996,608 bytes, raw recovery copies
6,118,207,488 bytes, compressed pairs 675,704,832 bytes, spool 46,637,056 bytes,
deployment recovery/diagnostics 76,001,280 bytes, plus SQLite sidecars. It was
9,872,031,744 bytes (9.19 GiB) in the earlier baseline. Keeping both backup
representations temporarily increases storage; savings are not yet realized.
The live 8 GiB database page cap is a growth limit, not preallocated storage,
and does not include recovery copies, spool or WAL. Active capture and SQLite
checkpointing make total allocation vary between samples.

### Live daemon

The installed binary SHA256 is
`a6f2d5ccc81febac71e4425341ab6e007a18e206ee1b09228034d4826833a4e0`.
Only the capture daemon was restarted. Configured roots/capacities and the archive
instance `6f14300b3b580847be55ffae69e4bb42` were retained; schema remains 7.
The initial post-restart verification retained at least 284,488 record versions
and 625 sessions with zero queued deliveries. New native sessions continue to be
discovered while the daemon runs. No agent parent or existing MCP process needed
restarting for these collection changes.

After 220 completed scans, local metrics recorded 4,982 suppressed empty polls,
2,493 health timestamps flushed in 22 grouped transactions, zero checkpoint
snapshot errors and zero heartbeat flush errors. The queue remained empty.
The capture-error counter includes known unavailable source files and should be
interpreted with the per-source health diagnostics.

Private recovery artifacts, previous executable/configuration, before/after
resource JSON, aggregate metrics and CPU/heap profiles are under:

`/home/david/.local/share/skald/deployment-backups/background-20261023T015501Z/`

`make check`, native/TUI/discovery/MCP smoke tests, diagnostics smoke with an
actual temporary OTLP protobuf receiver, and race tests for archive, daemon,
observability, CLI and client pass. The diagnostics smoke checks private bounded
profiles, no overwrite, metric export and exclusion of transcript identifiers.
The final elapsed-time instrumentation correction passed daemon/observability
tests, the full check target and diagnostics smoke. Socket/PTY/localhost smoke
tests required sandbox exceptions; an ordinary sandbox run of diagnostics smoke
was denied socket creation and passed with the required access. Whitespace
checks pass. No repository commit was created.

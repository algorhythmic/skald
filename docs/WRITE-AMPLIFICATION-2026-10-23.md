# Capture write amplification — 2026-10-23

This batch reduces transaction and mutable-index churn in capture. It follows
the schema-8 storage work without changing archive schema, spool schema, wire
contracts, source selection, polling interval, or SQLite FULL synchronization.
It does not publish a release or deploy monitoring services.

## Changes

Small captured deliveries can share the three durable stages: spool enqueue,
archive acceptance, and spool acknowledgement/pruning. Each group contains at
most 16 envelopes and 1 MiB of serialized envelope bytes. A scan checks the
25 ms staging age target between source captures and flushes at scan end and
before an oversized immediate delivery. This is a cooperative age target:
one bounded native capture can take longer. Larger supported deliveries retain
the immediate scalar path. Single-source capture still makes its evidence
durable before a later checkpoint can advance.

Staging never marks a source captured or publishes a verified-file cache entry.
The original source stamp is checked again before caching after acknowledgement.
Cancellation discards candidates that have not reached durable enqueue. Queued
envelopes survive uncertain archive responses and failed receipt transactions.
Only each source's oldest unacknowledged envelope joins a delivery group. Group
failure falls back to individual deliveries so one poisoned source does not
block unrelated sources; a source's later envelopes cannot overtake its head.

The archive accepts records, checkpoints, and exact retry receipts in one
transaction. Health caches advance after commit. Guards skip unchanged record
heads, checkpoints, capabilities, conversation locators, activity, and ambiguity
flags, using NULL-safe comparisons. Repeated attributable observations and
change-feed events remain distinct even when the current head is unchanged.
Registration no longer deactivates and reactivates retained locators.
Discovery enrollment also preserves health-cache observations for unaffected
streams; only registered or removed locators lose their cached health after the
registration commits. Useful
indexes remain in place; this batch requires no migration or migration backup.

Three aggregate diagnostic counters count attempted group flushes, their
candidate deliveries, and fallback events: `capture_group_flushes`,
`capture_group_batches`, and `capture_group_fallbacks`. These are not SQL commit
counters. No source identifiers or transcript data become metric labels.

## Verification and measurements

Focused tests cover group rollback, exact receipt retry after reopen, ordered
source chains, suppression, partial capacity, malformed acknowledgement fallback,
receipt transaction failure, cancellation, source-stamp changes, and byte/count
bounds. Tests also establish that unchanged mutable values do not issue updates
and that skipping a head update preserves new observation/change evidence.

The archive-only benchmark uses the same metadata guards for both paths and
compares 20 groups of 16 new deliveries. It measured 2,342,428 WAL bytes per
scalar group versus 513,354 grouped bytes: **78.1% fewer WAL bytes**. Acceptance
time was 31.3 versus 27.2 ms per group. Autocheckpoint was disabled only for this
disposable synthetic benchmark to measure WAL bytes directly; production settings
remain unchanged.

The integration workload in `scripts/benchmark_writes.py` seeds 512 records in
each of 16 synthetic source files, appends one small record per file for 20
rounds, and waits for committed archive counts plus an empty spool after every
round. It checks archive/producer/destination checkpoint parity and recovery
after restart. It does not read or modify the live archive. The default work
directory is the repository's `.cache` on Btrfs; `/tmp` is tmpfs on this machine
and cannot establish storage-write savings.

Run a matched comparison with:

```sh
python3 scripts/benchmark_writes.py --binary /absolute/baseline-skald --binary ./bin/skald --output /tmp/new-write-receipt.json
```

The receipt destination must not already exist. Linux `/proc` logical writes
include both WAL and checkpoint writes. Kernel-accounted storage writes also
depend on filesystem allocation and writeback; they are neither net archive
growth nor a direct measurement of SSD wear. Btrfs copy-on-write and frequent
FULL-synchronized transactions can amplify them. Synthetic results should not
be treated as a guaranteed reduction for a single active source or every live
workload.

## Final paired integration result

The final binaries were run sequentially with identical inputs on Btrfs after
the review settled. Both preserved all records and passed durable checkpoint
parity plus restart checks. Raw aggregate measurements and binary digests are
in the [benchmark receipt](WRITE-AMPLIFICATION-BENCHMARK-2026-10-23.json).

| 16 sources, 320 new records | Baseline | Candidate |
| --- | ---: | ---: |
| Native input | 88,440 B | 88,440 B |
| Logical writes | 107.77 MB | 67.33 MB |
| Kernel-accounted storage writes | 205.93 MB | 80.93 MB |
| Daemon CPU | 2.43 s | 1.67 s |
| Workload elapsed | 6.82 s | 4.90 s |
| Mean append-to-commit round | 340 ms | 244 ms |

This run reduced logical writes by **37.5%** and kernel-accounted storage writes
by **60.7%**. The candidate flushed 320 deliveries in 22 groups without fallback,
capture errors, checkpoint errors, or health-flush errors.

The single-source control has no cross-source batching opportunity. Its logical
writes were 6.28 versus 6.30 MB, kernel-accounted writes 9.52 versus 10.38 MB,
and CPU 0.27 s for both. It therefore establishes **no single-source write
improvement**. This control used the batching build before the enrollment-cache
follow-up; it does not enroll new sources during measurement. Batching benefits
depend on multiple small deliveries arriving
together; required new record/index evidence and filesystem write amplification
remain material. The 25 ms cooperative staging target trades a small amount of
capture latency for fewer durable transactions, without weakening commit rules.


## Enrollment health-cache follow-up

A live post-install sample exposed a second write burst: enrolling one new stream
cleared health observations for every stream, causing hundreds of individual
health commits on the next scan. Registration now invalidates only its committed
registered/removed keys. A 32-source regression verifies that enrolling one new
source leaves all 32 existing observations intact; their next unchanged scan
issues zero SQLite updates and zero WAL bytes. Their normally due heartbeats
still flush together. Full reload, relocation, retirement and failed enrollment
retain the required invalidation and rollback behavior. This follow-up was
independently reviewed and passed the archive suite plus focused race tests.

## Integrated verification

`make check` passed for the final code, including schema/manifest checks and vet.
Race tests passed for the modified spool, archive, daemon and observability
packages; archive bulk-bound and enrollment/heartbeat race regressions passed
again after their final changes. Native, TUI, discovery, MCP, diagnostics and
completion smoke checks passed. Native recovery and discovery smokes passed
again with the enrollment fix. Independent review found no remaining durability,
FIFO, cancellation or privacy issue. SQLite FULL synchronization remains enabled.

## Local deployment

The final working-tree build is installed with binary SHA-256
`df2d1e03144c57cf6a558817b8e47c418e37f59babb1571de1be866c6a4d5e4e`.
Only the capture daemon was restarted. Startup verification preserved the archive
identity and schema 8, then observed 679 sessions and 303,843 record versions with
zero queued deliveries. Thirteen candidate deliveries had passed through twelve
group flushes without fallback. Checkpoint-snapshot and health-flush error
counters were zero. These startup counters are not steady-state write measurements.

The source configuration is byte-for-byte unchanged. OTLP export is disabled and
backup status still reports `no off-device backup configured`, with L1 open.
The previous binaries/configurations and deployment receipts are private local
rollback material. No archive migration, backup retirement, monitoring-service
installation, public publication or Odin mutation occurred in this batch.
The existing local prerelease tag remains unchanged; this is its untagged working
successor. The live sample that uncovered the enrollment issue used the earlier
batching build and is not presented as a final-build performance claim.

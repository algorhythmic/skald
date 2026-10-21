# Idle polling CPU and disk investigation

The reported background load is reproducible. A read-only 20-second sample of
the installed daemon measured 43.9% of one CPU core, 292.0 MB/s of logical reads,
0.565 MB/s of logical writes and 6.24 MB/s of accounted storage writes. Physical
read bytes were zero: the repeated reads hit the page cache. During the same
sample, both archived record versions and the change boundary stayed unchanged.

This process was the detached daemon started by the TUI, not a systemd user
service. It had run for about four and a half days. The running executable matched
the installed binary (SHA-256
`8e8f7d6c54efec7e98a8522ca9f6615b8e27dc3d22c1147d4f632aa6cc6e5d45`),
predated the producer-batch build, and served archive schema 5 without a capture
spool. Its version banner incorrectly said schema 4; live status reported 5.
No VCS revision was embedded in that binary. The working-tree baseline for the
synthetic comparison was `cdee839` (schema 6). Both versions had the relevant
polling paths.

Only aggregate status, configuration, process counters and archive metadata were
inspected. No personal transcript bodies were read into this investigation or
copied into fixtures. The initial investigation left the installed executable,
live daemon, configuration and archive unchanged. The subsequently requested
deployment is recorded below.

## Causes

The configuration polls every second and has 174 enrolled streams. Its health
counts were 137 available, 7 blocked, 9 unavailable and 21 with capture gaps.

1. `Collector.Scan` called `Store.SetHealth` for every unchanged source on every
   poll. `SetHealth` always opened a write transaction and updated `checked_at`,
   even when health was unchanged. SQLite uses FULL synchronous WAL commits.
   These many small transactions generated disk traffic without new records or
   change-feed events. A synthetic test on this machine's Btrfs filesystem
   reproduced 11.97 MB/s of accounted storage writes from 1.09 MB/s of logical
   writes while all sources were unchanged.
2. The verification cache excluded blocked and partial files. The seven blocked
   Codex streams had 87,190,937 bytes of already captured prefixes and seven
   `record_too_large` gaps. Each retry hashed the committed prefix, encountered
   the same oversized line, then hashed the prefix again. The unchanged blocked
   tails made no progress but repeatedly consumed CPU and memory bandwidth.

These are confirmed sources of unnecessary Skald load. The live sample does not
isolate every contributor to Codex UI latency or all system-wide I/O pressure.

## Local fixes and evidence

Unchanged health observations now update an in-memory timestamp. The durable
heartbeat is written at most once a minute; health transitions and capture
transactions still persist immediately. Live status retains the latest check
time, while a crash/restore can expose the older durable heartbeat. Record age
does not change with these checks.

Stable blocked and partial tails now reuse their verified result, preserving
their health and error. Every poll still checks inode/device, size, mtime and
ctime. Appends, repairs and replacements invalidate the cache immediately, and
one-minute full-prefix audits remain. Hot Devin WALs retain their separate
activity/quiet handling. No oversized record is skipped or silently accepted.

The reproducible harness is `scripts/benchmark_idle.py`. Measurements below
exclude initial capture and recorded zero new record versions and change events.
CPU percentages refer to one core; MB means 1,000,000 bytes. `/tmp` is tmpfs on
this machine, so disk comparisons explicitly use the project filesystem.

| Workload | Before | After |
| --- | --- | --- |
| 174 sources, 7 blocked tails after 8 MiB prefixes, 10 seconds on tmpfs: CPU | 28.6% | 2.7% |
| Same blocked workload: logical reads | 105.28 MB/s | approximately zero (14 B/s) |
| Same blocked workload: logical writes | 1.04 MB/s | approximately zero (14 B/s) |
| 174 unchanged small sources on Btrfs: accounted storage writes | 11.97 MB/s over 10 seconds | 0.81 MB/s over 70 seconds |

The longer after-test includes the retained one-minute audit and heartbeat,
including initial empty-batch receipt work at the first audit. It is not a claim
of zero lifetime writes. These are synthetic measurements on an active machine,
not a post-deployment measurement of the user's archive.

Example reproduction against separately built binaries:

```sh
python3 scripts/benchmark_idle.py --binary /path/to/skald \
  --seconds 10 --blocked 7 --prefix-mib 8
python3 scripts/benchmark_idle.py --binary /path/to/skald \
  --seconds 70 --work-dir .cache
```

`make check smoke tui-smoke discovery-smoke` and the archive/daemon race tests
pass. New regression tests cover no per-poll SQLite mutations, current live check
timestamps, durable heartbeat/transition persistence, conservative backup restore,
registration invalidation, cached blocked/partial diagnostics, periodic audits
and immediate recovery after a source repair.

## Local deployment — 2026-10-21

At 21:28:54 UTC, the tested binary was installed atomically and the detached
daemon was restarted gracefully. The existing TUI was left running. The running
daemon's executable matches the installed build, SHA-256
`701332211821b8ffb97ba86e81ad1071b67bf53aa0440890db04e4064ed08842`.
Startup completed in approximately 29 seconds, including the verified recovery
copy and schema 5→6 migration.

The archive instance stayed unchanged and all 126,421 record versions were
preserved. After the first scan, all 175 sources had their prior health counts:
137 available, 7 blocked, 10 unavailable and 21 with capture gaps. The standalone
delivery spool was enabled with zero pending envelopes. Configuration and native
provider histories were not changed.

Recovery artifacts under the live Skald data directory:

- `deployment-backups/performance-20261021T212854Z/skald.previous`: the original
  installed executable, with deployment metadata and a private daemon log beside it.
- `pre-schema-6-6b152d32049a35c2cc154af995cbaeb9.sqlite`: the verified 1,509,888,000-byte
  pre-upgrade archive copy, mode 0600.

The old executable requires its matching schema-5 archive for rollback; it
cannot use the upgraded archive directly.

### Live verification

A 70-second sample after the first scan, with the existing TUI still running,
covered the retained one-minute audit and heartbeat. No new record versions or
change events were produced during the sample. All source health counts stayed
unchanged and delivery backlog remained zero.

| Metric | Before deployment (20 seconds) | After deployment (70 seconds) |
| --- | --- | --- |
| CPU, percentage of one core | 43.9% | 35.5% |
| Logical reads | 292.0 MB/s | 265.4 MB/s |
| Logical writes | 0.565 MB/s | 0.084 MB/s |
| Accounted storage writes | 6.24 MB/s | 0.554 MB/s |
| Physical reads | 0 | 0 |

Accounted storage writes fell approximately 91%. CPU fell approximately 19%,
but substantial CPU and page-cache read activity remain. This deployment does
not resolve the entire live performance issue; the synthetic blocked-tail CPU
result does not explain the full multi-provider archive and TUI workload.
The remaining read activity needs separate profiling before attributing it to
a specific code path.

Verification also confirmed exactly one daemon, the original TUI still alive,
an empty startup error log, and schema 5 with the original archive instance in
the recovery copy. Aggregate measurements are saved beside the rollback binary
as `live-verification.json`.

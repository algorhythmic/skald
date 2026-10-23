# Running the Skald archive

Skald now has a foreground Linux daemon and CLI reads. It collects explicitly
configured Claude/Codex JSONL files, independently archives their original bytes,
and keeps working without Heimdall, Braid, Herdr or a model provider. The native
[TUI](TUI.md) browses this archive. [Source connection](SOURCES.md) enrolls primary
conversations and optional native Claude children within selected roots.
[Read-only MCP](AGENT-READS.md) exposes the archive to agents with explicit grants.

## Start with synthetic sources

Build and run from the project directory:

```sh
make build
./bin/skald serve --config examples/fixture-sources.json
```

The default archive is `$XDG_DATA_HOME/skald` (or `~/.local/share/skald`); the
socket is `$XDG_RUNTIME_DIR/skald/skald.sock`. In another terminal:

```sh
./bin/skald tui
./bin/skald status
./bin/skald capabilities
./bin/skald changes --limit 25
./bin/skald sessions
./bin/skald artifacts --conversation CONVERSATION_KEY
./bin/skald get --record RECORD_KEY --revision SOURCE_REVISION --adapter ADAPTER_VERSION
./bin/skald backup
```

Use the exact keys/revisions/adapter from the preceding result. Add `--limit` and
`--cursor` to page sessions or artifact references. Add `--namespace` to narrow
reads to one configured namespace. The ready message means the socket is ready;
`status` reports initial backfill and per-source progress separately.
The live `checked_at` reflects the latest successful source check. Unchanged
health heartbeats are persisted at most once a minute; state transitions and
capture commits persist immediately. After a crash or restore, the durable
check time can therefore be up to a minute older than the last live check.

For a disposable or separate instance, choose both paths explicitly:

```sh
./bin/skald serve --config examples/fixture-sources.json \
  --data-dir /tmp/skald-example/data --socket /tmp/skald-example/run/skald.sock
./bin/skald status --socket /tmp/skald-example/run/skald.sock
```

The archive and socket directories must be owned by the current user and private
(mode 0700); Skald creates them when missing. Existing public directories are
refused rather than silently changing their permissions. Archive files and sockets
use mode 0600. Separate process locks prevent a second archive writer or socket
owner. The socket also checks Linux peer credentials for the same user. Reads
and online backup requests use the socket; CLI clients never open the live database.

Stop the foreground daemon with Ctrl-C or SIGTERM. Collection and HTTP requests
finish/cancel before SQLite closes. Restart with the same configuration and paths.
A stale socket left by a forced stop is reconciled under its ownership lock.

## Configure real collection explicitly

Use the [connection commands](SOURCES.md) to preview and select provider roots.
`serve` defaults to the saved private source configuration when `--config` is omitted.
For explicit single-file registration, copy the example and replace its fixture entries.
Each entry requires:

- `namespace`: stable shared collection identity, also used by any independent
  consumer of the same native source. No namespace is invented from a title or cwd.
- `provider`: `claude_code`, `codex` or `devin`.
- `stream_id`: stable configured logical stream token. Keep it when the same
  stream moves or rotates; distinct native streams need distinct tokens.
- `root`: configured source directory, absolute or relative to the configuration
  file. `path` is one safe relative filename below that root. Nested symlinks may
  not escape the root; directories, devices and pipes cannot be transcripts.
- Optional `conversation_id`: an explicit native identity assertion for a source
  whose records omit it. Unrelated or contradictory native IDs block capture;
  [verified Codex ancestor headers](SOURCES.md#codex-cli-and-forked-rollouts) may
  retain their original parent IDs within the child's rollout.
- Optional `root_aliases`: previously registered absolute or configuration-relative
  roots when deliberately relocating an existing namespace.

Registration is transactional and persists the provider, root aliases, logical
stream and current locator. Reusing a namespace for another provider is refused.
Moving a root requires the new configuration to name a known root as an alias;
changing only a locator never changes native record keys. The parser independently
verifies the content prefix before preserving a stream generation.

Configuration is loaded on startup. Stop and restart to change selected roots/files,
limits or raw-read policy. Removing a source stops its polling and preserves
its archive. Read scope consists of configured namespaces: retaining an entry with
an unavailable native file permits browsing its archived history. Removing the
last configured entry for a namespace also removes it from the active read corpus.
There is no default scan of home directories, hook installation or source activation.

`sources --provider PROVIDER` previews native identity metadata. `connect` saves
a root for periodic enrollment of newly created primary conversations. Claude
roots can enable `--include-subagents`. `discover --root DIRECTORY` remains a
generic filename inventory. Other provider subagents and general fork/alias
discovery remain future work.

## Capture and reads

Each scan takes one bounded batch per source, so large backfills do not consume
an unbounded queue. Checkpoints, original content, normalized revisions, record
heads, source-order observations, gaps and change records commit together. Restart
and duplicate delivery do not create new native versions. A malformed complete
line is archived opaque with a retained gap; an incomplete final line waits for
its newline. Oversized records and conflicting identities block before that line.
Unchanged complete, partial and blocked files reuse their last verified result
between one-minute prefix audits. Every poll still checks file identity, size,
mtime and ctime, so an append, replacement or repair triggers immediate capture.
Blocked tails retain their diagnostics and do not silently skip the blocked data.

After discovery and queued-delivery retries, each scan loads archive checkpoints,
spool acknowledgements and pending counts in bulk. These snapshots last only for
that scan; they have no TTL and are rebuilt from durable state on the next pass.
Checkpoint seeding, enqueue and sink acceptance invalidate the affected source
before mutation, so later reads use current durable state. Destination mismatch
and replay checks still apply.

Successful checks update live `checked_at` immediately. Unchanged heartbeats that
reach the one-minute persistence interval are grouped into one transaction at scan
exit, including a bounded flush after cancellation. Health transitions and capture
commits remain immediately durable. A failed heartbeat batch rolls back together and reports
`archive_write_failed` for affected sources in scoped status, while existing
capture diagnostics take precedence. Restart and backup retain committed check
times.

Redundant empty polls do not enqueue another delivery when the checkpoint and
metadata match a successfully accepted observation. First checkpoint initialization,
gaps, pending/blocked/backfill changes and new active liveness timestamps still
require delivery; an inactive Devin WAL bookkeeping timestamp alone does not. Enrollment
and redaction checks still run before suppression. Pending envelopes retain their
own receipt requirements and are retried until acknowledged.

Source loss is an availability diagnostic. It does not delete history, fabricate
an end signal, or complete a task. Session lists project archived lifecycle or
record-shape evidence with explicit unknowns; this is not live presence. The TUI selects
labeled excerpts and native recaps across the whole archived conversation, with
explicit ordering ambiguity, unknown coverage and exact citations.
The displayed title is a bounded archived native title with its source reference,
native ID fallback and a flag for ambiguous ordering between streams. A later
source ordinal supersedes an earlier title in the same stream, independently of
wall-clock timestamp ordering.

Artifact references are ordered within an explicitly named stream/generation by
native ordinal, with generation epochs separating rewrites. Multiple streams are
grouped deterministically; their order does not assert cross-stream causality.
Original envelopes preserve their first observation. Later delivery/order evidence
is stored separately; source revisions and normalization versions are immutable.

Exact reads require record key, source revision and adapter version. Missing
records and oversized responses are distinct errors. Defaults expose normalized
content; original bytes additionally require `allow_raw: true` in the daemon
configuration and `get --raw` for that read. Raw bytes are base64 in JSON.
Terminal controls and bidi formatting are escaped in CLI output without changing
round-trip content. The [MCP interface](AGENT-READS.md) adds fixed namespace grants
and smaller response bounds to these daemon reads.

Pages bind archive instance, namespace scope, ordering and committed boundary.
Any intervening archive change returns `cursor_expired`, requiring a fresh first
page; it never silently mixes pages from different boundaries. The separate
[archive feed](ARCHIVE-FEED-V1.md) offers a scoped snapshot-plus-tail metadata
cursor for consumers. Session/artifact pages allow 1–100 items; transcript pages allow 1–25 display
records. HTTP responses have a 32 MiB byte limit.

Use `changes --cursor CURSOR` to continue the feed. Its `more` flag indicates
another page is available; keep `next_cursor` for the next poll even when `more`
is false. Feed events contain metadata and exact locators, not transcript text.

To remove a selected record from the archive, use its namespace and record key:

```sh
./bin/skald redact --namespace NAMESPACE --scope record --target RECORD_KEY
```

For a conversation use `--scope conversation --target CONVERSATION_KEY`; for a
whole namespace use `--scope source --target NAMESPACE`. A successful response
is an idempotent logical-removal receipt. Future capture of the target is
suppressed; native files and earlier backups are separate retention surfaces.

## Capacity and recovery

`max_database_bytes` defaults to 4 GiB and caps SQLite pages. WAL and backups have
additional disk cost. `min_free_bytes` defaults to 256 MiB; a pre-transaction
space check reserves that headroom plus estimated batch growth. Capacity failures
roll back the entire batch and pause that source visibly in `status`. Capture
retries on later scans. Existing reads remain available. Authorized content
removal creates a [redaction receipt](CONTENT-REMOVAL-V1.md) and suppresses
reingestion of the target.

The daemon first persists each capture in `spool/capture-spool.sqlite`, then
delivers it to the local archive. `status.capture_delivery` reports namespace-scoped
pending counts and bytes. The default pending limits are 1,024 envelopes and
256 MiB of encoded payloads; these do not cap lifetime receipt metadata or WAL.
An unavailable native file does not prevent queued delivery. An unconfigured
source remains queued until its matching registration is restored. If its whole
namespace is outside the configured read scope, its backlog is also hidden.

`backup` asks the daemon to create a private file under its `backups` directory.
It uses SQLite's online backup API, checks SQLite integrity, foreign keys and every
original content digest, syncs the result, then publishes without overwriting an
existing file. No live SQLite/WAL file is copied directly, and no Git operation is
performed. The backup includes source registration and checkpoint metadata; keep
your explicit configuration file with your recovery materials as well.

Closed recovery files can be compressed independently of the running collector:

```sh
./bin/skald backup-compress --backup /path/to/archive-BACKUP_ID.sqlite
# Or choose a new output path in an existing private directory:
./bin/skald backup-compress --backup /path/to/pre-schema-7-ID.sqlite \
  --destination /path/to/private-recovery/pre-schema-7-ID.sqlite.zst
```

This command requires the `zstd` executable on `PATH`. It streams compression
through a single compression thread, then independently decompresses the result
and checks its exact SHA256, SQLite integrity, foreign keys and original content
digests before publication. Verification preserves the original schema; it does
not migrate the recovery file or hold the collector's scan lock. Allow temporary
disk space for the compressed output and one expanded verification copy.

The output defaults to `BACKUP.zst`, with a companion `BACKUP.zst.manifest.json`.
The manifest records the original schema and archive instance, plus both files'
byte lengths and SHA256 digests. Keep the compressed file and manifest together.
Both are published as mode 0600 files without replacing existing paths, in a
private directory (mode 0700). An interrupted publication without its manifest
cannot be restored. Compression never deletes the original or applies retention.

Compression and ordinary SQLite restore require a closed, regular recovery file
with a single filesystem link and no SQLite sidecars. A source named
`archive.sqlite`, live archive aliases, symlinked paths and existing output paths
are refused. Use the file returned by `backup` or a closed `pre-schema-*.sqlite`
recovery copy. Compression and restore default to a 16 GiB expanded-file
limit; `--max-backup-bytes N` can lower this limit for either command.

Restore to a directory that does not exist:

```sh
./bin/skald restore --backup /path/to/archive-BACKUP_ID.sqlite \
  --data-dir /path/to/new-skald-data
# A compressed backup uses the same restore command and requires zstd:
./bin/skald restore --backup /path/to/archive-BACKUP_ID.sqlite.zst \
  --data-dir /path/to/another-new-skald-data
./bin/skald serve --config /path/to/sources.json \
  --data-dir /path/to/new-skald-data --socket /path/to/private-run/skald.sock
```

Restore verifies before and after the SQLite copy. It preserves source identities
and checkpoints, records the original archive instance, and assigns a new instance
ID so old cursors cannot be reused. It never changes a provider's files or replaces
an existing destination. Missing original transcripts remain browseable through
configured namespaces. Corrupt backups or unsupported schema versions are refused.
Compressed restore checks the manifest and both byte digests, expands into private
temporary staging with bounded output and a 64 MB decoder window limit, then uses
the same SQLite verification, copy and schema-upgrade workflow as an ordinary backup.
It removes temporary staging on completion or failure.
Schema-1 upgrades create a consistent recovery copy before applying schema 2.
The archive backup does not include the independent [capture spool](CAPTURE-SPOOL-V1.md).
The daemon drains pending envelopes before backup and refuses unresolved work
with `capture_spool_pending`. The same guard applies to redaction. Schema-5
upgrades retain a verified recovery copy before schema 6. The schema 6→7
upgrade likewise keeps a verified `pre-schema-7-*.sqlite` recovery copy before
adding the covering index used by scoped status counts. It preserves archive
identity, records and capture checkpoints; the recovery copy adds disk usage
outside `max_database_bytes`.

A fresh archive restore starts with a fresh spool, seeded from its committed
archive checkpoints. Never attach an old spool to a different archive instance;
the daemon refuses that pairing. To preserve pending work for disaster recovery,
stop the daemon and preserve the complete private data directory and configuration
as a consistent unit, including both databases and any SQLite sidecars.

## Metrics and profiling

Local aggregate diagnostics are available through the same private, same-user
daemon socket:

```sh
./bin/skald metrics
# Select a separate instance with --socket /path/to/private-run/skald.sock.
```

The JSON snapshot includes scan/capture counts and timings, checkpoint snapshot
loads, file-cache hits, health heartbeat transactions, skipped empty polls and
process CPU, memory and I/O measurements. Counters restart with the daemon.
`capture_errors` includes attempts to read known unavailable files;
`captured_records` counts accepted capture operations, including receipt replay,
rather than the net increase in archived versions.
Measurements contain no transcript bodies, source paths, native IDs or namespace
labels. These administrative diagnostics are separate from namespace-scoped
evidence reads and are not exposed through MCP.

There is no default exporter or additional network listener. To opt into OTLP/HTTP
metrics export, add a `telemetry` field to the source configuration and restart:

```json
{
  "telemetry": {
    "otlp_metrics_endpoint": "http://192.168.86.21:4318/v1/metrics",
    "service_instance_id": "workstation-01",
    "export_interval_seconds": 30
  }
}
```

This is a future homelab configuration example, not the current live endpoint.
Retain the existing version, sources and other settings when deliberately enabling
export. An empty or omitted endpoint keeps export disabled, including when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set. The explicit endpoint must use HTTP or HTTPS
without embedded credentials, a query or a fragment. The interval defaults to
30 seconds and accepts 10–3,600 seconds. Export requires an explicit nonsecret
`service_instance_id` alias such as `workstation-01` (1–64 letters, digits,
underscores or hyphens, starting with a letter or digit). No hostname, source
path or machine identifier is generated for this alias.

Profiling is disabled by default. To enable local CPU and heap profiles, set
`telemetry.enable_profiling` to `true` and restart; this does not require an OTLP
endpoint. Then write a new profile file:

```sh
./bin/skald profile --kind cpu --seconds 10 --output /path/to/new-cpu.pprof
./bin/skald profile --kind heap --output /path/to/new-heap.pprof
```

CPU profiling defaults to 10 seconds, accepts 1–30 seconds and allows one active
CPU profile at a time. Heap profiles do not take `--seconds`. Output is bounded
to 32 MiB and published as a mode 0600 file without overwriting an existing path.
Both commands also accept `--socket` for another private daemon instance.

## Verification and current limits

`make check` runs unit, contract, SQLite and API checks without opening a socket.
`make smoke` runs the compiled daemon on a real private Unix socket with temporary
synthetic files: two providers, a partial tail, SIGKILL/restart, duplicate-free
capture, source removal, exact original reads, backup and fresh restore. These
checks do not open personal transcripts or leave a running daemon behind.

`python3 scripts/benchmark_archive.py` measures 100 synthetic sessions / 20,000
small records, including CLI overhead in list timings. Full-prefix hashing still
runs before and after each batch. Large native transcripts need a measured strategy
before claiming production tailing latency or completing L1's performance gates.
Provider compatibility remains fixture evidence, separate from live installed-app
verification. The current app supports Linux; packaging and user units are pending.


## Optional Braid context retrieval

Retrieval is disabled by default. Add a `retrieval` object to the existing daemon
configuration and restart to opt into a local, checksum-pinned Braid executable:

```json
{
  "retrieval": {
    "executable": "/absolute/path/to/braid",
    "sha256": "REPLACE_WITH_EXECUTABLE_SHA256",
    "directory": "/absolute/private/skald-retrieval",
    "timeout_ms": 10000,
    "max_nodes": 100000,
    "max_snapshot_bytes": 134217728,
    "max_scopes": 1,
    "max_cache_bytes": 268435456
  }
}
```

This is a configuration fragment; retain `version`, configured sources and other
settings. Use the actual executable digest, an existing private directory and a
Braid executable supporting the negotiated version-1 stdio protocol. The example
digest is deliberately not a usable pin. The dataset is scoped to explicitly
permitted namespaces and current user/assistant heads. System/developer messages
and historical revisions are excluded. Corpus byte/node budgets fail closed;
they do not silently truncate the corpus. Scoped cached datasets are checked
against current archive revisions before evidence is returned. An unavailable or
stale backend returns no context. Budget cache space separately from the archive,
WAL, spool and recovery files.

The final context budget includes exact citations as well as text. Skald keeps the
ranked prefix of complete items that fits; it does not truncate evidence text.
Unchanged scoped corpora reuse their publication. A new captured message or
correction in a selected namespace rebuilds the whole scoped publication on the
next query, so active capture can make successive queries pay the indexing cost.
A local active-corpus observation took about 27–28 seconds per build and used
about 91 MB of retrieval cache; this is an observation, not a latency guarantee.
Unrelated namespaces do not invalidate the publication. Append-only changes
during a query do not invalidate already captured current-head citations.

```sh
./bin/skald context --text "archive recovery" --namespace NAMESPACE --max-bytes 16384
./bin/skald descriptions --conversation CONVERSATION_KEY --namespace NAMESPACE
```

`context` provides lexical retrieval with exact citations and a bounded context
JSON budget (2–65,536 bytes). Descriptions work without Braid: they use current
whole-archive metadata, preserve up to four recap projections and cap display text
at 4 KiB per item. Neither operation calls a model or treats captured statements
as accepted task state. See [agent reads](AGENT-READS.md) for the two corresponding
read-only MCP tools. A TUI search screen and full L3 acceptance remain separate.

## Payload compression and physical reclamation

Schema 8 introduces lossless compressed storage while preserving exact public
record envelopes and original bytes. Upgrading metadata does not automatically
rewrite every retained payload. Compress bounded batches through the daemon:

```sh
./bin/skald compress --max-rows 100 --max-input-bytes 4194304
```

Each request accepts at most 1,000 rows and 64 MiB of input. Compression frees
SQLite pages for reuse; it does not promise to shrink the database file. It
preserves archive identity, source revisions and capture checkpoints. Read
integrity checks decode and verify retained payloads before returning evidence.

To reclaim free pages from the physical database file, stop the daemon and preview
an exclusive offline compaction, then explicitly apply:

```sh
./bin/skald archive-compact --data-dir /absolute/private/skald
./bin/skald archive-compact --data-dir /absolute/private/skald --apply
```

Both require the sole-writer lock. Match `--max-database-bytes` to the configured
capacity when it exceeds the 4 GiB default; `--min-free-bytes` controls reserved
filesystem headroom. `--apply` performs SQLite `VACUUM` after space checks. Allow
temporary copies and preserve verified recovery files before maintenance.
Compaction never deletes recovery files, source transcripts or the capture spool.

[Recovery operations](recovery-maintenance.md) document allocated-block accounting,
verified backup jobs to an independent destination, fresh-restore checks and
retention previews. `recovery-retain` requires explicit `--apply` to retire only
verified redundant raw rollback files; it preserves the latest raw rollback and
compressed recovery copies. `examples/maintenance` contains scheduler templates.
No independent or off-machine backup destination is configured by these examples,
and off-machine recovery acceptance remains unclaimed.

## Homelab monitoring drafts

The live workstation exporter endpoint remains unset. No monitoring service is
installed on the workstation, and this batch does not deploy anything to the
homelab. `examples/monitoring` contains drafts for an OpenTelemetry Collector
Contrib receiver on `192.168.86.21:4318`, forwarding metrics to the existing
Prometheus remote-write receiver at `http://127.0.0.1:9090/api/v1/write` on that
host. Prometheus must have `--web.enable-remote-write-receiver` enabled.

The proposed future workstation fragment is:

```json
{
  "telemetry": {
    "otlp_metrics_endpoint": "http://192.168.86.21:4318/v1/metrics",
    "service_instance_id": "workstation-01",
    "export_interval_seconds": 30
  }
}
```

The instance alias is explicitly chosen and nonsecret. The SDK resource contains
`service.name` and `service.instance.id`; the collector draft preserves those
attributes and drops future extras before converting resources into labels. It
does not include transcript text, hostnames, source paths or native IDs.
The collector draft bounds its queue and memory. Local alert rules and graph links
are provided without any outbound notification destination. Prometheus retention,
existing data, firewall access and the collector installation remain the homelab
operator's responsibility. These drafts do not establish remote monitoring
acceptance or configure an off-device backup.

## Backup status and independent recovery

The optional daemon `backup` configuration has no default destination:

```json
{
  "backup": {
    "destination": "/absolute/private/off-device-recovery",
    "off_device": true
  }
}
```

This fragment declares operator intent; it does not prove independence. A
different directory or partition on the archive's physical disk does not count.
Skald checks filesystem device identity, physical backing-device ancestry and
known remote filesystem mounts. Unknown topology, a missing destination or
shared physical backing remains explicitly unverified. No destination is
configured by this documentation or by the scheduler templates.

Successful `recovery-job --destination-dir PATH --off-device --apply` exports a
compressed backup, validates a fresh restore and saves a private durable receipt
in the archive data directory. The declaration still needs independent-device
proof before it counts as an off-device backup. Status reports
`no off-device backup configured` when unset, plus historical timestamps and ages
for the last matching archive's off-device backup and verified restore when
known. A successful local restore can update the restore timestamp without
establishing off-device protection. Historical receipts remain distinct from
verification of the currently configured destination. Device identities and
paths remain in the private receipt and are never exported as telemetry labels.
The broader L1 recovery gate remains open until its separate evidence is recorded.

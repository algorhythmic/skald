# Running the Skald archive

Skald now has a foreground Linux daemon and CLI reads. It collects explicitly
configured Claude/Codex JSONL files, independently archives their original bytes,
and keeps working without Heimdall, Braid, Herdr or a model provider. The native
[TUI](TUI.md) browses this archive. [Source connection](SOURCES.md) enrolls primary
conversations within selected roots; subagent discovery and agent-facing MCP
remain later work.

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
./bin/skald sessions
./bin/skald artifacts --conversation CONVERSATION_KEY
./bin/skald get --record RECORD_KEY --revision SOURCE_REVISION --adapter ADAPTER_VERSION
./bin/skald backup
```

Use the exact keys/revisions/adapter from the preceding result. Add `--limit` and
`--cursor` to page sessions or artifact references. Add `--namespace` to narrow
reads to one configured namespace. The ready message means the socket is ready;
`status` reports initial backfill and per-source progress separately.

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
owner. The socket also checks Linux peer credentials for the same user. All reads
and backup commands use the socket; CLI clients never open the live database.

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
- `provider`: `claude_code` or `codex`.
- `stream_id`: stable configured logical stream token. Keep it when the same
  stream moves or rotates; distinct native streams need distinct tokens.
- `root`: configured source directory, absolute or relative to the configuration
  file. `path` is one safe relative filename below that root. Nested symlinks may
  not escape the root; directories, devices and pipes cannot be transcripts.
- Optional `conversation_id`: an explicit native identity assertion for a source
  whose records omit it. Any exposed conflicting native ID blocks capture.
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
a root for periodic enrollment of newly created primary conversations. `discover
--root DIRECTORY` remains a generic filename inventory. Verified native subagent/
fork/alias discovery remains future work.

## Capture and reads

Each scan takes one bounded batch per source, so large backfills do not consume
an unbounded queue. Checkpoints, original content, normalized revisions, record
heads, source-order observations, gaps and change records commit together. Restart
and duplicate delivery do not create new native versions. A malformed complete
line is archived opaque with a retained gap; an incomplete final line waits for
its newline. Oversized records and conflicting identities block before that line.

Source loss is an availability diagnostic. It does not delete history, fabricate
an end signal, or complete a task. Session lists currently report activity as
`unknown`; evidence-based live activity remains future work. The TUI selects
labeled excerpts and native recaps within its bounded recent-record window.
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
round-trip content. These are local CLI interfaces, not the future scoped MCP grants.

Pages bind archive instance, namespace scope, ordering and committed boundary.
Any intervening archive change returns `cursor_expired`, requiring a fresh first
page; it never silently mixes pages from different boundaries. Retained historical
snapshot pagination and a public change feed remain later work. Session/artifact pages allow 1–100 items; transcript pages allow 1–25 display
records. HTTP responses have a 32 MiB byte limit.

## Capacity and recovery

`max_database_bytes` defaults to 4 GiB and caps SQLite pages. WAL and backups have
additional disk cost. `min_free_bytes` defaults to 256 MiB; a pre-transaction
space check reserves that headroom plus estimated batch growth. Capacity failures
roll back the entire batch and pause that source visibly in `status`. Capture
retries on later scans. Existing reads remain available. Retention/deletion commands
are not implemented; suppression tombstones are already respected by ingestion.

`backup` asks the daemon to create a private file under its `backups` directory.
It uses SQLite's online backup API, checks SQLite integrity, foreign keys and every
original content digest, syncs the result, then publishes without overwriting an
existing file. No live SQLite/WAL file is copied directly, and no Git operation is
performed. The backup includes source registration and checkpoint metadata; keep
your explicit configuration file with your recovery materials as well.

Restore to a directory that does not exist:

```sh
./bin/skald restore --backup /path/to/archive-BACKUP_ID.sqlite \
  --data-dir /path/to/new-skald-data
./bin/skald serve --config /path/to/sources.json \
  --data-dir /path/to/new-skald-data --socket /path/to/private-run/skald.sock
```

Restore verifies before and after the SQLite copy. It preserves source identities
and checkpoints, records the original archive instance, and assigns a new instance
ID so old cursors cannot be reused. It never changes a provider's files or replaces
an existing destination. Missing original transcripts remain browseable through
configured namespaces. Corrupt backups or unsupported schema versions are refused.
Schema-1 upgrades create a consistent recovery copy before applying schema 2.

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

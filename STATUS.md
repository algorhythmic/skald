# Skald implementation status

Updated 2026-10-23. **The Skald-owned OHS producer implementation batches are complete and verified locally.**
Skald can now enroll primary conversations and optional Claude children in selected roots and
Devin desktop session stores (`acp-messages` SQLite),
retain independent history, serve bounded reads, and back up/restore its archive.
The TUI browses project groups, bounded excerpts, native recaps and transcript
history through the socket API. Nine scoped MCP tools expose bounded reads to
agents. Optional pinned Braid lexical retrieval is implemented. Peer runtime
integration and the full release gates below remain open.

## Capture write amplification — 2026-10-23

Small captures now share bounded spool, archive and acknowledgement transactions
(up to 16 envelopes / 1 MiB), with a cooperative 25 ms staging age target.
Unchanged mutable projections skip updates. New-session enrollment invalidates
only affected health-cache entries, preventing archive-wide health-write bursts.
Exact receipts, per-source order,
suppression, atomic checkpoints and SQLite FULL synchronization are preserved.
Schema 8 and existing indexes remain unchanged; no migration backup is required.

The matched 16-source synthetic workload reduced logical writes from 107.77 to
67.33 MB (**37.5%**) and kernel-accounted storage writes from 205.93 to 80.93 MB
(**60.7%**), with checkpoint parity and restart recovery verified. The separate
archive benchmark reduced WAL bytes by 78.1%. The single-source control showed
no write improvement; these gains depend on concurrent small captures. See the
[write-amplification receipt](docs/WRITE-AMPLIFICATION-2026-10-23.md) for bounds,
tests, raw measurements and deployment disposition.

The final build is installed locally with archive identity and schema 8 preserved,
679 sessions, 303,843 record versions and zero queued deliveries at startup
verification. Checkpoint/health-flush errors and grouped fallbacks were zero.
Source configuration is unchanged; OTLP export remains off and L1 remains open.

## Resource and storage completion batch — 2026-10-22

Sol implementation agents and an independent Sol review completed schema-8
lossless payload compression, offline compaction, bounded recovery operations,
whole-archive descriptions, pinned Braid context retrieval, current provider
shape checks and operational metrics. Native ingestion remains independent of MCP.

The tested `0.2.0-rc.1` build is installed locally. Conversion reduced stored
payloads from 2.37 GB to 1.17 GB; exclusive compaction reclaimed 1.41 GB, reducing
the database from 3.78 GB to 2.37 GB. Capture resumed with the same archive
identity, all history retained and zero queued deliveries. Ongoing capture grows
the file again; compression and compaction are explicit maintenance operations.

Separate 70-second active-workload samples measured 25.4% versus 22.8% of one core
and 68.1 versus 60.2 MiB daemon RSS. Storage writes were 0.99 versus 1.15 MB/s;
this is not a matched-workload throughput improvement. Retained recovery copies
still allocate 6.69 GB, and the measured data directory totals 9.14 GB. Retrieval
index allocation and its child process are additional on-demand costs.

No monitoring service was installed. The collector/service/rule drafts target
`192.168.86.21`, forwarding to its existing Prometheus remote-write receiver.
The live endpoint remains unset; `service.instance.id` uses the nonsecret alias
`workstation-01`. Future endpoint and installation notes are in
[the monitoring drafts](examples/monitoring/README.md).

No backup destination is configured. Status reports **no off-device backup
configured**, unknown ages for the last off-device backup and last verified
restore, and `l1_complete: false`. Same-disk copies do not count as independent.
Recovery templates are not installed schedules and no retention deletion ran.

Whole-repository tests/vet, full race tests, native/TUI/discovery/MCP/diagnostics/
completion smoke checks, independent producer fixture checks and offline
collector/rule validation pass. The producer prerelease and Odin fixture pin are
prepared separately; neither establishes Odin runtime acceptance. See the
[integration receipt](docs/COMPLETION-IMPLEMENTATION-2026-10-22.md) for measured
limits and final publication disposition.

## Background load, metrics and recovery compression — 2026-10-22

The next Sol implementation batch adds per-scan bulk checkpoint reads, grouped
due health heartbeats, suppression of redundant empty deliveries, private
aggregate metrics and bounded CPU/heap profiling. The official OpenTelemetry
metrics SDK supports explicit opt-in OTLP/HTTP export; live export remains off
until a collector endpoint is configured. Native ingestion does not require MCP.

Matched 615-stream idle CPU fell from 11.36% to 2.21% of one core and reported
storage writes from 2.92 MB/s to 8.8 KB/s. Live write throughput fell from 2.17 to
0.61 MB/s in separate 70-second active-workload samples; live CPU remains
substantial at 29.3% of one core. Profiles identify prefix hashing and Devin dump
construction as the next measured optimization targets. Exact resource numbers,
workload limits, deployment and checks are in the
[background resource receipt](docs/BACKGROUND-RESOURCE-RECEIPT-2026-10-22.md).

Closed recovery backups can now be compressed to verified zstd/manifest pairs
and restored through the normal command. The tool retains originals and does
not enable automatic retention. See [archive setup](docs/ARCHIVE-SETUP.md).

## Codex session coverage and scoped-count performance — 2026-10-22

Three Sol implementation agents completed bounded enrollment scaling, native
Codex fork parsing and archive count optimization, followed by an independent
Sol review and integration checks.

- Capture supports up to 4,096 selected streams; detailed status remains bounded
  to 256 rows with omission counts. Missing sources retain archived identities,
  and capacity diagnostics survive later probe exhaustion or rejections.
- Codex forks use explicit, ordered `forked_from_id` evidence within a bounded
  prefix. Copied ancestor headers remain in the child's stream and cannot
  overwrite child project/version metadata. Parent relations survive recovery
  from checkpoints written by the previous adapter. General aliases and provider
  subagent compatibility remain outside this support.
- Schema 7 adds a namespace covering index. Scoped counts use indexed lookups;
  a count covering every persisted namespace uses SQLite's direct aggregate.
  Retired namespaces still constrain that fast path. Migration preserves a
  verified recovery copy and enforces disk headroom and archive capacity before
  index creation, rolling back budget failures.
- On 187,373 synthetic record versions, all-namespace counts improved from
  659.8 ms to 16.6 ms; a one-namespace count improved from 157.7 ms to 80.6 ms.
  These query benchmarks do not by themselves measure live daemon CPU.

Go tests, schema/manifest checks, vet, compiled native/TUI/discovery/MCP smoke
checks and full race tests pass. Archive and capture race tests passed again
after the recovery and migration safeguards. Source/checkpoint/delivery formats
remain version 1; original source histories are unchanged.

The tested build is deployed locally. All 357 current Codex rollout IDs,
including 189 Odin rollouts, were represented after backfill, with zero queued
deliveries. Four oversized-record streams remain visibly partial. The 70-second
resource sample measured 27.5% of one CPU core and 58.7 MiB daemon RSS; substantial
background load remains. See the [coverage and deployment receipt](docs/SESSION-COVERAGE-RECEIPT-2026-10-22.md)
for configuration, recovery artifacts, measurements and the MCP reconnect step.

## Native Claude children and agent reads — 2026-10-21

Two Sol implementation agents completed native Claude child capture and a scoped
read-only MCP adapter, with an independent Sol review and daemon integration.

- `include_subagents` is a per-Claude-root opt-in, also exposed by `sources` and
  `connect`. Native parent session ID, agent ID and sidechain evidence derive a
  separate child conversation identity. Records keep exact source bytes, a
  versioned child normalization and `native_subagent_of` relations. Primary
  streams continue rejecting sidechains. Existing delivery/checkpoint wire shapes
  and archive schema 6 are unchanged.
- Restart resumes persisted child streams. Disabling child capture retains
  history and stops polling it. Session rows identify children; source health and
  native timestamps are associated with the correct conversation and adapter.
- `skald mcp` provides seven bounded read tools through an existing daemon, with
  explicit namespace grants, revision citations and provenance on tool results.
  Default transcript context omits system/developer instructions. Raw reads need
  two opt-ins. No mutations, SQL, arbitrary paths or commands are exposed.
- Metadata-only local inspection found native Claude child shapes in 2.1.296 and
  2.1.303. Committed fixtures are synthetic. This evidence does not establish all
  capabilities in either provider release.

See [agent reads](docs/AGENT-READS.md), [source setup](docs/SOURCES.md) and the
[record identity contract](docs/CONTRACT-V1.md). General fork/alias tracking,
other provider subagents and ranked Braid retrieval remain separate capabilities.

Verification: `make check smoke tui-smoke discovery-smoke mcp-smoke`, full race
tests and a final capture/daemon race rerun pass. A metadata-only live inventory
identified 54 native children and eight primary Odin sessions; complete child
reads consumed 9,554 records with zero gaps, blocks or pending tails. The tested
binary is installed, child discovery is enabled for the dedicated Odin root,
and that Claude profile has a connected MCP registration granting only its
namespace. The deployment preserves schema 6 and the existing archive instance;
the prior binary/configuration and Claude configuration are backed up privately.
See the [deployment receipt](docs/AGENT-READS-RECEIPT-2026-10-21.md).

## Remaining Odin producer batches — 2026-10-21

An [idle polling performance investigation](docs/PERFORMANCE-RECEIPT-2026-10-21.md)
subsequently reproduced high CPU and disk writes with no new capture data. The
fix coalesces unchanged health heartbeats and caches stable blocked/partial
tails while retaining change detection and periodic audits. Synthetic blocked-tail
CPU fell from 28.6% to 2.7% of one core. The tested build was deployed locally on
2026-10-21, preserving all 126,421 record versions through the schema 5→6 upgrade
and retaining the old executable and a verified pre-upgrade archive copy.
Live verification over 70 seconds measured storage writes at 0.554 MB/s, down
from 6.24 MB/s (approximately 91%). CPU remained 35.5% of one core, down from
43.9%; substantial read activity and CPU require further profiling. Capture
backlog stayed empty and no records or change events were added during sampling.

Three Sol implementation agents completed the archive feed, durable delivery,
and capture/recovery batches, with integration and review in the standalone daemon.
This completes the selected Skald implementation scope from the
[producer plan](docs/ODIN-INTEGRATION-PLAN.md). It does not close the cross-repository
OHS exits, consumer rollout, or the broader L0–L6 release gates.

- Added public `archivefeed` v1 and `changes`/`capabilities` commands. The metadata
  feed provides a fixed snapshot followed by tail pages, exact revision locators,
  corrections, gaps, source removal and redaction. Cursors survive ordinary writes
  and same-scope restart; archive restore and scope revocation invalidate them.
- Added public `capturespool` with immutable persisted envelopes, atomic producer
  checkpoints, independent destination receipts, replay and visible scoped backlog.
  The daemon uses the spool for its standalone archive. Exact batch receipts prevent
  metadata-only batches from inheriting another batch's success. Archive instance
  binding and checkpoint reconciliation refuse mismatched recovery state.
- Upgraded the archive to schema 6 with atomic sink receipts, durable removal
  receipts and persisted read scope. A lost response can be retried after later
  batches and process restart without duplicating records or regressing progress.
  Upgrades preserve a verified pre-upgrade archive copy.
- Added explicit `redact` management for source, conversation and record scopes.
  Removal deletes archived content/projections, preserves unavailable-reference
  identity and suppression, and emits a feed withdrawal. Backup and redaction
  drain the spool under the collector lock, refusing unresolved pending work.
- Hardened identity probes and Devin source paths, rejected Claude sidechains in
  primary streams, and added Codex parent-evidence/unsupported-capability fixtures.
  A hot Devin WAL before the first capture now stores a valid deferred checkpoint
  and resumes once quiet. The default read batch is 512 records; synthetic parser
  backfill measured about 500 ms versus 887 ms at 256 records for 8,192 records.
- Added independently validated feed/removal schemas, shared synthetic fixtures,
  and a reproducible [contract manifest](docs/PRODUCER-CONTRACT-MANIFEST.json).
  The [producer candidate](docs/PRODUCER-RELEASE-CANDIDATE.md) records publication
  scope and remaining consumer/release evidence.

Verification: `make check smoke tui-smoke discovery-smoke`,
`go test -race ./...` with the project toolchain/caches, and `git diff --check`
pass. Coverage includes receipt failure rollback, independent sink failure,
metadata-only replay, removed-source backlog, archive/spool mismatch, source loss,
rewrite and reappearance, schema 5→6 recovery, redaction after restore, namespace
isolation, bounded feed snapshot/tail, and standalone terminal restoration.
Runtime checks used private synthetic data and required Unix socket/PTY sandbox
access. The compiled binary reports archive schema 6. These implementation checks
did not change live configuration or installed binaries. The later local daemon
deployment is recorded in the performance investigation linked above; provider
configuration, peer repositories and task authority remain unchanged.

The spool's 1,024-envelope/256 MiB limits bound pending payloads, not lifetime
SQLite metadata or WAL size. Removal is logical, not a physical erasure guarantee
for WAL, prior backups or downstream copies. Tagged producer publication, Odin
consumer pins/tests, historical overlap reconciliation, live destination transport,
and destination-specific removal acknowledgements remain release/integration work.

## Initial Odin integration batch — 2026-10-19

The first Skald-owned OHS-02/03/07 changes are implemented from baseline
`22114dd`; a tagged producer release and consumer pins remain pending. The
[producer plan](docs/ODIN-INTEGRATION-PLAN.md) retains the standalone archive,
CLI and TUI and records the remaining EC-14 publication and consumer checks.

- Added a versioned [capture-delivery envelope](docs/CAPTURE-DELIVERY-V1.md), stable batch identity, strict wire
  validation, JSON Schema and synthetic shared fixtures. The public sink boundary
  returns an acknowledgement for one destination only after atomic acceptance.
  The standalone daemon now uses its archive adapter through that boundary.
- Added scoped source enrollment, checkpoint generation/epoch/ordinal, capture
  and native record timestamps, and explicit omitted-source counts. TUI diagnostics
  separate source checks from record age and display unknown native timestamps.
  Idle polls do not make old capture evidence appear fresh.
- Extended recovery checks for source loss/reappearance, restart and restore;
  corrected the version command to use the actual archive schema constant.
  A pre-existing project-association fixture now uses an environment-independent
  synthetic scratch path instead of assuming `/tmp` has no Git ancestor.
- Recorded a metadata-only inventory of the installed daemon: 174 enrolled streams
  across Claude Code, Codex and Devin, including explicit blocked, unavailable
  and gapped sources. See the [receipt](docs/IMPLEMENTATION-RECEIPT-2026-10-19.md)
  for counts, baseline checks and remaining live-source investigation.

At the end of this initial batch, durable receipts, archive changes, the spool,
removal propagation and the first-checkpoint Devin WAL fix remained pending.
The 2026-10-21 batch above supersedes those implementation limitations. Producer
tagging and Odin acceptance remain separate. No live installation, source
configuration, peer repository or task authority was changed.

Integrated verification: `make check smoke tui-smoke discovery-smoke` passed,
including Go tests, JSON Schema conformance, SQLite DDL checks, vet, compiled
daemon recovery, standalone TUI terminal restoration and source discovery.
`go test -race ./...` also passed with the project toolchain and caches. The
compiled version command reports archive schema 5. Socket/PTY checks ran
with the required sandbox access; all runtime fixtures used private temporary
synthetic state. `git diff --check` and updated documentation links also pass.

## Delivered

- `sources` metadata preview and `connect` configuration commands, private atomic
  config updates, stable generated namespaces, selected-root discovery, persisted
  stream enrollment, verified prefix-preserving moves and explicit ambiguity/capacity
  diagnostics. New primary conversations are discovered without daemon restart.
  [Source connection guide](docs/SOURCES.md).
- Unchanged complete files use device/inode/size/mtime/ctime checks between periodic
  full-prefix audits. Changes, restarts and backfills retain prefix verification.

- Native `skald tui` with heimdall/terminal-theme/amber/monochrome display, project/provider/source grouping,
  session filtering, expanded recent context, transcript pages, tool folding, native
  recap/turn navigation, historical revisions, exact references, terminal clipboard,
  source diagnostics and scrollable help. [TUI guide](docs/TUI.md).
- Bounded transcript display API at one archive boundary, attributable project
  associations, shared Unix-only CLI/TUI transport, asynchronous refresh/reconnect,
  stable selection, scope-denial clearing and rejection of late request results.

- Renamed and revised implementation plan and Heimdall amendment; reviewed current
  sibling Heimdall docs at `5e0c790` with existing uncommitted S2a work. The amendment
  remains context only, and no Heimdall files were changed.
- Shared `sessionrecord` / `sessioncapture` packages, synthetic Claude/Codex fixtures,
  version-1 JSON Schema, canonical identity/hash vectors, native/opaque normalization,
  caller checkpoints and explicit continuity gaps.
- Foreground Linux daemon with a sole archive writer, private XDG paths, exclusive
  archive/socket locks, same-user Unix peer checks, cancellation and clean shutdown.
- Transactional source registration with pinned namespace/provider/logical stream
  identities and explicit root aliases. Configured paths stay inside their source
  roots; devices, pipes and escaping symlinks are refused.
- Session titles use native `ai-title` records (the real `aiTitle` field) when a
  provider emits them, otherwise a bounded projection of the first line of the
  earliest substantive user message — labeled `title_kind` with record provenance
  and backfilled for previously archived conversations. Injected context blocks
  are not eligible; a later native title replaces the derived one. Approval-
  assessment preambles contribute their embedded action justification.
- Conversation activity is projected from native lifecycle observations
  (working/idle) and unanswered input-request tool calls, with the newest
  observed record position and time. Providers without lifecycle records
  (Claude, Devin) derive the signal from the newest record's shape — an open
  user tail or a tool call in flight is working evidence, a reply tail is idle
  evidence. While a Devin store's WAL is hot, capture defers and a read-only
  probe watches the messages table — growing content projects a working
  signal at the observed write time, since the desktop app also writes
  bookkeeping pages to open-but-idle sessions. A consumed input request reads
  as working; cross-stream signal
  ordering is labeled ambiguous. This is archived evidence, not live presence.
- SQLite schema 7 with original bytes, immutable source and normalization versions,
  separate source-order observations/generation epochs, checkpoint/change commits,
  capture gaps, source health, record heads and project observations. Native titles
  have bounded attributable projections; ambiguous cross-stream ordering is labeled.
- Bounded periodic backfill/tailing. Partial lines wait, malformed lines remain
  opaque with gaps, rewrites are explicit, and source removal preserves history.
  Capacity/suppression failures do not advance checkpoints.
- `serve`, `status`, `capabilities`, `changes`, `redact`, `sessions`, `artifacts`, exact `get`, `backup`, and fresh-directory
  `restore`, alongside the earlier read-only diagnostics. All live CLI reads go
  through the socket. Activity remains `unknown` until L2 evidence selection.
- Namespace-bound pagination with explicit expiry after archive changes; source-ordered
  artifact references; exact revision/adapter reads; separately enabled raw bytes;
  terminal control escaping; bounded request and response sizes.
- SQLite online backups, original-object digest/foreign-key/integrity checks,
  atomic no-replace publication and restore with a new archive instance. Older-schema
  upgrades retain a consistent pre-upgrade backup. No live WAL file copying.

See [archive setup](docs/ARCHIVE-SETUP.md) for commands and the explicit source
configuration. [Fixture configuration](examples/fixture-sources.json) uses only
synthetic files in this project.

## Verification

Go 1.27.1 on Linux, `modernc.org/sqlite` v1.38.2, and standalone SQLite 3.53.4 DDL
checks. Dependency versions and checksums are pinned in `go.mod` / `go.sum`.

- Source-connection tests cover metadata confinement, native ID conflicts, private
  config updates, stable enrollment/restart/moves, ambiguous copies, discovery
  fairness/capacity, source loss, scope removal, idle caching and same-size rewrites.
  Compiled source-connection acceptance passes through a real private Unix socket.
- TUI tests cover grapheme wrapping, control escaping, monochrome fallback, project
  associations, clipped projections, historical revisions, keyboard navigation,
  stale replies, reconnects and scope revocation. Compiled PTY checks pass for
  resize and terminal restoration on both quit and SIGTERM. A Ghostty visual check
  exposed an inherited-NO_COLOR inversion; explicit monochrome styling fixes it.
- Go tests, vet, build and the race detector pass. Record fixtures also pass the
  independent JSON Schema validator. Earlier capture fuzzing passed 35,066 cases.
- Archive tests cover restart, duplicate delivery, stale checkpoints, SQL failure
  before checkpoint publication, full rollback, native revisions, exact historical
  reads, scope/cursor isolation, capacity pauses, suppression, explicit relocation,
  exclusive writers, schema upgrade/refusal and corrupt-backup rejection.
- Collector/API tests cover partial append/completion, source disappearance and
  restoration, root confinement, ordinary record ordering, raw projection policy,
  invalid requests, bounded pages, and absent peer services.
- Compiled native acceptance passed against a private real Unix socket: 17 original
  records across two JSONL providers plus the canonicalized Devin store, a
  pending appended record, SIGKILL/restart to 18
  records without duplicates, source deletion, exact original reads, online backup,
  fresh restore with no native files, and graceful shutdown. No test daemon remains.

### Initial scale measurement

[Saved measurements](docs/ARCHIVE-BENCHMARK.json) use 100 sessions / 20,000 small
synthetic records on an Intel i5-8600K, Linux 7.2.3, with warm local caches.
Thirty list samples include compiled CLI startup, socket transport and JSON output.

- Initial list p95 was 169 ms. A bounded native-title projection and source-health
  index reduced it to **49 ms p95** (48 ms median), below the 100 ms fixture target.
- Initial backfill took **9.45 seconds** in the final measured run.
- 3,908,890 source bytes produced 104,779,776 SQLite bytes: **26.81×** for these tiny
  records. This includes fixed identity/index/envelope overhead; it is not an
  estimate for typical large transcripts. Storage efficiency remains a release concern.

The benchmark is reproducible with `scripts/benchmark_archive.py`. This is a
small-record local measurement, not a universal production latency/storage claim.
Large native transcripts still incur repeated full-prefix hashing per batch.

## Remaining work

1. Publish and pin the new producer bundle, then run Odin consumer fixtures and
   record supported installed-provider capabilities. Existing `v0.1.0`/`v0.1.1`
   tags predate these interfaces. Native Claude child capture is implemented;
   other provider subagents, general aliases and broader provider verification
   remain separate release capabilities.
2. Measure and improve large-transcript backfill/tailing and storage growth. Test
   extended disk-pressure and migration/recovery scenarios. Keep current explicit
   capacity and coverage diagnostics; do not claim full L1 release acceptance yet.
3. Finish L2 beyond the delivered terminal reader: archive-wide description
   projections, fresh activity-evidence selection, Herdr/local groups and verified
   source activation. Current descriptions use a labeled 25-record window; filter
   and transcript find are page-local. Wider release acceptance remains open.
4. Complete L3 Braid retrieval beyond the delivered scoped read-only MCP, followed by optional peer,
   surface and portable-preservation integrations under their existing milestones.

Local read-only metadata and bounded parsing samples verified Claude 2.1.263 and
Codex 0.153.4 native files, including desktop-origin Codex data, and the Devin
desktop acp-messages store (13 session databases: canonical dumps, native titles,
file-path project evidence, quiet-gated capture). No personal
conversation bodies were persisted in test fixtures or archived during verification.
Only this project's files, build caches, prepared connection config and temporary
synthetic test data were written. No provider hooks, user services, desktop
settings or model requests were introduced.

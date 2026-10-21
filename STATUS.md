# Skald implementation status

Updated 2026-10-21. **The Skald-owned OHS producer implementation batches are complete and verified locally.**
Skald can now enroll primary conversations in selected Claude/Codex roots and
Devin desktop session stores (`acp-messages` SQLite),
retain independent history, serve bounded reads, and back up/restore its archive.
The TUI browses project groups, bounded excerpts, native recaps and transcript
history through the socket API. Retrieval and peer integrations remain future work. The full release gates below remain open.

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
- SQLite schema 6 with original bytes, immutable source and normalization versions,
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
   tags predate these interfaces. Native parent evidence and explicit unsupported
   subagent/alias cases are fixture-covered; full enrollment/projection support
   and broader provider verification remain separate release capabilities.
2. Measure and improve large-transcript backfill/tailing and storage growth. Test
   extended disk-pressure and migration/recovery scenarios. Keep current explicit
   capacity and coverage diagnostics; do not claim full L1 release acceptance yet.
3. Finish L2 beyond the delivered terminal reader: archive-wide description
   projections, fresh activity-evidence selection, Herdr/local groups and verified
   source activation. Current descriptions use a labeled 25-record window; filter
   and transcript find are page-local. Wider release acceptance remains open.
4. Implement L3 Braid retrieval and scoped read-only MCP, followed by optional peer,
   surface and portable-preservation integrations under their existing milestones.

Local read-only metadata and bounded parsing samples verified Claude 2.1.263 and
Codex 0.153.4 native files, including desktop-origin Codex data, and the Devin
desktop acp-messages store (13 session databases: canonical dumps, native titles,
file-path project evidence, quiet-gated capture). No personal
conversation bodies were persisted in test fixtures or archived during verification.
Only this project's files, build caches, prepared connection config and temporary
synthetic test data were written. No provider hooks, user services, desktop
settings or model requests were introduced.

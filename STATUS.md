# Skald implementation status

2026-09-10. **Source connection, archive daemon and native TUI are implemented and verified.**
Skald can now enroll primary conversations in selected Claude/Codex roots,
retain independent history, serve bounded reads, and back up/restore its archive.
The TUI browses project groups, bounded excerpts, native recaps and transcript
history through the socket API. Retrieval and peer integrations remain future work. The full release gates below remain open.

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
  are not eligible; a later native title replaces the derived one.
- SQLite schema 3 with original bytes, immutable source and normalization versions,
  separate source-order observations/generation epochs, checkpoint/change commits,
  capture gaps, source health, record heads and project observations. Native titles
  have bounded attributable projections; ambiguous cross-stream ordering is labeled.
- Bounded periodic backfill/tailing. Partial lines wait, malformed lines remain
  opaque with gaps, rewrites are explicit, and source removal preserves history.
  Capacity/suppression failures do not advance checkpoints.
- `serve`, `status`, `sessions`, `artifacts`, exact `get`, `backup`, and fresh-directory
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
  records across two providers, a pending appended record, SIGKILL/restart to 18
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

1. Finish capture release gates: native subagent/fork/alias fixtures and broader
   installed-provider capability verification. The module path is now
   `github.com/algorhythmic/skald`, tagged `v0.1.0` for consumers. Root aliases
   and stream tokens persist, but provider conversation aliases remain a separate
   capability.
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
Codex 0.153.4 native files, including desktop-origin Codex data. No personal
conversation bodies were persisted in test fixtures or archived during verification.
Only this project's files, build caches, prepared connection config and temporary
synthetic test data were written. No provider hooks, user services, desktop
settings or model requests were introduced.

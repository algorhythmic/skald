# Producer contract candidate

This Skald candidate implements the selected producer batches for OHS-02/03/07
and EC-14, starting from `f7126c7`. The commit containing this manifest identifies
the implementation and fixture bundle; obtain its immutable revision with
`git log -1 --format=%H -- docs/PRODUCER-CONTRACT-MANIFEST.json`.
It is **untagged**: `0.1.0-dev` is a development label, not a producer release or
Odin consumer pin. A release receipt must record a published tag separately.
Odin must then pin that release and run the same fixtures before
claiming producer/consumer compatibility or G-10.

The [candidate manifest](PRODUCER-CONTRACT-MANIFEST.json) records SHA-256
digests for the record, delivery, archive-feed and redaction schemas plus their
synthetic fixtures. Regenerate it after any contract change with
`python3 scripts/release_manifest.py --write`; verify with
`python3 scripts/release_manifest.py --check`. The manifest deliberately
does not assert a Git revision, tag, remote publication or test outcome.

The local boundary includes `sessionrecord`, `sessioncapture`,
`archivefeed` and `capturespool`, the archive sink/changes reader and
content-removal receipt. Archive schema 8 retains the sink/redaction receipts,
persisted read scope and schema-7 scoped-count index, and adds lossless payload
storage compression with lazy bounded conversion. The spool is an independent private database. Its
producer checkpoint may lead each destination's checkpoint; the archive backup
does not include pending spool envelopes. Drain the spool before archive-only
recovery or preserve it separately.

Current capability claims remain narrow. Claude Code and Codex ordinary
content use synthetic fixtures for the identified versions; Devin uses the
`acp-messages` store fixture. Native Claude children now have opt-in enrollment,
distinct derived conversation IDs and native parent relations; primary streams
continue rejecting sidechains. Child identity fixtures cover sampled Claude
2.1.296 and 2.1.303 shapes. Codex `session_meta.session_id`
supplies fixture-verified parent evidence for the older continuation format.
The newer bounded `forked_from_id` chain supports copied ancestor metadata and
`native_fork_of` relations under a distinct Codex fork adapter; unrelated native
IDs still fail. General conversation aliases are unavailable. Native browser/cloud collection,
provider hooks and live surface activation are unavailable. Native provider
format compatibility beyond inspected samples still needs a release receipt.

The 2026-10-21 candidate passed `make check smoke tui-smoke discovery-smoke`,
`go test -race ./...`, the manifest check, schema conformance, archive restore
and spool restart/mismatch checks. Runtime fixtures used private synthetic data.
Repeat the relevant checks if the selected release revision changes. Record exact revision, command
results, fixture digests and configured capability evidence in
[STATUS.md](../STATUS.md). The synthetic backfill measurement in the
[integration plan](ODIN-INTEGRATION-PLAN.md) improves the default batch size
but does not close large-transcript, disk-pressure or live-source gates.

Odin-owned consumer pins and fixture checks, historical overlap reconciliation,
live delivery and source activation, destination-specific removal
acknowledgements, and Heimdall's EC-13 writer fencing remain external work.
Skald's standalone archive, CLI and TUI remain independent of Odin.


## 2026-10-22 working candidate

The new batch adds whole-archive description selection, optional pinned Braid
lexical context retrieval, bounded payload compression, offline physical
compaction, recovery preview/apply operations and persistent backup-status
receipts. Monitoring files are homelab Collector/Prometheus drafts; the workstation
export endpoint remains unset. These implementation changes do not supersede the
dated 2026-10-21 test receipt above or prove current release acceptance.

[Local bundle tooling](producer-bundle.md) now creates a deterministic,
content-addressed tarball and checksum containing public packages, schemas,
fixtures and a standalone consumer verifier. The verifier checks exact file
hashes, canonical identities, delivery/feed version refusal and four schemas,
and emits a fixture-only receipt. It explicitly does not claim publication or
runtime consumer acceptance. Rebuild after public changes and record the final
artifact and manifest digests separately from the working base Git revision.

Odin's current README and AGENTS.md identify a documentation scaffold with no
service implementation or runtime acceptance tests. Its runtime sink, outage,
redaction and restore acceptance cannot be claimed from this Skald fixture
verifier. Publication, a release tag and Odin's immutable consumer pin remain
separate owner actions. See the dated
[implementation receipt](COMPLETION-IMPLEMENTATION-2026-10-22.md) and
[STATUS.md](../STATUS.md) for final checks and remaining gates.

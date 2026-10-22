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
content-removal receipt. Archive schema 6 stores sink and redaction receipts and
persisted read scope. The spool is an independent private database. Its
producer checkpoint may lead each destination's checkpoint; the archive backup
does not include pending spool envelopes. Drain the spool before archive-only
recovery or preserve it separately.

Current capability claims remain narrow. Claude Code and Codex ordinary
content use synthetic fixtures for the identified versions; Devin uses the
`acp-messages` store fixture. Native Claude children now have opt-in enrollment,
distinct derived conversation IDs and native parent relations; primary streams
continue rejecting sidechains. Child identity fixtures cover sampled Claude
2.1.296 and 2.1.303 shapes. Codex `session_meta.session_id`
supplies fixture-verified parent evidence, but relationship projection and
conversation aliases are unavailable. Native browser/cloud collection,
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

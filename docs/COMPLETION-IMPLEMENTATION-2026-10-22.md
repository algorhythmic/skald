# Implementation receipt — 2026-10-22

This receipt describes the current working batch. It preserves earlier dated
measurements and test receipts as historical evidence. It is not a release tag,
producer publication, consumer pin or assertion that every L0–L6 gate is closed.
Final whole-repository validation, live measurements and acceptance dispositions
are recorded separately below or in [STATUS.md](../STATUS.md) by the integrating
agent after code reviews settle.

## Implemented interfaces

- Schema-8 lossless payload storage compression, with explicit bounded conversion
  through `skald compress`; freed pages are reusable without claiming file shrink.
  `archive-compact` provides exclusive offline preview/apply physical reclamation.
- Whole-archive description selection, bounded recap/excerpt projections, exact
  references, unknown coverage and cross-stream/generation ordering ambiguity.
  The TUI uses this selection independently of its 25-record transcript page.
- Optional checksum-pinned Braid lexical context retrieval through `skald context`
  and MCP `context.query`; current authorized user/assistant heads, explicit
  corpus/cache/context budgets, exact citations and fail-closed unavailable/stale
  results. Retrieval is disabled without explicit configuration.
- Nine read-only MCP tools, adding `descriptions.read` and `context.query` to the
  existing seven tools. Startup namespace grants remain mandatory.
- Recovery export plus fresh restore, explicit retention preview/apply, latest raw
  rollback preservation, exact compressed/raw pairing checks and allocated-block
  accounting. Missing independent destinations are visible rather than defaulted.
- Private persistent backup-status receipts with archive instance, destination
  device evidence and successful export/restore times. Local exports cannot count
  as off-device backup; historical dates are separate from current configuration.
- Deterministic public producer candidate packaging and a standalone fixture
  compatibility verifier, with content hashes and explicit untagged status.
- Draft homelab OpenTelemetry Collector Contrib configuration feeding the existing
  Prometheus remote-write receiver on `.21:9090`, local graph/rule files and a
  draft service. No workstation service or homelab deployment is authorized by
  these files; the live workstation export endpoint remains unset. The future
  endpoint is `http://192.168.86.21:4318/v1/metrics`, with an explicitly configured
  nonsecret instance alias.

## Focused verification already run

The implementation agent ran the following focused checks successfully during
this batch, before final integration review:

```
go test ./internal/maintenance ./internal/archive ./internal/tui ./cmd/skald ./cmd/producer-verify
python3 scripts/test_producer_bundle.py
```

Recovery tests use disposable synthetic archives. They cover preview preservation,
explicit apply, missing/corrupt pairing, latest raw preservation, mixed archive
roots and pre-schema files, allocated blocks, insufficient space, bounded
manifests, cancelable directory locks and replacement/ctime checks. Backup-status
tests cover unset configuration, same-disk rejection, durable local receipts,
failed export preservation, partition ancestry and mapped-device backing.

Description tests cover more than 25 trailing records, namespace denial, retained
source history, observation-time changes, compressed payloads, clipped text with
exact references, user activity after recap, redaction and cross-stream ambiguity.
The verifier tests cover fixture compatibility, expected manifest digest mismatch,
tampering, traversal and symlink refusal. Packaging tests check deterministic bytes
and exact manifest content hashes.

A producer candidate was also extracted into an independent temporary module.
Tests for `sessionrecord`, `sessioncapture`, `archivefeed`, `capturespool` and the
fixture verifier passed there. The extracted final candidate's verifier accepted
the separately supplied manifest digest. Those checks establish public fixture
compatibility, not an Odin service result. Final artifact digests should be
regenerated or confirmed after all public contract changes settle.

## Remaining acceptance boundaries

No off-device backup destination is configured. The literal status is
`no off-device backup configured`; last off-device backup and verified-restore
ages remain unknown until matching successful receipts exist. A synthetic local
restore does not establish independent disaster recovery. Different partitions
sharing a physical device are insufficient. The broader L1 gate remains open.

No monitoring service is installed by the draft files. Homelab installation,
receiver validation, live resource measurements and any retention policy changes
need their own receipt. No outbound alert delivery is configured.

Odin's inspected repository is a documentation scaffold without a service or
runtime acceptance suite. Skald's fixture verifier cannot establish consumer
runtime enrollment, outage/backfill, task authority or cross-repository acceptance.
The producer remains an untagged, unpublished working candidate until the owner
records otherwise; no consumer pin is fabricated.

Core code reviews and final integrated validation are ongoing at the time this
implementation section was written. Final results, exact live storage/CPU
measurements and release-gate disposition belong to the integration addendum.

## Integration and local deployment addendum

The integrated build identifies itself as `0.2.0-rc.1`, sessionrecord v1, archive
schema 8. It is installed locally. The archive identity was preserved. After the
final restart, 659 sessions and 296,363 record versions were present, with zero
queued deliveries. The known 126 unavailable sources and four blocked streams
remain visible. Capture continues while these counts change. Current Claude
2.1.296/2.1.303 and Codex 0.163.0 synthetic shape/checkpoint tests pass; unknown
versions remain unknown and unsupported capabilities remain explicit.

Bounded live conversion ran for 315 seconds, examining 493,183 payload rows and
compressing 476,044. It reduced 2,365,849,047 logical input bytes to 1,165,410,762
stored bytes. More than 1,000 new record versions arrived during conversion;
history was not retired. The delivery queue was empty afterwards. Offline
compaction stopped only the capture daemon for 362 seconds, verified the archive,
and reduced the database from 3,778,740,224 to 2,365,435,904 bytes: 1,413,304,320
bytes reclaimed. The daemon restarted and backfilled successfully. Native session
processes were not restarted. New captures can leave new plain payloads pending
explicit conversion; no automatic conversion timer was installed.

The final settled resource sample excluded startup, conversion, compaction and
retrieval publication. These were separate active workloads, not a controlled
throughput benchmark:

| Measurement | Before batch | After batch |
| --- | ---: | ---: |
| Sample duration | 70.08 s | 70.12 s |
| CPU, percent of one core | 25.43% | 22.81% |
| Final daemon RSS | 68.09 MiB | 60.15 MiB |
| New record versions | 152 | 124 |
| Logical reads | 103.90 MB/s | 99.53 MB/s |
| Logical writes | 0.44 MB/s | 0.61 MB/s |
| Storage writes | 0.99 MB/s | 1.15 MB/s |

Physical reads were zero in both samples. Sampled peak RSS after the batch was
77.01 MiB. A separate startup sample was intentionally excluded because it
included verification and backfill. The owned immutable snapshot optimization
roughly halved its synthetic prefix-validation benchmark; ordinary mutable
source checks retain conservative prefix verification. Background CPU and
write load remain material and workload-dependent.

At the settled sample, allocated blocks were 2,402,406,400 bytes for the growing
main database, 7,925,760 for its WAL, 41,435,136 for the spool, and 138,182,656 for
deployment receipts/binaries. Retained schema-7/schema-8 raw rollback copies and
four earlier zstd/manifest pairs totalled 6,686,048,256 bytes. Total data-directory
allocation was 9,138,012,160 bytes. Recovery copies remain on the same device;
they do not establish independent backup. Retrieval cache allocation is separate.
No raw backup retirement was performed by this completion batch.

`make check smoke tui-smoke discovery-smoke mcp-smoke diagnostics-smoke
completion-smoke`, `go test -race ./...`, final provider race tests, hard-link
compaction refusal and retrieval child-parent-death regression tests passed.
Independent extracted producer package/verifier tests and deterministic packaging
checks passed. Collector Contrib 0.163.2 validation and Prometheus rule tests
passed offline; these do not establish acceptance against the live homelab
Prometheus 3.13.3 instance. No monitoring service is running from these drafts.

The live exporter endpoint is unset, and the resource instance alias is
`workstation-01`. The future endpoint is
`http://192.168.86.21:4318/v1/metrics`. No off-device destination or successful
recovery receipt exists: both status dates/ages remain unknown and L1 remains
open. Publication, consumer fixture pinning and runtime acceptance must be
distinguished in any subsequent release receipt.

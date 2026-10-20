# Skald producer integration plan

Prepared 2026-10-19 for the first Skald implementation batch of the proposed
Odin–Heimdall–Skald work plan. This is Skald-owned implementation scope and a
publication checklist, not a peer decision, release receipt or live cutover.
[STATUS.md](../STATUS.md) records verified implementation and remaining gates.

The source plan was read from
`/home/david/.codex/worktrees/23e7/odin/docs/design/ODIN-HEIMDALL-SKALD-WORK-PLAN.md`
(prepared 2026-10-19), with that checkout's `docs/DECISIONS.md` ADR-006/007 and
`docs/design/ODIN-ECOSYSTEM-INTEGRATION.md` EC-13/14. These paths identify review
inputs; Skald does not depend on those files at runtime or edit their authority.

## Retained product and ownership

Skald continues to maintain its local archive, standalone CLI, TUI and bounded
exact reads. Its public `sessionrecord` and `sessioncapture` packages preserve
native identity, source revisions, ordering evidence and capture gaps. The
archive remains a supported sink with its own retention, sole writer and
consistent backup/restore. Odin being unavailable must not prevent local
capture or browsing. No repository merger is required.

The target integration puts commitments, checkpoints and accepted task decisions
in Odin after a separately accepted import and enforceable retirement of
Heimdall's writer for the migrated scope. Skald owns no task replica or task
synchronization bridge. Existing Heimdall scope keeps its current authority until
its owner records and enforces EC-13; this plan does not record that decision.
The [Heimdall amendment](../SKALD-HEIMDALL-AMENDMENT.md) preserves the historical
consumer policy rather than requiring Heimdall ownership forever.

A captured statement, archived activity signal or imported checkpoint is evidence
with provenance, not independent verification or authority to complete a task.
Sessions and commitments can have many-to-many associations. Project names and
paths may suggest an association but never silently create obligations or widen
reader access. Exact evidence navigation must preserve source identity, revision,
reader scope and an explicit unavailable result.

## First implementation batch

Work against synthetic fixtures and the existing standalone path first. Treat
implementation and test receipts separately from publication and peer acceptance.

| Work package | Skald change | First-batch evidence and limit |
| --- | --- | --- |
| OHS-02: useful source visibility | Expose enrollment, capture/parse position, observation time and source age, failures and unavailable reasons in status and the TUI using metadata already owned by Skald | Synthetic Claude Code and Codex loss/reappearance and restart checks, plus browsing with peers absent; capture age is not live presence. Live enrollment and one attributable captured change per selected provider still need a separate receipt. |
| OHS-03: producer capture boundary | Add a versioned delivery envelope and public collector/sink boundary around existing normalized batches; preserve the record-v1 identity contract and the local archive adapter | Machine-readable schema, shared synthetic fixture, unsupported-version refusal, stable replay identity, interrupted delivery and checkpoint atomicity checks. This prepares EC-14; it does not publish a new release or an archive changes API. |
| OHS-07: prepare independent destinations | Exercise the public boundary against a standalone archive sink and a synthetic consumer | Show that sink failure leaves its checkpoint unacknowledged and a retry remains attributable. A durable workstation spool, multiple destination acknowledgement state, historical archive ingestion and live Odin integration remain later work. |

Run the repository checks for the selected revision (`make check`, build and
relevant synthetic smoke/recovery checks). Record commands, results, revision and
any unavailable environment in [STATUS.md](../STATUS.md). Existing release gates
require their own evidence; an isolated delivery or source-health test does not
close OHS-02, OHS-03, OHS-07 or L0–L6.

For OHS-02's real-source follow-up, first inspect configured providers and installed
versions using metadata-only diagnostics. Distinguish configured, enrolled,
unavailable and disabled sources. Do not infer fresh health from the October 17
product-review failures, change provider settings, or add a second Codex parser
in Heimdall. Browser pairing remains outside the selected first batch. Record
before/after diagnostics and source-specific coverage without checking personal
conversation text into the repository.

## EC-14 publication and consumer checklist

The public record and checkpoint contract is documented in
[CONTRACT-V1.md](CONTRACT-V1.md); the first local delivery boundary is specified
in [CAPTURE-DELIVERY-V1.md](CAPTURE-DELIVERY-V1.md). The following are separate deliverables; an
internal table or untagged package is not evidence that the corresponding
consumer interface is published.

| Deliverable | Required evidence / next action |
| --- | --- |
| Capture delivery contract | Review the first-batch envelope, sink semantics, schema and fixture. Specify batch/source identity, source revision, candidate checkpoint, gap semantics, ordering scope and unsupported-version behavior. Keep source identities distinct from transport delivery IDs. |
| Public collector and discovery | Identify which capture/discovery operations are public and which daemon enrollment/registry behavior remains internal. Reuse existing parsers; publish only the surface needed by the consumer. |
| Stable source namespace | Agree nonsecret host/source identity and preserve existing configured namespaces and logical stream tokens. Resolve hostname versus machine-derived namespace differences with explicit mapping/migration evidence; never silently rekey an existing archive or expose raw host identifiers in fixtures. |
| Archive changes reader | Publish a scoped, bounded snapshot-plus-change interface, schema and shared fixtures. Bind cursor to archive instance, scope and high-water boundary; define restore/cursor invalidation, correction/removal/redaction and exact revision locators. Do not expose an internal SQLite path/table as the interface. |
| Durable workstation delivery | Define spool ownership and atomic persistence of records, originals, gaps and checkpoint. Prove interruption/restart, stable batch identity, backpressure, replay and outage/backfill. Retain a single writer per destination namespace. |
| Destination isolation | Persist acknowledgements independently for each destination. One sink's success cannot advance another sink's checkpoint. Deduplicate by source identity/revision without promising exactly-once delivery. Retention and removal receipts remain destination-specific. |
| Producer publication | Choose and publish a release after the checks pass. Record the immutable release revision, contract/schema versions, fixture digests and supported capabilities, including explicitly unavailable features. Preserve the standalone archive and browser evidence. |
| Consumer pins and G-10 record | Odin records the exact published module/release and contract/fixture pins, runs the same fixtures and records its own compatibility result. Each affected peer records its part; this document alone does not close G-10. |

The inspected Skald working baseline was `22114dd`. Local tags `v0.1.0` and
`v0.1.1` were present during preparation; no remote publication or Odin consumer
pin was verified here. Reconcile the release actually containing each interface
against the selected consumer pin before declaring compatibility. Do not infer
EC-14 completion from the older status text naming `v0.1.0`.

The combined producer/consumer suite must cover duplicate delivery, interrupted
batches, unsupported versions, cursor invalidation, source rewrite/removal,
redaction, outage/backfill, restore, scope denial and exact evidence revisions.
Run each applicable case in both repositories against the same fixture versions.
A restored archive gets a new archive-instance cursor scope without changing
native source identity. A removed native source can remain in the archive under
retention; source loss and authorized redaction are different operations.

## Placement and remaining decisions

OHS identifiers coordinate work; they do not renumber or close Skald's existing
[L0–L6 gates](../SKALD-IMPLEMENTATION-PLAN.md#11-delivery-sequence-and-acceptance):

- L0 retains provider capability, native subagent/fork/alias and conformance gates.
- L1 retains large-transcript/storage work, disk-pressure/recovery checks and
  evidence for a configured independent backup destination and fresh restore.
- L2 retains archive-wide descriptions, fresh activity evidence, Herdr/local
  groups and verified source activation beyond the implemented browser.
- L3 retrieval and read-only MCP, L4 peer acceptance, L5 packaging/surface
  evidence and L6 portable preservation keep their separate requirements.

OHS-03 and OHS-07 start with synthetic data. Under Odin's current roadmap, live
archive integration remains I6. An earlier observation-only pilot needs a
separate recorded decision naming source scope, identity, retention, reader
permissions, producer pins and removal behavior. This first batch does not enable
that pilot, task cutover, managed session launch, worker dispatch or desktop input.

Remaining decisions belong to their owners: Heimdall records EC-13 and writer
fencing; Odin selects its implementation baseline and records consumer pins,
runtime readiness and any earlier-pilot placement; Skald publishes its tested
producer interfaces. Task import, production write switching and retired-writer
checks occur in the OHS-06/09 migration, outside this producer batch. Permanent
task synchronization, a merged product/repository, Braid integration, voice and
desktop control are not prerequisites for these first changes.

# Heimdall amendment: independent session capture and Skald

Originally reviewed 2026-09-09; renamed and relinked 2026-09-10.
Status: adopted design on 2026-09-10 and applied to handoff r4, with the
§4.2 purgeable evidence-table correction. Implementation remains S1/S3/S5 work.
“Session Lens” in the review refers to Skald; the product name remains Skald.

Companion: [Skald implementation plan](SKALD-IMPLEMENTATION-PLAN.md).
Its §4 defines the version-1 shared session-record contract and L0 freeze gates. This
amendment defines Heimdall's consumer policy rather than a second record schema.

## 1. Purpose and reviewed state

Heimdall and Skald must operate independently. Both may observe the same
agent conversations, while retaining different information for different purposes.
Heimdall records accepted work state and selected conversation evidence. Skald archives accessible conversation history and serves a session window and
agentic retrieval. Reuse provider parsing and source identity without requiring
either application's runtime or database in the other.

Adoption reviewed against Heimdall HEAD `d3e4694`: P0 code landed in
`5e0c790`; S2a is delivered at schema 21. P0 daily-use gates remain open. S1 conversation ingestion and S3 Braid integration are unstarted.
Existing task contracts, decisions, checkpoints, scoped writes and session
bindings already have defined ownership. There is no implemented Skald
task-state system or duplicate conversation parser to remove.

Heimdall also has an implemented manual dotprivate preservation workflow for
selected checkpoint artifacts. It previews and records the selection and observes
results; it does not invoke dotprivate or execute Git operations. The inspected
dotprivate `0.1.0` at `51d0fce` stages all private-checkout changes during sync.
See [preservation setup](../heimdall/docs/PRESERVATION-SETUP.md); the automated selected-files
interface proposed here is not yet provided by that implementation.

This amendment targets [handoff r4](../heimdall/docs/design/HANDOFF-heimdall-v1-r4.md), principally §7.1,
§7.2, §7.8, §7.9, §9.1, §11.6 and the S1/S3/S5 acceptance text in §12. It does not
reorder P0 → S2a → S1 → S2b → S3 → S4 → S5, reserve a new schema number, or change
S2a's action/grant/report semantics. See [backlog](../heimdall/docs/BACKLOG.md),
[status](../heimdall/docs/STATUS.md) and [S2a work](../heimdall/docs/S2A-IMPLEMENTATION.md).

The 2026-09-10 adoption reconciles r4’s retention and idle-end rules. It changes
design documents only; manual preservation still dispatches no Git operations.

## 2. Ownership and independence

| Information or behavior | Owner and boundary |
| --- | --- |
| Accepted contracts, decisions, task/step state, checkpoints, dependencies | Heimdall, through existing authorized commands and event transactions |
| Task-owned workspace, viewport and session bindings | Heimdall's existing declaration/observation/verification rules |
| Native conversation content and provider events | Originating provider; reusable capture code preserves source identity and provenance |
| Comprehensive archive, native description history, local session groups | Skald, under its own retention policy |
| Full session-archive recovery and portable selected exports | Skald; consistent archive backups and scoped exports have different completeness guarantees |
| Selected conversation evidence retained for Heimdall | Replayable metadata/digests in Heimdall; optional text in its purgeable evidence table; source references remain external identities |
| Private copies of selected project documents, artifacts and session exports | Dotprivate as an optional preservation destination; Heimdall retains its own checkpoint selection and preservation evidence |
| Action catalogs, applicability and recipe validation | WCU; Skald content is not live desktop-verification evidence |
| Retrieval | Braid integrated and configured separately by each consumer |

Heimdall must collect supported session evidence with Skald absent. Skald must capture, display and retrieve conversations with Heimdall absent. Shared
parser packages are implementation dependencies, not an additional service the
user must operate. Neither daemon opens or migrates the other's database.
Dotprivate availability and remote Git synchronization are also optional. They do
not gate either application's local state, checkpoints or supported collection.

Some duplicate persistence is intentional: a native description can exist in a
CLI transcript, Skald's archive, a bounded Heimdall evidence record and a
Braid index. Each copy has a declared purpose and source revision. There must be
one Heimdall authority for accepted task state, but there need not be one physical
copy of every piece of source text across independent products.

Heimdall’s S1 panel shows bound conversations, lifecycle and the current available
description (or a coverage gap). Skald’s window shows the conversation archive.

Skald may maintain local groups and display imported Heimdall task state.
Those displays retain Heimdall instance/target/revision and fetch time. A user
changing a Skald group does not reassign a Heimdall task or session binding.
An imported association never bypasses Heimdall's current binding rules.

## 3. Reusable capture boundary

Adopt `sessionrecord` and `sessioncapture` as the conceptual package boundaries
in the companion plan. The first implementation can live in the Skald
module and be consumed as a pinned release by Heimdall. If Heimdall's S1 work
arrives first, publish the reusable implementation from the first implementing
repository with the same contract and fixtures; ownership can move without
changing source IDs. Do not block S1 on completion of the Skald product.

The capture layer owns provider discovery/probing, native parsing, record identity,
incremental checkpoints and normalized source observations. Heimdall owns source
configuration, hook installation, task association, persistence, authorization and
event mapping. Parsing has no knowledge of `model.State`, task completion,
proposal acceptance or Heimdall's local ID allocator.

Source keys and record revisions follow the companion contract. Map them to
Heimdall IDs; do not change Heimdall's existing 32-hex identifiers or task syntax.
S1 consumes conversation-scoped records. Project/source notes without a proven
conversation remain outside that initial mapping; they do not create invented
Heimdall conversations or accepted resources.
Store adapter/contract versions and provenance with consumed records. A transport
delivery ID is not a native record ID. A path, matching title or common cwd alone
does not prove that two conversations are the same.

Heimdall and Skald run the same sanitized conformance fixtures against
their pinned adapter releases. Include absent IDs, byte-preserving revisions,
partial writes, rotation, repeated utterances, explicit forks and unsupported
records. Reject unsupported contract majors; retain/report capability gaps
instead of silently reinterpreting them. Consumer-specific retention remains
outside these fixtures' normalized-output expectations.

No second provider parser should be introduced merely because the data enters
through a hook, a file reader or a Skald feed. Those are transports into
the same normalized contract. Hook installation still belongs to the application
performing it and must preserve existing provider/Herdr hook chains.

## 4. Adopted changes to r4

### 4.1 S1 hooks and S5 provider adapters (§7.1, §7.2, §7.8)

Add this boundary to the provider sections:

> Provider-specific decoding and session-record identity use the versioned shared
> capture contract. Heimdall can read configured native sources directly, without
> Skald. An optional Skald feed is another source transport with
> the same origin keys. Heimdall applies its own task-binding and retention rules
> after normalization. Source capability and adapter version are recorded; a
> missing signal remains unknown rather than being inferred from a different app.

Keep the current hook-to-agent-state mapping and existing binding rules. Native
Claude Code recap ingestion needs a transcript reader; hook completion alone does
not imply a recap exists. Skald delivering a `native_recap` does not imply
the underlying agent is idle, ended or blocked.

Retain the S5 scheduling of Heimdall's Codex/Desktop adapters. Skald may
ship its own provider coverage earlier. Reusing that parser later does not
implicitly advance Heimdall's milestones or extend its native surface authority.

### 4.2 Conversation lifecycle and descriptions (§7.9)

Replace the assumption that tier-3 descriptions exist only after optional
conversation-end inference with these separate paths:

1. **Lifecycle observation:** native start/end signals and observed activity are
   deterministic source records. A conversation-end event stores its source
   reference, not a raw transcript body.
2. **Native description observation:** readable native titles, recaps, summaries
   and supported description-note types can arrive at any point. Ingesting them
   needs no configured inference provider and no new model request.
3. **Optional intent extraction:** S3 may produce Heimdall's structured intent
   fields from permitted input with an explicitly configured provider. A native
   recap is not automatically a `{decided, next_action, resume_by, drop_if}` object.
   This remains source-derived interpretation, not an accepted decision.

This resolves the existing timing ambiguity: S1 provides lifecycle and available
native descriptions; the extraction implementation remains S3. Proposed actions
and their authoring/ratification remain in their existing roadmap slices. No new
proposal-write grant is introduced by session ingestion.

The planned `conversation.description_observed` event is separate from S3's
`conversation.summarized` intent-extraction event. Register its version and replay
handling with the owning S1 migration; no new schema number is reserved here.
The event contains the Heimdall conversation ID, source/record reference and
revision, description digest, kind, adapter/contract version, provenance, times,
coverage and availability (`available` or `withdrawn`). **No description text is
stored in an event, command receipt or serialized replay projection.**

A projection-side evidence table holds optional normalized description text,
keyed by digest, with scope-checked source associations. Allowlist native types;
retain at most 4,096 UTF-8 bytes, safely truncated and marked. Keep the exact
source revision/digest separate from the digest of retained normalized bytes.
Arbitrary assistant messages or prompts cannot become eligible merely because an
adapter calls them a summary. This table is a purgeable evidence cache, not a
second authoritative state store or a full conversation archive.

Populate bytes at ingest. After deterministic event replay, a separate bounded
hydration pass may resolve permitted native sources or an explicitly configured
Skald feed, validate revision/digest and refill missing bytes. Replay succeeds
with neither source installed; absent bytes are an evidence coverage diagnostic,
not a changed historical event or a fabricated current description. Event/state
goldens exclude evidence presence and hydration never appends semantic events.

Withdrawal immediately makes the affected evidence unavailable, purges its cached
text and derived retrieval copies, and prevents old observations from rehydrating
that withdrawn association on replay. Authorization is checked through current
source/scope associations, never by possession of a digest. Shared digests must
not expose a withdrawn or out-of-scope association. Test withdrawal, replay,
source loss, digest mismatch and scope isolation. Heimdall has no event-log purge
policy; old events retain metadata and digests, never the recap bytes.

Fallback excerpts of ordinary assistant messages belong to Skald's
display/archive policy. Heimdall may resolve a permitted source reference for a
bounded display, but does not persist arbitrary transcript excerpts through the
new native-description event. Its task summaries continue to come from its
recorded task/checkpoint state.

Replace the unqualified idle-end rule with:

> Lack of activity can mark a conversation inactive under a declared timeout
> policy. It does not constitute a provider-observed end. Preserve observed
> lifecycle separately from derived inactivity; new records with the same native
> conversation ID resume that conversation. Source loss remains a coverage gap.

The 30-minute threshold can remain a configurable inactive-display default. It
does not fabricate `SessionEnd` evidence or prove completion of the bound task.

### 4.3 Retention consistency (§5.1 and §14)

Resolve §5.1's browser-conversation body wording and §7.9's `transcript_ref |
content` alternative in favor of §14's existing raw-transcript exclusion:

> Raw prompts, complete transcript records/bodies and WCU frames do not enter
> Heimdall's event log. Conversation events carry source references, digests and
> normalized metadata. Bounded native-description text lives only in the
> purgeable evidence table described above, outside the append-only log. Comprehensive conversation archival
> belongs to the source provider or an independently configured Skald.

This explicitly defines the permitted evidence storage; it is not permission to
copy raw JSONL envelopes into a string field. Selected accepted checkpoint text
continues through existing checkpoint commands. Source disappearance may make
an external reference unavailable; it does not erase a previously accepted
checkpoint or turn an old source description into current task state.

### 4.4 Braid consumption (§11.6 / S3)

Use current Braid exact reads, lexical retrieval, hard filters, revision-checked
publication and context export. Keep an application-owned dataset/configuration;
Skald and WCU integrations remain independent.

Heimdall publishes its own permitted retained records and accepted relationships.
Native conversation descriptions and accepted task decisions have distinct node
types and provenance. If an explicitly enabled Skald source contributes
context, preserve its original record/revision and apply the same caller scope
before publication or expansion. Do not automatically mirror its entire archive.

Record both the Heimdall/source boundary and Braid dataset revision/digest. Exact
indexed reads establish index consistency; accepted-state changes still use
current Heimdall checks. Required accompanying provenance is explicit in Braid's
dependency contract. Output filters alone are not a cross-scope traversal boundary.

Braid availability affects ranked context and assignment assistance. It does not
gate Heimdall's existing mandatory context, task commands, checkpoints or direct
source collection. No engine modification is required by this amendment.

Every MCP response must carry a required `provenance` envelope identifying the
producer, authority class and applicable source/target/index revisions and scope;
unknowns are explicit. Mixed results retain per-item provenance. Empty/error
responses retain producer/scope provenance too. `heimdall_context` identifies
accepted state separately from source evidence; Skald `context.query` identifies
what was said. Missing provenance fails response conformance tests.

### 4.5 Dotprivate preservation and external session sources (§9.1)

Retain Heimdall's existing manual preservation workflow and its separate source,
mirror, committed and remote observations. A local checkpoint remains valid
without remote publication. This amendment adds no copy/push authority and does
not convert the manual handoff into an automated action.

Skald captures configured native transcripts where they reside, including
outside project directories. Its archive and consistent backups own full-history
recovery. Dotprivate can preserve selected portable exports plus ordinary project
notes and artifacts. External-path copying already exists in dotprivate, but its
absolute-path restore behavior is not the Skald import contract. Imports
retain native identities and write only to Skald; they do not reconstruct
live provider application state. See the companion plan's §6.4–6.5.

A conversation can have several project associations or none. A project export
does not acquire authority over its session or task, and a copied transcript
retains its original record/revision keys. A document or output path mentioned
in a transcript is not preserved artifact content; actual selected file bytes
and versions require their own capture/preservation evidence.

Complete transcript payloads stay outside Heimdall's event log. Live Skald
and Braid databases stay outside Git synchronization; selected portable transcript
exports remain eligible under the companion plan's preservation policy. Do not
register an entire `.private` directory or Skald archive as a Heimdall resource.
Skald exports do not automatically become accepted Heimdall artifacts,
checkpoints or task relationships. Existing explicit artifact/checkpoint commands
and scope checks remain the only route for that accepted state.

Future automated preservation depends on the single
[dotprivate-owned selected-files contract](../dotprivate/docs/SELECTED-FILES-CONTRACT.md).
Both consumers pin its released version; this document does not define a second
wire/CLI interface. Each retains its own authorized request and receipt state.
The current `git add -A` path does not meet that gate. Skald scheduling does not
dispatch Heimdall actions. Dotprivate is not Skald's full-archive backup destination.

For retrieval, an optional configured adapter can read selected local dotprivate
documents or validated session exports. Use file/version references for documents
and original source identities for transcripts. Importing the same session from
native storage and dotprivate must not create duplicate supporting evidence.
Git commit/push state does not establish that a retrieved task claim was accepted
or that a WCU recipe is currently valid. Braid remains independently integrated
by each consumer; a shared storage backend is unnecessary for these reads.

## 5. Optional Skald feed

Direct collection is the independence baseline. A later configured feed can
reduce duplicate source scanning when both services are present, while Heimdall
still retains its own selected evidence.

The feed provides a scoped snapshot and a cursor at the same archive boundary,
then revisions, withdrawals and gaps through the shared contract. Heimdall
selects normalized lifecycle/description metadata; full transcript-body export is disabled
for this consumer; explicitly requested bounded description bytes hydrate only
the purgeable evidence table, never event payloads. Native record IDs remain unchanged across direct and feed
delivery. Duplicate versions cause no new Heimdall semantic event.

Feed cursors belong to the Skald archive instance and scope. Native file
checkpoints belong to the direct adapter. Switching transports requires source
reconciliation and compatible namespace mappings, not copying one cursor into
the other. A gap or expired cursor triggers bounded reconciliation; incomplete
coverage stays explicit. An unavailable direct provider path cannot be made
available merely by calling it fallback.

Automatic failover is optional after both transports have passed their tests.
The initial implementation may offer explicit `direct` and `skald` source
modes. Either way, a feed failure cannot stop unrelated Heimdall operations. In
direct mode, its supported conversation features function without Skald.

## 6. Acceptance additions

Add these checks to the applicable existing milestones rather than introducing
a new prerequisite for S2a:

| Slice | Added acceptance |
| --- | --- |
| S1 | With no Skald process or files installed, the direct Claude adapter consumes lifecycle/native-recap fixtures and a verified local transcript through the shared contract. Existing task-binding rules apply after parsing. |
| S1 | Duplicate source delivery and restart produce one semantic observation per source revision; a partial line and ambiguous rotation leave explicit gaps without fabricated end events. |
| S1 | Native recap ingestion works with no inference provider. Missing or opaque summaries remain unavailable. A later source turn updates freshness without accepting or completing anything. |
| S1 | New event/migration fixtures replay deterministically; raw prompts, transcripts and frames are absent from event payloads. Description text is absent from events/receipts/replay state; evidence bytes are bounded and marked. Replay without sources succeeds with a coverage gap; withdrawal purges text/index copies and prevents rehydration from old events. |
| S3 | Retrieved native descriptions remain source claims; accepted decisions/checkpoints retain their different origin. Current authority and target revisions are checked through existing paths. |
| S3 | Braid absence preserves mandatory context and task operations; exact and expanded reads do not mix source/index revisions or widen scope. |
| Optional feed | Source identity agrees with direct capture. Disconnect, expired cursor, snapshot-plus-tail race and transport switching recover without duplicates or silent gaps. |
| S5 | Each advertised Codex/Desktop capability is verified against its installed version. Ephemeral CLI recaps and inaccessible web content are not presented as persisted summaries. |
| Optional preservation integration | Existing manual checkpoint preservation retains its source/mirror/commit/remote distinctions. A Skald export, import or failed push does not change accepted task state or stop local operations. Automated publication waits for the generic selected-files contract and existing action/grant checks. |
| Optional external retrieval | Native and exported copies keep identical origin keys; project notes retain their file revision and provenance. Neither a private Git commit nor a retrieved recap acts as an accepted decision or verification record. |

One concrete authority regression test: ingest a recap saying a task is complete.
Verify that the description is visible as a claim while the task status, contract,
accepted decisions, checkpoint head and verification records remain unchanged.
Then exercise an existing authorized checkpoint/decision command separately.
The source text must not stand in for that command.

The paired Skald acceptance test runs capture, grouping and retrieval with
no Heimdall endpoint. Its local grouping action must not produce a Heimdall task
assignment if a Heimdall instance later becomes available.

## 7. Adoption and implementation sequence

1. Design adopted 2026-09-10: targeted r4, design-index and S1/S3/S5 roadmap
   updates include the digest-only event and purgeable evidence-table correction.
   This does not claim any S1 implementation.
2. Freeze the shared record schema, identity vectors and sanitized provider
   fixtures with Skald L0 or the first Heimdall S1 implementation. Pin the
   reusable adapter release. Keep database DDL and retention application-specific.
3. Implement Heimdall's direct consumer within S1. Preserve the active schema
   sequencing and retain delivered S2a marker 21 and planned S1 marker 22.
4. Consume available normalized descriptions in S3 retrieval without adding a
   mandatory model provider or changing accepted-state authority.
5. Add optional feed/enrichment integration when both independent paths work.
   Complete the capability spikes in the owning product's schedule.
6. Coordinate optional dotprivate automation around its selected-files interface;
   retain the manual workflow until that interface and the owning application's
   action/receipt integration are verified. Skald L6 does not gate Heimdall
   S1 or require Heimdall to store a comprehensive transcript archive.

No existing task-state subsystem is removed, no shared database is introduced,
and no CLI parser migration is required before those future features exist.

## 8. References

- [Skald implementation plan](SKALD-IMPLEMENTATION-PLAN.md), §4
  for source identities and records, §6.4–6.5 for preservation/export boundaries,
  §9.4 for dotprivate retrieval and §11–12 for independent-service tests.
- [Existing manual preservation](../heimdall/docs/PRESERVATION-SETUP.md),
  [dotprivate README](../dotprivate/README.md) and
  [dotprivate implementation](../dotprivate/dotprivate), reviewed at `51d0fce`.
- [Current r4](../heimdall/docs/design/HANDOFF-heimdall-v1-r4.md), [status](../heimdall/docs/STATUS.md) and
  [backlog](../heimdall/docs/BACKLOG.md). These continue to describe actual implementation.
- [Braid simple retrieval](../braid/README.md),
  [dataset publication](../braid/docs/datasets.md),
  [retrieval contract](../braid/docs/retrieval-contract.md) and
  [current Heimdall integration guidance](../braid/docs/heimdall-integration.md).
- [September 9 native recap investigation](/home/david/.codex/visualizations/2026/09/09/01a08794-c3a9-7c92-bc79-e301bbc3471e/session-recap-investigation.md).
  Version-specific availability is summarized in the companion plan; the
  amendment does not assume every native app exposes the same artifacts.

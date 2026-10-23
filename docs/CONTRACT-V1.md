# Session record v1

The module path is `github.com/algorhythmic/skald`.
Its public packages are `github.com/algorhythmic/skald/sessionrecord` and
`github.com/algorhythmic/skald/sessioncapture`. The public packages import no
Heimdall code, database, task state, hooks, model clients or desktop services.
Consumers must pin a published producer release rather than infer compatibility
from a local tag or development checkout. The record and identity encoding below
is version 1; the provider compatibility freeze and publication receipt remain
release work tracked in [STATUS.md](../STATUS.md).

The [Odin producer plan](ODIN-INTEGRATION-PLAN.md) tracks EC-14 preparation;
the first local [capture-delivery contract](CAPTURE-DELIVERY-V1.md) defines its
collector/sink envelope and standalone archive adapter.
The local producer also implements the [archive feed](ARCHIVE-FEED-V1.md),
[capture spool](CAPTURE-SPOOL-V1.md) and [content removal](CONTENT-REMOVAL-V1.md)
contracts. These interfaces still need a published release and matching consumer
pins. The record schema and canonical identity bytes remain unchanged.

## Identity encoding

`Canonical(domain, fields...)` begins with the exact bytes
`sessionrecord\x00v1\x00`. Append the domain and then every field in order, each
preceded by its unsigned 32-bit big-endian UTF-8 byte length. Empty strings have a
zero length. Do not trim, normalize Unicode, fold case or normalize paths here.
SHA-256 of these bytes produces `sr1:<domain>:<lowercase hex digest>`.
[Independent vectors](../testdata/identity-v1.json) include multibyte text and
ambiguous concatenations. Fields larger than uint32 are outside the contract.

| Domain | Ordered fields |
| --- | --- |
| `namespace` | provider, stable nonsecret host identity, canonical source root, nonsecret profile/account or empty |
| `conversation` | namespace, provider-native conversation ID |
| `record` with native ID | origin kind, origin key, `native`, native record ID |
| `record` without native ID | origin kind, origin key, `ordinal`, verified generation, decimal zero-based ordinal |
| `stream` | namespace, provider, configured persistent logical stream ID |
| `generation` | stream key, decimal rewrite epoch, digest of the first complete native line |
| `claude-subagent` | native parent session ID, native agent ID |

A configured namespace override is authoritative and is returned verbatim. Each
consumer must use the same namespace and logical stream token for the same native
source. The source registry persists these before capture and preserves
them through explicit root aliases. Caller filenames and ingest times are not
record identities. L0 inspect requires explicit namespace/stream arguments rather
than silently choosing identities from a path. The namespace helper accepts a
root already canonicalized by the caller and never reads host secrets itself.

A source revision is `sha256:<lowercase hex>` over the complete exact native JSONL
record **including its LF or CRLF terminator**. Original bytes are independent of
normalized bodies. Two identical lines without IDs have equal source revisions
and different occurrence keys. A native-ID record may acquire additional source
revisions without changing its key. A normalization change keeps both identity
and original revision; `adapter_version` identifies its different interpretation.

## Envelope and semantics

[JSON Schema](../schemas/session-record-v1.schema.json) defines the wire shape.
Required nullable fields must be present as null when unknown. Provider versions
and producer identities use the explicit string `unknown` if not established.
`extensions` carries future metadata; unknown core fields are rejected.

The Go validator additionally checks matching conversation/origin scope, raw
references, coverage ordering and digests against supplied bytes. Project/source
notes have no conversation key without proven association. The record envelope
has no accepted task-state fields. Native claims and explicit relations remain
source evidence; no parser interprets prose to invent decisions or handoffs.

Claude compound messages remain one native occurrence with typed text, tool-call,
tool-result or opaque body parts. `body.text` contains only exposed text blocks;
unknown native fields remain in the original bytes. Codex `response_item` messages
are the display-content stream; event-message mirrors stay opaque to avoid
showing one reply twice. Turn completion is idle evidence, never conversation end.
Compaction text does not automatically become an eligible native conversation
recap. No source claims recap coverage solely from a timestamp or nearby turn.

### Native Claude children

Opt-in Claude child discovery requires `sessionId`, `agentId` and
`isSidechain: true` in native records. The child's provider-local conversation ID
is `Key("claude-subagent", sessionId, agentId)`; its conversation key then uses
the ordinary `Conversation(namespace, childID)` encoding. Filenames and directory
names do not establish identity. `Source.ConversationID` explicitly pins this
derived child ID, using the existing Source/checkpoint/delivery-v1 wire shape.

Child normalization uses `claude-subagent-jsonl-v1.0.0`, leaving the primary
adapter version unchanged. Records carrying native child evidence include the
relation `native_subagent_of` targeting `Conversation(namespace, sessionId)`,
with evidence `native_sessionId_agentId_isSidechain`. Extensions retain
`native_parent_session_id`, `native_agent_id`, `native_is_sidechain`, and exposed
`native_parent_uuid`. The relation is conversation ancestry, not accepted task
delegation or proof that the parent has been archived.

Identity-free metadata can be retained opaque under an explicitly registered
child. Malformed lines retain the ordinary gap semantics; neither case invents
per-record native parent evidence. Primary or sibling identity inside a child,
incomplete child markers, and sidechain records inside a primary stream block
before consumption. A child-looking path containing primary records is rejected
by opt-in discovery. Compatibility evidence covers sampled Claude 2.1.296 and
2.1.303 child shapes, not every provider feature in those releases.

Claude control records (`api-request`, `api-request-blob`, `api-request-shape`,
and `fork-context-ref`) may omit the sidechain marker. They remain opaque and
must match the proven child's available native identity fields; they cannot
establish child identity on their own. Agent-only context references require
bounded verification against native child evidence in the stream.

### Codex fork history

The first native `session_meta.payload.id` fixes the rollout's conversation.
When it exposes `forked_from_id`, copied ancestor headers may carry only the
ordered, explicitly named parent IDs. When present, native thread-spawn parent
evidence must agree with a fork edge. Conflicting IDs without that evidence,
cycles and contradictory edges remain capture failures.

The fork adapter `codex-fork-jsonl-v1.0.0` keeps inherited occurrences in the
child's conversation. It emits `native_fork_of` targeting the immediate parent's
conversation key, with native or bounded-prefix evidence as appropriate. This
relation does not prove the parent is archived or identify individual inherited
messages as parent-owned. Copied metadata retains its original native ID and
inherited fields without assigning the ancestor's project to the child. The
ordinary Codex adapter output remains unchanged.

Identity replay is bounded to 256 complete records / 4 MiB and 16 ancestor edges;
it does not add checkpoint or delivery fields. See [source coverage](SOURCES.md#codex-cli-and-forked-rollouts)
for the supported prefix and late-parent behavior.

## Checkpoint boundary

`Read(io.ReadSeeker, Source, Checkpoint, Limits, now)` returns records, original
bytes, gaps and a **candidate** checkpoint. A consumer must persist all accepted
records/gaps and that checkpoint in one transaction. A returned Go error invalidates
the entire batch. A blocked batch can contain earlier complete records and an
unchanged boundary at the blocking line. `pending_partial_line` waits for LF;
`more` means another bounded read is needed. Neither indicates a native session end.

The checkpoint binds namespace/provider/logical stream, generation, rewrite epoch,
complete byte offset, ordinal and exact verified prefix digest. On continuation,
re-hash the prefix before reading; recheck the returned prefix afterward to reject
concurrent rewrites. Prefix-preserving rotation with the same configured stream
keeps the generation. Ambiguous rewrite starts a new generation and emits a
continuity gap. Native-ID occurrences retain their identities where exposed;
ID-less occurrences in an uncertain new generation are not silently merged.

Malformed complete lines are retained as opaque bytes with a gap. Capture can
continue, but `has_gaps` stays true and `parsed_offset` retains its earlier complete
boundary. Unknown record types are losslessly retained with an opaque availability
reason. Oversized lines and missing/conflicting native conversation IDs block
without consuming that line. Historical gaps require consumer reconciliation.

Defaults: 512 records, 4 MiB per line, 16 MiB of source bytes per batch. Memory is
bounded by those source limits plus normalized/JSON representation overhead.
The CLI `--raw` uses base64, so output bytes can exceed input bytes. Full-prefix
rehashing has linear read cost on every batch; it is a conservative L0 correctness
baseline, not an established large-history latency guarantee. The L1 daemon now
uses bounded periodic polling with atomic persistence; no default home-directory
scan or provider hooks are introduced. See [archive setup](ARCHIVE-SETUP.md).

## Archive DDL and dependencies

[Capture migration](../internal/archive/migrations/001_capture.sql) establishes the
application-owned first archive schema with foreign keys, scope constraints,
immutable source/normalization versions and same-database original bytes.
Separate record observations preserve later generation/order evidence without
rewriting a normalization's first envelope or source bytes. The schema-2 daemon
migration adds locators, record heads, native-title projections, generation epochs and
project observations. The schema-3 migration labels each projected title `native`
or `derived`; a derived title is the first line of the earliest substantive user
message, keeps record provenance, and yields to any later native title. The
schema-4 migration projects per-conversation activity: the latest native
lifecycle signal or unanswered input request, the newest observed record
position and source time, and an ordering-ambiguity label across streams.
Schema 5 additionally labels continuation-derived session titles. Schema 6 adds
atomic capture and redaction receipts and persisted archive read scope.
A sole-writer service now commits capture batches, serves
reads and creates/restores consistent backups. Later group/surface/description/
index tables belong to their owning milestones. Suppression tombstones and
content removal block reingestion; see the [removal contract](CONTENT-REMOVAL-V1.md).

Go language baseline: 1.25.0; tested toolchain: 1.27.1. The `sessionrecord`
package uses only the standard library. The
`sessioncapture` package also imports `modernc.org/sqlite` for Devin store capture;
the daemon uses the same pinned `modernc.org/sqlite` v1.38.2 dependency and its
transitive dependencies. The record types and identity encoding do not depend
on SQLite.
`github.com/google/jsonschema-go` v0.4.3
is pinned for independent JSON Schema conformance tests; module checksums are in
`go.sum`. SQLite requires 3.37 or later for STRICT tables; standalone DDL checks run
against system SQLite 3.53.4, and the archive suite exercises the pinned Go driver.
The terminal library will be pinned with L2. No mandatory peer service is added.

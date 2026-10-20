# Capture delivery v1

This is Skald's first local producer contract for OHS-03/EC-14 and the
collector-to-sink portion of OHS-07. It defines a narrow boundary around
`sessioncapture.Read`, `Source`, `Checkpoint` and `Batch`, and a standalone
archive adapter. It does not establish a completed EC-14 agreement, an Odin
consumer pin, or permission to ingest real history into another destination.

The [delivery schema](../schemas/capture-delivery-v1.schema.json) describes
the envelope; `#/$defs/acknowledgement` describes its receipt. Resolve the
relative [record schema](../schemas/session-record-v1.schema.json) from the
same pinned checkout. The schema's `.invalid` identifier is a stable local
identifier, not a service. The [record contract](CONTRACT-V1.md) retains
authority for source identity, source revisions and record meanings.

## Producer and sink API

```go
batch, err := sessioncapture.Read(input, source, previous, limits, observedAt)
// A read error invalidates the whole candidate.
delivery, err := sessioncapture.NewDelivery(source, previous, batch)
ack, err := sessioncapture.Deliver(ctx, sink, delivery)
// Advance only sink.Destination() after a valid acknowledgement and no error.
```

The abbreviated example omits Go error handling; every error must be checked.
`Sink` exposes `Destination() string` and
`Accept(context.Context, Delivery) (Acknowledgement, error)`.
`Delivery.Validate` checks the typed boundary; `DecodeDelivery` also rejects
missing fields, unknown or incorrectly cased core fields, duplicate core keys,
unexpected nulls, trailing JSON and unsupported envelope versions. Native JSON
inside record extensions and body part data remains opaque, including duplicate
keys and large numeric lexemes. Record v1 validation and exact raw-byte digest
verification still apply.

`Store.CaptureSink(destination, registration)` binds a standalone archive sink
to the complete configured `Source`. Construction does not enroll a source or
grant access. The collector supplies an already registered source. The archive's
existing process lock and `Ingest` transaction continue to enforce one writer
and atomic application. These new boundary files add only standard-library and
`sessionrecord` dependencies; the existing `sessioncapture` package also contains
the Devin SQLite adapter and therefore already depends on `modernc.org/sqlite`.

## Identity and version pinning

Envelope `version`, checkpoint `version`, and record `contract_version` are
separate version axes. This implementation accepts delivery v1, checkpoint v1
and record v1. Only `previous` may be the entirely zero-valued checkpoint,
which means no accepted prefix. A candidate checkpoint is always v1. Each
consumer must pin the producer revision, schema bundle and adapter support;
there is no negotiated fallback for an unknown major version.

`batch_id` is `sessionrecord.Key("capture-batch", encodedEnvelope)`. To obtain
`encodedEnvelope`, copy the typed Go `Delivery`, set `BatchID` to the empty
string, and encode it with `encoding/json.Marshal`. The pinned type definitions
specify field order and omission rules; map keys use Go JSON's sorted order.
JSON strings use its escaping, timestamps use the typed `time.Time` encoding,
and raw JSON subtrees use its compaction/escaping while preserving key order,
duplicate keys and number lexemes. This is a producer-specific encoding, not
RFC 8785. Other-language consumers should retain the exact producer envelope
and reproduce the pinned encoding before independently checking the ID.
The synthetic fixture supplies a known identity vector.

Identity binds the entire candidate: source, prior checkpoint, records and exact
raw bytes, gaps, candidate checkpoint, observation times and read status. It is
independent of destination, path/locator and delivery attempt. Persist the
complete envelope before retrying it. Running `Read` again at a different time
can produce a different identity even for the same source records; record keys
and source revisions still provide archive deduplication. Never mutate an
envelope after assigning its identity.

## Atomic acceptance and independent destinations

A sink returns an acknowledgement only after all accepted records, gaps and the
candidate checkpoint commit atomically. The receipt names the destination,
batch ID, source key and exact candidate checkpoint. `Deliver` validates that
binding and discards any receipt accompanied by an error. A receipt for one
destination cannot advance another destination. Registration, destination names,
reader authorization and writer ownership are caller/sink responsibilities;
a valid hash alone conveys no authority.

The standalone adapter calls existing `Ingest`. Its compare-and-swap check
requires the stored checkpoint to equal `Delivery.Previous`. Any failure before
commit rolls back records, gaps and checkpoint. A lost response after commit
is uncertain acceptance from the producer's perspective. Retrying an advancing
batch with its old previous checkpoint fails with `archive.ErrCheckpoint`;
the adapter does not manufacture a receipt merely because the stored checkpoint
matches the candidate. Empty batches whose checkpoint does not advance may be
accepted repeatedly under the existing archive transaction semantics.

For standalone recovery, explicitly read the archive's committed checkpoint and
resume `Read` from that checkpoint. This reconciles capture progress without
asserting that an old receipt was recovered. The archive's existing identity
constraints avoid duplicate records/changes on resumed reads. An external
multi-destination transport needs its own durable spool and receipt/reconciliation
policy before it can safely discard pending delivery. There is no exactly-once
delivery promise or durable batch-receipt ledger in this slice.

## Boundaries, gaps and corrections

The source key binds namespace, provider and logical stream. Record provider,
namespace, conversation, generation and consecutive ordinals must match the
delivery. Candidate offsets must account for exactly the enclosed LF-terminated
raw lines. Checkpoints cannot regress within an epoch. A rewrite increments the
epoch by one, resets the captured prefix, and includes the exact prior boundary
as a `continuity_gap`. Consumed parse gaps freeze `parsed_offset` and preserve
`has_gaps`; blocking gaps do not consume their line. Initial/rewrite full-prefix
digests can be verified from the enclosed raw bytes; continuation prefix
verification remains the reader's job because the envelope omits earlier bytes.

`pending_partial_line`, `more` and `blocked` retain the reader's meanings. A
source-activity deferral can carry no records and the same already valid v1
checkpoint. These flags, activity timestamps and archived changes are evidence
about capture, never task ownership, accepted obligations or verified completion.

Native-ID corrections retain the record key and change the source revision.
Uncertain rewritten occurrences retain existing generation rules. Missing source
files do not delete archived evidence. No new removal/redaction event or public
archive change cursor is defined here. Internal `changes` rows are not an API.

The reader's existing maxima remain 10,000 records, 64 MiB per line and 128 MiB
of raw bytes per batch. Transport JSON is additionally limited to 256 MiB,
including base64 and normalized content; choose smaller read limits if needed.
Validation and hashing scan/encode the envelope at construction and sink
boundaries. This favors correctness for the first integration; large-envelope
CPU/memory optimization and performance evidence remain work to do.

## Synthetic checks and remaining work

[Shared synthetic cases](../testdata/capture-delivery/scenarios-v1.json) cover
atomic acceptance, interrupted acceptance, stale checkpoint retry and unsupported
delivery version. [The accepted fixture](../testdata/capture-delivery/accepted-v1.json)
contains invented conversation text and stable identities. Go tests exercise
schema conformance for real emitted envelopes and acknowledgements, independent
destinations, source/order/digest failures, native JSON round trips, rewrite gaps,
transaction rollback and explicit checkpoint reconciliation.

Still pending: Odin-owned consumer pins and fixture checks; durable local spool,
backlog and receipt storage; multi-sink outage/backfill/reconciliation; a published
incremental archive changes API and cursor invalidation; removal/redaction and
retention receipts per destination; live feed integration and full OHS-07 gates.
The standalone archive and browser remain independently usable.

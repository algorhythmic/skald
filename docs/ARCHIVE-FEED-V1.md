# Archive metadata feed v1

`archivefeed` defines the public Go types and version 1 JSON shape for a scoped,
bounded archive changes reader. [The schema](../schemas/archive-feed-v1.schema.json)
and [synthetic scenarios](../testdata/archive-feed/scenarios-v1.json) plus a
[sample page](../testdata/archive-feed/page-v1.json) are shared
consumer inputs. The feed contains metadata only; normalized bodies, original
bytes, titles and transcript excerpts never appear in events. An authorized
consumer resolves a `locator` through the existing `/v1/record` exact-read
transport using all three fields: `record_key`, `source_revision` and
`adapter_version`. The same read scope must authorize that exact read.

`Store.Changes(ctx, namespaces, scopeGeneration, cursor, limit)` accepts a
server-authorized set of namespaces, an integer authorization/configuration generation,
an optional cursor and a limit from 1 to 100. The server owns authentication and
generation changes; this method does not grant access by itself. A new request
captures an archive-instance-specific high water sequence. `snapshot` pages
replay committed metadata observations in sequence through that fixed high
water. This is a history snapshot, including corrections, gaps and prior health
transitions, so a consumer can reconstruct state without silently dropping
provenance. After the prefix is exhausted, the cursor changes to `tail`, where
new committed observations are read. A write after pagination began cannot
move or expire the snapshot boundary. Empty tail pages return a cursor for
subsequent polling. `more` says whether another event is already available in
the current phase. Page items are sorted by sequence; sequence has meaning
only within one archive instance, and gaps in visible sequence numbers are
normal when other namespaces changed.

The opaque cursor binds the archive instance, exact sorted namespace scope,
server-provided integer scope generation, initial high water, phase and last read
sequence. Changing the archive instance on restore, changing read scope or
generation returns `archive_feed_cursor_expired`. Malformed cursors return
`invalid_archive_feed_cursor`; an unknown cursor major returns
`unsupported_archive_feed_version`. These are distinct from a successful empty
page. Consumers restart with an empty cursor after expiry and reconcile by
stable record/revision identity. Consumers must persist their own cursor and
deduplicate replay; this interface does not acknowledge delivery or confer
task authority. The version 1 endpoint does not offer a retention lease; if
event pruning is introduced, it must expire cursors that cross the pruned range.

`record` introduces exact evidence. `correction` names the same record key at a
new source revision and does not erase the old revision. Both always carry
top-level `record_key` and `source_revision`, including when the exact locator
has become unavailable. `gap` carries capture
code, source offset, generation and ordinal; it is evidence of a discontinuity,
not a fabricated missing record. `source_health` reports a source health or
configuration observation. `source_removed` says native capture is no longer
configured; retained archive evidence can still resolve. `redaction` is an
authorized archive withdrawal with source, conversation or record scope and
does not mean native file loss. An earlier record event whose exact evidence
was subsequently redacted sets `unavailable: "redacted"` and omits its locator
when replayed. An absent normalization without an authorized tombstone reports
`unavailable: "evidence_missing"`, which calls for archive repair or operator
investigation rather than implying a policy decision. A consumer must treat any exact-read unavailable response as
authoritative even when it previously held a locator.

Skald's internal SQLite layout, source file paths, capture delivery IDs and
checkpoint rows are outside this wire contract. Source identity, record keys,
source revisions and native ordering retain the meanings in
[the record contract](CONTRACT-V1.md). Feed version 1 does not include a
cross-destination acknowledgement or a promise of exactly-once delivery.

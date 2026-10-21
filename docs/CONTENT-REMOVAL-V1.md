# Content removal v1

Skald can withdraw captured content at source, conversation or record scope.
The archive operation is `Store.Redact(ctx, namespace, scopeKind, scopeKey)`.
The target must belong to the named namespace. A source target uses its namespace
as `scope_key`; conversation and record targets use their archived keys.
Unknown targets and cross-namespace targets are refused.

One SQLite transaction inserts a `purge_tombstones` suppression rule, a
`withdrawal` change with `reason: "redacted"`, and a durable
`redaction_receipts` row. It deletes targeted original bytes, normalization
envelopes, revision rows, observations, record heads and derived title, project
and activity projections. A content object shared by another retained record
is kept until no artifact version references it. The artifact and session
identity rows and change hashes remain, so an old exact reference can explain
why content is unavailable. The change feed emits a redaction event with the
scope kind and key. Native source removal is a separate event and does not
redact archived content.

The version-1 receipt is specified by
[redaction-receipt-v1.schema.json](../schemas/redaction-receipt-v1.schema.json).
The [synthetic receipt](../testdata/redaction-receipt/receipt-v1.json) is a
consumer fixture.
It records archive instance, namespace, scope, target, withdrawal sequence,
bounded record/version/object/byte counts and removal time. It contains no
conversation body. Retrying the same target returns the identical stored
receipt and emits no second withdrawal. The suppression rule survives backup
and restore; attempted future capture of the target is refused. A restored
archive has a new archive instance for new cursors, while its earlier receipt
retains the instance where removal was committed.

This is logical removal from the current archive and backups made afterward.
SQLite `secure_delete` is enabled for the delete transaction, but prior
backups, WAL history, filesystem snapshots, physical media, and already
delivered downstream copies may retain bytes. Operators must manage those
copies separately. The receipt does not claim physical erasure or downstream
acknowledgement.

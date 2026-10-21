# Capture spool v1

`capturespool` is Skald's local durable boundary between `sessioncapture.Read`
and one or more named sinks. It stores the complete immutable delivery envelope
and advances the producer checkpoint in the same SQLite transaction. A collector
must call `Enqueue` successfully before reading beyond the candidate checkpoint.
The stored `batch_id` remains stable across restarts and retries. The spool is
an independent private database, not a table in the archive.

The caller opens one directory with an exact destination registry and positive
`Limits{MaxPending, MaxBytes}`. Both limits count envelopes awaiting at least
one destination receipt; bytes count encoded JSON envelope bytes. `ErrCapacity`
means the collector must stop advancing and report backlog. No envelope is
silently dropped. A changed registry, including a newly added destination, is
refused at open and enqueue. Backfill or destination retirement requires an
explicit migration with its own scope and evidence; this version has no implicit
inheritance of old progress.

`Open` requires a private, owned directory and files, obtains a nonblocking
writer lock, validates the SQLite database, and uses full synchronous WAL
commits. Only one writer can own the directory. A failed or corrupt open does
not reset the spool. The directory and database should be backed up together
with the archive when retaining a delivery history matters.

`BindDestination(destination, instance)` pins one configured sink name to a
durable instance identity. Calling it again with a different identity returns
`ErrBinding`, including after restart. The standalone daemon binds its archive
instance at initialization so an older restore or replacement archive cannot
silently inherit a spool's later capture progress. Binding is opt-in for other
sinks that have their own identity and recovery policy.

To attach a new spool to an archive that already has a committed checkpoint,
call `SeedCheckpoint(source, checkpoint)` once before enqueueing that source.
It refuses an already tracked source. The seed initializes producer and named
destination progress from the same known archive position; seeding is limited
to a one-destination registry. A collector should
only seed from an explicitly verified, matching source checkpoint. For a new
source, enqueue its first delivery with a zero previous checkpoint. Thereafter
the prior checkpoint must exactly equal the spool's retained producer progress.
`Checkpoint(sourceKey)` returns that progress after a restart; it can be ahead
of every sink while deliveries remain pending.

`Deliver(ctx, sink, maxBatches)` attempts bounded work for one configured sink.
It passes the stored envelope to `sessioncapture.Deliver` and commits a receipt
only after that boundary validates the destination, batch ID, source key and
candidate checkpoint. An error, including a returned receipt plus an error,
leaves that destination pending. This is an at least once retry boundary: a
sink must make a previously committed batch retry return its durable receipt
if it wants to recover a lost response. Acknowledgement for one destination
does not advance another. Deliveries stay ordered by source for each destination.
A failed source does not block attempts on unrelated sources in the same drain.
The method returns the number of committed receipts and a joined error for any
failed attempts; callers should inspect both.

After all configured destinations acknowledge a delivery, its raw envelope and
per-delivery receipts are deleted. The spool retains the batch ID with a digest
of the original envelope, plus producer and destination checkpoints. An
identical batch replay is idempotent even after pruning; a reused ID with
different content is refused. This metadata is a local retry guard and progress
record, not a substitute for a sink's durable atomic acceptance receipt.

`Status` returns pending envelope count, encoded byte count, per-destination
pending counts and configured limits. `StatusForSource` limits those counts to
one source key; `StatusForNamespace` counts pending envelopes from every source
in a named namespace, including sources no longer in the current configuration.
The caller must authorize the namespace before exposing the latter result.
Neither status
surface includes conversation content. The [synthetic scenarios](../testdata/capture-spool/scenarios-v1.json)
describe the restart, capacity, failure and registry refusal cases exercised by
the package tests. This local contract does not grant reader authority, select
real history for another destination, or claim Odin acceptance.

The configured limits bound live pending envelope payloads, not total SQLite
file size. Completed batch IDs, envelope digests and checkpoints remain durable
and can grow over a long lifetime; WAL and page overhead are outside the byte
counter. A completed envelope is logically deleted when all destinations ack,
but SQLite page reuse and WAL checkpointing do not constitute verified physical
erasure. A redaction workflow must treat spool storage and its backups as
separate retention surfaces if physical sanitization is required.

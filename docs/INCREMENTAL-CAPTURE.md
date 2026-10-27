# Prefix-state acceleration and integrity

The collector retains a disposable private SHA-256 prefix state under
`<data-dir>/prefix-checkpoints`. Each file binds the source/checkpoint digest,
offset, device/inode and observed file size. Missing, corrupt, stale, rotated,
truncated or inaccessible cache files fall back to conservative capture. Public
checkpoint/delivery formats and source identities are unchanged. The cache is
not evidence, acknowledgement or a substitute for an archive checkpoint.

For a valid state, continuation restores SHA-256 state and parses from the
accepted byte offset. It then verifies the entire newly captured prefix once
before acceptance. This removes the old initial prefix-hash pass, while keeping
exact detection of arbitrary earlier rewrites, including a rewrite outside any
bounded end anchor followed by an append. A mismatch retries the ordinary
continuity-gap/rescan path; concurrent mutation still fails closed. Inode changes
force conservative validation, including byte-identical rotation.

Pure tail-only reads cannot preserve that integrity contract for arbitrary
mutable files: size, timestamps, inode identity and a bounded anchor do not prove
that earlier bytes are unchanged. This change deliberately retains one full
verification pass rather than silently weakening provenance. Provider-proven
append-only semantics or a new explicitly accepted continuity policy would be
needed to eliminate that pass. The existing one-minute unchanged-file cache is
unchanged. This is a reduction in prefix-read amplification, not a claim of zero
idle CPU or constant-time continuation.

Serialized hints contain only full-block hash state, excluding pending source
bytes. At most 63 checkpoint-boundary bytes are reread to restore the partial
block. Cache directories/files remain private (0700/0600); do not publish or
include them in public receipts. Cache writes are atomic hints; loss does not lose captured
records. Each hint is matched to the exact accepted checkpoint before reuse.

`BenchmarkPrefixContinuation` compares the original and accelerated paths on
the same generated transcript and appended line. It reports source read bytes
per operation as well as allocations and elapsed time; it does not predict the
live daemon's CPU percentage. Existing raw payloads remain readable; new archive
payloads use schema-8 compression automatically. Converting older rows remains
an explicit management operation, never startup work.

`BenchmarkPrefixPoll` additionally reports process CPU nanoseconds per changed
file/audit capture call for append and unchanged-input cases. It compares the
full-verification behavior at the base revision with the persisted-state path
on identical generated inputs. It includes runtime/GC CPU but excludes filesystem
I/O and the collector's already-existing stamp-cache fast path. Run benchmarks
without concurrent builds/tests before interpreting their CPU figures.

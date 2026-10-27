# Sanitized capture delivery v2

The producer replaces detected secret spans before a record leaves capture memory.
Native `Read` verifies the original source prefix for truncation/rewrite/rotation;
its checkpoint offsets and prefix digest still describe that native source. Raw
source bytes are discarded after verification, never included as a recovery field.

A sanitized captured item has `redaction` metadata, also persisted in the normalized
record's `extensions.skald_redaction`: version1, applied=true, digest of sanitized
raw bytes, native_bytes, sorted rule IDs, and positive counts per rule. Counts cover
both native and normalized surfaces and may count the same credential twice.
There are no per-secret hashes. Record source_revision and raw_ref point to the
sanitized raw digest; delivery identity covers the complete sanitized envelope.

Offsets advance by native_bytes for sanitized items and raw length otherwise.
Version2 is mandatory for a delivery containing any sanitized item. Version1 clean
and historical deliveries remain accepted. Acknowledgements retain their separate
version1 shape and echo the delivery identity and original-source checkpoint.
Replay validates sanitized byte digests, metadata shape/equality, source/order and
native boundaries. The sink cannot reconstruct an original native prefix from
redacted bytes: native continuity is an explicit collector trust boundary for v2,
not a claim that sanitized bytes hash to the native prefix. Losing this distinction
would either leak original bytes or reject legitimate redacted records.

Malformed metadata and detector exceptions/depth-limit errors fail closed using a
constant error without raw content. Positive matches produce records and advance
checkpoints. Existing capture/batch byte limits bound work; no unbounded background
detector or timeout goroutine is introduced. Legacy sinks reject v2 explicitly and
must be upgraded before enabling sanitized producers.

Legacy archive exact reads can return a sanitized representation with a different
source_revision than the requested native revision. The extension marks this
explicit derivative; verify its digest against returned sanitized raw bytes.
Historical rows are not rewritten. JSON/HTTP/MCP/CLI outputs have a shared final
projection filter, including base64 raw fields. Backups are fresh sanitized logical databases, not byte-preserving copies.
Unused pages/WAL are omitted, content references remapped, and changed archives
receive a fresh generation. Compressed manifests set redaction_version=1;
OriginalSHA256 identifies the sanitized uncompressed payload. Older manifests
remain readable, but only newly generated exports have this sanitation guarantee.

## Private checkpoint metadata

Free-form provider-version strings are filtered before checkpoint persistence.
A detected sensitive conversation ID becomes a stream-local
`[REDACTED:checkpoint_identity]:<existing-stream-key>` marker. This surrogate
contains neither the ID nor a fingerprint of it. Scope keys for new records use
that public identity. Transport sources, previous checkpoints and acknowledgements
use the same representation. Ordinary nonsensitive harness IDs are unchanged.

Native identity comparisons stay in adapter memory. On resume, the reader uses
explicit private source context, the bounded Codex root probe, or the first bounded
Claude identity line to recover it, then verifies the exact native prefix before
acceptance. An unrecoverable native identity is an adapter-context error with no
source text in its diagnostic. Private IDs are stream-local; cross-stream aliasing
of a sensitive ID is intentionally not inferred. Existing prefix/content digests
remain integrity fields under the native-continuity contract, not per-secret
fingerprints or redaction receipts.

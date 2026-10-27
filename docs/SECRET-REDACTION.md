# Secret redaction (work in progress)

The shared `internal/secretredact` policy uses linear-time Go regular expressions
and `[REDACTED:<pattern-id>]` markers. No marker contains secret bytes or hashes.
Detection is best effort: unknown formats, split/encoded values and image content
can evade text patterns. Detected credential fields include passwords, API keys,
access/refresh tokens and authorization values; token patterns cover GitHub,
provider keys, AWS access IDs, bearer tokens and complete PEM private keys.

Current capture safety gates reject detected sensitive records before durable
archive or spool writes. HTTP, MCP and CLI replies containing detected secrets
are withheld. This is intentionally a transitional **fail-closed** implementation:
a sensitive source can block further capture, and an existing sensitive record
can block a requested reply. It does not yet substitute sanitized capture bytes.
Do not deploy it as the completed redaction feature. Byte-preserving backups are not yet protected by these gates. Exact archive
reads and retrieval snapshot construction reject sensitive envelopes before
clipping or materializing Braid caches. TUI text has a replacement filter. Existing
Braid cache files still require explicit invalidation.

Capture map: Claude (including subagents), Codex (including forks), and Devin
snapshots converge through `sessioncapture.Read`/`ReadSnapshot` and normalization.
`internal/daemon/collector.go` delivers through the capture spool or archive sink;
`capturespool.enqueueBatch` durably buffers deliveries; `archive.validateIngest`
checks direct and delivered ingestion before transactions. Native payloads and
normalized envelopes are compressed in `archive/payload_compression.go`; titles,
projects and activity are derived inside `archive/ingest.go`. Archive reads feed
HTTP, MCP, CLI/TUI and Braid retrieval/context. Database backup/compressed backup
is a separate byte-preserving export path, not a transcript JSON projection.

Sanitized capture requires a new explicit representation carrying original byte
boundaries separately from sanitized raw bytes. Revisions must hash sanitized
bytes; collector prefix verification must remain over the native source. Do not
reuse a native revision for changed bytes or silently weaken delivery validation.
Legacy consumers need explicit version rejection/negotiation. Historical scrub
must be separately authorized; this change never edits historical rows.

## Historical scrub and downstream ingestion

Prefer a sanitized replacement archive generation over mutating bytes under old
content digests. Rebuild projections, indexes and feed references consistently;
retain native capture boundaries separately from sanitized payload lengths. Hold
Muninn ingestion until that generation is verified; invalidate any previously
consumed downstream data and Braid caches under an explicit recovery plan.
Quarantining originals requires restricted encrypted storage and operator-owned
retention. Plaintext in old backups, WAL and btrfs snapshots survives a logical
row scrub; authorize retirement separately after a sanitized restore test. No
historical scrub, backup retirement or deployment is performed by this change.

# Secret redaction

`internal/secretredact` supplies shared linear-time RE2 rules and deterministic
`[REDACTED:<rule-id>]` markers with no secret bytes or per-secret hashes. Patterns
cover GitHub/provider keys, AWS access IDs, bearer tokens, password/key/token
assignments, sensitive JSON keys and complete PEM private keys. Detection remains
best effort: unknown formats, encoded/split values and image text need expanded
policy. Markers describe detected patterns, not confirmed working credentials.

Positive matches are **redact-and-store**, never a reason to drop a record or
stall its checkpoint. Sanitized capture delivery v2 separates native byte lengths
and collector-verified native prefix checkpoints from sanitized content digests.
See [the v2 contract](CAPTURE-DELIVERY-V2.md). Clean v1 input remains supported.
Detector exceptions, depth-limit failures or inconsistent sanitized metadata use
a constant fail-closed error, without echoing source text. Existing capture size
limits bound detector work; no unbounded timeout goroutine is spawned.

Password precision excludes empty values, shell variable references, angle-bracket
placeholders, all-asterisk masks, `changeme`, `password=password`, and common type
annotations such as `str`, `string` and `Optional[str]`. This is lexical filtering;
it does not authenticate values or prove they were never used as credentials.

Claude (including subagents), Codex (including forks) and Devin snapshots converge
through `sessioncapture.Read`/`ReadSnapshot`. Records are sanitized before delivery
or spool persistence. Direct archive ingestion sanitizes too, then derives titles,
projects and activity from sanitized envelopes. Native payload compression stores
sanitized bytes. Legacy exact reads return explicit sanitized derivatives without
rewriting historical rows. HTTP/MCP/CLI projections include a final shared filter
for text and base64 raw fields; TUI text is filtered before wrapping. Retrieval
snapshots sanitize before materializing new Braid caches. Existing cache files
require a separate invalidation plan.

## Remaining coverage / historical scrub

Legacy title/project reprojections sanitize values before newly writing labels;
existing derived rows are not rewritten. Backups and compressed exports rebuild a fresh logical database and sanitize
rows before insertion. They omit unused source pages/WAL and remap content
references. If logical content changes, a fresh archive generation invalidates
old consumer cursors; original files remain unchanged.

Prefer a sanitized replacement archive generation over mutating bytes under old
content digests. Rebuild projections, indexes and feed references consistently;
retain native boundaries separately from sanitized lengths. Hold Muninn ingestion
until that generation is verified and invalidate previously consumed downstream
data under an explicit recovery plan. Quarantine originals in restricted encrypted
storage with operator-owned retention. Plaintext in old backups, WAL and btrfs
snapshots survives a logical scrub; authorize retirement separately after a
sanitized restore test. This change never rewrites historical archive rows.

## Backup bounds

Rows are streamed individually. Revision remapping uses a private disk-backed
scratch index with a 1 MiB SQLite page-cache target, committed in batches of128
mappings. Destination index sorts may spill sanitized keys to disk. Scratch files
contain only whole-content digests and are removed on success or failure.
The default maximum is2,000,000 content objects. Crossing it returns the stable
`backup_object_limit` error without publishing a backup; capture history is not
evicted and the source is unchanged. There is no unbounded in-memory fallback.
Tests configure much smaller limits/batches to exercise all boundary cases quickly.

Compressed backups require the `zstd` executable on PATH. A missing executable
returns the stable `backup_zstd_unavailable` error; execution failures return
`backup_zstd_failed`, without subprocess diagnostics or executable paths. Neither
failure publishes a backup. Private output directories remain mandatory.

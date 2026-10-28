# Migration snapshot retention

Each schema transition of an existing archive creates its pre-migration snapshot,
compresses it, and verifies it before running that transition's DDL. Empty archive
bootstrap has no previous data to retain. Multi-version upgrades retain one
compressed snapshot for each transition. Existing historical snapshots are never
scanned, replaced or pruned by this rule.

Migration snapshots use an internal exact-byte compressor with the existing
CompressBackup version1 zstd/manifest format. Unlike public CompressBackup exports,
it does not perform another logical rebuild: the decompressed bytes must match
both the manifest size/SHA-256 and the raw snapshot size/SHA-256. Public backup
export sanitation remains unchanged. Exact snapshot manifests use redaction_version0
(no additional transformation by the compressor).

Output and manifest are staged privately next to their destination, synchronized,
and atomically renamed individually. Both are durable before raw deletion. A
process stop between the renames retains the raw file; restore refuses a pair
without its manifest. Partial staging files are removed on ordinary error returns.
An uncatchable process/power stop may leave a private staging directory or an
incomplete final pair; raw recovery remains, and neither is automatically pruned.

Any compression, manifest, verification, publication or I/O error keeps the raw
snapshot and stops the pending migration with migration_snapshot_retention_failed.
Previously completed transitions remain completed. Reopen retries from the actual
stored schema version; no unsafe rollback or deletion of historical files occurs.
Zstd must be installed; missing compression support blocks the upgrade safely.
No schema bump or startup rewrite is introduced by the retention mechanism itself.

Logical file-size ratios describe compression, not filesystem space recovery.
Btrfs zstd may already compress the raw file's extents, and snapshots/reflinks may
retain shared blocks. Measure df before/after the next authorized real migration
after normal writeback; synthetic logical ratios are not physical savings claims.

# Capture delivery fixtures

All conversation text, namespaces and identities are synthetic. No source paths
or real histories are included.

- `accepted-v1.json` is a complete envelope produced by `Read` and `NewDelivery`
  at `2026-10-19T00:00:00Z`. It is also a known batch identity vector.
- `unsupported-v2.json` changes only the outer version to 2. A consumer must
  reject the unsupported envelope before attempting acceptance.
- `scenarios-v1.json` defines independent archive acceptance scenarios. Start
  each with a fresh destination and register the envelope's source using a
  synthetic locator. `seed` accepts the envelope first, then discards the
  response to simulate a lost acknowledgement. `interrupt_checkpoint` injects
  failure at the checkpoint write, after record writes but before commit.

For `acknowledged`, the receipt must match destination, batch ID, source key and
candidate checkpoint. For `rolled_back_without_acknowledgement`, neither records
nor checkpoint may remain. For `checkpoint_conflict_without_acknowledgement`,
the initial accepted records/checkpoint remain unchanged and no receipt is
invented for the retry. Reconcile by reading the stored checkpoint, then resume
capture from it. `unsupported_delivery_version` makes no destination changes.

Run `go test ./sessioncapture ./internal/archive` with the project's configured
Go toolchain/caches. The public package tests validate both wire schemas and the
fixture identity; archive tests exercise atomic rollback, stale retry and
checkpoint reconciliation against SQLite. Cross-repository Odin tests and
consumer pins remain pending.

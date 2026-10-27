# Local producer candidate bundle

`python3 scripts/producer_bundle.py --output-dir /absolute/artifacts` snapshots
public `sessionrecord`, `sessioncapture`, `archivefeed`, and `capturespool`
packages, their tests, sanitized fixtures, schemas, contract documents and the
standalone fixture consumer. It creates a deterministic gzip/tar artifact with
a SHA-256 filename, a checksum sidecar and `PRODUCER-BUNDLE.json` listing exact
file digests. Existing artifacts are never overwritten. The manifest names the
current base Git revision but identifies these working-tree bytes as an
`untagged_candidate`; the base revision is not a commit of dirty changes.

After extracting a trusted artifact into a fresh directory, run:

```
go run ./cmd/producer-verify --bundle-root . --expected-manifest-sha256 MANIFEST_DIGEST
go test ./sessionrecord ./sessioncapture ./archivefeed ./capturespool ./cmd/producer-verify
```

Record the manifest digest from the producer's separately obtained
receipt. The verifier checks every manifest file digest, canonical identity
vectors, accepted delivery, unsupported delivery/feed major versions and four
contract schemas. Schema resolution uses local bundled files and never downloads
schema URLs. It emits a JSON fixture-compatibility receipt explicitly denying
publication verification and runtime-consumer acceptance. Without an expected
manifest digest it verifies fixture compatibility without checking an external
pin. A checksum does not establish who published an artifact.

Run `python3 scripts/test_producer_bundle.py` for deterministic packaging checks.
No command publishes a tag, uploads a module, edits a consumer pin or installs a
service. Rebuild after public contract changes; the digest changes automatically.
Odin's current repository is a documentation scaffold: its README and AGENTS.md
state that no service implementation or runtime acceptance tests exist. This
fixture consumer makes compatibility review executable while preserving that
runtime gate. Odin must separately adopt a published release, implement its
sink, and record its own outage, scope, redaction, restore and delivery acceptance.

## Tagged release-candidate preparation

After the owner creates a release commit and exact version tag, use:

```
python3 scripts/producer_bundle.py --output-dir /absolute/artifacts --release-tag v0.2.0-rc.1
```

This mode requires that the named tag resolve to the current HEAD, that every
selected bundle file be tracked, and that the working tree and index have no
diff from HEAD. Every selected file is checked against its committed blob before
packaging. The manifest then uses `release_candidate`, `release_tag` and the exact
`source_revision`. The default mode continues to identify untagged working-tree
candidates. A tag-shaped string alone is insufficient for release-mode creation.
The `v0.2.0-rc.1` tag is planned here; this documentation does not create it or
claim publication.

A consumer can pin both the separately obtained manifest digest and tag:

```
go run ./cmd/producer-verify --bundle-root . --expected-manifest-sha256 MANIFEST_DIGEST --expected-release-tag v0.2.0-rc.1
```

The tag option requires the manifest digest pin. The verifier reports the claimed
release tag and exact source revision, while keeping `publication_verified` and
`runtime_consumer_acceptance` false. Establish remote publication and Odin's
consumer acceptance through their separate owner receipts. Public interfaces
remain version 1 and the archive schema remains version 8; the product prerelease
version is `0.2.0-rc.1`.

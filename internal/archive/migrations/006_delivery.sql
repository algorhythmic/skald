-- Atomic sink receipts, content-removal receipts and persisted reader scope.
BEGIN IMMEDIATE;
CREATE TABLE capture_receipts (
 destination TEXT NOT NULL,
 batch_id TEXT NOT NULL,
 source_key TEXT NOT NULL REFERENCES streams(stream_key),
 acknowledgement_json TEXT NOT NULL CHECK(json_valid(acknowledgement_json)),
 accepted_at TEXT NOT NULL,
 PRIMARY KEY(destination,batch_id)
) STRICT;
CREATE TABLE redaction_receipts (
 namespace TEXT NOT NULL REFERENCES sources(namespace),
 scope_kind TEXT NOT NULL CHECK(scope_kind IN ('source','conversation','record')),
 scope_key TEXT NOT NULL,
 receipt_json TEXT NOT NULL CHECK(json_valid(receipt_json)),
 PRIMARY KEY(namespace,scope_kind,scope_key)
) STRICT;
CREATE TABLE archive_scope (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 generation INTEGER NOT NULL CHECK(generation>=0),
 namespaces_json TEXT NOT NULL CHECK(json_valid(namespaces_json))
) STRICT;
INSERT INTO archive_scope VALUES(1,0,'[]');
CREATE INDEX changes_namespace_sequence ON changes(namespace,sequence);
ALTER TABLE archive_meta RENAME TO archive_meta_v5;
CREATE TABLE archive_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 schema_version INTEGER NOT NULL CHECK(schema_version=6),
 archive_instance TEXT NOT NULL UNIQUE,
 contract_version INTEGER NOT NULL CHECK(contract_version=1),
 restored_from TEXT
) STRICT;
INSERT INTO archive_meta SELECT singleton,6,archive_instance,contract_version,restored_from FROM archive_meta_v5;
DROP TABLE archive_meta_v5;
PRAGMA user_version=6;
COMMIT;

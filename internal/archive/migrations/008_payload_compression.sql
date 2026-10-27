-- Metadata-only migration. Payload conversion is explicit, bounded maintenance.
BEGIN IMMEDIATE;
DROP TRIGGER immutable_content;
DROP TRIGGER immutable_normalizations;
ALTER TABLE content_objects RENAME TO content_storage;
ALTER TABLE content_storage ADD COLUMN decoded_length INTEGER;
ALTER TABLE normalizations RENAME TO normalization_storage;
ALTER TABLE normalization_storage ADD COLUMN compressed_envelope BLOB;
ALTER TABLE normalization_storage ADD COLUMN decoded_length INTEGER;
ALTER TABLE normalization_storage ADD COLUMN envelope_digest TEXT;
ALTER TABLE normalization_storage ADD COLUMN display_role TEXT;
ALTER TABLE normalization_storage ADD COLUMN display_usable INTEGER;
CREATE INDEX content_pending_compression ON content_storage(digest) WHERE decoded_length IS NULL;
CREATE INDEX normalization_pending_compression ON normalization_storage(record_key,source_revision,adapter_version) WHERE decoded_length IS NULL;
CREATE VIEW content_objects AS SELECT digest,CASE WHEN decoded_length>=0 THEN decoded_length ELSE byte_length END AS byte_length,media_type,
 CASE WHEN decoded_length IS NULL OR decoded_length=-1 THEN original_bytes ELSE skald_inflate(original_bytes,decoded_length,digest) END AS original_bytes FROM content_storage;
CREATE VIEW normalizations AS SELECT record_key,source_revision,adapter_version,contract_version,kind,source_time,stream_generation,source_ordinal,
 CASE WHEN compressed_envelope IS NULL THEN envelope_json ELSE CAST(skald_inflate(compressed_envelope,decoded_length,envelope_digest) AS TEXT) END AS envelope_json FROM normalization_storage;
CREATE TRIGGER content_insert INSTEAD OF INSERT ON content_objects BEGIN
 INSERT INTO content_storage(digest,byte_length,media_type,original_bytes) VALUES(new.digest,new.byte_length,new.media_type,new.original_bytes) ON CONFLICT DO NOTHING; END;
CREATE TRIGGER normalization_insert INSTEAD OF INSERT ON normalizations BEGIN
 INSERT INTO normalization_storage(record_key,source_revision,adapter_version,contract_version,kind,source_time,stream_generation,source_ordinal,envelope_json,display_role,display_usable)
 VALUES(new.record_key,new.source_revision,new.adapter_version,new.contract_version,new.kind,new.source_time,new.stream_generation,new.source_ordinal,new.envelope_json,json_extract(new.envelope_json,'$.role'),trim(coalesce(json_extract(new.envelope_json,'$.body.text'),''))<>'' AND coalesce(json_extract(new.envelope_json,'$.availability.state'),'') IN ('available','partial') AND coalesce(json_extract(new.envelope_json,'$.body.state'),'') NOT IN ('unavailable','opaque','unsupported')) ON CONFLICT DO NOTHING; END;
CREATE TRIGGER content_delete INSTEAD OF DELETE ON content_objects BEGIN DELETE FROM content_storage WHERE digest=old.digest; END;
CREATE TRIGGER normalization_delete INSTEAD OF DELETE ON normalizations BEGIN DELETE FROM normalization_storage WHERE record_key=old.record_key AND source_revision=old.source_revision AND adapter_version=old.adapter_version; END;
ALTER TABLE archive_meta RENAME TO archive_meta_v7;
CREATE TABLE archive_meta(singleton INTEGER PRIMARY KEY CHECK(singleton=1),schema_version INTEGER NOT NULL CHECK(schema_version=8),archive_instance TEXT NOT NULL UNIQUE,contract_version INTEGER NOT NULL CHECK(contract_version=1),restored_from TEXT) STRICT;
INSERT INTO archive_meta SELECT singleton,8,archive_instance,contract_version,restored_from FROM archive_meta_v7;
DROP TABLE archive_meta_v7;
PRAGMA user_version=8;
COMMIT;

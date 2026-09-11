"""Exercise the proposed capture DDL with real SQLite; no external dependencies."""
import hashlib
from pathlib import Path
import sqlite3

ROOT = Path(__file__).resolve().parents[1]
db = sqlite3.connect(":memory:", isolation_level=None)
db.executescript((ROOT / "internal/archive/migrations/001_capture.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (1,)
assert db.execute("PRAGMA foreign_keys").fetchone() == (1,)


def refuses(sql, values=()):
    try:
        db.execute(sql, values)
    except sqlite3.IntegrityError:
        return
    raise AssertionError(f"constraint did not refuse: {sql}")


for ns in ("fixture:a", "fixture:b"):
    db.execute("INSERT INTO sources VALUES (?, 'claude_code', 'v1', '{}', '{}')", (ns,))
db.execute("INSERT INTO streams VALUES ('stream', 'fixture:a', 'logical')")
db.execute("INSERT INTO sessions VALUES ('session', 'fixture:a', 'native', 'available')")
raw = b'{"synthetic":"original bytes"}\n'
digest = "sha256:" + hashlib.sha256(raw).hexdigest()
db.execute("INSERT INTO content_objects VALUES (?, ?, 'application/jsonl', ?)", (digest, len(raw), raw))
for key in ("first-occurrence", "second-occurrence"):
    db.execute("INSERT INTO artifacts VALUES (?, 'fixture:a', 'conversation', 'session', 'session')", (key,))
    db.execute("INSERT INTO artifact_versions VALUES (?, ?, '2026-09-10T00:00:00Z')", (key, digest))
assert db.execute("SELECT count(*) FROM content_objects").fetchone() == (1,)
assert db.execute("SELECT count(*) FROM artifact_versions").fetchone() == (2,)
refuses("INSERT INTO artifact_versions VALUES ('first-occurrence', ?, 'later')", (digest,))
refuses("INSERT INTO artifacts VALUES ('cross-scope', 'fixture:b', 'conversation', 'session', 'session')")
refuses("INSERT INTO artifacts VALUES ('invented', 'fixture:a', 'project', '/fixture/project', 'session')")
db.execute("INSERT INTO artifacts VALUES ('note', 'fixture:a', 'project', '/fixture/project', NULL)")
refuses("UPDATE content_objects SET original_bytes = ?", (raw,))
refuses("UPDATE artifact_versions SET first_observed_at = 'later'")
refuses("DELETE FROM content_objects WHERE digest = ?", (digest,))
for version in ("v1", "v2"):
    db.execute("INSERT INTO normalizations VALUES ('first-occurrence', ?, ?, 1, 'message', NULL, 'g1', 0, '{}')", (digest, version))
assert db.execute("SELECT count(*) FROM normalizations").fetchone() == (2,)
refuses("UPDATE normalizations SET kind = 'native_recap'")
db.execute("INSERT INTO ingest_checkpoints VALUES ('stream', 'g1', 10, 10, 1, '{}', 0)")
db.execute("BEGIN IMMEDIATE")
db.execute("UPDATE ingest_checkpoints SET captured_offset = 20, ordinal = 2")
db.execute("INSERT INTO changes(change_kind, namespace, payload_json) VALUES ('record', 'fixture:a', '{}')")
db.execute("ROLLBACK")
assert db.execute("SELECT captured_offset FROM ingest_checkpoints").fetchone() == (10,)
assert db.execute("SELECT count(*) FROM changes").fetchone() == (0,)
refuses("UPDATE ingest_checkpoints SET parsed_offset = 99")
assert not db.execute("PRAGMA foreign_key_check").fetchall()
assert db.execute("PRAGMA integrity_check").fetchone() == ("ok",)

# The daemon migrations must apply cleanly on standalone SQLite as well.
db.executescript((ROOT / "internal/archive/migrations/002_daemon.sql").read_text())
db.executescript((ROOT / "internal/archive/migrations/003_titles.sql").read_text())
assert db.execute("PRAGMA user_version").fetchone() == (3,)
columns = [row[1] for row in db.execute("PRAGMA table_info(session_titles)")]
assert "origin" in columns
refuses("INSERT INTO session_titles VALUES ('session','first-occurrence',?, 'v1','t','stream',0,0,0,'invented')", (digest,))
assert not db.execute("PRAGMA foreign_key_check").fetchall()
print(f"SQLite {sqlite3.sqlite_version}: capture DDL, identity/scope constraints, immutable versions, normalization history, atomic checkpoint rollback, and daemon migrations pass")

"""Real pinned Braid + daemon/MCP/compression/recovery smoke, synthetic only."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from mcp_smoke import MCP, PROTOCOL

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"
BRAID = Path(os.environ.get("SKALD_TEST_BRAID", str(Path.home() / ".local/bin/braid")))


def cli(*args):
    r = subprocess.run([str(BINARY), *map(str, args)], capture_output=True, text=True, timeout=60)
    assert r.returncode == 0, (args, r.stderr)
    return json.loads(r.stdout)


with tempfile.TemporaryDirectory(prefix="skald-completion-") as temporary:
    work = Path(temporary)
    sources = work / "sources"
    sources.mkdir(mode=0o700)
    def message(kind, key, text, role=None):
        return {"type": kind, "uuid": key, "sessionId": "synthetic-completion",
                "message": {"role": role or kind, "content": text}}
    records = [{"type": "system", "subtype": "away_summary", "uuid": "recap",
                "sessionId": "synthetic-completion", "content": "A native recap outside the recent page."},
               message("assistant", "answer", "zebraconnection allowed evidence " + "repeat " * 1000),
               message("assistant", "instruction", "zebraconnection SECRET_SYSTEM", "system")]
    records += [message("user", f"tail-{i}", "Later user input " * 30) for i in range(35)]
    (sources / "one.jsonl").write_text("".join(json.dumps(r) + "\n" for r in records))
    (sources / "two.jsonl").write_text(json.dumps(message("assistant", "foreign", "zebraconnection OTHER_NAMESPACE")) + "\n")
    config = work / "sources.json"
    config.write_text(json.dumps({"version": 1, "poll_interval_ms": 100,
        "min_free_bytes": 0, "max_database_bytes": 1 << 27,
        "retrieval": {"executable": str(BRAID), "sha256": hashlib.sha256(BRAID.read_bytes()).hexdigest(),
            "directory": str(work / "index"), "timeout_ms": 20000, "max_nodes": 1000,
            "max_snapshot_bytes": 4 << 20, "max_cache_bytes": 64 << 20, "max_scopes": 1},
        "sources": [{"namespace": namespace, "provider": "claude_code", "stream_id": name,
            "root": str(sources), "path": name + ".jsonl"}
            for namespace, name in [("fixture:claude", "one"), ("fixture:codex", "two")]]}))
    data, socket = work / "data", work / "run/skald.sock"
    daemon = None
    mcp = None
    def start():
        p = subprocess.Popen([str(BINARY), "serve", "--config", str(config), "--data-dir", str(data), "--socket", str(socket)],
                             stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        for _ in range(200):
            assert p.poll() is None, p.stderr.read()
            try:
                state = cli("status", "--socket", socket)
                if state["record_versions"] == len(records) + 1:
                    return p, state
            except (AssertionError, json.JSONDecodeError):
                pass
            time.sleep(.05)
        raise AssertionError("daemon not ready")
    try:
        daemon, state = start()
        assert state["backup"]["status"] == "no off-device backup configured"
        assert state["backup"]["last_off_device_backup"] is None and not state["backup"]["l1_complete"]
        session = cli("sessions", "--namespace", "fixture:claude", "--socket", socket)["items"][0]
        description = cli("descriptions", "--conversation", session["conversation_key"], "--namespace", "fixture:claude", "--socket", socket)
        assert description["selected"]["record_key"] and description["prior_recaps"] and description["new_activity"]
        args = ("context", "--text", "zebraconnection", "--namespace", "fixture:claude", "--max-bytes", "16000", "--socket", socket)
        before = cli(*args)
        assert before["status"] == "ok" and before["coverage"] == "complete"
        assert "SECRET_SYSTEM" not in json.dumps(before) and "OTHER_NAMESPACE" not in json.dumps(before)
        assert before["context_bytes"] == len(json.dumps(before["context"], ensure_ascii=False, separators=(",", ":")).encode())
        converted = cli("compress", "--max-rows", "1000", "--max-input-bytes", str(64 << 20), "--socket", socket)
        # Fresh ingest already applies the payload policy. Explicit maintenance
        # has no legacy rows to convert and must not rewrite processed values.
        assert converted["examined"] == 0 and converted["compressed"] == 0
        after = cli(*args)
        assert after["context"] == before["context"]
        mcp = MCP(socket)
        mcp.request("initialize", {"protocolVersion": PROTOCOL, "capabilities": {}, "clientInfo": {"name": "completion-smoke", "version": "1"}})
        mcp.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        result, payload = mcp.call("context.query", namespace="fixture:claude", text="zebraconnection", max_bytes=16000)
        assert not result.get("isError") and payload["data"]["context"] == after["context"]
        result, payload = mcp.call("descriptions.read", namespace="fixture:claude", conversation=session["conversation_key"])
        assert not result.get("isError") and payload["data"]["selected"]["record_key"]
        mcp.close(); mcp = None
        # A successful redaction receipt synchronously removes published bytes.
        cli("redact", "--namespace", "fixture:claude", "--scope", "record", "--target", after["context"][0]["ref"]["record_key"], "--socket", socket)
        assert not list((work / "index").glob("publication-*"))
        assert cli(*args)["status"] == "no_answer"
        daemon.terminate(); assert daemon.wait(timeout=10) == 0, daemon.stderr.read(); daemon.stderr.close(); daemon = None
        compact = cli("archive-compact", "--data-dir", data, "--max-database-bytes", str(1 << 27), "--min-free-bytes", "0", "--apply")
        assert compact["applied"]
        # Redaction removes one version, so use a bounded direct restart check.
        daemon = subprocess.Popen([str(BINARY), "serve", "--config", str(config), "--data-dir", str(data), "--socket", str(socket)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        for _ in range(200):
            assert daemon.poll() is None, daemon.stderr.read()
            try:
                restored = cli("status", "--socket", socket)
                if restored["archive_instance"] == state["archive_instance"]:
                    break
            except AssertionError:
                pass
            time.sleep(.05)
        else:
            raise AssertionError("post-compaction daemon not ready")
        assert cli(*args)["status"] == "no_answer"
    finally:
        if mcp:
            mcp.close()
        if daemon and daemon.poll() is None:
            daemon.terminate(); assert daemon.wait(timeout=10) == 0, daemon.stderr.read(); daemon.stderr.close()
print("Completion smoke passed: real pinned scoped Braid, MCP citations/bounds, whole-archive descriptions, compressed reads, synchronous redaction, offline compaction and restart.")

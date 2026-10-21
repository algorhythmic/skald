"""Compiled daemon acceptance with private synthetic sources and a real Unix socket."""
import hashlib
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"


def cli(*args):
    result = subprocess.run([str(BINARY), *map(str, args)], capture_output=True, text=True, timeout=20)
    if result.returncode:
        raise AssertionError(result.stderr)
    return json.loads(result.stdout)


def cli_error(expected, *args):
    result = subprocess.run([str(BINARY), *map(str, args)], capture_output=True, text=True, timeout=20)
    assert result.returncode and expected in result.stderr, (result.returncode, result.stderr)


with tempfile.TemporaryDirectory(prefix="skald-native-") as work:
    work = Path(work)
    sources = work / "sources"
    sources.mkdir(mode=0o700)
    registrations = []
    for name, provider in (("claude", "claude_code"), ("codex", "codex")):
        shutil.copyfile(ROOT / f"testdata/{name}/session.jsonl", sources / f"{name}.jsonl")
        registrations.append(dict(namespace=f"fixture:{name}", provider=provider,
                                  stream_id=f"fixture-{name}", root=str(sources), path=f"{name}.jsonl"))
    config = work / "config.json"
    config.write_text(json.dumps(dict(version=1, poll_interval_ms=100, max_database_bytes=64 << 20,
                                    min_free_bytes=0, allow_raw=True, sources=registrations)))
    socket = work / "run/skald.sock"
    data = work / "data"
    processes = []

    def start(directory):
        proc = subprocess.Popen([str(BINARY), "serve", "--config", str(config), "--data-dir", str(directory),
                                 "--socket", str(socket)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        processes.append(proc)
        until = time.monotonic() + 15
        while time.monotonic() < until:
            if proc.poll() is not None:
                raise AssertionError(proc.stderr.read())
            try:
                cli("status", "--socket", socket)
                return proc
            except AssertionError:
                time.sleep(0.05)
        raise AssertionError("daemon did not become available")

    def wait_for(predicate):
        until = time.monotonic() + 15
        while time.monotonic() < until:
            value = cli("status", "--socket", socket)
            if predicate(value):
                return value
            time.sleep(0.05)
        raise AssertionError(f"capture did not reach expected state: {value}")

    try:
        proc = start(data)
        initial = wait_for(lambda s: s["record_versions"] == 17 and not s["capture_issues"])
        assert initial["sources_total"] == 2 and initial["sources_omitted"] == 0
        for health in initial["sources"]:
            assert health["enrolled"] and health["checkpoint_available"]
            assert health["stream_generation"] and health["next_ordinal"] > 0
            assert health["last_captured_at"] and health["checked_at"]
            assert health["captured_offset"] == health["parsed_offset"] > 0
        denied = subprocess.run([str(BINARY), "status", "--socket", str(socket),
                                 "--namespace", "fixture:outside"], capture_output=True, text=True, timeout=5)
        assert denied.returncode and "scope_denied" in denied.stderr
        assert (socket.stat().st_mode & 0o777) == 0o600
        assert (data.stat().st_mode & 0o777) == 0o700
        capabilities = cli("capabilities", "--socket", socket)
        assert capabilities["contracts"]["archive_feed"] == 1
        assert capabilities["contracts"]["redaction_receipt"] == 1
        assert capabilities["archive_schema"] == 6
        assert capabilities["namespaces"] == ["fixture:claude", "fixture:codex"]
        first_feed = cli("changes", "--socket", socket, "--limit", 3)
        assert first_feed["version"] == 1 and first_feed["phase"] == "snapshot"
        assert first_feed["more"] and len(first_feed["events"]) == 3
        snapshot_boundary = first_feed["high_water"]
        feed = first_feed
        snapshot_events = list(feed["events"])
        while feed["more"]:
            feed = cli("changes", "--socket", socket, "--limit", 3, "--cursor", feed["next_cursor"])
            assert feed["phase"] == "snapshot" and feed["high_water"] == snapshot_boundary
            snapshot_events.extend(feed["events"])
        assert all(event["sequence"] <= snapshot_boundary for event in snapshot_events)
        assert sum(event["kind"] == "record" for event in snapshot_events) == 17
        tail_cursor = feed["next_cursor"]
        caught_up = cli("changes", "--socket", socket, "--cursor", tail_cursor)
        assert caught_up["phase"] == "tail" and not caught_up["more"] and not caught_up["events"]
        tail_cursor = caught_up["next_cursor"]
        cli_error("scope_denied", "changes", "--socket", socket, "--namespace", "fixture:outside")
        second = subprocess.run([str(BINARY), "serve", "--config", str(config), "--data-dir", str(data),
                                 "--socket", str(work / "second/socket")], capture_output=True, text=True, timeout=5)
        assert second.returncode and "archive_in_use" in second.stderr
        sessions = cli("sessions", "--socket", socket)["items"]
        assert len(sessions) == 2
        claude = next(s for s in sessions if s["provider"] == "claude_code")
        refs = cli("artifacts", "--socket", socket, "--conversation", claude["conversation_key"])["items"]
        recap = next(r for r in refs if r["kind"] == "native_recap")

        def read_recap():
            result = cli("get", "--socket", socket, "--record", recap["record_key"], "--revision", recap["source_revision"],
                         "--adapter", recap["adapter_version"], "--raw")
            raw = base64.b64decode(result["raw"])
            assert "sha256:" + hashlib.sha256(raw).hexdigest() == recap["source_revision"]
            return result

        original = read_recap()
        tail = dict(type="assistant", uuid="native-tail", sessionId="synthetic-claude",
                    message=dict(role="assistant", content="A synthetic tail survives daemon restart."))
        with (sources / "claude.jsonl").open("a") as out:
            out.write(json.dumps(tail))
        wait_for(lambda s: any(h["health"] == "partial_line" for h in s["sources"]))
        proc.kill()
        proc.wait(timeout=5)
        with (sources / "claude.jsonl").open("a") as out:
            out.write("\n")
        proc = start(data)
        resumed = wait_for(lambda s: s["record_versions"] == 18 and not s["capture_issues"])
        assert resumed["archive_instance"] == initial["archive_instance"]
        tail_page = cli("changes", "--socket", socket, "--cursor", tail_cursor)
        assert tail_page["phase"] == "tail" and any(
            e["kind"] == "record" and e["sequence"] > snapshot_boundary for e in tail_page["events"])
        tail_cursor = tail_page["next_cursor"]
        time.sleep(0.25)
        assert cli("status", "--socket", socket)["record_versions"] == 18
        source_bytes = {p.name: p.read_bytes() for p in sources.iterdir()}
        position_fields = ("stream_generation", "stream_epoch", "next_ordinal", "captured_offset",
                           "parsed_offset", "last_captured_at", "last_source_time")
        captured_positions = {h["stream_key"]: {k: h[k] for k in position_fields} for h in resumed["sources"]}
        (sources / "claude.jsonl").unlink()
        (sources / "codex.jsonl").unlink()
        wait_for(lambda s: len(s["capture_issues"]) == 2)
        assert read_recap()["raw"] == original["raw"]
        backup = cli("backup", "--socket", socket)["backup"]
        proc.terminate()
        assert proc.wait(timeout=10) == 0
        restored = work / "restored"
        cli("restore", "--backup", backup, "--data-dir", restored)
        proc = start(restored)
        state = wait_for(lambda s: s["record_versions"] == 18 and len(s["capture_issues"]) == 2)
        assert state["archive_instance"] != initial["archive_instance"]
        cli_error("archive_feed_cursor_expired", "changes", "--socket", socket, "--cursor", tail_cursor)
        assert state["sources_total"] == 2 and state["sources_omitted"] == 0
        for health in state["sources"]:
            assert health["health"] == "unavailable" and health["checkpoint_available"]
            assert {k: health[k] for k in position_fields} == captured_positions[health["stream_key"]]
        assert read_recap()["raw"] == original["raw"]
        for name, raw in source_bytes.items():
            (sources / name).write_bytes(raw)
        recovered = wait_for(lambda s: not s["capture_issues"] and
                             all(h["health"] == "available" for h in s["sources"]))
        assert recovered["record_versions"] == 18
        for health in recovered["sources"]:
            assert {k: health[k] for k in position_fields} == captured_positions[health["stream_key"]]
        receipt = cli("redact", "--socket", socket, "--namespace", "fixture:claude",
                      "--scope", "record", "--target", recap["record_key"])
        assert receipt["version"] == 1 and receipt["scope_key"] == recap["record_key"]
        assert receipt["records_removed"] == 1 and receipt["versions_removed"] >= 1
        cli_error("evidence_redacted", "get", "--socket", socket, "--record", recap["record_key"],
                  "--revision", recap["source_revision"], "--adapter", recap["adapter_version"])
        redacted_feed = cli("changes", "--socket", socket, "--namespace", "fixture:claude", "--limit", 100)
        assert any(e["kind"] == "redaction" and e["scope_key"] == recap["record_key"]
                   for e in redacted_feed["events"])
        assert any(e["record_key"] == recap["record_key"] and
                   e["source_revision"] == recap["source_revision"] and
                   e.get("unavailable") == "redacted" and "locator" not in e
                   for e in redacted_feed["events"] if e["kind"] in ("record", "correction"))
        proc.terminate()
        assert proc.wait(timeout=10) == 0
        assert not socket.exists()
        print("Native daemon: scoped source positions/freshness, two providers, capabilities, bounded snapshot/tail feed, private socket, exclusive writer, pending tail, SIGKILL/restart, duplicate-free capture, source loss/reappearance, exact raw reads, backup, fresh restore cursor expiry, redaction and clean shutdown pass")
    finally:
        for proc in processes:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)

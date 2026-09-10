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
        assert (socket.stat().st_mode & 0o777) == 0o600
        assert (data.stat().st_mode & 0o777) == 0o700
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
        time.sleep(0.25)
        assert cli("status", "--socket", socket)["record_versions"] == 18
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
        assert read_recap()["raw"] == original["raw"]
        proc.terminate()
        assert proc.wait(timeout=10) == 0
        assert not socket.exists()
        print("Native daemon: private socket, two providers, exclusive writer, pending tail, SIGKILL/restart, duplicate-free capture, source removal, exact raw reads, backup, fresh restore and clean shutdown pass")
    finally:
        for proc in processes:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)

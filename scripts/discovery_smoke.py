"""Compiled source connection/discovery acceptance with synthetic native files."""
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
    p = subprocess.run([str(BINARY), *map(str, args)], capture_output=True, text=True, timeout=20)
    if p.returncode:
        raise AssertionError(p.stderr)
    return json.loads(p.stdout)


with tempfile.TemporaryDirectory(prefix="skald-discovery-") as directory:
    work = Path(directory)
    config = work / "config/sources.json"
    socket = work / "run/skald.sock"
    archive = work / "archive"
    roots = {}
    for name, provider in (("claude", "claude_code"), ("codex", "codex")):
        root = work / name
        root.mkdir(mode=0o700)
        roots[name] = root
        shutil.copyfile(ROOT / f"testdata/{name}/session.jsonl", root / "original.jsonl")
        preview = cli("sources", "--provider", provider, "--root", root, "--since", "all")
        assert preview["preview_only"] and preview["items"][0]["identity"]["conversation_id"]
        receipt = cli("connect", "--provider", provider, "--root", root, "--since", "all",
                      "--config", config, "--namespace", f"fixture:{name}", "--max-sessions", "4")
        assert receipt["configured"] and not receipt["collecting"]
    assert config.stat().st_mode & 0o777 == 0o600
    proc = None

    def start():
        return subprocess.Popen([str(BINARY), "serve", "--config", str(config), "--data-dir", str(archive),
                                 "--socket", str(socket)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)

    def wait_for(predicate):
        end = time.monotonic() + 25
        last = None
        while time.monotonic() < end:
            if proc.poll() is not None:
                raise AssertionError(proc.stderr.read())
            try:
                last = cli("status", "--socket", socket)
                if predicate(last):
                    return last
            except AssertionError:
                pass
            time.sleep(0.1)
        raise AssertionError(last)

    def stop():
        if proc and proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=5)
        if proc:
            proc.stderr.close()

    try:
        proc = start()
        initial = wait_for(lambda s: s["sessions"] == 2 and s["record_versions"] == 17)
        assert len(initial["discovery"]) == 2 and sum(r["enrolled"] for r in initial["discovery"]) == 2
        sessions = cli("sessions", "--socket", socket)["items"]
        refs = {s["conversation_key"]: cli("artifacts", "--socket", socket, "--conversation", s["conversation_key"])["items"]
                for s in sessions}
        # Discover a new conversation without restarting the daemon.
        raw = (roots["codex"] / "original.jsonl").read_text()
        (roots["codex"] / "new.jsonl").write_text(raw.replace("synthetic-codex", "new-codex"))
        wait_for(lambda s: s["sessions"] == 3 and s["record_versions"] == 26)
        # A verified rename keeps all fallback record keys and checkpoint positions.
        checked = next(r["checked_at"] for r in cli("status", "--socket", socket)["discovery"] if r["namespace"] == "fixture:claude")
        (roots["claude"] / "original.jsonl").rename(roots["claude"] / "moved.jsonl")
        wait_for(lambda s: all(r["health"] == "available" for r in s["sources"]) and any(r["namespace"] == "fixture:claude" and r["checked_at"] != checked for r in s["discovery"]))
        stop()
        proc = start()
        after = wait_for(lambda s: s["sessions"] == 3 and not s["capture_issues"])
        assert after["record_versions"] == 26
        for key, before in refs.items():
            got = cli("artifacts", "--socket", socket, "--conversation", key)["items"]
            assert [(r["record_key"], r["source_revision"], r["stream_key"]) for r in got] == [
                (r["record_key"], r["source_revision"], r["stream_key"]) for r in before]
        # Remove the native files; archived conversations remain browsable.
        shutil.rmtree(roots["claude"])
        lost = wait_for(lambda s: any(r["health"] == "unavailable" for r in s["sources"]))
        assert lost["sessions"] == 3 and lost["record_versions"] == 26
    finally:
        stop()
print("Source connection acceptance passed: metadata preview, private config, both providers, new sessions, stable rename/restart identities, source-loss history.")

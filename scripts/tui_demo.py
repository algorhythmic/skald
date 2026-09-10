"""Run the TUI with temporary synthetic conversations; never reads personal sources."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from contextlib import contextmanager

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"


@contextmanager
def fixture_daemon():
    with tempfile.TemporaryDirectory(prefix="skald-tui-") as directory:
        work = Path(directory)
        sources = work / "sources"
        sources.mkdir(mode=0o700)
        registrations = []
        for name, provider in (("claude", "claude_code"), ("codex", "codex")):
            content = (ROOT / f"testdata/{name}/session.jsonl").read_text()
            (sources / f"{name}.jsonl").write_text(content)
            registrations.append(dict(namespace=f"fixture:{name}", provider=provider,
                                      stream_id=f"fixture-{name}", root=str(sources), path=f"{name}.jsonl"))
        # A longer conversation exercises history pages, paragraphs, recaps, and
        # terminal-safe rendering using entirely invented text.
        records = [dict(type="ai-title", sessionId="synthetic-review", title="Review the conversation archive and terminal reader")]
        for i in range(38):
            role = "user" if i % 2 == 0 else "assistant"
            content = ("Keep each conversation connected to its original project. Preserve the exact source record when its file disappears."
                       if role == "user" else
                       "The archive keeps the original bytes and source order. The terminal reader shows an excerpt, with the earlier native recap available underneath.\n\n"
                       "The next check covers restoring the archive on another machine. Unicode examples: 界面, café, and 👩‍💻. Activity remains unknown without fresh evidence.")
            records.append(dict(type=role, uuid=f"m{i}", sessionId="synthetic-review", cwd="/fixtures/heimdall",
                                timestamp=f"2026-09-10T10:{i:02d}:00Z", message=dict(role=role, content=content)))
            if i in (10, 30):
                records.append(dict(type="system", subtype="away_summary", uuid=f"recap{i}", sessionId="synthetic-review",
                                    content="You asked for a durable conversation archive and a readable terminal overview. Source removal is covered; next, verify history pages and native terminal rendering."))
        (sources / "review.jsonl").write_text("".join(json.dumps(r, ensure_ascii=False)+"\n" for r in records))
        registrations.append(dict(namespace="fixture:review", provider="claude_code", stream_id="fixture-review",
                                  root=str(sources), path="review.jsonl"))
        config = work / "config.json"
        config.write_text(json.dumps(dict(version=1, poll_interval_ms=200, min_free_bytes=0,
                                        max_database_bytes=64 << 20, sources=registrations)))
        socket = work / "run/skald.sock"
        proc = subprocess.Popen([str(BINARY), "serve", "--config", str(config), "--data-dir", str(work / "archive"),
                                 "--socket", str(socket)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        try:
            until = time.monotonic() + 15
            while time.monotonic() < until:
                if proc.poll() is not None:
                    raise RuntimeError(proc.stderr.read())
                status = subprocess.run([str(BINARY), "status", "--socket", str(socket)], capture_output=True, text=True)
                if status.returncode == 0 and json.loads(status.stdout)["sessions"] == 3:
                    break
                time.sleep(0.05)
            else:
                raise RuntimeError("fixture daemon did not become ready")
            yield socket, proc
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            proc.stderr.close()


if __name__ == "__main__":
    with fixture_daemon() as (socket, _):
        raise SystemExit(subprocess.call([str(BINARY), "tui", "--socket", str(socket)]))

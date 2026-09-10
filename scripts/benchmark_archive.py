"""Reproducible local archive measurement: 100 synthetic sessions / 20,000 records."""
import json
from pathlib import Path
import statistics
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"


def read(socket, command):
    run = subprocess.run([str(BINARY), command, "--socket", str(socket)] + (["--limit", "100"] if command == "sessions" else []),
                         capture_output=True, text=True, timeout=15)
    if run.returncode:
        raise RuntimeError(run.stderr)
    return json.loads(run.stdout)


with tempfile.TemporaryDirectory(prefix="skald-benchmark-") as name:
    work = Path(name)
    sources = work / "sources"
    sources.mkdir(mode=0o700)
    registrations = []
    raw_bytes = 0
    for session in range(100):
        sid = f"synthetic-{session:03}"
        path = sources / f"{sid}.jsonl"
        records = [dict(type="ai-title", sessionId=sid, version="2.1.263", title=f"Synthetic project {session}")]
        records += [dict(type="assistant", uuid=f"record-{i}", sessionId=sid,
                         timestamp="2026-09-10T12:00:00Z", message=dict(role="assistant", content=f"Synthetic bounded context record {i}.")) for i in range(199)]
        path.write_text("".join(json.dumps(record) + "\n" for record in records))
        raw_bytes += path.stat().st_size
        registrations.append(dict(namespace="fixture:benchmark", provider="claude_code", stream_id=sid, root=str(sources), path=path.name))
    cfg = work / "config.json"
    cfg.write_text(json.dumps(dict(version=1, poll_interval_ms=1000, max_database_bytes=1 << 30, min_free_bytes=0, sources=registrations)))
    socket, data = work / "run/skald.sock", work / "data"
    started = time.monotonic()
    proc = subprocess.Popen([str(BINARY), "serve", "--config", str(cfg), "--data-dir", str(data), "--socket", str(socket)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    try:
        deadline = started + 90
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                raise RuntimeError(proc.stderr.read())
            try:
                status = read(socket, "status")
                if status["record_versions"] == 20000:
                    break
            except RuntimeError:
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError("backfill deadline exceeded")
        backfill = time.monotonic() - started
        samples = []
        for _ in range(30):
            start = time.perf_counter()
            page = read(socket, "sessions")
            samples.append((time.perf_counter() - start) * 1000)
            assert len(page["items"]) == 100 and not page.get("next_cursor")
        proc.terminate()
        assert proc.wait(timeout=10) == 0
        archive_bytes = (data / "archive.sqlite").stat().st_size
        result = dict(sessions=100, records=20000, source_bytes=raw_bytes, archive_bytes=archive_bytes,
                      archive_to_source_ratio=round(archive_bytes / raw_bytes, 2), backfill_seconds=round(backfill, 3),
                      list_samples=30, list_median_ms=round(statistics.median(samples), 3),
                      list_p95_ms=round(sorted(samples)[28], 3),
                      timing_scope="compiled CLI process + Unix socket + archive list + JSON output",
                      fixture="small synthetic Claude records; warm local caches; no Heimdall or Braid")
        print(json.dumps(result, indent=2))
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=5)

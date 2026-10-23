"""Compiled private diagnostics and opt-in OTLP export, synthetic data only."""
import gzip
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"
exports = queue.Queue()


class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        exports.put((self.path, self.headers.get("Content-Type"), body))
        self.send_response(200)
        self.send_header("Content-Type", "application/x-protobuf")
        self.end_headers()

    def log_message(self, *args):
        pass


def cli(*args, success=True):
    result = subprocess.run([str(BINARY), *map(str, args)], capture_output=True,
                            text=True, timeout=40)
    assert (result.returncode == 0) == success, result.stderr
    return json.loads(result.stdout if success else result.stderr)


receiver = HTTPServer(("127.0.0.1", 0), Receiver)
thread = threading.Thread(target=receiver.serve_forever, daemon=True)
thread.start()
try:
    with tempfile.TemporaryDirectory(prefix="skald-diagnostics-") as temp:
        work = Path(temp)
        sources = work / "sources"
        sources.mkdir(mode=0o700)
        # This marker must remain in the archive and never enter metrics.
        private_marker = "SYNTHETIC_TRANSCRIPT_NOT_A_METRIC"
        source = sources / "session.jsonl"
        source.write_text(json.dumps({"type": "assistant", "sessionId": "private-id",
                                     "uuid": "one", "message": {"role": "assistant",
                                     "content": private_marker}}) + "\n")
        config = work / "sources.json"
        config.write_text(json.dumps({"version": 1, "poll_interval_ms": 100,
            "min_free_bytes": 0, "max_database_bytes": 1 << 28,
            "telemetry": {"enable_profiling": True, "export_interval_seconds": 10,
                          "service_instance_id": "fixture-device",
                          "otlp_metrics_endpoint": f"http://127.0.0.1:{receiver.server_port}/v1/metrics"},
            "sources": [{"namespace": "fixture:private", "provider": "claude_code",
                         "stream_id": "one", "root": str(sources), "path": source.name}]}))
        socket = work / "run/skald.sock"
        proc = subprocess.Popen([str(BINARY), "serve", "--config", str(config),
                                 "--data-dir", str(work / "data"), "--socket", str(socket)],
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                assert proc.poll() is None, proc.stderr.read()
                result = subprocess.run([str(BINARY), "metrics", "--socket", str(socket)],
                                        capture_output=True, text=True, timeout=5)
                if not result.returncode and json.loads(result.stdout)["counters"]["source_checks"]:
                    break
                time.sleep(.05)
            else:
                raise AssertionError("diagnostics did not become ready")
            metrics = cli("metrics", "--socket", socket)
            assert metrics["otlp_enabled"] and metrics["process"]["resident_bytes"] > 0
            for kind in ("heap", "cpu"):
                output = work / f"{kind}.pprof"
                options = ["--seconds", "1"] if kind == "cpu" else []
                cli("profile", "--socket", socket, "--kind", kind, "--output", output, *options)
                assert output.stat().st_mode & 0o777 == 0o600
                profile = gzip.decompress(output.read_bytes())
                assert profile and private_marker.encode() not in profile
                previous = output.read_bytes()
                cli("profile", "--socket", socket, "--kind", kind, "--output", output,
                    *options, success=False)
                assert output.read_bytes() == previous
            path, content_type, body = exports.get(timeout=15)
            assert path == "/v1/metrics" and content_type == "application/x-protobuf"
            assert b"skald.source_checks" in body and b"skald.scan.duration" in body
            assert b"service.instance.id" in body and b"fixture-device" in body
            for forbidden in (private_marker.encode(), b"fixture:private", b"private-id", str(sources).encode()):
                assert forbidden not in body, "private evidence leaked into OTLP metrics"
        finally:
            proc.terminate()
            assert proc.wait(timeout=10) == 0, proc.stderr.read()
finally:
    receiver.shutdown()
    receiver.server_close()

print("Diagnostics passed: private bounded profiles, no overwrite, process/capture metrics, opt-in OTLP protobuf export without transcript identifiers.")

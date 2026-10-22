"""Compiled MCP stdio acceptance against an isolated daemon and Unix socket."""

import json
import os
from pathlib import Path
import select
import shutil
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/skald"
PROTOCOL = "2025-11-25"


def cli(*args):
    result = subprocess.run(
        [str(BINARY), *map(str, args)], capture_output=True, text=True, timeout=20
    )
    if result.returncode:
        raise AssertionError(f"CLI {args}: {result.stderr}")
    return json.loads(result.stdout)


class MCP:
    def __init__(self, socket):
        self.proc = subprocess.Popen(
            [str(BINARY), "mcp", "--socket", str(socket),
             "--namespace", "fixture:claude", "--namespace", "fixture:codex"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, bufsize=1,
        )
        self.next_id = 1

    def send(self, message):
        assert self.proc.poll() is None, self.proc.stderr.read()
        self.proc.stdin.write(json.dumps(message, separators=(",", ":")) + "\n")
        self.proc.stdin.flush()

    def response(self):
        ready, _, _ = select.select([self.proc.stdout], [], [], 10)
        assert ready, "MCP response timed out"
        line = self.proc.stdout.readline()
        assert line and len(line) <= 2 << 20, "MCP response absent or unbounded"
        return json.loads(line)

    def request(self, method, params=None):
        number = self.next_id
        self.next_id += 1
        message = {"jsonrpc": "2.0", "id": number, "method": method}
        if params is not None:
            message["params"] = params
        self.send(message)
        response = self.response()
        assert response["jsonrpc"] == "2.0" and response["id"] == number, response
        return response

    def call(self, name, **arguments):
        response = self.request("tools/call", {"name": name, "arguments": arguments})
        assert "error" not in response, response
        result = response["result"]
        payload = result["structuredContent"]
        assert json.loads(result["content"][0]["text"]) == payload
        provenance = payload["provenance"]
        assert provenance["producer"] == "skald"
        assert provenance["authority"] == "conversation_evidence"
        assert provenance["configured_scope"] == ["fixture:claude", "fixture:codex"]
        assert provenance["archive_instance"] and "archive_boundary" in provenance
        assert "source_revision" in provenance and "target_revision" in provenance
        return result, payload

    def close(self):
        if self.proc.poll() is None:
            self.proc.stdin.close()
            self.proc.wait(timeout=5)
        assert self.proc.returncode == 0, self.proc.stderr.read()
        self.proc.stdout.close()
        self.proc.stderr.close()


def wait_for_daemon(proc, socket, predicate):
    deadline = time.monotonic() + 20
    last = None
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"daemon exited: {proc.stderr.read()}")
        try:
            last = cli("status", "--socket", socket)
            if predicate(last):
                return last
        except (AssertionError, json.JSONDecodeError):
            pass
        time.sleep(0.05)
    raise AssertionError(f"daemon state timed out: {last}")


def start_daemon(config, data, socket, predicate):
    proc = subprocess.Popen(
        [str(BINARY), "serve", "--config", str(config), "--data-dir", str(data),
         "--socket", str(socket)],
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True,
    )
    try:
        state = wait_for_daemon(proc, socket, predicate)
    except BaseException:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=5)
        proc.stderr.close()
        raise
    return proc, state


def stop_daemon(proc):
    if proc is None:
        return
    if proc.poll() is None:
        proc.terminate()
        assert proc.wait(timeout=10) == 0, proc.stderr.read()
    proc.stderr.close()


def run():
    assert BINARY.is_file(), "build bin/skald before running the MCP smoke test"
    with tempfile.TemporaryDirectory(prefix="skald-mcp-") as directory:
        work = Path(directory)
        sources = work / "sources"
        sources.mkdir(mode=0o700)
        registrations = []
        for name, provider in (("claude", "claude_code"), ("codex", "codex")):
            shutil.copyfile(ROOT / "testdata" / name / "session.jsonl", sources / f"{name}.jsonl")
            registrations.append({
                "namespace": f"fixture:{name}", "provider": provider,
                "stream_id": f"mcp-{name}", "root": str(sources),
                "path": f"{name}.jsonl",
            })
        config = work / "config.json"
        configuration = {
            "version": 1, "poll_interval_ms": 100,
            "max_database_bytes": 64 << 20, "min_free_bytes": 0,
            "allow_raw": False, "sources": registrations,
        }

        def write_config():
            config.write_text(json.dumps(configuration), encoding="utf-8")
            os.chmod(config, 0o600)

        write_config()
        socket = work / "run" / "skald.sock"
        data = work / "data"
        daemon = None
        mcp = None
        try:
            daemon, state = start_daemon(
                config, data, socket,
                lambda s: s["record_versions"] == 17 and s["sessions"] == 2,
            )
            assert socket.stat().st_mode & 0o777 == 0o600
            mcp = MCP(socket)
            initialized = mcp.request("initialize", {
                "protocolVersion": PROTOCOL, "capabilities": {},
                "clientInfo": {"name": "skald-mcp-smoke", "version": "1"},
            })
            assert initialized["result"]["protocolVersion"] == PROTOCOL
            assert "tools" in initialized["result"]["capabilities"]
            mcp.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
            listed = mcp.request("tools/list")["result"]["tools"]
            names = {tool["name"] for tool in listed}
            assert names == {
                "capabilities", "status", "sessions.list", "artifacts.list",
                "artifacts.get", "changes.read", "transcript.read",
            }, names
            assert all(tool["inputSchema"]["additionalProperties"] is False for tool in listed)

            _, capabilities = mcp.call("capabilities", namespace="fixture:claude")
            assert capabilities["data"]["archive_instance"] == state["archive_instance"]
            _, status = mcp.call("status", namespace="fixture:claude")
            assert status["data"]["sessions"] == 1
            assert str(work) not in json.dumps(status["data"])
            assert status["provenance"]["archive_boundary"] == status["data"]["boundary"]

            _, sessions = mcp.call("sessions.list", namespace="fixture:claude", limit=10)
            assert len(sessions["data"]["items"]) == 1
            conversation = sessions["data"]["items"][0]["conversation_key"]
            assert sessions["data"]["items"][0]["provenance"]["namespace"] == "fixture:claude"
            _, refs = mcp.call("artifacts.list", namespace="fixture:claude", conversation=conversation)
            assert refs["data"]["items"]
            recap = next(x for x in refs["data"]["items"] if x["kind"] == "native_recap")
            assert recap["provenance"]["source_revision"] == recap["source_revision"]
            _, transcript = mcp.call("transcript.read", namespace="fixture:claude", conversation=conversation)
            assert any(x.get("evidence_class") == "native_recap" for x in transcript["data"]["items"])
            assert all(x.get("role") != "developer" for x in transcript["data"]["items"])
            assert all("record_key" in x and "source_revision" in x for x in transcript["data"]["items"])
            exact_args = {
                "namespace": "fixture:claude", "record": recap["record_key"],
                "revision": recap["source_revision"], "adapter": recap["adapter_version"],
            }
            _, exact = mcp.call("artifacts.get", **exact_args)
            assert exact["data"]["record"]["source_revision"] == recap["source_revision"]
            assert "raw" not in exact["data"]
            assert exact["provenance"]["source_revision"] == recap["source_revision"]
            _, changes = mcp.call("changes.read", namespace="fixture:claude", limit=25)
            assert changes["data"]["events"] and changes["data"]["next_cursor"]
            assert changes["provenance"]["archive_boundary"] == "unknown"

            denied = mcp.call("sessions.list", namespace="fixture:outside")[1]
            assert denied["error"] == "scope_denied"
            missing = mcp.call("sessions.list")[1]
            assert missing["error"] == "namespace_required"
            raw = mcp.call("artifacts.get", **exact_args, raw=True)[1]
            assert raw["error"] == "invalid_arguments"
            bounded = mcp.call("sessions.list", namespace="fixture:claude", limit=999)[1]
            assert bounded["error"] == "invalid_arguments"

            receipt = cli("redact", "--socket", socket, "--namespace", "fixture:claude",
                          "--scope", "record", "--target", recap["record_key"])
            assert receipt["records_removed"] == 1
            gone = mcp.call("artifacts.get", **exact_args)[1]
            assert gone["error"] == "evidence_redacted"

            stop_daemon(daemon)
            daemon = None
            configuration["sources"] = [registrations[0]]
            write_config()
            daemon, _ = start_daemon(
                config, data, socket,
                lambda s: s["sessions"] >= 1 and s["archive_instance"] == state["archive_instance"],
            )
            revoked = mcp.call("sessions.list", namespace="fixture:codex")[1]
            assert revoked["error"] == "scope_denied"
            assert revoked["provenance"]["configured_scope"] == ["fixture:claude", "fixture:codex"]
            _, still_granted = mcp.call("sessions.list", namespace="fixture:claude")
            assert still_granted["data"]["items"]

            idle = MCP(socket)
            idle.request("initialize", {
                "protocolVersion": PROTOCOL, "capabilities": {},
                "clientInfo": {"name": "skald-mcp-idle-test", "version": "1"},
            })
            idle.proc.terminate()
            assert idle.proc.wait(timeout=3) == 0, "idle MCP did not exit on SIGTERM"
            idle.proc.stdin.close()
            idle.proc.stdout.close()
            idle.proc.stderr.close()
        finally:
            if mcp is not None:
                mcp.close()
            stop_daemon(daemon)
    print("MCP stdio smoke passed: scoped reads, provenance, bounds, redaction, and restart revocation")


if __name__ == "__main__":
    run()

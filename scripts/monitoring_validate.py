"""Offline draft validation only. Never starts a monitoring service or contacts .21.

Optional OTELCOL_BIN/PROMTOOL_BIN override the project-local validation binaries.
Install/reload on 192.168.86.21 remains a separate user action.
"""
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
MONITOR = ROOT / "examples/monitoring"
PROMTOOL = Path(os.environ.get("PROMTOOL_BIN", ROOT / ".tools/prometheus/promtool"))
COLLECTOR = Path(os.environ.get("OTELCOL_BIN", ROOT / ".tools/otelcol/otelcol-contrib"))
for command in (
    [PROMTOOL, "check", "rules", MONITOR / "alerts.yml"],
    [PROMTOOL, "test", "rules", MONITOR / "alerts.test.yml"],
    [COLLECTOR, "validate", "--config=" + str(MONITOR / "otelcol-contrib.homelab.yml")],
):
    subprocess.run(list(map(str, command)), check=True)
links = json.loads((MONITOR / "dashboard-links.json").read_text())
assert links["base_url"] == "http://192.168.86.21:9090"
assert all(graph["url"].startswith(links["base_url"]) for graph in links["graphs"])
print("Homelab drafts validated offline; no monitoring service started or remote host contacted.")

#!/usr/bin/env python3
"""Generate or verify the candidate producer contract bundle digest manifest."""

import argparse
import hashlib
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "docs" / "PRODUCER-CONTRACT-MANIFEST.json"
FILES = (
    "schemas/session-record-v1.schema.json",
    "schemas/capture-delivery-v1.schema.json",
    "schemas/capture-delivery-v2.schema.json",
    "schemas/archive-feed-v1.schema.json",
    "schemas/redaction-receipt-v1.schema.json",
    "testdata/identity-v1.json",
    "testdata/claude/session.jsonl",
    "testdata/claude/subagent.jsonl",
    "testdata/codex/session.jsonl",
    "testdata/capture-delivery/accepted-v1.json",
    "testdata/capture-delivery/unsupported-v2.json",
    "testdata/capture-delivery/scenarios-v1.json",
    "testdata/archive-feed/page-v1.json",
    "testdata/archive-feed/scenarios-v1.json",
    "testdata/capture-spool/scenarios-v1.json",
    "testdata/redaction-receipt/receipt-v1.json",
)


def current_manifest() -> dict:
    return {
        "manifest_version": 1,
        "module": "github.com/algorhythmic/skald",
        "status": "untagged_candidate",
        "contract_versions": {
            "session_record": 1,
            "capture_delivery": 2,
            "archive_feed": 1,
            "redaction_receipt": 1,
        },
        "files": {
            name: "sha256:" + hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
            for name in FILES
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", action="store_true", help="write candidate manifest")
    group.add_argument("--check", action="store_true", help="verify current files against manifest")
    args = parser.parse_args()
    expected = current_manifest()
    if args.write:
        MANIFEST.write_text(json.dumps(expected, indent=2, sort_keys=True) + "\n")
        print(MANIFEST.relative_to(ROOT))
        return 0
    try:
        saved = json.loads(MANIFEST.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"manifest unavailable: {exc}", file=sys.stderr)
        return 1
    if saved != expected:
        print("producer contract manifest differs from current bundle", file=sys.stderr)
        return 1
    print("producer contract manifest matches current bundle")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

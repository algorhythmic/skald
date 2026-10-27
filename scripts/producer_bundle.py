#!/usr/bin/env python3
"""Create an immutable, content-addressed producer candidate (never publishes)."""
import argparse
import gzip
import hashlib
import io
import json
import os
import re
from pathlib import Path
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ("sessionrecord", "sessioncapture", "archivefeed", "capturespool", "schemas", "testdata", "cmd/producer-verify")
DOCS = ("CONTRACT-V1.md", "CAPTURE-DELIVERY-V1.md", "ARCHIVE-FEED-V1.md", "CAPTURE-SPOOL-V1.md", "CONTENT-REMOVAL-V1.md", "PRODUCER-CONTRACT-MANIFEST.json", "producer-bundle.md")

VERSION_TAG = re.compile(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?")

def release_revision(root, tag, names):
    if not VERSION_TAG.fullmatch(tag):
        raise ValueError("invalid release version tag")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    tagged = subprocess.check_output(["git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}"], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
    if head != tagged:
        raise ValueError("release tag does not identify HEAD")
    subprocess.run(["git", "ls-files", "--error-unmatch", "--", *names], cwd=root, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], cwd=root, check=True)
    return head

def build(root=ROOT, release_tag=None):
    paths = {root / name for name in ("go.mod", "go.sum", "LICENSE")}
    for directory in PACKAGES:
        paths.update(p for p in (root / directory).rglob("*") if p.is_file())
    paths.update(root / "docs" / name for name in DOCS)
    names = sorted(path.relative_to(root).as_posix() for path in paths)
    revision = release_revision(root, release_tag, names) if release_tag else subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    files = {}
    for path in sorted(paths):
        if path.is_symlink() or any(p.is_symlink() for p in path.parents if p != root.parent):
            raise ValueError(f"symlink refused: {path}")
        name = path.relative_to(root).as_posix()
        data = path.read_bytes()
        if release_tag and data != subprocess.check_output(["git", "show", f"{revision}:{name}"], cwd=root):
            raise ValueError(f"working bytes differ from release revision: {name}")
        files[name] = data
    if release_tag and release_revision(root, release_tag, names) != revision:
        raise ValueError("release revision changed during packaging")
    manifest = {"bundle_version": 1, "module": "github.com/algorhythmic/skald", "status": "untagged_candidate", "base_revision": revision,
                "files": {name: "sha256:" + hashlib.sha256(data).hexdigest() for name, data in files.items()}}
    if release_tag:
        manifest.update(status="release_candidate", release_tag=release_tag, source_revision=revision)
    wire = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
    files["PRODUCER-BUNDLE.json"] = wire
    tar = io.BytesIO()
    with tarfile.open(fileobj=tar, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name, data in sorted(files.items()):
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(data), 0o644, 0
            archive.addfile(entry, io.BytesIO(data))
    compressed = io.BytesIO()
    with gzip.GzipFile(fileobj=compressed, mode="wb", filename="", mtime=0) as stream:
        stream.write(tar.getvalue())
    return compressed.getvalue(), hashlib.sha256(wire).hexdigest()

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--release-tag", help="exact version tag at clean HEAD; selected files must be tracked")
    args = parser.parse_args()
    try:
        payload, manifest_hash = build(release_tag=args.release_tag)
    except (ValueError, subprocess.CalledProcessError) as exc:
        parser.error(f"release candidate validation failed: {exc}")
    digest = hashlib.sha256(payload).hexdigest()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    path = args.output_dir / f"skald-producer-candidate-{digest}.tar.gz"
    # Never overwrite a prior artifact, even one with an identical filename.
    with path.open("xb") as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())
    checksum = path.with_suffix(path.suffix + ".sha256")
    with checksum.open("x") as stream:
        stream.write(f"{digest}  {path.name}\n")
    print(json.dumps({"artifact": str(path), "sha256": digest, "manifest_sha256": manifest_hash, "status": "release_candidate" if args.release_tag else "untagged_candidate", "release_tag": args.release_tag, "published": False}, indent=2))

if __name__ == "__main__":
    main()

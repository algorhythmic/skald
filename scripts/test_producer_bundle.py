#!/usr/bin/env python3
"""Synthetic checks for deterministic, self-contained candidate packaging."""
import hashlib
import io
import json
import tarfile
import unittest
import subprocess
import tempfile
from pathlib import Path
from producer_bundle import build, PACKAGES, DOCS

class BundleTests(unittest.TestCase):
    def test_content_addressed_bundle_is_deterministic_and_complete(self):
        first, digest = build()
        second, second_digest = build()
        self.assertEqual(first, second)
        self.assertEqual(digest, second_digest)
        with tarfile.open(fileobj=io.BytesIO(first), mode="r:gz") as archive:
            members = archive.getmembers()
            self.assertTrue(all(m.isfile() and not m.name.startswith("/") and ".." not in m.name.split("/") for m in members))
            wire = archive.extractfile("PRODUCER-BUNDLE.json").read()
            self.assertEqual(digest, hashlib.sha256(wire).hexdigest())
            manifest = json.loads(wire)
            self.assertEqual(manifest["status"], "untagged_candidate")
            for name, expected in manifest["files"].items():
                self.assertEqual(expected, "sha256:" + hashlib.sha256(archive.extractfile(name).read()).hexdigest())
            self.assertIn("cmd/producer-verify/main.go", manifest["files"])
            self.assertIn("sessioncapture/delivery.go", manifest["files"])


class ReleaseBundleTests(unittest.TestCase):
    def fixture(self):
        temporary = tempfile.TemporaryDirectory(prefix="skald-release-test-")
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        for name in ("go.mod", "go.sum", "LICENSE"):
            (root / name).write_text("synthetic fixture\n")
        for name in PACKAGES:
            path = root / name / "fixture.txt"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("synthetic package fixture\n")
        for name in DOCS:
            path = root / "docs" / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("synthetic contract fixture\n")
        subprocess.run(["git", "init", "-q", str(root)], check=True)
        self.git(root, "add", ".")
        self.git(root, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "synthetic fixture")
        self.git(root, "tag", "v0.2.0-rc.1")
        return root

    def git(self, root, *args):
        return subprocess.check_output(["git", *args], cwd=root, stderr=subprocess.DEVNULL)

    def test_release_requires_exact_clean_tracked_revision(self):
        root = self.fixture()
        payload, digest = build(root, "v0.2.0-rc.1")
        with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as archive:
            wire = archive.extractfile("PRODUCER-BUNDLE.json").read()
            manifest = json.loads(wire)
            self.assertEqual(manifest["status"], "release_candidate")
            self.assertEqual(manifest["release_tag"], "v0.2.0-rc.1")
            self.assertEqual(manifest["source_revision"], self.git(root, "rev-parse", "HEAD").decode().strip())
            self.assertEqual(digest, hashlib.sha256(wire).hexdigest())
        with self.assertRaises((ValueError, subprocess.CalledProcessError)):
            build(root, "v0.2.0-rc.2")
        with self.assertRaises(ValueError):
            build(root, "../false-tag")
        (root / "go.mod").write_text("dirty fixture\n")
        with self.assertRaises(subprocess.CalledProcessError):
            build(root, "v0.2.0-rc.1")

    def test_release_refuses_untracked_selected_files_and_old_tag(self):
        root = self.fixture()
        (root / PACKAGES[0] / "untracked.txt").write_text("not committed\n")
        with self.assertRaises(subprocess.CalledProcessError):
            build(root, "v0.2.0-rc.1")
        self.git(root, "add", ".")
        self.git(root, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "second fixture")
        with self.assertRaises(ValueError):
            build(root, "v0.2.0-rc.1")

if __name__ == "__main__":
    unittest.main()

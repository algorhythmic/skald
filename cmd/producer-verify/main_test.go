package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixtureBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	m := bundleManifest{Version: 1, Status: "untagged_candidate", Module: "github.com/algorhythmic/skald", BaseRevision: "fixture", Files: map[string]string{}}
	names := []string{"testdata/identity-v1.json", "testdata/capture-delivery/accepted-v1.json", "testdata/capture-delivery/unsupported-v2.json", "testdata/archive-feed/page-v1.json", "testdata/redaction-receipt/receipt-v1.json", "schemas/session-record-v1.schema.json", "schemas/capture-delivery-v1.schema.json", "schemas/archive-feed-v1.schema.json", "schemas/redaction-receipt-v1.schema.json"}
	for _, name := range names {
		data, e := os.ReadFile(filepath.Join("../..", name))
		if e != nil {
			t.Fatal(e)
		}
		target := filepath.Join(root, name)
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(target, data, 0600); e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(data)
		m.Files[name] = "sha256:" + hex.EncodeToString(h[:])
	}
	wire, _ := json.Marshal(m)
	if e := os.WriteFile(filepath.Join(root, "PRODUCER-BUNDLE.json"), wire, 0600); e != nil {
		t.Fatal(e)
	}
	return root
}
func TestFixtureConsumerAndDigestPin(t *testing.T) {
	root := fixtureBundle(t)
	wire, _ := os.ReadFile(filepath.Join(root, "PRODUCER-BUNDLE.json"))
	var out bytes.Buffer
	if e := verify(root, sum(wire), &out); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"runtime_consumer_acceptance": false`)) {
		t.Fatal(out.String())
	}
	if e := verify(root, "incorrect", &out); e == nil {
		t.Fatal("wrong digest accepted")
	}
	if e := os.WriteFile(filepath.Join(root, "testdata/identity-v1.json"), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := verify(root, sum(wire), &out); e == nil {
		t.Fatal("tampered fixture accepted")
	}
}
func TestBundleRefusesTraversalAndLinks(t *testing.T) {
	root := t.TempDir()
	if _, e := read(root, "../outside"); e == nil {
		t.Fatal("traversal accepted")
	}
	target := filepath.Join(root, "target")
	os.WriteFile(target, []byte("data"), 0600)
	os.Symlink(target, filepath.Join(root, "link"))
	if _, e := read(root, "link"); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestReleaseManifestAndExpectedTag(t *testing.T) {
	root := fixtureBundle(t)
	path := filepath.Join(root, "PRODUCER-BUNDLE.json")
	wire, _ := os.ReadFile(path)
	var manifest bundleManifest
	if e := json.Unmarshal(wire, &manifest); e != nil {
		t.Fatal(e)
	}
	manifest.Status = "release_candidate"
	manifest.ReleaseTag = "v0.2.0-rc.1"
	manifest.SourceRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest.BaseRevision = manifest.SourceRevision
	save := func() string {
		data, _ := json.Marshal(manifest)
		if e := os.WriteFile(path, data, 0600); e != nil {
			t.Fatal(e)
		}
		return sum(data)
	}
	digest := save()
	var out bytes.Buffer
	if e := verifyRelease(root, digest, "v0.2.0-rc.1", &out); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"expected_release_tag_verified": true`)) || !bytes.Contains(out.Bytes(), []byte(`"publication_verified": false`)) {
		t.Fatal(out.String())
	}
	for _, pin := range []struct{ digest, tag string }{{"", "v0.2.0-rc.1"}, {digest, "v0.2.0-rc.2"}, {digest, "../false-tag"}} {
		if e := verifyRelease(root, pin.digest, pin.tag, &out); e == nil {
			t.Fatal("invalid release pin accepted", pin)
		}
	}
	manifest.ReleaseTag = "latest"
	if e := verify(root, save(), &out); e == nil {
		t.Fatal("nonversion release tag accepted")
	}
	manifest.ReleaseTag = "v0.2.0-rc.1"
	manifest.SourceRevision = ""
	if e := verify(root, save(), &out); e == nil {
		t.Fatal("missing release revision accepted")
	}
	manifest.SourceRevision = manifest.BaseRevision
	manifest.Files["../traversal"] = "sha256:fake"
	if e := verify(root, save(), &out); e == nil {
		t.Fatal("release traversal accepted")
	}
}

// producer-verify is a standalone, fixture-only consumer of Skald's public API.
// It intentionally imports no Skald internal package or consumer service code.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/algorhythmic/skald/archivefeed"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	"github.com/google/jsonschema-go/jsonschema"
)

type bundleManifest struct {
	Version        int               `json:"bundle_version"`
	Status         string            `json:"status"`
	Module         string            `json:"module"`
	BaseRevision   string            `json:"base_revision"`
	ReleaseTag     string            `json:"release_tag,omitempty"`
	SourceRevision string            `json:"source_revision,omitempty"`
	Files          map[string]string `json:"files"`
}

func sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func read(root, name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, errors.New("unsafe_bundle_path")
	}
	p := filepath.Join(root, name)
	for q := p; q != filepath.Dir(root); q = filepath.Dir(q) {
		i, e := os.Lstat(q)
		if e != nil {
			return nil, e
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("bundle_symlink_refused")
		}
		if q == root {
			break
		}
	}
	i, e := os.Stat(p)
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() || i.Size() > 16<<20 {
		return nil, errors.New("invalid_bundle_file")
	}
	return os.ReadFile(p)
}

var releaseTagPattern = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)
var revisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func verify(root, expected string, out io.Writer) error {
	return verifyRelease(root, expected, "", out)
}
func verifyRelease(root, expected, expectedTag string, out io.Writer) error {
	absolute, e := filepath.Abs(root)
	if e != nil {
		return e
	}
	root = absolute
	wire, e := read(root, "PRODUCER-BUNDLE.json")
	if e != nil {
		return e
	}
	manifestHash := sum(wire)
	if expected != "" && strings.TrimPrefix(expected, "sha256:") != manifestHash {
		return errors.New("bundle_manifest_pin_mismatch")
	}
	var m bundleManifest
	if e = json.Unmarshal(wire, &m); e != nil {
		return e
	}
	if m.Version != 1 || m.Module != "github.com/algorhythmic/skald" || len(m.Files) == 0 {
		return errors.New("invalid_bundle_manifest")
	}
	switch m.Status {
	case "untagged_candidate":
		if m.ReleaseTag != "" || m.SourceRevision != "" {
			return errors.New("invalid_candidate_release_claim")
		}
	case "release_candidate":
		if !releaseTagPattern.MatchString(m.ReleaseTag) || !revisionPattern.MatchString(m.SourceRevision) || m.BaseRevision != m.SourceRevision {
			return errors.New("invalid_release_candidate")
		}
	default:
		return errors.New("invalid_bundle_status")
	}
	if expectedTag != "" {
		if expected == "" {
			return errors.New("release_tag_requires_manifest_pin")
		}
		if !releaseTagPattern.MatchString(expectedTag) || m.Status != "release_candidate" || m.ReleaseTag != expectedTag {
			return errors.New("bundle_release_tag_pin_mismatch")
		}
	}
	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, e := read(root, name)
		if e != nil {
			return e
		}
		if "sha256:"+sum(data) != m.Files[name] {
			return fmt.Errorf("bundle_digest_mismatch: %s", name)
		}
	}
	fixture := func(name string) ([]byte, error) {
		if _, ok := m.Files[name]; !ok {
			return nil, errors.New("fixture_not_in_manifest")
		}
		return read(root, name)
	}
	identities, e := fixture("testdata/identity-v1.json")
	if e != nil {
		return e
	}
	var vectors []struct {
		Domain       string
		Fields       []string
		CanonicalHex string `json:"canonical_hex"`
		Key          string
	}
	if e = json.Unmarshal(identities, &vectors); e != nil {
		return e
	}
	for _, v := range vectors {
		if sessionrecord.Key(v.Domain, v.Fields...) != v.Key || hex.EncodeToString(sessionrecord.Canonical(v.Domain, v.Fields...)) != v.CanonicalHex {
			return errors.New("identity_vector_mismatch")
		}
	}
	accepted, e := fixture("testdata/capture-delivery/accepted-v1.json")
	if e != nil {
		return e
	}
	delivery, e := sessioncapture.DecodeDelivery(accepted)
	if e != nil {
		return e
	}
	unsupported, e := fixture("testdata/capture-delivery/unsupported-v2.json")
	if e != nil {
		return e
	}
	if _, e = sessioncapture.DecodeDelivery(unsupported); !errors.Is(e, sessioncapture.ErrDeliveryVersion) {
		return errors.New("unsupported_version_was_not_refused")
	}
	feed, e := fixture("testdata/archive-feed/page-v1.json")
	if e != nil {
		return e
	}
	page, e := archivefeed.DecodePage(feed)
	if e != nil {
		return e
	}
	page.Version = 2
	unsupportedFeed, _ := json.Marshal(page)
	if _, e = archivefeed.DecodePage(unsupportedFeed); !errors.Is(e, archivefeed.ErrUnsupportedVersion) {
		return errors.New("unsupported_feed_was_not_refused")
	}
	if len(delivery.Batch.Records) == 0 {
		return errors.New("accepted_fixture_has_no_records")
	}
	var record any
	recordWire, _ := json.Marshal(delivery.Batch.Records[0].Record)
	if e = json.Unmarshal(recordWire, &record); e != nil {
		return e
	}
	for _, pair := range []struct{ schema, fixture string }{{"capture-delivery-v1.schema.json", "testdata/capture-delivery/accepted-v1.json"}, {"archive-feed-v1.schema.json", "testdata/archive-feed/page-v1.json"}, {"redaction-receipt-v1.schema.json", "testdata/redaction-receipt/receipt-v1.json"}, {"session-record-v1.schema.json", ""}} {
		schemaWire, e := fixture("schemas/" + pair.schema)
		if e != nil {
			return e
		}
		var schema jsonschema.Schema
		if e = json.Unmarshal(schemaWire, &schema); e != nil {
			return e
		}
		resolved, e := schema.Resolve(&jsonschema.ResolveOptions{Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
			if uri.String() != "https://schemas.skald.invalid/session-record-v1.schema.json" {
				return nil, errors.New("unexpected_schema_reference")
			}
			data, e := fixture("schemas/session-record-v1.schema.json")
			if e != nil {
				return nil, e
			}
			var recordSchema jsonschema.Schema
			e = json.Unmarshal(data, &recordSchema)
			return &recordSchema, e
		}})
		if e != nil {
			return e
		}
		value := record
		if pair.fixture != "" {
			data, e := fixture(pair.fixture)
			if e != nil {
				return e
			}
			if e = json.Unmarshal(data, &value); e != nil {
				return e
			}
		}
		if e = resolved.Validate(value); e != nil {
			return fmt.Errorf("fixture_schema_mismatch: %s: %w", pair.schema, e)
		}
	}
	receipt := map[string]any{"status": "fixture_compatibility_verified", "bundle_status": m.Status, "manifest_sha256": "sha256:" + manifestHash, "expected_manifest_verified": expected != "", "base_revision": m.BaseRevision, "release_tag": m.ReleaseTag, "source_revision": m.SourceRevision, "expected_release_tag_verified": expectedTag != "", "files_verified": len(names), "identity_vectors": len(vectors), "runtime_consumer_acceptance": false, "publication_verified": false}
	encoded, e := json.MarshalIndent(receipt, "", "  ")
	if e != nil {
		return e
	}
	_, e = io.Copy(out, bytes.NewReader(append(encoded, '\n')))
	return e
}
func main() {
	root := flag.String("bundle-root", ".", "extracted producer candidate root")
	expected := flag.String("expected-manifest-sha256", "", "externally recorded manifest digest; omission verifies fixture compatibility only")
	expectedTag := flag.String("expected-release-tag", "", "expected release tag; requires expected manifest digest")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(1)
	}
	if e := verifyRelease(*root, *expected, *expectedTag, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

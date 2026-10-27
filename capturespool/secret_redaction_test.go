package capturespool

import (
	"encoding/json"
	"github.com/algorhythmic/skald/sessioncapture"
	"os"
	"path/filepath"
	"testing"
)

func TestSensitiveEnvelopeSpooledSanitized(t *testing.T) {
	d := fixture(t)
	d.Batch.Records[0].Record.Extensions = map[string]json.RawMessage{"password": json.RawMessage(`"synthetic"`)}
	d, e := sessioncapture.NewDelivery(d.Source, d.Previous, d.Batch)
	if e != nil {
		t.Fatal("fixture invalid")
	}
	// A testing.TempDir may inherit a public mode from TMPDIR/umask.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal("fixture parent mode")
	}
	dir := filepath.Join(parent, "private-spool")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal("private fixture directory")
	}
	s := open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	if e = s.Enqueue(d, []string{"archive"}); e != nil {
		t.Fatal("sanitized envelope not spooled")
	}
	if d.Version != 2 || d.Batch.Records[0].Redaction == nil {
		t.Fatal("sanitized representation missing")
	}
}

func TestSensitiveSpoolRefusesPublicDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal("fixture directory mode")
	}
	s, err := Open(dir, []string{"archive"}, generous())
	if s != nil {
		s.Close()
	}
	if err == nil || err.Error() != "private_owned_directory_required" {
		t.Fatal("public spool directory not refused with stable error")
	}
}

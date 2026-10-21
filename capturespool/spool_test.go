package capturespool

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

type sink struct {
	name      string
	fail      bool
	failID    string
	lostOnce  bool
	committed map[string]bool
	wrong     bool
	seen      []string
}

func (s *sink) Destination() string { return s.name }
func (s *sink) Accept(_ context.Context, d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
	s.seen = append(s.seen, d.BatchID)
	if s.lostOnce && !s.committed[d.BatchID] {
		s.committed[d.BatchID] = true
		return sessioncapture.Acknowledgement{}, errors.New("response lost after commit")
	}
	if s.fail || s.failID == d.BatchID {
		return sessioncapture.Acknowledgement{}, errors.New("synthetic outage")
	}
	name := s.name
	if s.wrong {
		name = "foreign"
	}
	return sessioncapture.Acknowledgement{Version: 1, Destination: name, BatchID: d.BatchID, SourceKey: d.Source.Key(), Checkpoint: d.Batch.Checkpoint}, nil
}

func fixture(t *testing.T) sessioncapture.Delivery {
	t.Helper()
	wire, err := os.ReadFile("../testdata/capture-delivery/accepted-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := sessioncapture.DecodeDelivery(wire)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func open(t *testing.T, dir string, names []string, limits Limits) *Spool {
	t.Helper()
	s, err := Open(dir, names, limits)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func generous() Limits { return Limits{MaxPending: 10, MaxBytes: 10 << 20} }

func TestRestartIndependentAcksAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	d := fixture(t)
	s := open(t, dir, []string{"archive", "peer"}, generous())
	if err := s.Enqueue(d, []string{"peer", "archive"}); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := s.Acknowledged(d.BatchID, "archive"); err != nil || acknowledged {
		t.Fatalf("premature archive receipt %v %v", acknowledged, err)
	}
	cp, ok, err := s.Checkpoint(d.Source.Key())
	if err != nil || !ok || cp != d.Batch.Checkpoint {
		t.Fatalf("checkpoint %v %v %v", cp, ok, err)
	}
	archive := &sink{name: "archive"}
	if n, err := s.Deliver(context.Background(), archive, 1); err != nil || n != 1 {
		t.Fatalf("archive %d %v", n, err)
	}
	if acknowledged, err := s.Acknowledged(d.BatchID, "archive"); err != nil || !acknowledged {
		t.Fatalf("missing archive receipt %v %v", acknowledged, err)
	}
	if acknowledged, err := s.Acknowledged(d.BatchID, "peer"); err != nil || acknowledged {
		t.Fatalf("peer borrowed archive receipt %v %v", acknowledged, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, []string{"peer", "archive"}, generous())
	defer s.Close()
	status, err := s.Status()
	if err != nil || status.Pending != 1 || status.ByDestination["archive"] != 0 || status.ByDestination["peer"] != 1 {
		t.Fatalf("status %+v %v", status, err)
	}
	peer := &sink{name: "peer"}
	if n, err := s.Deliver(context.Background(), peer, 1); err != nil || n != 1 {
		t.Fatalf("peer %d %v", n, err)
	}
	for _, destination := range []string{"archive", "peer"} {
		if acknowledged, err := s.Acknowledged(d.BatchID, destination); err != nil || !acknowledged {
			t.Fatalf("pruned receipt %s %v %v", destination, acknowledged, err)
		}
	}
	if acknowledged, err := s.Acknowledged("unknown", "archive"); err != nil || acknowledged {
		t.Fatalf("unknown batch %v %v", acknowledged, err)
	}
	if _, err := s.Acknowledged(d.BatchID, "foreign"); !errors.Is(err, ErrRegistry) {
		t.Fatalf("foreign destination %v", err)
	}
	status, err = s.Status()
	if err != nil || status.Pending != 0 {
		t.Fatalf("prune %+v %v", status, err)
	}
	if err := s.Enqueue(d, []string{"archive", "peer"}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	status, _ = s.Status()
	if status.Pending != 0 {
		t.Fatalf("replayed completed: %+v", status)
	}
	for _, name := range []string{"archive", "peer"} {
		cp, ok, err := s.DestinationCheckpoint(d.Source.Key(), name)
		if err != nil || !ok || cp != d.Batch.Checkpoint {
			t.Fatalf("destination %s %v %v %v", name, cp, ok, err)
		}
	}
}

func TestFailuresAndUncertainAcceptance(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive", "peer"}, generous())
	defer s.Close()
	d := fixture(t)
	if err := s.Enqueue(d, []string{"archive", "peer"}); err != nil {
		t.Fatal(err)
	}
	broken := &sink{name: "archive", wrong: true}
	if n, err := s.Deliver(context.Background(), broken, 1); n != 0 || err == nil {
		t.Fatalf("foreign ack accepted: %d %v", n, err)
	}
	broken.wrong = false
	broken.fail = true
	if n, err := s.Deliver(context.Background(), broken, 1); n != 0 || err == nil {
		t.Fatalf("uncertain error accepted: %d %v", n, err)
	}
	peer := &sink{name: "peer"}
	if n, err := s.Deliver(context.Background(), peer, 1); n != 1 || err != nil {
		t.Fatalf("independent peer: %d %v", n, err)
	}
	broken.fail = false
	if n, err := s.Deliver(context.Background(), broken, 1); n != 1 || err != nil || len(broken.seen) != 3 {
		t.Fatalf("retry: %d %v %v", n, err, broken.seen)
	}
}

func TestCapacityRegistryLockAndCheckpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	d := fixture(t)
	s := open(t, dir, []string{"archive"}, Limits{MaxPending: 1, MaxBytes: 10 << 20})
	if _, err := Open(dir, []string{"archive"}, generous()); !errors.Is(err, ErrInUse) {
		t.Fatalf("lock: %v", err)
	}
	if err := s.Enqueue(d, []string{"peer"}); !errors.Is(err, ErrRegistry) {
		t.Fatalf("registry: %v", err)
	}
	if err := s.Enqueue(d, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	b := d.Batch
	b.Records = []sessioncapture.Captured{}
	b.Gaps = []sessioncapture.Gap{}
	b.More = !b.More
	second, err := sessioncapture.NewDelivery(d.Source, d.Batch.Checkpoint, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(second, []string{"archive"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, []string{"archive", "peer"}, generous()); !errors.Is(err, ErrRegistry) {
		t.Fatalf("changed registry: %v", err)
	}
	s = open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	if err := s.Enqueue(second, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(second, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(d, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	ack := &sink{name: "archive"}
	if n, err := s.Deliver(context.Background(), ack, 2); err != nil || n != 2 || len(ack.seen) != 2 || ack.seen[0] != d.BatchID || ack.seen[1] != second.BatchID {
		t.Fatalf("order %d %v %v", n, err, ack.seen)
	}
}

func TestSeedAndCorruptState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	d := fixture(t)
	multi := open(t, filepath.Join(t.TempDir(), "multi"), []string{"archive", "peer"}, generous())
	if err := multi.SeedCheckpoint(d.Source, d.Batch.Checkpoint); !errors.Is(err, ErrRegistry) {
		t.Fatalf("multi destination seed %v", err)
	}
	multi.Close()
	s := open(t, dir, []string{"archive"}, generous())
	if err := s.SeedCheckpoint(d.Source, d.Batch.Checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCheckpoint(d.Source, d.Batch.Checkpoint); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("reseed %v", err)
	}
	cp, ok, err := s.DestinationCheckpoint(d.Source.Key(), "archive")
	if err != nil || !ok || cp != d.Batch.Checkpoint {
		t.Fatalf("seed destination %v %v %v", cp, ok, err)
	}
	if err := s.Enqueue(d, []string{"archive"}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("stale prefix %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// SQLite's integrity check must reject a damaged header before a writer opens.
	path := filepath.Join(dir, "capture-spool.sqlite")
	if err := os.WriteFile(path, []byte("damaged sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, []string{"archive"}, generous()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption %v", err)
	}
}

func TestRetainedProgressWithoutRawEnvelope(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	d := fixture(t)
	if err := s.Enqueue(d, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(context.Background(), &sink{name: "archive"}, 1); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err := s.db.QueryRow("SELECT envelope_digest FROM batch_ids WHERE batch_id=?", d.BatchID).Scan(&retained); err != nil || len(retained) != 32 {
		t.Fatalf("retained digest %d %v", len(retained), err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM deliveries").Scan(&count); err != nil || count != 0 {
		t.Fatalf("envelopes %d %v", count, err)
	}
}

func TestFailedSourceDoesNotBlockOtherSource(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
	defer s.Close()
	first := fixture(t)
	if err := s.Enqueue(first, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	b := first.Batch
	b.Records = []sessioncapture.Captured{}
	b.Gaps = []sessioncapture.Gap{}
	b.More = !b.More
	second, err := sessioncapture.NewDelivery(first.Source, first.Batch.Checkpoint, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(second, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	source := sessioncapture.Source{Namespace: "fixture:another", Provider: sessioncapture.Claude, StreamID: "second-stream"}
	cp := sessioncapture.Checkpoint{Version: 1, SourceKey: source.Key(), PrefixDigest: sessionrecord.Digest(nil), ProviderVersion: "test"}
	other, err := sessioncapture.NewDelivery(source, sessioncapture.Checkpoint{}, sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: cp})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(other, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	sink := &sink{name: "archive", failID: first.BatchID}
	if n, err := s.Deliver(context.Background(), sink, 3); n != 1 || err == nil || len(sink.seen) != 2 || sink.seen[0] != first.BatchID || sink.seen[1] != other.BatchID {
		t.Fatalf("fairness %d %v %v", n, err, sink.seen)
	}
	status, _ := s.Status()
	if status.Pending != 2 {
		t.Fatalf("pending %+v", status)
	}
}

func TestLostReceiptRestartRetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	d := fixture(t)
	s := open(t, dir, []string{"archive"}, generous())
	if err := s.Enqueue(d, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	sink := &sink{name: "archive", lostOnce: true, committed: map[string]bool{}}
	if n, err := s.Deliver(context.Background(), sink, 1); n != 0 || err == nil {
		t.Fatalf("uncertain %d %v", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	if n, err := s.Deliver(context.Background(), sink, 1); n != 1 || err != nil || len(sink.seen) != 2 || sink.seen[0] != sink.seen[1] {
		t.Fatalf("retry %d %v %v", n, err, sink.seen)
	}
}

func TestBindingAndNamespaceBacklogSurviveRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	d := fixture(t)
	s := open(t, dir, []string{"archive"}, generous())
	if err := s.BindDestination("archive", "archive-instance-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindDestination("archive", "archive-instance-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindDestination("archive", "archive-instance-two"); !errors.Is(err, ErrBinding) {
		t.Fatalf("different archive bound: %v", err)
	}
	if err := s.Enqueue(d, []string{"archive"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.StatusForNamespace(d.Source.Namespace)
	if err != nil || status.Pending != 1 || status.ByDestination["archive"] != 1 {
		t.Fatalf("namespace backlog %+v %v", status, err)
	}
	foreign, err := s.StatusForNamespace("fixture:foreign")
	if err != nil || foreign.Pending != 0 {
		t.Fatalf("foreign backlog %+v %v", foreign, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	if err := s.BindDestination("archive", "archive-instance-two"); !errors.Is(err, ErrBinding) {
		t.Fatalf("restart binding mismatch: %v", err)
	}
	if err := s.BindDestination("archive", "archive-instance-one"); err != nil {
		t.Fatal(err)
	}
	status, err = s.StatusForNamespace(d.Source.Namespace)
	if err != nil || status.Pending != 1 {
		t.Fatalf("restart namespace %+v %v", status, err)
	}
	if _, err := s.Deliver(context.Background(), &sink{name: "archive"}, 1); err != nil {
		t.Fatal(err)
	}
	status, err = s.StatusForNamespace(d.Source.Namespace)
	if err != nil || status.Pending != 0 {
		t.Fatalf("pruned namespace %+v %v", status, err)
	}
	if err := s.BindDestination("foreign", "archive-instance-one"); !errors.Is(err, ErrRegistry) {
		t.Fatalf("foreign binding %v", err)
	}
}

func TestInterruptedInitialDDLCanOpenCleanly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := privateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "capture-spool.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO missing_meta VALUES (1)"); err == nil {
		t.Fatal("injected init failure did not fail")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "capture-spool.sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	var count, version int
	if err := s.db.QueryRow("SELECT count(*) FROM spool_meta").Scan(&count); err != nil || count != 1 {
		t.Fatalf("registry rows %d %v", count, err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version %d %v", version, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, []string{"archive"}, generous())
	defer s.Close()
	if err := s.db.QueryRow("SELECT count(*) FROM spool_meta").Scan(&count); err != nil || count != 1 {
		t.Fatalf("double registry insert %d %v", count, err)
	}
}

func TestSeedCheckpointRejectsMalformedBoundary(t *testing.T) {
	d := fixture(t)
	for _, mutate := range []func(*sessioncapture.Checkpoint){
		func(cp *sessioncapture.Checkpoint) { cp.PrefixDigest = "invalid" },
		func(cp *sessioncapture.Checkpoint) { cp.Generation = "" },
	} {
		s := open(t, filepath.Join(t.TempDir(), "spool"), []string{"archive"}, generous())
		cp := d.Batch.Checkpoint
		mutate(&cp)
		if err := s.SeedCheckpoint(d.Source, cp); !errors.Is(err, ErrCheckpoint) {
			t.Fatalf("malformed seed accepted: %v", err)
		}
		if _, found, err := s.Checkpoint(d.Source.Key()); err != nil || found {
			t.Fatalf("malformed seed persisted: %v %v", found, err)
		}
		s.Close()
	}
}

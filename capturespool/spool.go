// Package capturespool persists immutable capture deliveries and independent
// destination receipts before a collector advances to another source prefix.
package capturespool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/algorhythmic/skald/internal/secretredact"
	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
	_ "modernc.org/sqlite"
)

var (
	ErrInUse      = errors.New("capture_spool_in_use")
	ErrRegistry   = errors.New("capture_spool_destination_registry_mismatch")
	ErrCapacity   = errors.New("capture_spool_capacity")
	ErrCheckpoint = errors.New("capture_spool_checkpoint_mismatch")
	ErrDuplicate  = errors.New("capture_spool_duplicate_batch_conflict")
	ErrCorrupt    = errors.New("capture_spool_corrupt_state")
	ErrClosed     = errors.New("capture_spool_closed")
	ErrBinding    = errors.New("capture_spool_destination_binding_mismatch")
)

const schemaVersion = 1

// Limits bounds envelopes awaiting at least one acknowledgement. Both values
// must be positive. MaxBytes counts persisted JSON envelope bytes.
type Limits struct {
	MaxPending int
	MaxBytes   int64
}

type Status struct {
	Pending       int            `json:"pending"`
	PendingBytes  int64          `json:"pending_bytes"`
	ByDestination map[string]int `json:"by_destination"`
	MaxPending    int            `json:"max_pending"`
	MaxBytes      int64          `json:"max_bytes"`
}

type Spool struct {
	mu           sync.Mutex
	drainMu      sync.Mutex
	db           *sql.DB
	lock         *os.File
	destinations []string
	limits       Limits
}

func registry(destinations []string) ([]string, string, error) {
	if len(destinations) == 0 {
		return nil, "", ErrRegistry
	}
	copyNames := append([]string(nil), destinations...)
	sort.Strings(copyNames)
	for i, name := range copyNames {
		if name == "" || strings.TrimSpace(name) != name || strings.IndexByte(name, 0) >= 0 || (i > 0 && name == copyNames[i-1]) {
			return nil, "", ErrRegistry
		}
	}
	wire, _ := json.Marshal(copyNames)
	return copyNames, string(wire), nil
}

func privateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("private_owned_file_required")
	}
	return nil
}

func privateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("private_owned_directory_required")
	}
	return nil
}

// Open claims the directory's sole writer lock. A changed destination registry
// is refused, including a newly added destination: historical envelopes cannot
// be implicitly backfilled or considered acknowledged.
func Open(dir string, destinations []string, limits Limits) (_ *Spool, finalErr error) {
	if limits.MaxPending <= 0 || limits.MaxBytes <= 0 {
		return nil, ErrCapacity
	}
	names, registryWire, err := registry(destinations)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "writer.lock")
	if err := privateFile(lockPath); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrInUse
	}
	s := &Spool{lock: lock, destinations: names, limits: limits}
	defer func() {
		if finalErr != nil {
			_ = s.Close()
		}
	}()
	path := filepath.Join(dir, "capture-spool.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			if err := privateFile(path + suffix); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := privateFile(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if version != 0 && version != schemaVersion {
		return nil, ErrCorrupt
	}
	if version == 0 {
		var tables int
		if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
			return nil, err
		}
		if tables != 0 {
			return nil, ErrCorrupt
		}
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schema); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("INSERT INTO spool_meta(id,destinations) VALUES (1,?)", registryWire); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=256"); err != nil {
		return nil, err
	}
	var stored string
	err = s.db.QueryRow("SELECT destinations FROM spool_meta WHERE id=1").Scan(&stored)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	} else if stored != registryWire {
		return nil, ErrRegistry
	}
	var integrity string
	if err := s.db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, ErrCorrupt
	}
	if err := s.validateState(); err != nil {
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE spool_meta (id INTEGER PRIMARY KEY CHECK(id=1), destinations TEXT NOT NULL);
CREATE TABLE source_progress (source_key TEXT PRIMARY KEY, checkpoint BLOB NOT NULL);
CREATE TABLE batch_ids (batch_id TEXT PRIMARY KEY, source_key TEXT NOT NULL, envelope_digest BLOB NOT NULL);
CREATE TABLE deliveries (seq INTEGER PRIMARY KEY AUTOINCREMENT, batch_id TEXT NOT NULL UNIQUE REFERENCES batch_ids(batch_id), source_key TEXT NOT NULL, source_namespace TEXT NOT NULL, envelope BLOB NOT NULL, bytes INTEGER NOT NULL CHECK(bytes>0));
CREATE TABLE receipts (seq INTEGER NOT NULL REFERENCES deliveries(seq) ON DELETE CASCADE, destination TEXT NOT NULL, receipt BLOB NOT NULL, PRIMARY KEY(seq,destination));
CREATE TABLE destination_progress (source_key TEXT NOT NULL, destination TEXT NOT NULL, checkpoint BLOB NOT NULL, PRIMARY KEY(source_key,destination));
CREATE TABLE destination_binding (destination TEXT PRIMARY KEY, instance TEXT NOT NULL);
CREATE INDEX deliveries_source_seq ON deliveries(source_key,seq);
CREATE INDEX deliveries_namespace_seq ON deliveries(source_namespace,seq);
PRAGMA user_version=1;
`

func (s *Spool) validateState() error {
	rows, err := s.db.Query("SELECT d.batch_id,d.source_key,d.source_namespace,d.envelope,b.envelope_digest FROM deliveries d JOIN batch_ids b ON b.batch_id=d.batch_id ORDER BY d.seq")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	for rows.Next() {
		var id, key, namespace string
		var wire, retained []byte
		if err := rows.Scan(&id, &key, &namespace, &wire, &retained); err != nil {
			rows.Close()
			return ErrCorrupt
		}
		d, err := sessioncapture.DecodeDelivery(wire)
		digest := sha256.Sum256(wire)
		if err != nil || d.BatchID != id || d.Source.Key() != key || d.Source.Namespace != namespace || !bytes.Equal(digest[:], retained) {
			rows.Close()
			return ErrCorrupt
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ErrCorrupt
	}
	rows.Close()
	var missing int
	if err := s.db.QueryRow(`SELECT count(*) FROM deliveries d WHERE NOT EXISTS (SELECT 1 FROM batch_ids b WHERE b.batch_id=d.batch_id)`).Scan(&missing); err != nil || missing != 0 {
		return ErrCorrupt
	}
	if err := s.db.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&missing); err != nil || missing != 0 {
		return ErrCorrupt
	}
	progress, err := s.db.Query("SELECT source_key,checkpoint FROM source_progress UNION ALL SELECT source_key,checkpoint FROM destination_progress")
	if err != nil {
		return ErrCorrupt
	}
	for progress.Next() {
		var key string
		var wire []byte
		if progress.Scan(&key, &wire) != nil {
			progress.Close()
			return ErrCorrupt
		}
		var cp sessioncapture.Checkpoint
		if json.Unmarshal(wire, &cp) != nil || cp.Version != 1 || cp.SourceKey != key {
			progress.Close()
			return ErrCorrupt
		}
	}
	if progress.Err() != nil {
		progress.Close()
		return ErrCorrupt
	}
	progress.Close()
	receipts, err := s.db.Query("SELECT r.destination,r.receipt,d.envelope FROM receipts r JOIN deliveries d ON d.seq=r.seq")
	if err != nil {
		return ErrCorrupt
	}
	for receipts.Next() {
		var destination string
		var wire, envelope []byte
		if receipts.Scan(&destination, &wire, &envelope) != nil {
			receipts.Close()
			return ErrCorrupt
		}
		if !s.hasDestination(destination) {
			receipts.Close()
			return ErrCorrupt
		}
		d, err := sessioncapture.DecodeDelivery(envelope)
		if err != nil {
			receipts.Close()
			return ErrCorrupt
		}
		var ack sessioncapture.Acknowledgement
		if json.Unmarshal(wire, &ack) != nil || ack.Validate(destination, d) != nil {
			receipts.Close()
			return ErrCorrupt
		}
	}
	if receipts.Err() != nil {
		receipts.Close()
		return ErrCorrupt
	}
	receipts.Close()
	bindings, err := s.db.Query("SELECT destination,instance FROM destination_binding")
	if err != nil {
		return ErrCorrupt
	}
	for bindings.Next() {
		var destination, instance string
		if bindings.Scan(&destination, &instance) != nil || !s.hasDestination(destination) || instance == "" {
			bindings.Close()
			return ErrCorrupt
		}
	}
	if bindings.Err() != nil {
		bindings.Close()
		return ErrCorrupt
	}
	bindings.Close()
	return nil
}

// BindDestination pins a named sink to an immutable local instance identity.
// Reopening with a different archive instance is refused even if all queued
// deliveries have already been acknowledged and pruned. Other sink types may
// omit this binding and retain their own endpoint authentication policy.
func (s *Spool) BindDestination(destination, instance string) error {
	if !s.hasDestination(destination) {
		return ErrRegistry
	}
	if instance == "" || strings.TrimSpace(instance) != instance || len(instance) > 256 {
		return ErrBinding
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	var stored string
	err := s.db.QueryRow("SELECT instance FROM destination_binding WHERE destination=?", destination).Scan(&stored)
	if err == nil {
		if stored != instance {
			return ErrBinding
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec("INSERT INTO destination_binding(destination,instance) VALUES (?,?)", destination, instance)
	return err
}

// Enqueue commits the complete envelope and producer checkpoint in one durable
// transaction. A known identical batch ID is an idempotent no-op, even after
// the envelope itself was pruned following all receipts.
const MaxBatchDeliveries = 16
const MaxBatchBytes = 1 << 20

func (s *Spool) Enqueue(d sessioncapture.Delivery, destinations []string) error {
	return s.enqueueBatch(context.Background(), []sessioncapture.Delivery{d}, destinations, false)
}

// EnqueueBatch atomically commits bounded immutable envelopes and all producer
// positions under FULL synchronization. A failure advances none of the sources.
func (s *Spool) EnqueueBatch(ctx context.Context, deliveries []sessioncapture.Delivery, destinations []string) error {
	return s.enqueueBatch(ctx, deliveries, destinations, true)
}
func (s *Spool) enqueueBatch(ctx context.Context, deliveries []sessioncapture.Delivery, destinations []string, bounded bool) error {
	if len(deliveries) == 0 || len(deliveries) > MaxBatchDeliveries {
		return errors.New("invalid_capture_spool_batch_size")
	}
	_, wireRegistry, err := registry(destinations)
	if err != nil {
		return err
	}
	configured, _ := json.Marshal(s.destinations)
	if wireRegistry != string(configured) {
		return ErrRegistry
	}
	wires := make([][]byte, len(deliveries))
	total := 0
	for i, d := range deliveries {
		if err := d.Validate(); err != nil {
			return err
		}
		wire, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := secretredact.CheckWire(wire); err != nil {
			return err
		}
		wires[i] = wire
		total += len(wire)
		if bounded && total > MaxBatchBytes {
			return errors.New("capture_spool_batch_too_large")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, d := range deliveries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.enqueueTx(tx, d, wires[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Spool) enqueueTx(tx *sql.Tx, d sessioncapture.Delivery, wire []byte) error {
	var err error
	var retained []byte
	err = tx.QueryRow("SELECT envelope_digest FROM batch_ids WHERE batch_id=?", d.BatchID).Scan(&retained)
	if err == nil {
		digest := sha256.Sum256(wire)
		if !bytes.Equal(retained, digest[:]) {
			return ErrDuplicate
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var checkpointWire []byte
	err = tx.QueryRow("SELECT checkpoint FROM source_progress WHERE source_key=?", d.Source.Key()).Scan(&checkpointWire)
	if errors.Is(err, sql.ErrNoRows) {
		if d.Previous != (sessioncapture.Checkpoint{}) {
			return ErrCheckpoint
		}
	} else if err != nil {
		return err
	} else {
		var cp sessioncapture.Checkpoint
		if json.Unmarshal(checkpointWire, &cp) != nil || cp != d.Previous {
			return ErrCheckpoint
		}
	}
	var pending int
	var pendingBytes int64
	if err := tx.QueryRow("SELECT count(*),coalesce(sum(bytes),0) FROM deliveries").Scan(&pending, &pendingBytes); err != nil {
		return err
	}
	if pending >= s.limits.MaxPending || int64(len(wire)) > s.limits.MaxBytes-pendingBytes {
		return ErrCapacity
	}
	checkpointWire, err = json.Marshal(d.Batch.Checkpoint)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(wire)
	if _, err := tx.Exec("INSERT INTO batch_ids(batch_id,source_key,envelope_digest) VALUES (?,?,?)", d.BatchID, d.Source.Key(), digest[:]); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO deliveries(batch_id,source_key,source_namespace,envelope,bytes) VALUES (?,?,?,?,?)", d.BatchID, d.Source.Key(), d.Source.Namespace, wire, len(wire)); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO source_progress(source_key,checkpoint) VALUES (?,?) ON CONFLICT(source_key) DO UPDATE SET checkpoint=excluded.checkpoint", d.Source.Key(), checkpointWire); err != nil {
		return err
	}
	return nil
}

// Checkpoint is the durable producer capture position, independent of sink
// acknowledgements. The caller may resume reading from it after a restart.
func (s *Spool) Checkpoint(sourceKey string) (sessioncapture.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return sessioncapture.Checkpoint{}, false, ErrClosed
	}
	var wire []byte
	err := s.db.QueryRow("SELECT checkpoint FROM source_progress WHERE source_key=?", sourceKey).Scan(&wire)
	if errors.Is(err, sql.ErrNoRows) {
		return sessioncapture.Checkpoint{}, false, nil
	}
	if err != nil {
		return sessioncapture.Checkpoint{}, false, err
	}
	var cp sessioncapture.Checkpoint
	if err := json.Unmarshal(wire, &cp); err != nil || cp.Version != 1 || cp.SourceKey != sourceKey {
		return sessioncapture.Checkpoint{}, false, ErrCorrupt
	}
	return cp, true, nil
}

// SeedCheckpoint explicitly imports a previously committed source position.
// It only succeeds before this source has producer progress or queued batches.
// This is intended for attaching a new spool to an existing local archive.
func (s *Spool) SeedCheckpoint(source sessioncapture.Source, cp sessioncapture.Checkpoint) error {
	// A single existing sink can supply an attributable checkpoint. Seeding a
	// multi-sink registry from one sink would falsely credit every other sink.
	if len(s.destinations) != 1 {
		return ErrRegistry
	}
	if err := source.Validate(); err != nil {
		return err
	}
	if cp.Version != 1 || cp.SourceKey != source.Key() || cp.Epoch < 0 || cp.Offset < 0 || cp.Ordinal < 0 || cp.ParsedOffset < 0 || cp.ParsedOffset > cp.Offset || cp.ProviderVersion == "" || !sessionrecord.ValidDigest(cp.PrefixDigest) || (cp.Offset > 0 && cp.Generation == "") || (cp.Ordinal > 0 && cp.ConversationID == "") || (source.ConversationID != "" && cp.ConversationID != "" && source.ConversationID != cp.ConversationID) || (cp.Offset == 0 && (cp.Ordinal != 0 || cp.PrefixDigest != sessionrecord.Digest(nil))) || (!cp.HasGaps && cp.ParsedOffset != cp.Offset) {
		return ErrCheckpoint
	}
	wire, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM source_progress WHERE source_key=?", source.Key()).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrCheckpoint
	}
	if err := tx.QueryRow("SELECT count(*) FROM batch_ids WHERE source_key=?", source.Key()).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrCheckpoint
	}
	if _, err := tx.Exec("INSERT INTO source_progress(source_key,checkpoint) VALUES (?,?)", source.Key(), wire); err != nil {
		return err
	}
	for _, destination := range s.destinations {
		if _, err := tx.Exec("INSERT INTO destination_progress(source_key,destination,checkpoint) VALUES (?,?,?)", source.Key(), destination, wire); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DestinationCheckpoint is the most recently acknowledged position for this
// destination and source. It may lag behind the producer checkpoint.
func (s *Spool) DestinationCheckpoint(sourceKey, destination string) (sessioncapture.Checkpoint, bool, error) {
	if !s.hasDestination(destination) {
		return sessioncapture.Checkpoint{}, false, ErrRegistry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return sessioncapture.Checkpoint{}, false, ErrClosed
	}
	var wire []byte
	err := s.db.QueryRow("SELECT checkpoint FROM destination_progress WHERE source_key=? AND destination=?", sourceKey, destination).Scan(&wire)
	if errors.Is(err, sql.ErrNoRows) {
		return sessioncapture.Checkpoint{}, false, nil
	}
	if err != nil {
		return sessioncapture.Checkpoint{}, false, err
	}
	var cp sessioncapture.Checkpoint
	if json.Unmarshal(wire, &cp) != nil || cp.Version != 1 || cp.SourceKey != sourceKey {
		return sessioncapture.Checkpoint{}, false, ErrCorrupt
	}
	return cp, true, nil
}

// Acknowledged reports whether this exact batch has a durable receipt for the
// named destination. Completed batches remain known by ID after their envelope
// and per-delivery receipts are pruned; completion implies all configured
// destinations acknowledged under the fixed registry.
func (s *Spool) Acknowledged(batchID, destination string) (bool, error) {
	if !s.hasDestination(destination) {
		return false, ErrRegistry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, ErrClosed
	}
	var pending, acknowledged int
	err := s.db.QueryRow(`SELECT
	 EXISTS(SELECT 1 FROM deliveries WHERE batch_id=?),
	 EXISTS(SELECT 1 FROM receipts r JOIN deliveries d ON d.seq=r.seq WHERE d.batch_id=? AND r.destination=?)`, batchID, batchID, destination).Scan(&pending, &acknowledged)
	if err != nil {
		return false, err
	}
	if pending != 0 {
		return acknowledged != 0, nil
	}
	var known int
	if err := s.db.QueryRow("SELECT count(*) FROM batch_ids WHERE batch_id=?", batchID).Scan(&known); err != nil {
		return false, err
	}
	return known != 0, nil
}

// Deliver attempts at most maxBatches envelopes for the sink. A failure leaves
// its receipt pending and permits unrelated sources to proceed in this call.
// A source's next envelope remains blocked until its prior receipt commits.
// BatchSink optionally accepts an atomic bounded group. An error or malformed
// acknowledgement causes individual retries; sinks must retain exact receipts.
type BatchSink interface {
	sessioncapture.Sink
	AcceptBatch(context.Context, []sessioncapture.Delivery) ([]sessioncapture.Acknowledgement, error)
}

func (s *Spool) Deliver(ctx context.Context, sink sessioncapture.Sink, maxBatches int) (int, error) {
	if maxBatches <= 0 {
		return 0, nil
	}
	destination := sink.Destination()
	if !s.hasDestination(destination) {
		return 0, ErrRegistry
	}
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	failed := map[string]bool{}
	succeeded := 0
	attempts := 0
	var failures []error
	for attempts < maxBatches {
		if err := ctx.Err(); err != nil {
			return succeeded, errors.Join(append(failures, err)...)
		}
		group, err := s.nextGroup(ctx, destination, failed, min(MaxBatchDeliveries, maxBatches-attempts))
		if err != nil {
			return succeeded, errors.Join(append(failures, err)...)
		}
		if len(group) == 0 {
			break
		}
		attempts += len(group)
		accepted := []acceptedDelivery{}
		grouped := false
		if batchSink, ok := sink.(BatchSink); ok && len(group) > 1 {
			deliveries := make([]sessioncapture.Delivery, len(group))
			for i, item := range group {
				deliveries[i] = item.delivery
			}
			acks, batchErr := batchSink.AcceptBatch(ctx, deliveries)
			if batchErr == nil && len(acks) == len(group) {
				grouped = true
				for i, ack := range acks {
					if ack.Validate(destination, group[i].delivery) != nil {
						grouped = false
						break
					}
				}
				if grouped {
					for i, item := range group {
						accepted = append(accepted, acceptedDelivery{queuedDelivery: item, ack: acks[i]})
					}
				}
			}
		}
		if !grouped {
			for _, item := range group {
				ack, err := sessioncapture.Deliver(ctx, sink, item.delivery)
				if err != nil {
					failed[item.delivery.Source.Key()] = true
					failures = append(failures, fmt.Errorf("%s %s: %w", destination, item.delivery.BatchID, err))
					continue
				}
				accepted = append(accepted, acceptedDelivery{queuedDelivery: item, ack: ack})
			}
		}
		if len(accepted) > 0 {
			if err := s.commitReceipts(ctx, destination, accepted); err != nil {
				return succeeded, errors.Join(append(failures, err)...)
			}
			succeeded += len(accepted)
		}
	}
	return succeeded, errors.Join(failures...)
}

func (s *Spool) hasDestination(destination string) bool {
	i := sort.SearchStrings(s.destinations, destination)
	return destination != "" && i < len(s.destinations) && s.destinations[i] == destination
}

func (s *Spool) nextGroup(ctx context.Context, destination string, failed map[string]bool, limit int) ([]queuedDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.seq,d.source_key,d.envelope FROM deliveries d
WHERE NOT EXISTS(SELECT 1 FROM receipts r WHERE r.seq=d.seq AND r.destination=?)
AND NOT EXISTS(SELECT 1 FROM deliveries earlier WHERE earlier.source_key=d.source_key AND earlier.seq<d.seq AND NOT EXISTS(SELECT 1 FROM receipts prior WHERE prior.seq=earlier.seq AND prior.destination=?))
ORDER BY d.seq`, destination, destination)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	group := []queuedDelivery{}
	total := 0
	for rows.Next() {
		var seq int64
		var key string
		var wire []byte
		if err := rows.Scan(&seq, &key, &wire); err != nil {
			return nil, err
		}
		if failed[key] {
			continue
		}
		d, err := sessioncapture.DecodeDelivery(wire)
		if err != nil || d.Source.Key() != key {
			return nil, ErrCorrupt
		}
		if len(group) > 0 && total+len(wire) > MaxBatchBytes {
			break
		}
		group = append(group, queuedDelivery{seq: seq, delivery: d, bytes: len(wire)})
		total += len(wire)
		if len(group) >= limit || total >= MaxBatchBytes {
			break
		}
	}
	return group, rows.Err()
}

type queuedDelivery struct {
	seq      int64
	delivery sessioncapture.Delivery
	bytes    int
}
type acceptedDelivery struct {
	queuedDelivery
	ack sessioncapture.Acknowledgement
}

func (s *Spool) commitReceipt(seq int64, destination string, d sessioncapture.Delivery, ack sessioncapture.Acknowledgement) error {
	return s.commitReceipts(context.Background(), destination, []acceptedDelivery{{queuedDelivery: queuedDelivery{seq: seq, delivery: d}, ack: ack}})
}
func (s *Spool) commitReceipts(ctx context.Context, destination string, accepted []acceptedDelivery) error {
	for _, item := range accepted {
		if err := item.ack.Validate(destination, item.delivery); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range accepted {
		if err := s.commitReceiptTx(tx, item.seq, destination, item.delivery, item.ack); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Spool) commitReceiptTx(tx *sql.Tx, seq int64, destination string, d sessioncapture.Delivery, ack sessioncapture.Acknowledgement) error {
	wire, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO receipts(seq,destination,receipt) VALUES (?,?,?)", seq, destination, wire); err != nil {
		return err
	}
	cpWire, err := json.Marshal(d.Batch.Checkpoint)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO destination_progress(source_key,destination,checkpoint) VALUES (?,?,?) ON CONFLICT(source_key,destination) DO UPDATE SET checkpoint=excluded.checkpoint", d.Source.Key(), destination, cpWire); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM receipts WHERE seq=?", seq).Scan(&count); err != nil {
		return err
	}
	if count == len(s.destinations) {
		if _, err := tx.Exec("DELETE FROM deliveries WHERE seq=?", seq); err != nil {
			return err
		}
	}
	return nil
}

func (s *Spool) Status() (Status, error) {
	return s.status("", "")
}

// StatusForSource exposes only one source's backlog for a scoped status view.
func (s *Spool) StatusForSource(sourceKey string) (Status, error) {
	if sourceKey == "" {
		return Status{}, ErrCheckpoint
	}
	return s.status(sourceKey, "")
}

// StatusForNamespace includes queued sources in this namespace even if they
// are absent from the caller's current source registry. Callers must authorize
// the namespace before exposing the returned aggregate.
func (s *Spool) StatusForNamespace(namespace string) (Status, error) {
	if namespace == "" {
		return Status{}, ErrCheckpoint
	}
	return s.status("", namespace)
}

func (s *Spool) status(sourceKey, namespace string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return Status{}, ErrClosed
	}
	status := Status{ByDestination: make(map[string]int), MaxPending: s.limits.MaxPending, MaxBytes: s.limits.MaxBytes}
	where := ""
	args := []any{}
	if sourceKey != "" {
		where = " WHERE source_key=?"
		args = append(args, sourceKey)
	} else if namespace != "" {
		where = " WHERE source_namespace=?"
		args = append(args, namespace)
	}
	if err := s.db.QueryRow("SELECT count(*),coalesce(sum(bytes),0) FROM deliveries"+where, args...).Scan(&status.Pending, &status.PendingBytes); err != nil {
		return Status{}, err
	}
	for _, destination := range s.destinations {
		var n int
		query := "SELECT count(*) FROM deliveries d WHERE NOT EXISTS(SELECT 1 FROM receipts r WHERE r.seq=d.seq AND r.destination=?)"
		queryArgs := []any{destination}
		if sourceKey != "" {
			query += " AND d.source_key=?"
			queryArgs = append(queryArgs, sourceKey)
		} else if namespace != "" {
			query += " AND d.source_namespace=?"
			queryArgs = append(queryArgs, namespace)
		}
		if err := s.db.QueryRow(query, queryArgs...).Scan(&n); err != nil {
			return Status{}, err
		}
		status.ByDestination[destination] = n
	}
	return status, nil
}

func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
		s.db = nil
	}
	if s.lock != nil {
		errs = append(errs, syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
		s.lock = nil
	}
	return errors.Join(errs...)
}

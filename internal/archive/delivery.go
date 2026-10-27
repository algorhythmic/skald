package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

type captureSink struct {
	store        *Store
	destination  string
	registration Registration
}

// CaptureSink binds delivery to one explicitly registered source and one named
// destination. The archive's process lock and Ingest transaction retain writer
// ownership and atomic checkpoint application. Construction does not enroll a
// source or authorize a destination; callers supply those configured bindings.
func (s *Store) CaptureSink(destination string, r Registration) (sessioncapture.Sink, error) {
	if destination == "" {
		return nil, errors.New("destination_required")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &captureSink{store: s, destination: destination, registration: r}, nil
}

func (s *captureSink) Destination() string { return s.destination }

func (s *captureSink) Accept(ctx context.Context, d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
	acks, err := s.store.AcceptCaptureBatch(ctx, s.destination, []Registration{s.registration}, []sessioncapture.Delivery{d})
	if err != nil {
		return sessioncapture.Acknowledgement{}, err
	}
	return acks[0], nil
}

// AcceptCaptureBatch atomically accepts an ordered group of deliveries. The
// registrations and acknowledgements align with deliveries. Every new receipt,
// record and checkpoint commits together; an error returns no acknowledgements.
// Exact receipt replay remains subject to current enrollment and suppression.
// Multiple deliveries for one source must follow its checkpoint order. Groups
// are bounded to 16 deliveries and 1 MiB of JSON; scalar acceptance retains the
// normal delivery size limit.
func (s *Store) AcceptCaptureBatch(ctx context.Context, destination string, registrations []Registration, deliveries []sessioncapture.Delivery) (_ []sessioncapture.Acknowledgement, finalErr error) {
	defer func() { finalErr = capacityError(finalErr) }()
	if destination == "" {
		return nil, errors.New("destination_required")
	}
	if len(registrations) != len(deliveries) || len(deliveries) > 16 {
		return nil, errors.New("invalid_capture_batch")
	}
	batches := make([]sessioncapture.Batch, len(deliveries))
	needed := make([]uint64, len(deliveries))
	wireBytes := 0 // Match the spool's sum of individual JSON envelope sizes.
	for i, d := range deliveries {
		if err := d.Validate(); err != nil {
			return nil, err
		}
		if len(deliveries) > 1 {
			wire, err := json.Marshal(d)
			if err != nil {
				return nil, err
			}
			wireBytes += len(wire)
			if wireBytes > 1<<20 {
				return nil, errors.New("capture_batch_too_large")
			}
		}
		if sessioncapture.PublicSource(d.Source) != sessioncapture.PublicSource(registrations[i].Source) {
			return nil, errors.New("delivery_source_not_registered")
		}
		var err error
		batches[i], err = sessioncapture.SanitizeBatch(d.Batch)
		if err != nil {
			return nil, err
		}
		needed[i], err = validateIngest(registrations[i], sessioncapture.PublicCheckpoint(d.Previous), batches[i])
		if err != nil {
			return nil, err
		}
	}
	acks := make([]sessioncapture.Acknowledgement, len(deliveries))
	if len(deliveries) == 0 {
		return acks, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	committedHealth := make(map[string]string, len(deliveries))
	var totalNeeded uint64
	for i, d := range deliveries {
		r := registrations[i]
		if err := captureAllowed(ctx, tx, r, d.Batch); err != nil {
			return nil, err
		}
		var stored string
		err := tx.QueryRowContext(ctx, "SELECT acknowledgement_json FROM capture_receipts WHERE destination=? AND batch_id=?", destination, d.BatchID).Scan(&stored)
		if err == nil {
			if json.Unmarshal([]byte(stored), &acks[i]) != nil || acks[i].Validate(destination, d) != nil {
				return nil, errors.New("corrupt_capture_receipt")
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		totalNeeded += needed[i]
		if err := s.checkIngestSpace(ctx, tx, totalNeeded); err != nil {
			return nil, err
		}
		acks[i] = sessioncapture.Acknowledgement{Version: sessioncapture.DeliveryVersion, Destination: destination, BatchID: d.BatchID, SourceKey: d.Source.Key(), Checkpoint: batches[i].Checkpoint}
		health, err := s.ingestTx(ctx, tx, r, sessioncapture.PublicCheckpoint(d.Previous), batches[i], now, &acks[i])
		if err != nil {
			return nil, err
		}
		committedHealth[r.Key()] = health
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for key, health := range committedHealth {
		s.rememberHealth(key, health, now)
	}
	return acks, nil
}

// CheckCapture rejects suppressed evidence before a collector persists it in a
// spool. The sink repeats this check because policy may change before delivery.
func (s *Store) CheckCapture(ctx context.Context, r Registration, batch sessioncapture.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureAllowed(ctx, r, batch)
}

func (s *Store) captureAllowed(ctx context.Context, r Registration, batch sessioncapture.Batch) error {
	return captureAllowed(ctx, s.db, r, batch)
}

func captureAllowed(ctx context.Context, q rowQuerier, r Registration, batch sessioncapture.Batch) error {
	var active int
	if err := q.QueryRowContext(ctx, "SELECT active FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&active); err != nil || active != 1 {
		return errors.New("source_not_registered")
	}
	conv := ""
	if batch.Checkpoint.ConversationID != "" {
		conv = sessionrecord.Conversation(r.Namespace, batch.Checkpoint.ConversationID)
	}
	if err := checkSuppression(ctx, q, r.Namespace, conv, ""); err != nil {
		return err
	}
	for _, item := range batch.Records {
		if err := checkSuppression(ctx, q, r.Namespace, conv, item.Record.RecordKey); err != nil {
			return err
		}
	}
	return nil
}

func checkSuppression(ctx context.Context, q rowQuerier, namespace, conversation, record string) error {
	var suppressed int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM purge_tombstones WHERE namespace=? AND
 ((scope_kind='source' AND scope_key=?) OR (scope_kind='conversation' AND scope_key=?) OR (scope_kind='record' AND scope_key=?))`, namespace, namespace, conversation, record).Scan(&suppressed); err != nil {
		return err
	}
	if suppressed > 0 {
		return errors.New("capture_suppressed")
	}
	return nil
}

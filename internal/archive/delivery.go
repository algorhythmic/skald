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
	if err := d.Validate(); err != nil {
		return sessioncapture.Acknowledgement{}, err
	}
	if d.Source != s.registration.Source {
		return sessioncapture.Acknowledgement{}, errors.New("delivery_source_not_registered")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if err := s.store.captureAllowed(ctx, s.registration, d.Batch); err != nil {
		return sessioncapture.Acknowledgement{}, err
	}
	var stored string
	err := s.store.db.QueryRowContext(ctx, "SELECT acknowledgement_json FROM capture_receipts WHERE destination=? AND batch_id=?", s.destination, d.BatchID).Scan(&stored)
	if err == nil {
		var ack sessioncapture.Acknowledgement
		if json.Unmarshal([]byte(stored), &ack) != nil || ack.Validate(s.destination, d) != nil {
			return sessioncapture.Acknowledgement{}, errors.New("corrupt_capture_receipt")
		}
		return ack, nil
	}
	if err != sql.ErrNoRows {
		return sessioncapture.Acknowledgement{}, err
	}
	ack := sessioncapture.Acknowledgement{
		Version: sessioncapture.DeliveryVersion, Destination: s.destination,
		BatchID: d.BatchID, SourceKey: d.Source.Key(), Checkpoint: d.Batch.Checkpoint,
	}
	if err := s.store.ingest(ctx, s.registration, d.Previous, d.Batch, time.Now(), &ack); err != nil {
		return sessioncapture.Acknowledgement{}, capacityError(err)
	}
	return ack, nil
}

// CheckCapture rejects suppressed evidence before a collector persists it in a
// spool. The sink repeats this check because policy may change before delivery.
func (s *Store) CheckCapture(ctx context.Context, r Registration, batch sessioncapture.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureAllowed(ctx, r, batch)
}

func (s *Store) captureAllowed(ctx context.Context, r Registration, batch sessioncapture.Batch) error {
	var active int
	if err := s.db.QueryRowContext(ctx, "SELECT active FROM stream_locators WHERE stream_key=?", r.Key()).Scan(&active); err != nil || active != 1 {
		return errors.New("source_not_registered")
	}
	conv := ""
	if batch.Checkpoint.ConversationID != "" {
		conv = sessionrecord.Conversation(r.Namespace, batch.Checkpoint.ConversationID)
	}
	if err := checkSuppression(ctx, s.db, r.Namespace, conv, ""); err != nil {
		return err
	}
	for _, item := range batch.Records {
		if err := checkSuppression(ctx, s.db, r.Namespace, conv, item.Record.RecordKey); err != nil {
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

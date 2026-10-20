package archive

import (
	"context"
	"errors"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
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
	if err := s.store.Ingest(ctx, s.registration, d.Previous, d.Batch, time.Now()); err != nil {
		return sessioncapture.Acknowledgement{}, err
	}
	return sessioncapture.Acknowledgement{
		Version: sessioncapture.DeliveryVersion, Destination: s.destination,
		BatchID: d.BatchID, SourceKey: d.Source.Key(), Checkpoint: d.Batch.Checkpoint,
	}, nil
}

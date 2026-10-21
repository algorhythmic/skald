// Package archivefeed defines the version 1 metadata feed from a Skald archive.
// Events identify evidence; they never contain transcript or original bytes.
package archivefeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const Version = 1
const MaxPageEvents = 100

var ErrUnsupportedVersion = errors.New("unsupported_archive_feed_version")

type Locator struct {
	RecordKey      string `json:"record_key"`
	SourceRevision string `json:"source_revision"`
	AdapterVersion string `json:"adapter_version"`
}

// Event is a committed metadata observation. Sequence orders events within one
// archive instance. Record locators resolve through the scoped /v1/record read;
// an unavailable event gives the explicit reason when that evidence was removed.
type Event struct {
	Sequence       int64    `json:"sequence"`
	Kind           string   `json:"kind"`
	Namespace      string   `json:"namespace"`
	RecordKey      string   `json:"record_key,omitempty"`
	SourceRevision string   `json:"source_revision,omitempty"`
	Locator        *Locator `json:"locator,omitempty"`
	StreamKey      string   `json:"stream_key,omitempty"`
	Generation     string   `json:"stream_generation,omitempty"`
	Ordinal        *int64   `json:"ordinal,omitempty"`
	GapCode        string   `json:"gap_code,omitempty"`
	GapOffset      *int64   `json:"gap_offset,omitempty"`
	Health         string   `json:"health,omitempty"`
	ScopeKind      string   `json:"scope_kind,omitempty"`
	ScopeKey       string   `json:"scope_key,omitempty"`
	Unavailable    string   `json:"unavailable,omitempty"`
}

// Page first replays a finite snapshot of committed metadata through HighWater.
// Phase is snapshot until that prefix is exhausted, then tail for new events.
// NextCursor is always returned, including for an empty tail page.
type Page struct {
	Version    int     `json:"version"`
	Phase      string  `json:"phase"`
	HighWater  int64   `json:"high_water"`
	More       bool    `json:"more"`
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor"`
}

// DecodePage enforces the major wire version and rejects unknown fields. A
// consumer must not silently treat a newer major version as version 1.
func DecodePage(data []byte) (Page, error) {
	var page Page
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&page); err != nil {
		return Page{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Page{}, err
	}
	for _, required := range []string{"version", "phase", "high_water", "more", "events", "next_cursor"} {
		if _, ok := fields[required]; !ok {
			return Page{}, errors.New("invalid_archive_feed_page")
		}
	}
	if page.Version != Version {
		return Page{}, ErrUnsupportedVersion
	}
	if page.Phase != "snapshot" && page.Phase != "tail" {
		return Page{}, errors.New("invalid_archive_feed_phase")
	}
	if page.HighWater < 0 || page.Events == nil || len(page.Events) > MaxPageEvents || page.NextCursor == "" {
		return Page{}, errors.New("invalid_archive_feed_page")
	}
	var previous int64
	for _, event := range page.Events {
		if event.Sequence <= previous || event.Namespace == "" {
			return Page{}, errors.New("invalid_archive_feed_event")
		}
		previous = event.Sequence
		switch event.Kind {
		case "record", "correction":
			if event.RecordKey == "" || event.SourceRevision == "" || (event.Locator == nil && event.Unavailable == "") {
				return Page{}, errors.New("invalid_archive_feed_event")
			}
			if event.Locator != nil && (event.Locator.RecordKey != event.RecordKey || event.Locator.SourceRevision != event.SourceRevision || event.Locator.AdapterVersion == "") {
				return Page{}, errors.New("invalid_archive_feed_event")
			}
		case "gap", "source_health", "source_removed", "redaction":
		default:
			return Page{}, errors.New("invalid_archive_feed_event")
		}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Page{}, errors.New("invalid_archive_feed_trailing_data")
	}
	return page, nil
}

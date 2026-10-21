package archivefeed

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodePageRejectsUnsupportedAndMalformedWire(t *testing.T) {
	valid := []byte(`{"version":1,"phase":"tail","high_water":3,"more":false,"events":[],"next_cursor":"opaque"}`)
	if _, err := DecodePage(valid); err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{
		[]byte(`{"version":1,"phase":"tail","high_water":3,"more":false,"events":[],"next_cursor":"opaque","body":"private"}`),
		[]byte(`{"version":1,"phase":"tail","high_water":3,"more":false,"events":[],"next_cursor":"opaque"} {}`),
		[]byte(`{"version":1,"phase":"unknown","high_water":3,"more":false,"events":[],"next_cursor":"opaque"}`),
	} {
		if _, err := DecodePage(wire); err == nil {
			t.Fatalf("invalid wire accepted: %s", wire)
		}
	}
	if _, err := DecodePage([]byte(`{"version":2,"phase":"tail","high_water":3,"more":false,"events":[],"next_cursor":"opaque"}`)); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatal("new major version accepted", err)
	}
}

func TestDecodePageRejectsInvalidEvents(t *testing.T) {
	base := Page{Version: 1, Phase: "snapshot", HighWater: 2, Events: []Event{{Sequence: 1, Kind: "record", Namespace: "fixture:source", RecordKey: "record", SourceRevision: "revision", Locator: &Locator{RecordKey: "record", SourceRevision: "revision", AdapterVersion: "adapter"}}}, NextCursor: "opaque"}
	for _, mutate := range []func(*Page){
		func(p *Page) { p.Events[0].Kind = "unknown" },
		func(p *Page) { p.Events[0].RecordKey = "" },
		func(p *Page) { p.Events[0].SourceRevision = "" },
		func(p *Page) { p.Events[0].Locator = nil },
		func(p *Page) { p.Events[0].Locator.RecordKey = "different" },
		func(p *Page) { p.Events[0].Locator.SourceRevision = "different" },
		func(p *Page) { p.Events[0].Locator.AdapterVersion = "" },
		func(p *Page) { p.Events = append(p.Events, p.Events[0]) },
		func(p *Page) {
			p.Events = append(p.Events, Event{Sequence: 0, Kind: "gap", Namespace: "fixture:source"})
		},
	} {
		var p Page
		wire, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(wire, &p); err != nil {
			t.Fatal(err)
		}
		mutate(&p)
		wire, err = json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodePage(wire); err == nil {
			t.Fatalf("invalid event accepted: %s", wire)
		}
	}
}

package sessioncapture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/google/jsonschema-go/jsonschema"
)

func delivery(t *testing.T) sessioncapture.Delivery {
	t.Helper()
	s := source(sessioncapture.Claude)
	d, err := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, read(t, fixture(t, s.Provider), s, sessioncapture.Checkpoint{}))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeliveryIdentityAndWire(t *testing.T) {
	d := delivery(t)
	wire, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := sessioncapture.DecodeDelivery(wire)
	if err != nil || !reflect.DeepEqual(d, decoded) {
		t.Fatalf("round trip: %v", err)
	}
	var unordered map[string]json.RawMessage
	if err := json.Unmarshal(wire, &unordered); err != nil {
		t.Fatal(err)
	}
	reordered, _ := json.MarshalIndent(unordered, "", "  ")
	if got, err := sessioncapture.DecodeDelivery(reordered); err != nil || got.BatchID != d.BatchID {
		t.Fatalf("object order changed identity: %v", err)
	}
	d.Batch.Records[0].Record.ObservedAt = clock.Add(time.Second)
	if err := d.Validate(); err == nil {
		t.Fatal("changed envelope kept identity")
	}
	fresh, err := sessioncapture.NewDelivery(d.Source, d.Previous, d.Batch)
	if err != nil || fresh.BatchID == d.BatchID {
		t.Fatalf("fresh observation needs a distinct batch identity: %v", err)
	}
}

func TestDeliveryRefusesInvalidBoundaries(t *testing.T) {
	cases := map[string]func(*sessioncapture.Delivery){
		"version":           func(d *sessioncapture.Delivery) { d.Version++ },
		"source":            func(d *sessioncapture.Delivery) { d.Source.Namespace = "another-source" },
		"previous version":  func(d *sessioncapture.Delivery) { d.Previous.Version = 2 },
		"candidate version": func(d *sessioncapture.Delivery) { d.Batch.Checkpoint.Version = 2 },
		"candidate source":  func(d *sessioncapture.Delivery) { d.Batch.Checkpoint.SourceKey = "other" },
		"offset":            func(d *sessioncapture.Delivery) { d.Batch.Checkpoint.Offset++ },
		"parsed offset":     func(d *sessioncapture.Delivery) { d.Batch.Checkpoint.ParsedOffset-- },
		"ordinal":           func(d *sessioncapture.Delivery) { d.Batch.Checkpoint.Ordinal++ },
		"raw":               func(d *sessioncapture.Delivery) { d.Batch.Records[0].Raw[0] = '!' },
		"record version":    func(d *sessioncapture.Delivery) { d.Batch.Records[0].Record.ContractVersion++ },
		"record source":     func(d *sessioncapture.Delivery) { d.Batch.Records[0].Record.SourceNamespace = "other" },
		"record order":      func(d *sessioncapture.Delivery) { d.Batch.Records[0].Record.SourceOrder.Ordinal++ },
		"gap": func(d *sessioncapture.Delivery) {
			d.Batch.Gaps = []sessioncapture.Gap{{Code: "malformed_json", Offset: 1}}
		},
		"nil records": func(d *sessioncapture.Delivery) { d.Batch.Records = nil },
		"nil gaps":    func(d *sessioncapture.Delivery) { d.Batch.Gaps = nil },
		"activity":    func(d *sessioncapture.Delivery) { d.Batch.SourceActive = true },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			d := delivery(t)
			corrupt(&d)
			if err := d.Validate(); err == nil {
				t.Fatal("invalid delivery accepted")
			}
			if d.Version == sessioncapture.DeliveryVersion {
				if _, err := sessioncapture.NewDelivery(d.Source, d.Previous, d.Batch); err == nil {
					t.Fatal("invalid boundary assigned a new identity")
				}
			}
		})
	}
}

func TestDeliveryStrictWire(t *testing.T) {
	d := delivery(t)
	wire, _ := json.Marshal(d)
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"null":         []byte("null"),
		"duplicate":    []byte(strings.Replace(string(wire), `"version":1`, `"version":1,"version":1`, 1)),
		"case alias":   []byte(strings.Replace(string(wire), `"version":1`, `"version":1,"Version":1`, 1)),
		"null flag":    []byte(strings.Replace(string(wire), `"more":false`, `"more":null`, 1)),
		"missing flag": []byte(strings.Replace(string(wire), `"more":false,`, "", 1)),
		"unknown":      []byte(strings.Replace(string(wire), `"version":1`, `"unexpected":1,"version":1`, 1)),
		"unsupported":  []byte(strings.Replace(string(wire), `"version":1`, `"version":2`, 1)),
		"trailing":     append(append([]byte{}, wire...), []byte(" {}")...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := sessioncapture.DecodeDelivery(data); err == nil {
				t.Fatal("malformed envelope accepted")
			}
		})
	}
}

func TestDeliveryPreservesOpaqueNativeJSON(t *testing.T) {
	s := source(sessioncapture.Codex)
	s.ConversationID = "synthetic-raw-json"
	raw := []byte(`{"type":"response_item","payload":{"type":"function_call","name":"synthetic","name":"synthetic-again","arguments":"{}","huge":1e999}}` + "\n")
	batch := read(t, raw, s, sessioncapture.Checkpoint{})
	batch.Records[0].Record.Extensions = map[string]json.RawMessage{"native_arbitrary": json.RawMessage(`{"same":1,"same":2,"huge":1e999}`)}
	d, err := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, batch)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessioncapture.DecodeDelivery(wire)
	if err != nil || got.BatchID != d.BatchID {
		t.Fatal("opaque native JSON refused", err)
	}
	if !bytes.Contains(wire, []byte(`"name":"synthetic","name":"synthetic-again"`)) || !bytes.Contains(wire, []byte(`1e999`)) {
		t.Fatal("native duplicate keys or numeric lexemes changed")
	}
	got.Batch.Records[0].Record.Extensions["native_arbitrary"] = json.RawMessage(`{"same":2,"huge":1e999}`)
	if got.Validate() == nil {
		t.Fatal("dropping a duplicate native key preserved identity")
	}
}

func TestDeliveryContinuationRewriteAndDeferredSource(t *testing.T) {
	s := source(sessioncapture.Claude)
	raw := fixture(t, s.Provider)
	limits := sessioncapture.DefaultLimits()
	limits.Records = 2
	first, err := sessioncapture.Read(bytes.NewReader(raw), s, sessioncapture.Checkpoint{}, limits, clock)
	if err != nil {
		t.Fatal(err)
	}
	next := read(t, raw, s, first.Checkpoint)
	if _, err := sessioncapture.NewDelivery(s, first.Checkpoint, next); err != nil {
		t.Fatal("continuation", err)
	}
	deferred := sessioncapture.Batch{Records: []sessioncapture.Captured{}, Gaps: []sessioncapture.Gap{}, Checkpoint: next.Checkpoint, Pending: true, SourceActive: true, SourceActiveAt: clock}
	if _, err := sessioncapture.NewDelivery(s, next.Checkpoint, deferred); err != nil {
		t.Fatal("source activity", err)
	}
	rewritten := bytes.Replace(raw, []byte("Keep archive history"), []byte("Preserve archive history"), 1)
	rewrite := read(t, rewritten, s, next.Checkpoint)
	if _, err := sessioncapture.NewDelivery(s, next.Checkpoint, rewrite); err != nil {
		t.Fatal("rewrite", err)
	}
	rewrite.Gaps = []sessioncapture.Gap{}
	if _, err := sessioncapture.NewDelivery(s, next.Checkpoint, rewrite); err == nil {
		t.Fatal("rewrite accepted without continuity gap")
	}
	for _, raw := range [][]byte{nil, []byte(`{"type":"user","sessionId":"synthetic"}` + "\n{malformed}\n"), []byte("{partial")} {
		batch := read(t, raw, s, sessioncapture.Checkpoint{})
		if _, err := sessioncapture.NewDelivery(s, sessioncapture.Checkpoint{}, batch); err != nil {
			t.Fatal("empty, opaque or partial source", err)
		}
	}
}

type fixtureSink struct {
	destination string
	accept      func(sessioncapture.Delivery) (sessioncapture.Acknowledgement, error)
}

func (s fixtureSink) Destination() string { return s.destination }
func (s fixtureSink) Accept(_ context.Context, d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
	return s.accept(d)
}

func receipt(d sessioncapture.Delivery, destination string) sessioncapture.Acknowledgement {
	return sessioncapture.Acknowledgement{Version: 1, Destination: destination, BatchID: d.BatchID, SourceKey: d.Source.Key(), Checkpoint: d.Batch.Checkpoint}
}

func TestDeliveryAcknowledgementIsPerDestination(t *testing.T) {
	d := delivery(t)
	accepted := fixtureSink{"archive", func(d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
		return receipt(d, "archive"), nil
	}}
	ack, err := sessioncapture.Deliver(context.Background(), accepted, d)
	if err != nil || ack.Validate("archive", d) != nil {
		t.Fatal("committed receipt", err)
	}
	if ack.Validate("odin", d) == nil {
		t.Fatal("archive receipt accepted for another destination")
	}
	for _, wrong := range []sessioncapture.Acknowledgement{
		{}, receipt(d, "another"),
		{Version: 1, Destination: "archive", BatchID: d.BatchID, SourceKey: d.Source.Key()},
	} {
		accepted.accept = func(sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) { return wrong, nil }
		if ack, err := sessioncapture.Deliver(context.Background(), accepted, d); !errors.Is(err, sessioncapture.ErrAcknowledgement) || ack != (sessioncapture.Acknowledgement{}) {
			t.Fatal("invalid receipt advanced a destination", err)
		}
	}
	interrupted := errors.New("interrupted_before_acknowledgement")
	accepted.accept = func(d sessioncapture.Delivery) (sessioncapture.Acknowledgement, error) {
		return receipt(d, "archive"), interrupted
	}
	if ack, err := sessioncapture.Deliver(context.Background(), accepted, d); !errors.Is(err, interrupted) || ack != (sessioncapture.Acknowledgement{}) {
		t.Fatal("failed response exposed a receipt", err)
	}
}

func deliverySchema(t *testing.T, acknowledgement bool) *jsonschema.Resolved {
	t.Helper()
	data, err := os.ReadFile("../schemas/capture-delivery-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if acknowledgement {
		schema.Ref = "#/$defs/acknowledgement"
		schema.Required = nil
		schema.Properties = nil
		schema.AdditionalProperties = nil
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
		if !strings.HasSuffix(uri.Path, "/session-record-v1.schema.json") && uri.Path != "session-record-v1.schema.json" {
			return nil, errors.New("unexpected_schema_reference")
		}
		data, err := os.ReadFile("../schemas/session-record-v1.schema.json")
		if err != nil {
			return nil, err
		}
		var record jsonschema.Schema
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		return &record, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDeliverySchemaAndSharedFixture(t *testing.T) {
	unsupported, err := os.ReadFile("../testdata/capture-delivery/unsupported-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncapture.DecodeDelivery(unsupported); !errors.Is(err, sessioncapture.ErrDeliveryVersion) {
		t.Fatal("unsupported fixture did not fail closed", err)
	}
	data, err := os.ReadFile("../testdata/capture-delivery/accepted-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sessioncapture.DecodeDelivery(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []sessioncapture.Delivery{delivery(t), fixture} {
		for _, isAck := range []bool{false, true} {
			var value any = d
			if isAck {
				value = receipt(d, "synthetic:archive")
			}
			wire, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var instance map[string]any
			if err := json.Unmarshal(wire, &instance); err != nil {
				t.Fatal(err)
			}
			schema := deliverySchema(t, isAck)
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("schema acknowledgement=%v: %v", isAck, err)
			}
			instance["version"] = float64(2)
			if err := schema.Validate(instance); err == nil {
				t.Fatal("schema allowed unsupported version")
			}
		}
	}
}

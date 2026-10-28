package archive

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
)

func oneHealth(t *testing.T, s *Store, namespace string) SourceHealth {
	t.Helper()
	health, err := s.Health(context.Background(), []string{namespace})
	if err != nil || len(health) != 1 {
		t.Fatalf("source health: %v %v", health, err)
	}
	return health[0]
}

func TestHealthSeparatesEnrollmentCheckpointChecksAndRecordAge(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	initial := oneHealth(t, s, r.Namespace)
	if !initial.Enrolled || initial.Health != "unknown" || initial.CheckedAt != nil || initial.CheckpointAvailable || initial.Generation != nil || initial.Epoch != nil || initial.NextOrdinal != nil || initial.LastCapturedAt != nil || initial.LastSourceTime != nil {
		t.Fatalf("enrollment invented capture evidence: %+v", initial)
	}
	// Source clock order is not stream order: the latest ordinal wins even
	// when its timestamp is earlier than the previous record's timestamp.
	raw := []byte(`{"type":"assistant","sessionId":"health","uuid":"a","timestamp":"2026-09-09T11:00:00Z","message":{"role":"assistant","content":"first"}}
{"type":"assistant","sessionId":"health","uuid":"b","timestamp":"2026-09-09T10:00:00Z","message":{"role":"assistant","content":"second"}}
`)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	h := oneHealth(t, s, r.Namespace)
	if !h.CheckpointAvailable || h.Generation == nil || *h.Generation != batch.Checkpoint.Generation || h.Epoch == nil || *h.Epoch != batch.Checkpoint.Epoch || h.NextOrdinal == nil || *h.NextOrdinal != 2 || h.CapturedOffset != int64(len(raw)) || h.ParsedOffset != int64(len(raw)) {
		t.Fatalf("checkpoint position missing: %+v", h)
	}
	if h.LastCapturedAt == nil || *h.LastCapturedAt != stamp(testNow) || h.LastSourceTime == nil || *h.LastSourceTime != "2026-09-09T10:00:00Z" {
		t.Fatalf("record freshness not attributed to latest ordinal: %+v", h)
	}
	replay := testBatch(t, raw, r, batch.Checkpoint)
	later := testNow.Add(time.Hour)
	if err := s.Ingest(ctx, r, batch.Checkpoint, replay, later); err != nil {
		t.Fatal(err)
	}
	idle := oneHealth(t, s, r.Namespace)
	if idle.CheckedAt == nil || *idle.CheckedAt != stamp(later) || !reflect.DeepEqual(idle.LastCapturedAt, h.LastCapturedAt) || !reflect.DeepEqual(idle.LastSourceTime, h.LastSourceTime) {
		t.Fatal("idle check refreshed record evidence", idle)
	}
	// No timestamp on a newer record means unknown native age. A timestamp
	// from an older record must not be substituted to make coverage look known.
	raw = append(raw, []byte("{\"type\":\"assistant\",\"sessionId\":\"health\",\"uuid\":\"c\",\"message\":{\"role\":\"assistant\",\"content\":\"untimed\"}}\n")...)
	tail := testBatch(t, raw, r, replay.Checkpoint)
	if err := s.Ingest(ctx, r, replay.Checkpoint, tail, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	untimed := oneHealth(t, s, r.Namespace)
	if untimed.LastSourceTime != nil || untimed.LastCapturedAt == nil || *untimed.LastCapturedAt != stamp(later.Add(time.Hour)) {
		t.Fatal("missing native timestamp was not unknown", untimed)
	}
	// A rewritten empty stream has a checkpoint but no current record age.
	empty := testBatch(t, nil, r, tail.Checkpoint)
	mustIngest(t, s, r, tail.Checkpoint, empty)
	reset := oneHealth(t, s, r.Namespace)
	if !reset.CheckpointAvailable || !reset.HasGaps || reset.LastCapturedAt != nil || reset.LastSourceTime != nil || reset.NextOrdinal == nil || *reset.NextOrdinal != 0 {
		t.Fatal("empty rewrite borrowed old record evidence", reset)
	}
	if reset.Epoch == nil || *reset.Epoch != empty.Checkpoint.Epoch || reset.Generation == nil || *reset.Generation != "" {
		t.Fatal("empty rewrite lost its known checkpoint epoch", reset)
	}
}

func TestHealthSurvivesRestartAndRestore(t *testing.T) {
	ctx := context.Background()
	s, r, raw := testStore(t)
	batch := testBatch(t, raw, r, sessioncapture.Checkpoint{})
	mustIngest(t, s, r, sessioncapture.Checkpoint{}, batch)
	if err := s.SetHealth(ctx, r, "unavailable", testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := oneHealth(t, s, r.Namespace)
	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir, opts := s.dir, s.options
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if after := oneHealth(t, reopened, r.Namespace); !reflect.DeepEqual(before, after) {
		t.Fatal("restart changed persisted source evidence", before, after)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := Restore(ctx, backup, destination, opts); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(destination, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if after := oneHealth(t, restored, r.Namespace); !reflect.DeepEqual(before, after) {
		t.Fatal("restore changed persisted source evidence", before, after)
	}
}

func TestStatusReportsAllSelectedSourcesWithinScope(t *testing.T) {
	ctx := context.Background()
	s, r, _ := testStore(t)
	registrations := make([]Registration, 257)
	for i := range registrations {
		registrations[i] = r
		registrations[i].StreamID = fmt.Sprintf("stream-%03d", i)
	}
	foreign := r
	foreign.Namespace, foreign.StreamID = "fixture:foreign", "foreign"
	registrations = append(registrations, foreign)
	if err := s.Enroll(ctx, registrations); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ns             []string
		total, omitted int64
		shown          int
	}{
		{[]string{r.Namespace}, 258, 0, 258},
		{[]string{foreign.Namespace}, 1, 0, 1},
		{nil, 0, 0, 0},
	} {
		status, err := s.Status(ctx, tc.ns)
		if err != nil {
			t.Fatal(err)
		}
		if status["sources_total"] != tc.total || status["sources_omitted"] != tc.omitted {
			t.Fatal("incorrect scoped source counts", status)
		}
		sources := status["sources"].([]SourceHealth)
		if len(sources) != tc.shown {
			t.Fatalf("expected %d bounded sources, got %d", tc.shown, len(sources))
		}
		for _, source := range sources {
			if source.Namespace != tc.ns[0] {
				t.Fatal("source health escaped scope", source)
			}
		}
	}
}

package sessioncapture_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func prefixLines(n, padding int) []byte {
	var out bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&out, "{\"type\":\"user\",\"uuid\":\"fixture-%d\",\"sessionId\":\"prefix-fixture\",\"message\":{\"role\":\"user\",\"content\":\"%s\"}}\n", i, strings.Repeat("a", padding))
	}
	return out.Bytes()
}

func TestPrefixStateExactRewriteDetection(t *testing.T) {
	src := source(sessioncapture.Claude)
	original := prefixLines(100, 2048)
	limits := sessioncapture.DefaultLimits()
	first, state, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(original), src, sessioncapture.Checkpoint{}, sessioncapture.PrefixState{}, limits, clock)
	if err != nil || len(first.Records) != 100 {
		t.Fatalf("initial: %v records=%d", err, len(first.Records))
	}
	// Exercise persisted state, rather than relying on retained hash objects.
	wire, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var persisted sessioncapture.PrefixState
	if err = json.Unmarshal(wire, &persisted); err != nil {
		t.Fatal(err)
	}
	appended := append(append([]byte{}, original...), prefixLines(1, 64)...)
	for _, tc := range []struct {
		name    string
		data    []byte
		state   sessioncapture.PrefixState
		gap     bool
		records int
	}{
		{"append", appended, persisted, false, 1},
		{"unchanged", original, persisted, false, 0},
		{"truncate", prefixLines(2, 2048), persisted, true, 2},
		{"invalid-state-fallback", appended, sessioncapture.PrefixState{Version: 1, SourceKey: persisted.SourceKey, Offset: persisted.Offset, PrefixDigest: persisted.PrefixDigest, HashState: []byte("broken")}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(tc.data), src, first.Checkpoint, tc.state, limits, clock)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Records) != tc.records || got.Checkpoint.HasGaps != tc.gap {
				t.Fatalf("records=%d gap=%v", len(got.Records), got.Checkpoint.HasGaps)
			}
		})
	}
	// Rewrite more than64KiB before the tail, and append. Bounded end anchors
	// would miss this; the final full verification must still detect it.
	rewritten := append([]byte{}, appended...)
	at := bytes.Index(rewritten, []byte(strings.Repeat("a", 80)))
	if at < 0 {
		t.Fatal("fixture")
	}
	rewritten[at] = 'b'
	got, _, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(rewritten), src, first.Checkpoint, persisted, limits, clock)
	if err != nil || !got.Checkpoint.HasGaps || got.Checkpoint.Epoch != first.Checkpoint.Epoch+1 || len(got.Records) != 101 {
		t.Fatalf("historical rewrite missed: %v gap=%v epoch=%d records=%d", err, got.Checkpoint.HasGaps, got.Checkpoint.Epoch, len(got.Records))
	}
}

type countedPrefixReader struct {
	*bytes.Reader
	bytes int64
}

func (r *countedPrefixReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.bytes += int64(n)
	return n, e
}

func TestPrefixStateRemovesOnePrefixPass(t *testing.T) {
	raw := prefixLines(100, 4096)
	src := source(sessioncapture.Claude)
	first, state, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(raw), src, sessioncapture.Checkpoint{}, sessioncapture.PrefixState{}, sessioncapture.DefaultLimits(), clock)
	if err != nil {
		t.Fatal(err)
	}
	data := append(append([]byte{}, raw...), prefixLines(1, 64)...)
	before := &countedPrefixReader{Reader: bytes.NewReader(data)}
	if _, err = sessioncapture.Read(before, src, first.Checkpoint, sessioncapture.DefaultLimits(), clock); err != nil {
		t.Fatal(err)
	}
	after := &countedPrefixReader{Reader: bytes.NewReader(data)}
	if _, _, err = sessioncapture.ReadWithPrefixState(after, src, first.Checkpoint, state, sessioncapture.DefaultLimits(), clock); err != nil {
		t.Fatal(err)
	}
	if before.bytes-after.bytes != first.Checkpoint.Offset {
		t.Fatalf("expected one prefix saved: before=%d after=%d offset=%d", before.bytes, after.bytes, first.Checkpoint.Offset)
	}
}

// Both paths use the identical generated16MiB transcript and one appended line.
// bytes.Reader deliberately excludes filesystem/page-cache differences: this
// measures the CPU/read amplification removed, not live-daemon CPU utilization.
func BenchmarkPrefixContinuation(b *testing.B) {
	raw := prefixLines(2048, 8192)
	src := source(sessioncapture.Claude)
	limits := sessioncapture.Limits{Records: 10000, RecordBytes: 1 << 20, BatchBytes: 128 << 20}
	first, state, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(raw), src, sessioncapture.Checkpoint{}, sessioncapture.PrefixState{}, limits, clock)
	if err != nil {
		b.Fatal(err)
	}
	data := append(append([]byte{}, raw...), prefixLines(1, 64)...)
	b.Run("BeforeFullVerificationTwice", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			r := &countedPrefixReader{Reader: bytes.NewReader(data)}
			got, err := sessioncapture.Read(r, src, first.Checkpoint, limits, clock)
			if err != nil || len(got.Records) != 1 {
				b.Fatal(err)
			}
			b.ReportMetric(float64(r.bytes), "read-B/op")
		}
	})
	b.Run("AfterPersistedPrefixState", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			r := &countedPrefixReader{Reader: bytes.NewReader(data)}
			got, _, err := sessioncapture.ReadWithPrefixState(r, src, first.Checkpoint, state, limits, clock)
			if err != nil || len(got.Records) != 1 {
				b.Fatal(err)
			}
			b.ReportMetric(float64(r.bytes), "read-B/op")
		}
	})
}

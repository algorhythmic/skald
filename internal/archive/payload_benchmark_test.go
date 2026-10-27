package archive

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

// Retain outputs so both policies include their actual serialization work.
var syntheticPayloadResult struct {
	raw, envelope []byte
	digest        string
}

// BenchmarkSyntheticIngestPayloadStorage uses adapter-produced records, but
// measures payload encoding only: SQLite, indexes, transactions and filesystem
// work are excluded. Every fixture is generated here, without archive input.
func BenchmarkSyntheticIngestPayloadStorage(b *testing.B) {
	randomBytes := make([]byte, 24<<10)
	_, _ = rand.New(rand.NewSource(20261027)).Read(randomBytes)
	fixtures := []struct {
		name    string
		content any
	}{
		{"repeated_prose", strings.Repeat("Synthetic archive prose: keep exact evidence and source order. ", 512)},
		{"repeated_tool", []any{map[string]any{
			"type": "tool_result", "tool_use_id": "synthetic-call",
			"content": strings.Repeat("Synthetic tool row: status=ready; count=42; result=unchanged\n", 512),
		}}},
		{"seeded_random_base64", base64.StdEncoding.EncodeToString(randomBytes)},
	}
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			raw, err := json.Marshal(map[string]any{
				"type": "user", "uuid": "synthetic-record", "sessionId": "synthetic-benchmark",
				"message": map[string]any{"role": "user", "content": fixture.content},
			})
			if err != nil {
				b.Fatal(err)
			}
			raw = append(raw, '\n')
			batch, err := sessioncapture.Read(bytes.NewReader(raw), sessioncapture.Source{
				Namespace: "synthetic:payload-benchmark", Provider: sessioncapture.Claude,
				StreamID: "synthetic-stream", ProviderVersion: "2.1.263",
			}, sessioncapture.Checkpoint{}, sessioncapture.DefaultLimits(), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
			if err != nil || len(batch.Records) != 1 {
				b.Fatalf("synthetic fixture normalization: records=%d error=%v", len(batch.Records), err)
			}
			record := batch.Records[0].Record
			plainBytes := len(raw) + len(encode(record))
			for _, compress := range []bool{false, true} {
				name := "plain_before"
				if compress {
					name = "stored_after"
				}
				b.Run(name, func(b *testing.B) {
					b.SetBytes(int64(plainBytes))
					b.ReportAllocs()
					storedBytes := 0
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						envelope := []byte(encode(record))
						content, normalized := raw, envelope
						digest := ""
						envelopePlaceholder := 0
						if compress {
							content, _, err = storedPayload(raw)
							if err != nil {
								b.Fatal(err)
							}
							var decodedLength int64
							normalized, decodedLength, err = storedPayload(envelope)
							if err != nil {
								b.Fatal(err)
							}
							if decodedLength >= 0 {
								digest = sessionrecord.Digest(envelope)
								envelopePlaceholder = len("{}")
							}
						}
						syntheticPayloadResult.raw, syntheticPayloadResult.envelope = content, normalized
						syntheticPayloadResult.digest = digest
						storedBytes = len(content) + len(normalized) + envelopePlaceholder
					}
					b.StopTimer()
					// Count payload columns, including the compressed-envelope '{}'
					// placeholder; digest and decoded-length metadata are excluded.
					b.ReportMetric(float64(plainBytes), "plain-B/record")
					b.ReportMetric(float64(storedBytes), "payload-B/record")
					b.ReportMetric(float64(storedBytes)/float64(plainBytes), "stored/plain")
				})
			}
		})
	}
}

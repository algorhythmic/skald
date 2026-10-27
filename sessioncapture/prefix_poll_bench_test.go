package sessioncapture_test

import (
	"bytes"
	"syscall"
	"testing"

	"github.com/algorhythmic/skald/sessioncapture"
)

func processCPUNanoseconds(b *testing.B) int64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return (usage.Utime.Sec+usage.Stime.Sec)*1e9 + (usage.Utime.Usec+usage.Stime.Usec)*1e3
}

// BenchmarkPrefixPoll measures an actual audit/changed-file capture call, not
// every scheduler tick. The collector's unchanged-file stamp-cache fast path
// is unchanged. Read is the full-verification behavior used at87268a7.
// Inputs are generated and identical for the before/after paths. Process CPU
// includes GC and must be measured without concurrent build/test processes.
func BenchmarkPrefixPoll(b *testing.B) {
	raw := prefixLines(2048, 8192)
	src := source(sessioncapture.Claude)
	limits := sessioncapture.Limits{Records: 10000, RecordBytes: 1 << 20, BatchBytes: 128 << 20}
	first, state, err := sessioncapture.ReadWithPrefixState(bytes.NewReader(raw), src, sessioncapture.Checkpoint{}, sessioncapture.PrefixState{}, limits, clock)
	if err != nil {
		b.Fatal(err)
	}
	appended := append(append([]byte{}, raw...), prefixLines(1, 64)...)
	for _, fixture := range []struct {
		name    string
		data    []byte
		records int
	}{
		{"Append", appended, 1}, {"UnchangedAudit", raw, 0},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			for _, incremental := range []bool{false, true} {
				name := "Before87268a7"
				if incremental {
					name = "AfterPrefixState"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					startCPU := processCPUNanoseconds(b)
					b.ResetTimer()
					var readBytes int64
					for i := 0; i < b.N; i++ {
						input := &countedPrefixReader{Reader: bytes.NewReader(fixture.data)}
						var got sessioncapture.Batch
						var err error
						if incremental {
							got, _, err = sessioncapture.ReadWithPrefixState(input, src, first.Checkpoint, state, limits, clock)
						} else {
							got, err = sessioncapture.Read(input, src, first.Checkpoint, limits, clock)
						}
						if err != nil || len(got.Records) != fixture.records {
							b.Fatalf("unexpected poll result: err=%v records=%d", err, len(got.Records))
						}
						readBytes += input.bytes
					}
					b.StopTimer()
					elapsedCPU := processCPUNanoseconds(b) - startCPU
					b.ReportMetric(float64(elapsedCPU)/float64(b.N), "cpu-ns/poll")
					b.ReportMetric(float64(readBytes)/float64(b.N), "read-B/poll")
				})
			}
		})
	}
}

package observability

import (
	"context"
	"testing"
	"time"
)

func TestProfilesValidateBoundAndCancel(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		seconds int
	}{{"cpu", 0}, {"cpu", 31}, {"heap", 1}, {"goroutine", 0}} {
		if _, err := Profile(context.Background(), tc.kind, tc.seconds); err == nil {
			t.Fatal("invalid profile accepted", tc)
		}
	}
	heap, err := Profile(context.Background(), "heap", 0)
	if err != nil || len(heap) < 2 || heap[0] != 0x1f || heap[1] != 0x8b {
		t.Fatal("invalid heap pprof", len(heap), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Profile(ctx, "cpu", 30); err != context.DeadlineExceeded {
		t.Fatal("canceled profile did not stop", err)
	}
	// Cancellation must release the process-wide profiling resource.
	if !cpuProfileLock.TryLock() {
		t.Fatal("CPU profiler remained locked")
	}
	cpuProfileLock.Unlock()
	var buffer profileBuffer
	if _, err := buffer.Write(make([]byte, MaxProfileBytes+1)); err == nil || !buffer.oversized || buffer.Len() != 0 {
		t.Fatal("profile buffer exceeded bound")
	}
}

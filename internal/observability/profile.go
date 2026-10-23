package observability

import (
	"bytes"
	"context"
	"errors"
	"runtime/pprof"
	"sync"
	"time"
)

const MaxProfileBytes = 32 << 20

var cpuProfileLock sync.Mutex

type profileBuffer struct {
	bytes.Buffer
	oversized bool
}

func (b *profileBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxProfileBytes-b.Len() {
		b.oversized = true
		return 0, errors.New("profile_too_large")
	}
	return b.Buffer.Write(p)
}

// Profile produces only Go profiling data, with bounded duration and output.
// Cancellation always stops CPU profiling, including daemon shutdown.
func Profile(ctx context.Context, kind string, seconds int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out profileBuffer
	switch kind {
	case "cpu":
		if seconds < 1 || seconds > 30 {
			return nil, errors.New("invalid_profile_duration")
		}
		if !cpuProfileLock.TryLock() {
			return nil, errors.New("profile_in_progress")
		}
		defer cpuProfileLock.Unlock()
		if err := pprof.StartCPUProfile(&out); err != nil {
			return nil, errors.New("profile_in_progress")
		}
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		pprof.StopCPUProfile()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	case "heap":
		if seconds != 0 {
			return nil, errors.New("invalid_profile_duration")
		}
		if err := pprof.WriteHeapProfile(&out); err != nil {
			return nil, errors.New("profile_write_failed")
		}
	default:
		return nil, errors.New("invalid_profile_kind")
	}
	if out.oversized {
		return nil, errors.New("profile_too_large")
	}
	return out.Bytes(), nil
}

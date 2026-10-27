package observability

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsAreAggregateConcurrentAndBounded(t *testing.T) {
	r := New()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 1000 {
				r.Add("source_checks", 1)
				r.Duration("scan", time.Millisecond)
			}
		}()
	}
	group.Wait()
	r.Add("private/session/path", 1)
	r.Add("source_checks", -1)
	r.Duration("scan", -1)
	s := r.Snapshot()
	if s.Counters["source_checks"] != 8000 || len(s.Counters) != len(counterNames) || s.Durations["scan"].Count != 8000 || s.Durations["scan"].TotalSeconds != 8 {
		t.Fatal(s)
	}
	if s.Process.HeapAllocBytes == 0 || s.Process.Goroutines == 0 || s.Process.CPUSeconds < 0 {
		t.Fatal(s.Process)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "private/session/path") {
		t.Fatal("unbounded label retained")
	}
	var disabled *Recorder
	disabled.Add("source_checks", 1)
	disabled.Duration("scan", time.Second)
}

func TestOTLPIsExplicitAndConfigurationValidated(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	r := New()
	if err := r.StartOTLP(context.Background(), Config{}); err != nil || r.provider != nil || r.Snapshot().OTLPEnabled {
		t.Fatal("implicit exporter", err)
	}
	for _, cfg := range []Config{{OTLPMetricsEndpoint: "file:///tmp/private"}, {OTLPMetricsEndpoint: "http://user:password@example.com"}, {OTLPMetricsEndpoint: "https://example.com/path?token=secret"}, {ExportIntervalSeconds: 1}, {ServiceInstanceID: "host.example.com"}, {ServiceInstanceID: "/home/private"}, {OTLPMetricsEndpoint: "http://127.0.0.1:4318/v1/metrics"}} {
		if cfg.Validate() == nil {
			t.Fatal("invalid config accepted", cfg)
		}
	}
	if err := (Config{OTLPMetricsEndpoint: "http://127.0.0.1:4318/v1/metrics", ExportIntervalSeconds: 30, ServiceInstanceID: "fixture-device"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOTELSDKCollectsCountersDurationsAndResourceUsage(t *testing.T) {
	r := New()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(context.Background())
	if err := r.register(provider); err != nil {
		t.Fatal(err)
	}
	r.Add("source_checks", 615)
	r.Duration("scan", 20*time.Millisecond)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	foundCounter, foundHistogram, foundMemory := false, false, false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch m.Name {
			case "skald.source_checks":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 615 || sum.DataPoints[0].Attributes.Len() != 0 {
					t.Fatal("incorrect/unbounded scope", m)
				}
				foundCounter = true
			case "skald.scan.duration":
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok || len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 {
					t.Fatal(m)
				}
				foundHistogram = true
			case "process.memory.usage":
				foundMemory = true
			}
		}
	}
	if !foundCounter || !foundHistogram || !foundMemory {
		t.Fatal("missing instruments", data)
	}
}

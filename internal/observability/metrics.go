// Package observability records aggregate operational measurements. It never
// receives transcript bodies, native IDs, source paths or namespace labels.
package observability

import (
	"bufio"
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

type Config struct {
	ServiceInstanceID     string `json:"service_instance_id,omitempty"`
	OTLPMetricsEndpoint   string `json:"otlp_metrics_endpoint,omitempty"`
	ExportIntervalSeconds int    `json:"export_interval_seconds,omitempty"`
	EnableProfiling       bool   `json:"enable_profiling,omitempty"`
}

var deviceAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (c Config) Validate() error {
	if c.ServiceInstanceID != "" && !deviceAliasPattern.MatchString(c.ServiceInstanceID) {
		return errors.New("invalid_telemetry_device_alias")
	}
	if c.ExportIntervalSeconds != 0 && (c.ExportIntervalSeconds < 10 || c.ExportIntervalSeconds > 3600) {
		return errors.New("invalid_telemetry_interval")
	}
	if c.OTLPMetricsEndpoint == "" {
		return nil
	}
	if c.ServiceInstanceID == "" {
		return errors.New("telemetry_device_alias_required")
	}
	u, err := url.Parse(c.OTLPMetricsEndpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid_telemetry_endpoint")
	}
	return nil
}

var counterNames = []string{"scans", "source_checks", "file_cache_hits", "capture_attempts", "capture_input_bytes", "captured_records", "capture_errors", "checkpoint_snapshot_loads", "checkpoint_snapshot_errors", "health_flush_calls", "health_heartbeat_transactions", "health_heartbeat_sources", "health_flush_errors", "empty_polls_skipped"}
var durationNames = []string{"scan", "capture", "checkpoint_snapshot"}
var gaugeNames = []string{"data_allocated_bytes", "database_allocated_bytes", "wal_allocated_bytes", "spool_allocated_bytes", "recovery_allocated_bytes", "filesystem_free_bytes", "max_database_bytes", "capture_pending", "capture_pending_bytes", "sources_available", "sources_unavailable", "sources_blocked", "sources_capture_gaps", "operational_sample_errors"}

type duration struct{ count, sum, last, max atomic.Int64 }
type Recorder struct {
	started    time.Time
	counters   map[string]*atomic.Int64
	gauges     map[string]*atomic.Int64
	durations  map[string]*duration
	histograms map[string]otelmetric.Float64Histogram
	provider   *sdkmetric.MeterProvider
}

func New() *Recorder {
	r := &Recorder{started: time.Now().UTC(), counters: make(map[string]*atomic.Int64), gauges: make(map[string]*atomic.Int64), durations: make(map[string]*duration), histograms: make(map[string]otelmetric.Float64Histogram)}
	for _, n := range counterNames {
		r.counters[n] = new(atomic.Int64)
	}
	for _, n := range durationNames {
		r.durations[n] = new(duration)
	}
	for _, n := range gaugeNames {
		r.gauges[n] = new(atomic.Int64)
	}
	return r
}

// Gauge accepts a fixed aggregate vocabulary; paths and source IDs are never labels.
func (r *Recorder) Gauge(name string, value int64) {
	if r != nil && value >= 0 {
		if g := r.gauges[name]; g != nil {
			g.Store(value)
		}
	}
}

func (r *Recorder) Add(name string, value int64) {
	if r == nil || value < 0 {
		return
	}
	if counter := r.counters[name]; counter != nil {
		counter.Add(value)
	}
}

func (r *Recorder) Duration(name string, elapsed time.Duration) {
	if r == nil || elapsed < 0 {
		return
	}
	d := r.durations[name]
	if d == nil {
		return
	}
	n := int64(elapsed)
	d.count.Add(1)
	d.sum.Add(n)
	d.last.Store(n)
	for old := d.max.Load(); n > old && !d.max.CompareAndSwap(old, n); old = d.max.Load() {
	}
	if h := r.histograms[name]; h != nil {
		h.Record(context.Background(), elapsed.Seconds())
	}
}

type Timing struct {
	Count        int64   `json:"count"`
	TotalSeconds float64 `json:"total_seconds"`
	LastSeconds  float64 `json:"last_seconds"`
	MaxSeconds   float64 `json:"max_seconds"`
}
type Process struct {
	CPUSeconds        float64 `json:"cpu_seconds"`
	ResidentBytes     int64   `json:"resident_bytes"`
	HeapAllocBytes    uint64  `json:"go_heap_alloc_bytes"`
	Goroutines        int     `json:"goroutines"`
	LogicalReadBytes  int64   `json:"logical_read_bytes"`
	LogicalWriteBytes int64   `json:"logical_write_bytes"`
	StorageReadBytes  int64   `json:"storage_read_bytes"`
	StorageWriteBytes int64   `json:"storage_write_bytes"`
}
type Snapshot struct {
	StartedAt   time.Time         `json:"started_at"`
	ObservedAt  time.Time         `json:"observed_at"`
	Counters    map[string]int64  `json:"counters"`
	Gauges      map[string]int64  `json:"gauges"`
	Durations   map[string]Timing `json:"durations"`
	Process     Process           `json:"process"`
	OTLPEnabled bool              `json:"otlp_enabled"`
}

func process() Process {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	p := Process{HeapAllocBytes: mem.HeapAlloc, Goroutines: runtime.NumGoroutine()}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		p.CPUSeconds = float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				parts := strings.Fields(line)
				if len(parts) == 3 {
					p.ResidentBytes, _ = strconv.ParseInt(parts[1], 10, 64)
					p.ResidentBytes *= 1024
				}
			}
		}
	}
	if f, err := os.Open("/proc/self/io"); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			fields := strings.Fields(s.Text())
			if len(fields) != 2 {
				continue
			}
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "rchar:":
				p.LogicalReadBytes = n
			case "wchar:":
				p.LogicalWriteBytes = n
			case "read_bytes:":
				p.StorageReadBytes = n
			case "write_bytes:":
				p.StorageWriteBytes = n
			}
		}
	}
	return p
}

func (r *Recorder) Snapshot() Snapshot {
	s := Snapshot{ObservedAt: time.Now().UTC(), Counters: map[string]int64{}, Gauges: map[string]int64{}, Durations: map[string]Timing{}, Process: process()}
	if r == nil {
		return s
	}
	s.StartedAt = r.started
	s.OTLPEnabled = r.provider != nil
	for n, c := range r.counters {
		s.Counters[n] = c.Load()
	}
	for n, g := range r.gauges {
		s.Gauges[n] = g.Load()
	}
	for n, d := range r.durations {
		s.Durations[n] = Timing{Count: d.count.Load(), TotalSeconds: float64(d.sum.Load()) / 1e9, LastSeconds: float64(d.last.Load()) / 1e9, MaxSeconds: float64(d.max.Load()) / 1e9}
	}
	return s
}

// StartOTLP is opt-in: an empty explicit endpoint never consults exporter
// environment defaults and never initializes an SDK or network exporter.
func (r *Recorder) StartOTLP(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.OTLPMetricsEndpoint == "" {
		return nil
	}
	exporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.OTLPMetricsEndpoint), otlpmetrichttp.WithTimeout(3*time.Second), otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
	if err != nil {
		return errors.New("telemetry_exporter_initialization_failed")
	}
	interval := time.Duration(cfg.ExportIntervalSeconds) * time.Second
	if interval == 0 {
		interval = 30 * time.Second
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithResource(resource.NewSchemaless(attribute.String("service.name", "skald"), attribute.String("service.instance.id", cfg.ServiceInstanceID))), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval), sdkmetric.WithTimeout(3*time.Second))))
	if err := r.register(provider); err != nil {
		_ = provider.Shutdown(ctx)
		return err
	}
	r.provider = provider
	return nil
}

func (r *Recorder) register(provider *sdkmetric.MeterProvider) error {
	meter := provider.Meter("github.com/algorhythmic/skald")
	var instruments []otelmetric.Observable
	counters := map[string]otelmetric.Int64ObservableCounter{}
	gauges := map[string]otelmetric.Int64ObservableGauge{}
	for _, name := range counterNames {
		c, err := meter.Int64ObservableCounter("skald." + name)
		if err != nil {
			return err
		}
		counters[name] = c
		instruments = append(instruments, c)
	}
	for _, name := range gaugeNames {
		unit := ""
		if strings.HasSuffix(name, "_bytes") {
			unit = "By"
		}
		g, e := meter.Int64ObservableGauge("skald."+name, otelmetric.WithUnit(unit))
		if e != nil {
			return e
		}
		gauges[name] = g
		instruments = append(instruments, g)
	}
	rss, err := meter.Int64ObservableGauge("process.memory.usage", otelmetric.WithUnit("By"))
	if err != nil {
		return err
	}
	instruments = append(instruments, rss)
	heap, err := meter.Int64ObservableGauge("skald.go_heap_alloc", otelmetric.WithUnit("By"))
	if err != nil {
		return err
	}
	instruments = append(instruments, heap)
	goCount, err := meter.Int64ObservableGauge("skald.go_goroutines")
	if err != nil {
		return err
	}
	instruments = append(instruments, goCount)
	cpu, err := meter.Float64ObservableCounter("process.cpu.time", otelmetric.WithUnit("s"))
	if err != nil {
		return err
	}
	instruments = append(instruments, cpu)
	ioCounters := map[string]otelmetric.Int64ObservableCounter{}
	for _, name := range []string{"logical_read_bytes", "logical_write_bytes", "storage_read_bytes", "storage_write_bytes"} {
		c, e := meter.Int64ObservableCounter("skald.process."+name, otelmetric.WithUnit("By"))
		if e != nil {
			return e
		}
		ioCounters[name] = c
		instruments = append(instruments, c)
	}
	for _, name := range durationNames {
		h, e := meter.Float64Histogram("skald."+name+".duration", otelmetric.WithUnit("s"), otelmetric.WithExplicitBucketBoundaries(.0001, .0005, .001, .005, .01, .05, .1, .5, 1, 5, 30))
		if e != nil {
			return e
		}
		r.histograms[name] = h
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o otelmetric.Observer) error {
		for n, c := range counters {
			o.ObserveInt64(c, r.counters[n].Load())
		}
		for n, g := range gauges {
			o.ObserveInt64(g, r.gauges[n].Load())
		}
		p := process()
		o.ObserveInt64(rss, p.ResidentBytes)
		o.ObserveInt64(heap, int64(p.HeapAllocBytes))
		o.ObserveInt64(goCount, int64(p.Goroutines))
		o.ObserveFloat64(cpu, p.CPUSeconds)
		o.ObserveInt64(ioCounters["logical_read_bytes"], p.LogicalReadBytes)
		o.ObserveInt64(ioCounters["logical_write_bytes"], p.LogicalWriteBytes)
		o.ObserveInt64(ioCounters["storage_read_bytes"], p.StorageReadBytes)
		o.ObserveInt64(ioCounters["storage_write_bytes"], p.StorageWriteBytes)
		return nil
	}, instruments...)
	return err
}

func (r *Recorder) Shutdown(ctx context.Context) error {
	if r == nil || r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}

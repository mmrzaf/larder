// Package metrics implements a small Prometheus text exposition registry.
package metrics

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type collector interface {
	metricName() string
	metricHelp() string
	metricType() string
	writeSamples(*strings.Builder)
}

// Registry owns a set of metrics and renders them deterministically.
type Registry struct {
	mu         sync.RWMutex
	collectors map[string]collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{collectors: make(map[string]collector)}
}

// CounterVec is a counter with a declared label set.
type CounterVec struct {
	name   string
	help   string
	labels []string
	mu     sync.RWMutex
	values map[string]*Counter
}

// Counter is one labeled counter value.
type Counter struct {
	mu    sync.Mutex
	value float64
}

// GaugeVec is a gauge with a declared label set.
type GaugeVec struct {
	name   string
	help   string
	labels []string
	mu     sync.RWMutex
	values map[string]*Gauge
}

// Gauge is one labeled gauge value.
type Gauge struct {
	mu    sync.Mutex
	value float64
}

// HistogramVec is a histogram with a declared label set.
type HistogramVec struct {
	name    string
	help    string
	labels  []string
	buckets []float64
	mu      sync.RWMutex
	values  map[string]*Histogram
}

// Histogram is one labeled histogram value.
type Histogram struct {
	mu      sync.Mutex
	bounds  []float64
	buckets []uint64
	sum     float64
	count   uint64
}

// NewCounter registers a counter vector.
func (r *Registry) NewCounter(name, help string, labels ...string) (*CounterVec, error) {
	if err := r.validate(name, labels); err != nil {
		return nil, err
	}
	v := &CounterVec{name: name, help: help, labels: labels, values: make(map[string]*Counter)}
	if err := r.register(v); err != nil {
		return nil, err
	}
	return v, nil
}

// NewGauge registers a gauge vector.
func (r *Registry) NewGauge(name, help string, labels ...string) (*GaugeVec, error) {
	if err := r.validate(name, labels); err != nil {
		return nil, err
	}
	v := &GaugeVec{name: name, help: help, labels: labels, values: make(map[string]*Gauge)}
	if err := r.register(v); err != nil {
		return nil, err
	}
	return v, nil
}

// NewHistogram registers a histogram vector.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) (*HistogramVec, error) {
	if err := r.validate(name, labels); err != nil {
		return nil, err
	}
	if len(buckets) == 0 {
		return nil, fmt.Errorf("metric %s: at least one bucket is required", name)
	}
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	for i, b := range sorted {
		if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
			return nil, fmt.Errorf("metric %s: invalid bucket %v", name, b)
		}
		if i > 0 && sorted[i-1] == b {
			return nil, fmt.Errorf("metric %s: duplicate bucket %v", name, b)
		}
	}
	v := &HistogramVec{name: name, help: help, labels: labels, buckets: sorted, values: make(map[string]*Histogram)}
	if err := r.register(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *Registry) validate(name string, labels []string) error {
	if !metricNamePattern.MatchString(name) {
		return fmt.Errorf("invalid metric name %q", name)
	}
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		if !labelNamePattern.MatchString(label) {
			return fmt.Errorf("metric %s: invalid label name %q", name, label)
		}
		if seen[label] {
			return fmt.Errorf("metric %s: duplicate label %q", name, label)
		}
		seen[label] = true
	}
	return nil
}

func (r *Registry) register(c collector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.collectors[c.metricName()]; exists {
		return fmt.Errorf("metric %s is already registered", c.metricName())
	}
	r.collectors[c.metricName()] = c
	return nil
}

// WritePrometheus renders all metrics in Prometheus text format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.collectors))
	for name := range r.collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := r.collectors[name]
		var b strings.Builder
		fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(c.metricHelp()))
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, c.metricType())
		c.writeSamples(&b)
		if _, err := io.WriteString(w, b.String()); err != nil {
			return fmt.Errorf("write metric %s: %w", name, err)
		}
	}
	return nil
}

// Text returns the Prometheus text exposition.
func (r *Registry) Text() string {
	var b strings.Builder
	_ = r.WritePrometheus(&b)
	return b.String()
}

func (v *CounterVec) metricName() string { return v.name }
func (v *CounterVec) metricHelp() string { return v.help }
func (v *CounterVec) metricType() string { return "counter" }

// With returns the counter for a label set.
func (v *CounterVec) With(labelValues ...string) *Counter {
	key := labelKey(v.name, len(v.labels), labelValues)
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.values[key]
	if c == nil {
		c = &Counter{}
		v.values[key] = c
	}
	return c
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds v.
func (c *Counter) Add(v float64) {
	c.mu.Lock()
	c.value += v
	c.mu.Unlock()
}

// Value returns the current value.
func (c *Counter) Value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (v *CounterVec) writeSamples(b *strings.Builder) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	keys := sortedKeys(v.values)
	for _, key := range keys {
		c := v.values[key]
		fmt.Fprintf(b, "%s%s %s\n", v.name, renderLabels(v.labels, labelsFromKey(len(v.labels), key)), formatFloat(c.Value()))
	}
}

func (v *GaugeVec) metricName() string { return v.name }
func (v *GaugeVec) metricHelp() string { return v.help }
func (v *GaugeVec) metricType() string { return "gauge" }

// With returns the gauge for a label set.
func (v *GaugeVec) With(labelValues ...string) *Gauge {
	key := labelKey(v.name, len(v.labels), labelValues)
	v.mu.Lock()
	defer v.mu.Unlock()
	g := v.values[key]
	if g == nil {
		g = &Gauge{}
		v.values[key] = g
	}
	return g
}

// Set replaces the value.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}

// Add adds v.
func (g *Gauge) Add(v float64) {
	g.mu.Lock()
	g.value += v
	g.mu.Unlock()
}

// Inc adds one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.value
}

func (v *GaugeVec) writeSamples(b *strings.Builder) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	keys := sortedKeys(v.values)
	for _, key := range keys {
		g := v.values[key]
		fmt.Fprintf(b, "%s%s %s\n", v.name, renderLabels(v.labels, labelsFromKey(len(v.labels), key)), formatFloat(g.Value()))
	}
}

func (v *HistogramVec) metricName() string { return v.name }
func (v *HistogramVec) metricHelp() string { return v.help }
func (v *HistogramVec) metricType() string { return "histogram" }

// With returns the histogram for a label set.
func (v *HistogramVec) With(labelValues ...string) *Histogram {
	key := labelKey(v.name, len(v.labels), labelValues)
	v.mu.Lock()
	defer v.mu.Unlock()
	h := v.values[key]
	if h == nil {
		h = &Histogram{bounds: v.buckets, buckets: make([]uint64, len(v.buckets))}
		v.values[key] = h
	}
	return h
}

// Observe records one observation.
func (h *Histogram) Observe(value float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += value
	for i, bound := range h.bounds {
		if value <= bound {
			h.buckets[i]++
			break
		}
	}
}

func (v *HistogramVec) writeSamples(b *strings.Builder) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, key := range sortedKeys(v.values) {
		h := v.values[key]
		labels := labelsFromKey(len(v.labels), key)
		h.mu.Lock()
		cumulative := uint64(0)
		for i, bound := range v.buckets {
			cumulative += h.buckets[i]
			fmt.Fprintf(b, "%s_bucket%s %d\n", v.name,
				renderLabelsWith(v.labels, labels, "le", formatFloat(bound)), cumulative)
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", v.name,
			renderLabelsWith(v.labels, labels, "le", "+Inf"), h.count)
		fmt.Fprintf(b, "%s_sum%s %s\n", v.name, renderLabels(v.labels, labels), formatFloat(h.sum))
		fmt.Fprintf(b, "%s_count%s %d\n", v.name, renderLabels(v.labels, labels), h.count)
		h.mu.Unlock()
	}
}

func labelKey(metric string, expected int, values []string) string {
	if len(values) != expected {
		panic(fmt.Sprintf("metric %s: got %d label values, want %d", metric, len(values), expected))
	}
	var b strings.Builder
	for _, value := range values {
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
	}
	return b.String()
}

func labelsFromKey(count int, key string) []string {
	labels := make([]string, 0, count)
	for len(labels) < count && key != "" {
		colon := strings.IndexByte(key, ':')
		if colon < 0 {
			break
		}
		n, err := strconv.Atoi(key[:colon])
		if err != nil || n < 0 || len(key) < colon+1+n {
			break
		}
		labels = append(labels, key[colon+1:colon+1+n])
		key = key[colon+1+n:]
	}
	return labels
}

func renderLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func renderLabelsWith(names, values []string, extraName, extraValue string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	if len(names) > 0 {
		b.WriteByte(',')
	}
	b.WriteString(extraName)
	b.WriteString(`="`)
	b.WriteString(escapeLabel(extraValue))
	b.WriteByte('"')
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func escapeHelp(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Metrics is the complete metric set exposed by Larder.
type Metrics struct {
	Requests         *CounterVec
	RequestDuration  *HistogramVec
	BytesServed      *CounterVec
	BytesFetched     *CounterVec
	UpstreamRequests *CounterVec
	UpstreamUp       *GaugeVec
	InflightFetches  *GaugeVec
	CacheObjects     *GaugeVec
	CacheBytes       *GaugeVec
	PinnedBytes      *GaugeVec
	BudgetBytes      *GaugeVec
	UsedBytes        *GaugeVec
	DiskFreeBytes    *GaugeVec
	PinnedOverBudget *GaugeVec
	BypassActive     *GaugeVec
	Offline          *GaugeVec
	StaleServed      *CounterVec
	VerifyFailures   *CounterVec
	ScrubCorrupt     *CounterVec
	GCRuns           *CounterVec
	GCEvictedBytes   *CounterVec
	GCEvictedObjects *CounterVec
	BuildInfo        *GaugeVec

	registry *Registry
}

// New registers every metric exposed by the server.
func New(version string) (*Metrics, error) {
	r := NewRegistry()
	m := &Metrics{registry: r}
	var err error

	if m.Requests, err = r.NewCounter("larder_requests_total", "Requests by repo, class, result and status.", "repo", "class", "result", "status"); err != nil {
		return nil, err
	}
	if m.RequestDuration, err = r.NewHistogram("larder_request_duration_seconds", "Request duration in seconds.", requestDurationBuckets, "repo", "class"); err != nil {
		return nil, err
	}
	if m.BytesServed, err = r.NewCounter("larder_bytes_served_total", "Bytes served by repo.", "repo"); err != nil {
		return nil, err
	}
	if m.BytesFetched, err = r.NewCounter("larder_bytes_fetched_total", "Bytes fetched by repo and upstream.", "repo", "upstream"); err != nil {
		return nil, err
	}
	if m.UpstreamRequests, err = r.NewCounter("larder_upstream_requests_total", "Upstream requests by repo, upstream and outcome.", "repo", "upstream", "outcome"); err != nil {
		return nil, err
	}
	if m.UpstreamUp, err = r.NewGauge("larder_upstream_up", "Upstream circuit state: 1 closed, 0 open.", "upstream"); err != nil {
		return nil, err
	}
	if m.InflightFetches, err = r.NewGauge("larder_inflight_fetches", "Fetches currently in flight."); err != nil {
		return nil, err
	}
	if m.CacheObjects, err = r.NewGauge("larder_cache_objects", "Current objects by repo and class.", "repo", "class"); err != nil {
		return nil, err
	}
	if m.CacheBytes, err = r.NewGauge("larder_cache_bytes", "Stored bytes by repo and class.", "repo", "class"); err != nil {
		return nil, err
	}
	if m.PinnedBytes, err = r.NewGauge("larder_pinned_bytes", "Bytes pinned."); err != nil {
		return nil, err
	}
	if m.BudgetBytes, err = r.NewGauge("larder_budget_bytes", "Effective storage budget."); err != nil {
		return nil, err
	}
	if m.UsedBytes, err = r.NewGauge("larder_used_bytes", "Used storage bytes."); err != nil {
		return nil, err
	}
	if m.DiskFreeBytes, err = r.NewGauge("larder_disk_free_bytes", "Free bytes on the data filesystem."); err != nil {
		return nil, err
	}
	if m.PinnedOverBudget, err = r.NewGauge("larder_pinned_over_budget", "Pinned content exceeds the budget."); err != nil {
		return nil, err
	}
	if m.BypassActive, err = r.NewGauge("larder_bypass_active", "Emergency bypass is active."); err != nil {
		return nil, err
	}
	if m.Offline, err = r.NewGauge("larder_offline", "Offline mode is active."); err != nil {
		return nil, err
	}
	if m.StaleServed, err = r.NewCounter("larder_stale_served_total", "Stale responses served by repo.", "repo"); err != nil {
		return nil, err
	}
	if m.VerifyFailures, err = r.NewCounter("larder_verify_failures_total", "Verification failures by repo.", "repo"); err != nil {
		return nil, err
	}
	if m.ScrubCorrupt, err = r.NewCounter("larder_scrub_corrupt_total", "Corrupt blobs found by scrub."); err != nil {
		return nil, err
	}
	if m.GCRuns, err = r.NewCounter("larder_gc_runs_total", "Garbage collection runs."); err != nil {
		return nil, err
	}
	if m.GCEvictedBytes, err = r.NewCounter("larder_gc_evicted_bytes_total", "Bytes evicted by tier.", "tier"); err != nil {
		return nil, err
	}
	if m.GCEvictedObjects, err = r.NewCounter("larder_gc_evicted_objects_total", "Objects evicted by tier.", "tier"); err != nil {
		return nil, err
	}
	if m.BuildInfo, err = r.NewGauge("larder_build_info", "Build information.", "version", "go_version"); err != nil {
		return nil, err
	}
	m.BuildInfo.With(version, runtime.Version()).Set(1)
	return m, nil
}

var requestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Registry returns the registry backing this metric set.
func (m *Metrics) Registry() *Registry { return m.registry }

// WritePrometheus renders the metric set.
func (m *Metrics) WritePrometheus(w io.Writer) error { return m.registry.WritePrometheus(w) }

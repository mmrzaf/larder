package metrics

import (
	"strings"
	"testing"
)

func TestCounterGaugeHistogramExposition(t *testing.T) {
	r := NewRegistry()
	counter, err := r.NewCounter("larder_test_total", "A counter with \\ and\nnewline.", "repo", "class")
	if err != nil {
		t.Fatal(err)
	}
	counter.With(`a"b`+"\n"+`c`, `x\y`).Add(2)
	gauge, err := r.NewGauge("larder_test_gauge", "A gauge.")
	if err != nil {
		t.Fatal(err)
	}
	gauge.With().Set(7)
	histogram, err := r.NewHistogram("larder_test_seconds", "A histogram.", []float64{1, 0.1}, "repo")
	if err != nil {
		t.Fatal(err)
	}
	h := histogram.With("npm")
	h.Observe(0.05)
	h.Observe(0.5)
	h.Observe(2)

	text := r.Text()
	for _, want := range []string{
		"# HELP larder_test_total A counter with \\\\ and\\nnewline.",
		"# TYPE larder_test_total counter",
		`larder_test_total{repo="a\"b\nc",class="x\\y"} 2`,
		"# TYPE larder_test_gauge gauge",
		"larder_test_gauge 7",
		"# TYPE larder_test_seconds histogram",
		`larder_test_seconds_bucket{repo="npm",le="0.1"} 1`,
		`larder_test_seconds_bucket{repo="npm",le="1"} 2`,
		`larder_test_seconds_bucket{repo="npm",le="+Inf"} 3`,
		`larder_test_seconds_sum{repo="npm"} 2.55`,
		`larder_test_seconds_count{repo="npm"} 3`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing %q:\n%s", want, text)
		}
	}
	if text != r.Text() {
		t.Fatal("exposition is not deterministic")
	}
}

func TestMetricSetContainsEveryDocumentedMetric(t *testing.T) {
	m, err := New("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	text := m.Registry().Text()
	names := []string{
		"larder_requests_total",
		"larder_request_duration_seconds",
		"larder_bytes_served_total",
		"larder_bytes_fetched_total",
		"larder_upstream_requests_total",
		"larder_upstream_up",
		"larder_inflight_fetches",
		"larder_cache_objects",
		"larder_cache_bytes",
		"larder_pinned_bytes",
		"larder_budget_bytes",
		"larder_used_bytes",
		"larder_disk_free_bytes",
		"larder_pinned_over_budget",
		"larder_bypass_active",
		"larder_offline",
		"larder_stale_served_total",
		"larder_verify_failures_total",
		"larder_scrub_corrupt_total",
		"larder_gc_runs_total",
		"larder_gc_evicted_bytes_total",
		"larder_gc_evicted_objects_total",
		"larder_build_info",
	}
	for _, name := range names {
		if !strings.Contains(text, "# TYPE "+name+" ") {
			t.Errorf("metric %s is not exposed", name)
		}
	}
	if !strings.Contains(text, `larder_build_info{version="1.2.3",go_version="`) {
		t.Errorf("build info labels missing:\n%s", text)
	}
}

func TestMetricValidation(t *testing.T) {
	r := NewRegistry()
	if _, err := r.NewCounter("bad-name", "x"); err == nil {
		t.Fatal("invalid metric name accepted")
	}
	if _, err := r.NewCounter("good_total", "x", "bad-label"); err == nil {
		t.Fatal("invalid label name accepted")
	}
	if _, err := r.NewCounter("good_total", "x", "a", "a"); err == nil {
		t.Fatal("duplicate label name accepted")
	}
	if _, err := r.NewCounter("good_total", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewCounter("good_total", "x"); err == nil {
		t.Fatal("duplicate metric accepted")
	}
	if _, err := r.NewHistogram("hist", "x", nil); err == nil {
		t.Fatal("empty buckets accepted")
	}
	if _, err := r.NewHistogram("hist", "x", []float64{1, 1}); err == nil {
		t.Fatal("duplicate buckets accepted")
	}
	if _, err := r.NewHistogram("hist", "x", []float64{-1}); err == nil {
		t.Fatal("negative bucket accepted")
	}
}

func TestLabelCountMismatchPanics(t *testing.T) {
	r := NewRegistry()
	c, err := r.NewCounter("labels_total", "x", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("label mismatch did not panic")
		}
	}()
	c.With("only-one")
}

func TestDeterministicOrdering(t *testing.T) {
	render := func() string {
		r := NewRegistry()
		c, _ := r.NewCounter("ordered_total", "x", "label")
		c.With("z").Inc()
		c.With("a").Inc()
		c.With("m").Inc()
		return r.Text()
	}
	first := render()
	second := render()
	if first != second {
		t.Fatalf("exposition order differs:\n%s\n%s", first, second)
	}
	if !strings.Contains(first, `label="a"} 1`) {
		t.Fatalf("unexpected exposition:\n%s", first)
	}
}

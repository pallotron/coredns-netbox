package metrics_test

import (
	"testing"

	"github.com/pallotron/coredns-netbox/internal/metrics"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
)

// gather collects all metric families from the registry and returns them keyed
// by metric name for easy lookup.
func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func TestNewSidecar_RegistersAllMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)

	// Observe/increment each metric so Gather returns non-empty families.
	m.PollTotal.WithLabelValues("success").Inc()
	m.PollDurationSeconds.Observe(1.0)
	m.LastSuccessfulPollTimestamp.Set(1_700_000_000)
	m.NetboxFetchDurationSeconds.Observe(0.5)
	m.NetboxRecordsFetched.Set(42)
	m.NetboxEmptyResponseTotal.Inc()
	m.ZonesActive.Set(3)
	m.ZoneWritesTotal.WithLabelValues("create").Inc()
	m.ZoneWriteErrorsTotal.Inc()

	want := []string{
		"netbox_sidecar_poll_total",
		"netbox_sidecar_poll_duration_seconds",
		"netbox_sidecar_last_successful_poll_timestamp_seconds",
		"netbox_sidecar_netbox_fetch_duration_seconds",
		"netbox_sidecar_netbox_records_fetched",
		"netbox_sidecar_netbox_empty_response_total",
		"netbox_sidecar_zones_active",
		"netbox_sidecar_zone_writes_total",
		"netbox_sidecar_zone_write_errors_total",
	}

	got := gather(t, reg)
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("metric %q not found in gathered output", name)
		}
	}
}

func TestNewSidecar_DuplicateRegistrationPanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.NewSidecar(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when registering metrics twice on the same registry")
		}
	}()
	metrics.NewSidecar(reg)
}

func TestPollTotal_Labels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)

	m.PollTotal.WithLabelValues("success").Inc()
	m.PollTotal.WithLabelValues("success").Inc()
	m.PollTotal.WithLabelValues("error").Inc()

	mfs := gather(t, reg)
	mf, ok := mfs["netbox_sidecar_poll_total"]
	if !ok {
		t.Fatal("netbox_sidecar_poll_total not found")
	}

	counts := make(map[string]float64)
	for _, metric := range mf.GetMetric() {
		for _, lp := range metric.GetLabel() {
			if lp.GetName() == "result" {
				counts[lp.GetValue()] = metric.GetCounter().GetValue()
			}
		}
	}

	if counts["success"] != 2 {
		t.Errorf("expected success=2, got %v", counts["success"])
	}
	if counts["error"] != 1 {
		t.Errorf("expected error=1, got %v", counts["error"])
	}
}

func TestZoneWritesTotal_OpLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)

	m.ZoneWritesTotal.WithLabelValues("create").Add(5)
	m.ZoneWritesTotal.WithLabelValues("update").Add(3)
	m.ZoneWritesTotal.WithLabelValues("delete").Add(1)

	mfs := gather(t, reg)
	mf, ok := mfs["netbox_sidecar_zone_writes_total"]
	if !ok {
		t.Fatal("netbox_sidecar_zone_writes_total not found")
	}

	counts := make(map[string]float64)
	for _, metric := range mf.GetMetric() {
		for _, lp := range metric.GetLabel() {
			if lp.GetName() == "op" {
				counts[lp.GetValue()] = metric.GetCounter().GetValue()
			}
		}
	}

	if counts["create"] != 5 {
		t.Errorf("expected create=5, got %v", counts["create"])
	}
	if counts["update"] != 3 {
		t.Errorf("expected update=3, got %v", counts["update"])
	}
	if counts["delete"] != 1 {
		t.Errorf("expected delete=1, got %v", counts["delete"])
	}
}

func TestPollDurationSeconds_CustomBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)

	// Observe values that fall into different buckets
	m.PollDurationSeconds.Observe(0.3)
	m.PollDurationSeconds.Observe(3.0)
	m.PollDurationSeconds.Observe(90.0)

	mfs := gather(t, reg)
	mf, ok := mfs["netbox_sidecar_poll_duration_seconds"]
	if !ok {
		t.Fatal("netbox_sidecar_poll_duration_seconds not found")
	}
	if len(mf.GetMetric()) == 0 {
		t.Fatal("expected at least one metric")
	}
	h := mf.GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 3 {
		t.Errorf("expected sample_count=3, got %d", h.GetSampleCount())
	}
}

func TestNetboxFetchDurationSeconds_CustomBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)

	m.NetboxFetchDurationSeconds.Observe(0.05)
	m.NetboxFetchDurationSeconds.Observe(1.5)

	mfs := gather(t, reg)
	mf, ok := mfs["netbox_sidecar_netbox_fetch_duration_seconds"]
	if !ok {
		t.Fatal("netbox_sidecar_netbox_fetch_duration_seconds not found")
	}
	h := mf.GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 2 {
		t.Errorf("expected sample_count=2, got %d", h.GetSampleCount())
	}
}

func TestNetboxFetchRetriesTotal_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	m.NetboxFetchRetriesTotal.Inc()

	mfs := gather(t, reg)
	if _, ok := mfs["netbox_sidecar_netbox_fetch_retries_total"]; !ok {
		t.Error("metric netbox_sidecar_netbox_fetch_retries_total not found")
	}
}

func TestZoneStalenessSeconds_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	m.ZoneStalenessSeconds.Set(42.0)

	mfs := gather(t, reg)
	if _, ok := mfs["netbox_sidecar_zone_staleness_seconds"]; !ok {
		t.Error("metric netbox_sidecar_zone_staleness_seconds not found")
	}
}

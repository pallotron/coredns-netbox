package metrics

import "github.com/prometheus/client_golang/prometheus"

// Sidecar holds all Prometheus metrics for the coredns-netbox sidecar process.
type Sidecar struct {
	PollTotal                   *prometheus.CounterVec
	PollDurationSeconds         prometheus.Histogram
	PollCycleDurationSeconds    prometheus.Histogram
	LastSuccessfulPollTimestamp prometheus.Gauge
	NetboxFetchDurationSeconds  prometheus.Histogram
	NetboxRecordsFetched        prometheus.Gauge
	NetboxEmptyResponseTotal    prometheus.Counter
	ShrinkGuardRefusalsTotal    *prometheus.CounterVec
	ShrinkGuardBlocked          prometheus.Gauge
	ShrinkGuardOverridesTotal   prometheus.Counter
	ZonesActive                 prometheus.Gauge
	ZoneWritesTotal             *prometheus.CounterVec
	ZoneWriteErrorsTotal        prometheus.Counter
	NetboxFetchRetriesTotal     prometheus.Counter
	ZoneStalenessSeconds        prometheus.Gauge
	CoreDNSReloadTotal          *prometheus.CounterVec
	CoreDNSReloadRetriesTotal   prometheus.Counter
	CoreDNSReloadDirtyTargets   prometheus.Gauge
	WebhookRequestsTotal        *prometheus.CounterVec
	WebhookLastEventTimestamp   prometheus.Gauge
}

// NewSidecar registers all sidecar metrics with the given Registerer and returns
// the populated Sidecar. Use prometheus.NewRegistry() in tests for isolation.
func NewSidecar(reg prometheus.Registerer) *Sidecar {
	m := &Sidecar{
		PollTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_poll_total",
			Help: "Total number of poll attempts, partitioned by result.",
		}, []string{"result"}),

		PollDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "netbox_sidecar_poll_duration_seconds",
			// Observed in internal/merge around the merge+write phase only:
			// it does NOT include the Netbox fetch, which dominates the cycle
			// at production scale (measured: ~4.5s merge vs ~34s fetch). Use
			// netbox_sidecar_poll_cycle_duration_seconds for the whole cycle
			// — capacity planning against POLL_INTERVAL needs that one. The
			// name is kept as-is so existing dashboards/alerts keep working.
			Help:    "Duration of the merge+write phase (generate, write, reload trigger), excluding the Netbox fetch.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
		}),

		PollCycleDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "netbox_sidecar_poll_cycle_duration_seconds",
			Help:    "Duration of a complete periodic poll cycle (Netbox fetch + merge + write). Compare against POLL_INTERVAL for headroom.",
			Buckets: []float64{1, 5, 10, 20, 30, 45, 60, 90, 120, 300},
		}),

		LastSuccessfulPollTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_last_successful_poll_timestamp_seconds",
			Help: "Unix timestamp of the last successful poll.",
		}),

		NetboxFetchDurationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "netbox_sidecar_netbox_fetch_duration_seconds",
			Help:    "Duration of the Netbox IP address fetch (HTTP).",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),

		NetboxRecordsFetched: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_netbox_records_fetched",
			Help: "Number of IP records returned by Netbox in the last poll.",
		}),

		NetboxEmptyResponseTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "netbox_sidecar_netbox_empty_response_total",
			Help: "Total number of polls where Netbox returned zero records.",
		}),

		ShrinkGuardRefusalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_shrink_guard_refusals_total",
			Help: "Total number of publishes refused because the record set shrank implausibly, partitioned by which rule refused (empty, global, zone).",
		}, []string{"scope"}),

		ShrinkGuardBlocked: prometheus.NewGauge(prometheus.GaugeOpts{
			// 1 for as long as the guard keeps refusing: the served zones are
			// frozen and need an operator. Alert on this together with
			// netbox_sidecar_zone_staleness_seconds, which also climbs since
			// a refused publish is not a successful poll.
			Name: "netbox_sidecar_shrink_guard_blocked",
			Help: "1 while the shrink guard is refusing to publish, 0 otherwise.",
		}),

		ShrinkGuardOverridesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "netbox_sidecar_shrink_guard_overrides_total",
			Help: "Total number of shrink-guard refusals suppressed by an operator override.",
		}),

		ZonesActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_zones_active",
			Help: "Number of DNS zones currently managed by the sidecar.",
		}),

		ZoneWritesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_zone_writes_total",
			Help: "Total number of zone file operations, partitioned by op.",
		}, []string{"op"}),

		ZoneWriteErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "netbox_sidecar_zone_write_errors_total",
			Help: "Total number of zone file write failures.",
		}),

		NetboxFetchRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "netbox_sidecar_netbox_fetch_retries_total",
			Help: "Total number of Netbox fetch retry attempts (excludes the initial attempt).",
		}),

		ZoneStalenessSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_zone_staleness_seconds",
			Help: "Seconds since the last successful zone write. Zero on success; rises on Netbox outage.",
		}),

		CoreDNSReloadTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_coredns_reload_total",
			Help: "Total CoreDNS reload pushes per target address, partitioned by final result (after retries).",
		}, []string{"result"}),

		CoreDNSReloadRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "netbox_sidecar_coredns_reload_retries_total",
			Help: "Total CoreDNS reload push retry attempts (excludes the initial attempt).",
		}),

		CoreDNSReloadDirtyTargets: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_coredns_reload_dirty_targets",
			Help: "CoreDNS addresses whose last reload push has not succeeded yet. Non-zero for more than a few poll cycles means a pod is not accepting reloads.",
		}),

		WebhookRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_webhook_requests_total",
			Help: "Total Netbox webhook requests received, partitioned by result.",
		}, []string{"result"}),

		WebhookLastEventTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_webhook_last_event_timestamp_seconds",
			Help: "Unix timestamp of the last successfully applied Netbox webhook event.",
		}),
	}

	// Pre-create the refusal series for every scope (see
	// internal/shrinkguard) so a dashboard or alert reads 0 from the first
	// scrape instead of a missing series that only appears once something
	// has already gone wrong.
	for _, scope := range []string{"empty", "global", "zone"} {
		m.ShrinkGuardRefusalsTotal.WithLabelValues(scope)
	}

	reg.MustRegister(
		m.PollTotal,
		m.PollDurationSeconds,
		m.PollCycleDurationSeconds,
		m.LastSuccessfulPollTimestamp,
		m.NetboxFetchDurationSeconds,
		m.NetboxRecordsFetched,
		m.NetboxEmptyResponseTotal,
		m.ShrinkGuardRefusalsTotal,
		m.ShrinkGuardBlocked,
		m.ShrinkGuardOverridesTotal,
		m.ZonesActive,
		m.ZoneWritesTotal,
		m.ZoneWriteErrorsTotal,
		m.NetboxFetchRetriesTotal,
		m.ZoneStalenessSeconds,
		m.CoreDNSReloadTotal,
		m.CoreDNSReloadRetriesTotal,
		m.CoreDNSReloadDirtyTargets,
		m.WebhookRequestsTotal,
		m.WebhookLastEventTimestamp,
	)

	return m
}

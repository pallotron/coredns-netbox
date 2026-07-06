package metrics

import "github.com/prometheus/client_golang/prometheus"

// Sidecar holds all Prometheus metrics for the coredns-netbox sidecar process.
type Sidecar struct {
	PollTotal                   *prometheus.CounterVec
	PollDurationSeconds         prometheus.Histogram
	LastSuccessfulPollTimestamp prometheus.Gauge
	NetboxFetchDurationSeconds  prometheus.Histogram
	NetboxRecordsFetched        prometheus.Gauge
	NetboxEmptyResponseTotal    prometheus.Counter
	ZonesActive                 prometheus.Gauge
	ZoneWritesTotal             *prometheus.CounterVec
	ZoneWriteErrorsTotal        prometheus.Counter
	NetboxFetchRetriesTotal     prometheus.Counter
	ZoneStalenessSeconds        prometheus.Gauge
	CoreDNSReloadTotal          *prometheus.CounterVec
	CoreDNSReloadRetriesTotal   prometheus.Counter
	CoreDNSReloadDirtyTargets   prometheus.Gauge
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
			Name:    "netbox_sidecar_poll_duration_seconds",
			Help:    "Duration of each full poll cycle (fetch + discover + write).",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
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
	}

	reg.MustRegister(
		m.PollTotal,
		m.PollDurationSeconds,
		m.LastSuccessfulPollTimestamp,
		m.NetboxFetchDurationSeconds,
		m.NetboxRecordsFetched,
		m.NetboxEmptyResponseTotal,
		m.ZonesActive,
		m.ZoneWritesTotal,
		m.ZoneWriteErrorsTotal,
		m.NetboxFetchRetriesTotal,
		m.ZoneStalenessSeconds,
		m.CoreDNSReloadTotal,
		m.CoreDNSReloadRetriesTotal,
		m.CoreDNSReloadDirtyTargets,
	)

	return m
}

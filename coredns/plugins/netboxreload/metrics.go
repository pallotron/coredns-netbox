package netboxreload

import (
	"github.com/coredns/coredns/plugin"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the netboxreload plugin, exported via the standard CoreDNS
// prometheus plugin endpoint.
var (
	// reloadTotal counts reload attempts by what initiated them (trigger:
	// grpc = sidecar push, poll = fallback poll, startup = initial load) and
	// what they did (result: reloaded = zone data swapped, unchanged = all
	// source content matched the previous load, error = fetch/parse failed).
	reloadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "reload_total",
		Help:      "Zone reload attempts, partitioned by trigger (grpc|poll|startup) and result (reloaded|unchanged|error).",
	}, []string{"trigger", "result"})

	lastReloadTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "last_reload_timestamp_seconds",
		Help:      "Unix timestamp of the last successful reload check, including unchanged ones.",
	})

	// notifyTotal counts NOTIFY sends, one per zone whose serial changed.
	// "success" means the transfer plugin reported no error, which includes
	// the case where it had no concrete peer to send to (a wildcard-only
	// transfer.to sends nothing) — the deployment, not this counter, says
	// whether a secondary was actually told.
	notifyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "notify_total",
		Help:      "DNS NOTIFY sends after a zone's SOA serial changed, partitioned by result (success|error).",
	}, []string{"result"})

	zonesLoaded = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "zones_loaded",
		Help:      "Number of zones currently loaded in memory.",
	})
)

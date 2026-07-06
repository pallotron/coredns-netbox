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

	zonesLoaded = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "zones_loaded",
		Help:      "Number of zones currently loaded in memory.",
	})
)

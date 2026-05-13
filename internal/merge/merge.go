package merge

import (
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
)

// Write merges Netbox zones with dynamic store records, generates PTR entries
// for dynamic records (when reverseDisc is non-nil), and writes zone files via mgr.
func Write(
	netboxZones zonediscovery.ZoneMap,
	store dynamicstore.DynamicStore,
	mgr *zonemanager.Manager,
	m *metrics.Sidecar,
	reverseDisc zonediscovery.Discoverer,
) error {
	pollStart := time.Now()

	// Merge dynamic records into a copy of netboxZones.
	merged := make(zonediscovery.ZoneMap, len(netboxZones))
	maps.Copy(merged, netboxZones)

	var allDynamic []netboxclient.IPRecord

	for _, zone := range store.ListZones() {
		dynRecs := store.GetRecords(zone)
		if len(dynRecs) == 0 {
			if _, ok := merged[zone]; !ok {
				merged[zone] = nil
			}
			continue
		}

		allDynamic = append(allDynamic, dynRecs...)

		// Dynamic records shadow Netbox records with the same DNS name.
		dynByName := make(map[string]struct{}, len(dynRecs))
		for _, r := range dynRecs {
			dynByName[r.DNSName] = struct{}{}
		}
		result := make([]netboxclient.IPRecord, 0, len(merged[zone])+len(dynRecs))
		for _, r := range merged[zone] {
			if _, overridden := dynByName[r.DNSName]; !overridden {
				result = append(result, r)
			}
		}
		result = append(result, dynRecs...)
		merged[zone] = result
	}

	// Generate PTR records for dynamic entries and merge them into reverse zones.
	// Dynamic PTRs shadow Netbox PTRs for the same reverse label (IP address).
	if reverseDisc != nil && len(allDynamic) > 0 {
		dynReverseZones, err := reverseDisc.Discover(allDynamic)
		if err != nil {
			slog.Warn("failed to generate reverse zones for dynamic records", "err", err)
		} else {
			for zone, ptrs := range dynReverseZones {
				dynByAddr := make(map[string]struct{}, len(ptrs))
				for _, p := range ptrs {
					dynByAddr[p.Address] = struct{}{}
				}
				filtered := make([]netboxclient.IPRecord, 0, len(merged[zone]))
				for _, r := range merged[zone] {
					if _, shadowed := dynByAddr[r.Address]; !shadowed {
						filtered = append(filtered, r)
					}
				}
				merged[zone] = append(filtered, ptrs...)
			}
		}
	}

	stats, err := mgr.Update(merged)
	m.ZoneWritesTotal.WithLabelValues("create").Add(float64(stats.Created))
	m.ZoneWritesTotal.WithLabelValues("update").Add(float64(stats.Updated))
	m.ZoneWritesTotal.WithLabelValues("delete").Add(float64(stats.Deleted))
	if stats.WriteErrors > 0 {
		m.ZoneWriteErrorsTotal.Add(float64(stats.WriteErrors))
	}
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
		return fmt.Errorf("update zones: %w", err)
	}

	m.ZonesActive.Set(float64(len(mgr.Zones())))
	m.LastSuccessfulPollTimestamp.SetToCurrentTime()
	m.PollTotal.WithLabelValues("success").Inc()
	m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())

	slog.Info("zone update complete", "active_zones", mgr.Zones())
	return nil
}

package ipcategorizer

import (
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// SelectionStats reports how device records were resolved, feeding the
// netbox_sidecar_devices_total metric. Fallback is the NetBox-hygiene
// burn-down number: devices still resolved by the regex heuristic because
// primary_ip4 is unset or points at an unpublished IP.
type SelectionStats struct {
	PrimaryIP int
	Fallback  int
	None      int
}

// SelectDeviceIPsHybrid selects each device's primary and BMC IPs with
// NetBox's primary_ip4 as the source of truth and the regex heuristic as
// fallback (spec §2):
//
//	1. devices whose primary_ip4 id is present in the fetched (published)
//	   IP set get that IP as their A record;
//	2. everything else keeps today's categorizer selection — including
//	   device names that appear in IP assignments but not in the device
//	   list, so a partial device fetch can never shrink the zone set.
//
// BMC: the device's oob_ip is the source of truth when it is set and
// resolves to a published IP, and always overrides whatever the heuristic
// found — on both the primary-resolved path above and the fallback path
// (a device with no usable primary_ip4 can still have a published oob_ip).
// The bmc/ipmi/ilo/idrac interface-name heuristic is only the BMC source
// for devices with no published oob_ip.
func (c *Categorizer) SelectDeviceIPsHybrid(
	records []netboxclient.IPRecord,
	devices []netboxclient.DeviceRecord,
) (map[string]*DeviceDNSRecords, SelectionStats) {
	byID := make(map[int]netboxclient.IPRecord, len(records))
	for _, r := range records {
		if r.ID != 0 {
			byID[r.ID] = r
		}
	}

	// Heuristic result for every device seen in IP assignments.
	result := c.SelectDeviceIPs(records)

	var stats SelectionStats

	// NetBox allows a VM and a device (or two devices in different sites)
	// to share a name, and FetchDevices concatenates devices then virtual
	// machines. Decide the winner per distinct name in one pass, keeping
	// devices (which come first) ahead of VMs: the first entry for a name
	// whose primary_ip4 resolves to a published IP wins the override; a
	// later entry for an already-decided name never re-decides it. Track
	// per-name winners here, then apply overrides and stats once per name
	// below so duplicates are never double-counted.
	type winner struct {
		primary  netboxclient.IPRecord
		oobID    int
		resolved bool // true once a resolvable primary has been found for this name
	}
	winners := make(map[string]*winner)
	seen := make(map[string]bool, len(devices))

	for _, dev := range devices {
		if dev.Name == "" {
			// Unnamed entries can't be keyed into the zone, so stats
			// intentionally exclude them (they are neither None nor
			// PrimaryIP nor Fallback).
			continue
		}
		seen[dev.Name] = true

		w, ok := winners[dev.Name]
		if ok && w.resolved {
			// Already decided with a resolvable primary; first entry wins.
			continue
		}

		primary, havePrimary := byID[dev.PrimaryIP4ID]
		if dev.PrimaryIP4ID == 0 || !havePrimary {
			// Unresolvable primary: still carry oobID forward (first entry
			// for this name wins, matching the resolvable case below) so the
			// fallback apply pass below can still produce a BMC record.
			if !ok {
				winners[dev.Name] = &winner{oobID: dev.OOBIPID}
			}
			continue
		}

		winners[dev.Name] = &winner{primary: primary, oobID: dev.OOBIPID, resolved: true}
	}

	for name := range seen {
		w := winners[name]

		// A published, resolvable oob_ip always wins the BMC record,
		// regardless of what the bmc-pattern heuristic found.
		var oobIP *netboxclient.IPRecord
		if w != nil && w.oobID != 0 {
			if oob, ok := byID[w.oobID]; ok {
				o := oob
				oobIP = &o
			}
		}

		if w == nil || !w.resolved {
			dns, hadEntry := result[name]
			if !hadEntry {
				if oobIP == nil {
					stats.None++
					continue
				}
				// No heuristic primary or BMC match at all, but a published
				// oob_ip still produces a BMC record (spec §2) — never drop
				// dev.OOBIPID just because the primary is unresolvable.
				dns = &DeviceDNSRecords{DeviceName: name, Zone: c.extractZone(name)}
				result[name] = dns
				stats.None++ // no primary IP was found for this device
			} else {
				stats.Fallback++
			}
			if oobIP != nil {
				dns.BMCIP = oobIP
			}
			continue
		}

		dns := result[name]
		if dns == nil {
			dns = &DeviceDNSRecords{
				DeviceName: name,
				Zone:       c.extractZone(name),
			}
			result[name] = dns
		}
		p := w.primary
		dns.PrimaryIP = &p

		if oobIP != nil {
			dns.BMCIP = oobIP
		}
		stats.PrimaryIP++
	}

	// Fallback union: heuristic entries for names absent from devices
	// entirely (never seen above) stay untouched and count as Fallback.
	for name := range result {
		if !seen[name] {
			stats.Fallback++
		}
	}
	return result, stats
}

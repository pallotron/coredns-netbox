package zonediscovery

import (
	"fmt"
	"net"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// ReverseZoneDiscoverer creates reverse DNS zones (in-addr.arpa, ip6.arpa) from IP addresses
// using statically configured parent zones.
type ReverseZoneDiscoverer struct {
	IPv4Zones []string // Static IPv4 reverse zones (e.g., ["10.in-addr.arpa", "172.16.in-addr.arpa"])
	IPv6Zones []string // Static IPv6 reverse zones (e.g., ["8.b.d.0.1.0.0.2.ip6.arpa"])
}

// NewReverseZoneDiscoverer creates a new reverse zone discoverer with static parent zones.
// Example zones:
//   - IPv4: ["10.in-addr.arpa"] covers all of 10.0.0.0/8
//   - IPv4: ["16.172.in-addr.arpa", "17.172.in-addr.arpa"] covers 172.16.0.0/16 and 172.17.0.0/16
//   - IPv6: ["8.b.d.0.1.0.0.2.ip6.arpa"] covers 2001:db8::/32
func NewReverseZoneDiscoverer(ipv4Zones, ipv6Zones []string) *ReverseZoneDiscoverer {
	return &ReverseZoneDiscoverer{
		IPv4Zones: ipv4Zones,
		IPv6Zones: ipv6Zones,
	}
}

// Discover groups IP records into reverse DNS zones.
// Returns a ZoneMap where keys are reverse zone names (e.g., "10.in-addr.arpa")
// and values are IPRecords with DNSName representing the PTR target.
func (d *ReverseZoneDiscoverer) Discover(records []netboxclient.IPRecord) (ZoneMap, error) {
	zones := make(ZoneMap)

	for _, rec := range records {
		// Skip records without DNS names
		if rec.DNSName == "" {
			continue
		}

		// Parse the IP address
		ip := net.ParseIP(rec.Address)
		if ip == nil {
			continue
		}

		var zoneName, ptrName string
		var err error

		switch rec.Family {
		case 4:
			zoneName, ptrName, err = d.reverseIPv4(ip)
		case 6:
			zoneName, ptrName, err = d.reverseIPv6(ip)
		default:
			continue
		}

		if err != nil {
			// Skip IPs that don't match any configured zone
			continue
		}

		// Create a PTR record entry
		// We reuse IPRecord but with DNSName as the PTR target (FQDN)
		// and the ptrName (reverse notation) stored for the zone file generation
		ptrRecord := netboxclient.IPRecord{
			DNSName:       rec.DNSName,       // PTR target (forward FQDN)
			Address:       ptrName,           // Reverse notation (e.g., "3.2.1")
			Family:        rec.Family,
			DeviceName:    rec.DeviceName,
			InterfaceName: rec.InterfaceName,
			VRF:           rec.VRF,
		}

		zones[zoneName] = append(zones[zoneName], ptrRecord)
	}

	return zones, nil
}

// reverseIPv4 converts an IPv4 address to reverse zone name and PTR record name.
// Finds the matching static zone and generates the PTR name within that zone.
// For example, with zone "10.in-addr.arpa":
//   - 10.1.2.3 → zone "10.in-addr.arpa", PTR name "3.2.1"
// With zone "1.10.in-addr.arpa":
//   - 10.1.2.3 → zone "1.10.in-addr.arpa", PTR name "3.2"
func (d *ReverseZoneDiscoverer) reverseIPv4(ip net.IP) (zone, ptrName string, err error) {
	// Ensure IPv4
	ip = ip.To4()
	if ip == nil {
		return "", "", fmt.Errorf("not an IPv4 address")
	}

	// Split into octets
	octets := []string{
		fmt.Sprintf("%d", ip[0]),
		fmt.Sprintf("%d", ip[1]),
		fmt.Sprintf("%d", ip[2]),
		fmt.Sprintf("%d", ip[3]),
	}

	// Full reverse notation (all 4 octets reversed)
	fullReverse := []string{octets[3], octets[2], octets[1], octets[0]}

	// Find the matching static zone
	var matchedZone string
	var zoneDepth int

	for _, staticZone := range d.IPv4Zones {
		// Remove .in-addr.arpa suffix to get zone octets
		zoneParts := strings.TrimSuffix(staticZone, ".in-addr.arpa")
		if zoneParts == "" {
			continue
		}

		zoneOctets := strings.Split(zoneParts, ".")
		depth := len(zoneOctets)

		// Check if this IP matches this zone
		// Zone "10.in-addr.arpa" → octets[0] must be "10"
		// Zone "1.10.in-addr.arpa" → octets[0] must be "10" and octets[1] must be "1"
		matches := true
		for i := 0; i < depth; i++ {
			if i >= 4 || zoneOctets[depth-1-i] != octets[i] {
				matches = false
				break
			}
		}

		if matches && depth > zoneDepth {
			matchedZone = staticZone
			zoneDepth = depth
		}
	}

	if matchedZone == "" {
		return "", "", fmt.Errorf("no matching reverse zone for IP %s", ip.String())
	}

	// Generate PTR name (remaining octets after zone match)
	// For zone "10.in-addr.arpa" (depth 1): PTR name is "3.2.1"
	// For zone "1.10.in-addr.arpa" (depth 2): PTR name is "3.2"
	ptrOctets := fullReverse[:4-zoneDepth]
	ptrName = strings.Join(ptrOctets, ".")

	return matchedZone, ptrName, nil
}

// reverseIPv6 converts an IPv6 address to reverse zone name and PTR record name.
// Finds the matching static zone and generates the PTR name within that zone.
func (d *ReverseZoneDiscoverer) reverseIPv6(ip net.IP) (zone, ptrName string, err error) {
	// Ensure IPv6
	ip = ip.To16()
	if ip == nil || ip.To4() != nil {
		return "", "", fmt.Errorf("not an IPv6 address")
	}

	// Convert to nibbles (32 hex digits)
	var nibbles []string
	for _, b := range ip {
		nibbles = append(nibbles, fmt.Sprintf("%x", b&0x0f))
		nibbles = append(nibbles, fmt.Sprintf("%x", (b>>4)&0x0f))
	}

	// Reverse for PTR notation
	for i, j := 0, len(nibbles)-1; i < j; i, j = i+1, j-1 {
		nibbles[i], nibbles[j] = nibbles[j], nibbles[i]
	}

	// Find the matching static zone
	var matchedZone string
	var zoneDepth int

	for _, staticZone := range d.IPv6Zones {
		// Remove .ip6.arpa suffix to get zone nibbles
		zoneParts := strings.TrimSuffix(staticZone, ".ip6.arpa")
		if zoneParts == "" {
			continue
		}

		zoneNibbles := strings.Split(zoneParts, ".")
		depth := len(zoneNibbles)

		// Check if this IP matches this zone
		// Zone nibbles are in reverse order, matching the end of reversed IP nibbles
		// e.g., zone "b.8.0.d.0.1.2.0.ip6.arpa" for 2001:db8::/32
		// should match nibbles[24:32] = [b 8 0 d 0 1 2 0]
		matches := true
		for i := 0; i < depth; i++ {
			if i >= 32 || zoneNibbles[i] != nibbles[32-depth+i] {
				matches = false
				break
			}
		}

		if matches && depth > zoneDepth {
			matchedZone = staticZone
			zoneDepth = depth
		}
	}

	if matchedZone == "" {
		return "", "", fmt.Errorf("no matching reverse zone for IP %s", ip.String())
	}

	// Generate PTR name (remaining nibbles after zone match)
	ptrNibbles := nibbles[:32-zoneDepth]
	ptrName = strings.Join(ptrNibbles, ".")

	return matchedZone, ptrName, nil
}

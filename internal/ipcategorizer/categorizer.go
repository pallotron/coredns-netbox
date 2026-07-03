package ipcategorizer

import (
	"regexp"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// InterfaceCategory represents the purpose of an interface
type InterfaceCategory int

const (
	CategoryUnknown InterfaceCategory = iota
	CategoryLoopback
	CategoryDataplane
	CategoryBMC
	CategoryMgmtVRF
	CategoryMgmtInterface
)

// String returns the string representation of the category
func (c InterfaceCategory) String() string {
	switch c {
	case CategoryLoopback:
		return "loopback"
	case CategoryDataplane:
		return "dataplane"
	case CategoryBMC:
		return "bmc"
	case CategoryMgmtVRF:
		return "mgmt-vrf"
	case CategoryMgmtInterface:
		return "mgmt-interface"
	default:
		return "unknown"
	}
}

// Categorizer categorizes network interfaces based on regex patterns
type Categorizer struct {
	bmcPattern       *regexp.Regexp
	loopbackPattern  *regexp.Regexp
	dataplanePattern *regexp.Regexp
	mgmtVRFPattern   *regexp.Regexp
	mgmtIfacePattern *regexp.Regexp
	domainSuffix     string
}

// NewCategorizer creates a new Categorizer with the given patterns and domain suffix
func NewCategorizer(bmcPattern, loopbackPattern, dataplanePattern, mgmtVRFPattern, mgmtIfacePattern, domainSuffix string) (*Categorizer, error) {
	bmc, err := regexp.Compile(bmcPattern)
	if err != nil {
		return nil, err
	}
	loopback, err := regexp.Compile(loopbackPattern)
	if err != nil {
		return nil, err
	}
	dataplane, err := regexp.Compile(dataplanePattern)
	if err != nil {
		return nil, err
	}
	mgmtVRF, err := regexp.Compile(mgmtVRFPattern)
	if err != nil {
		return nil, err
	}
	mgmtIface, err := regexp.Compile(mgmtIfacePattern)
	if err != nil {
		return nil, err
	}

	return &Categorizer{
		bmcPattern:       bmc,
		loopbackPattern:  loopback,
		dataplanePattern: dataplane,
		mgmtVRFPattern:   mgmtVRF,
		mgmtIfacePattern: mgmtIface,
		domainSuffix:     domainSuffix,
	}, nil
}

// CategorizedIP represents an IP address with its category
type CategorizedIP struct {
	netboxclient.IPRecord
	Category InterfaceCategory
}

// Categorize returns the category of the given interface
func (c *Categorizer) Categorize(record netboxclient.IPRecord) InterfaceCategory {
	ifaceName := record.InterfaceName
	vrf := record.VRF

	// Check in priority order
	if c.bmcPattern.MatchString(ifaceName) {
		return CategoryBMC
	}
	if c.loopbackPattern.MatchString(ifaceName) {
		return CategoryLoopback
	}
	if c.dataplanePattern.MatchString(ifaceName) {
		return CategoryDataplane
	}
	if c.mgmtVRFPattern.MatchString(vrf) {
		return CategoryMgmtVRF
	}
	if c.mgmtIfacePattern.MatchString(ifaceName) {
		return CategoryMgmtInterface
	}

	return CategoryUnknown
}

// DeviceDNSRecords represents the DNS records to create for a device
type DeviceDNSRecords struct {
	DeviceName string
	PrimaryIP  *netboxclient.IPRecord // Management IP for base hostname
	BMCIP      *netboxclient.IPRecord // BMC IP for -bmc hostname
	Zone       string                  // DNS zone (extracted from device name)
}

// SelectDeviceIPs groups IPs by device and selects the primary management and BMC IPs
func (c *Categorizer) SelectDeviceIPs(records []netboxclient.IPRecord) map[string]*DeviceDNSRecords {
	// Group by device name
	deviceIPs := make(map[string][]CategorizedIP)
	for _, record := range records {
		if record.DeviceName == "" {
			continue
		}
		cat := c.Categorize(record)
		deviceIPs[record.DeviceName] = append(deviceIPs[record.DeviceName], CategorizedIP{
			IPRecord: record,
			Category: cat,
		})
	}

	// Select best IPs for each device
	result := make(map[string]*DeviceDNSRecords)
	for deviceName, ips := range deviceIPs {
		dnsRecords := &DeviceDNSRecords{
			DeviceName: deviceName,
			Zone:       c.extractZone(deviceName),
		}

		// Separate by category
		var mgmtVRFIPs []netboxclient.IPRecord
		var mgmtIfaceIPs []netboxclient.IPRecord
		var bmcIPs []netboxclient.IPRecord

		for _, ip := range ips {
			switch ip.Category {
			case CategoryBMC:
				bmcIPs = append(bmcIPs, ip.IPRecord)
			case CategoryMgmtVRF:
				mgmtVRFIPs = append(mgmtVRFIPs, ip.IPRecord)
			case CategoryMgmtInterface:
				mgmtIfaceIPs = append(mgmtIfaceIPs, ip.IPRecord)
			}
		}

		// Pick primary management IP (prefer VRF-based over interface name)
		if len(mgmtVRFIPs) > 0 {
			// Sort to ensure consistent selection
			sort.Slice(mgmtVRFIPs, func(i, j int) bool {
				return mgmtVRFIPs[i].InterfaceName < mgmtVRFIPs[j].InterfaceName
			})
			dnsRecords.PrimaryIP = &mgmtVRFIPs[0]
		} else if len(mgmtIfaceIPs) > 0 {
			sort.Slice(mgmtIfaceIPs, func(i, j int) bool {
				return mgmtIfaceIPs[i].InterfaceName < mgmtIfaceIPs[j].InterfaceName
			})
			dnsRecords.PrimaryIP = &mgmtIfaceIPs[0]
		}

		// Pick BMC IP
		if len(bmcIPs) > 0 {
			sort.Slice(bmcIPs, func(i, j int) bool {
				return bmcIPs[i].InterfaceName < bmcIPs[j].InterfaceName
			})
			dnsRecords.BMCIP = &bmcIPs[0]
		}

		// Only add if we have at least one IP to create DNS for
		if dnsRecords.PrimaryIP != nil || dnsRecords.BMCIP != nil {
			result[deviceName] = dnsRecords
		}
	}

	return result
}

// DeviceDNSToRecords converts the output of SelectDeviceIPs into a flat slice
// of IPRecords with fully-qualified DNS names, suitable for zone generation.
// Results are sorted by device name for deterministic output.
//
// When formatter is non-nil and one of its parsers matches the device name,
// the canonical FQDN comes from the canonical template (replacing the legacy
// <device>.<zone> form), the BMC record uses the canonical with "-bmc"
// inserted before the first dot, and each alias template contributes a CNAME
// pointing at the canonical (and, when a BMC IP exists, a BMC alias pointing
// at the BMC canonical). Devices that do not match keep the legacy behavior.
func DeviceDNSToRecords(deviceDNS map[string]*DeviceDNSRecords, formatter *nameformat.Formatter) []netboxclient.IPRecord {
	var records []netboxclient.IPRecord

	deviceNames := make([]string, 0, len(deviceDNS))
	for deviceName := range deviceDNS {
		deviceNames = append(deviceNames, deviceName)
	}
	sort.Strings(deviceNames)

	for _, deviceName := range deviceNames {
		dns := deviceDNS[deviceName]

		canonical := deviceName + "." + dns.Zone
		var aliases []string
		if names, ok := formatter.Format(deviceName); ok {
			canonical = names.Canonical
			aliases = names.Aliases
		}

		if dns.PrimaryIP != nil {
			records = append(records, netboxclient.IPRecord{
				DNSName:       canonical,
				Address:       dns.PrimaryIP.Address,
				Family:        dns.PrimaryIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.PrimaryIP.InterfaceName,
				VRF:           dns.PrimaryIP.VRF,
			})
		}
		if dns.BMCIP != nil {
			records = append(records, netboxclient.IPRecord{
				DNSName:       nameformat.BMCName(canonical),
				Address:       dns.BMCIP.Address,
				Family:        dns.BMCIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.BMCIP.InterfaceName,
				VRF:           dns.BMCIP.VRF,
			})
		}
		for _, alias := range aliases {
			if dns.PrimaryIP != nil {
				records = append(records, netboxclient.IPRecord{
					DNSName:     alias,
					Type:        netboxclient.RecordTypeCNAME,
					CNAMETarget: canonical,
					DeviceName:  deviceName,
				})
			}
			if dns.BMCIP != nil {
				records = append(records, netboxclient.IPRecord{
					DNSName:     nameformat.BMCName(alias),
					Type:        netboxclient.RecordTypeCNAME,
					CNAMETarget: nameformat.BMCName(canonical),
					DeviceName:  deviceName,
				})
			}
		}
	}

	return records
}

// extractZone extracts the DNS zone from a device name
// Examples: dc1-site13a-r101-prod-hv-01 -> dc1-site.example.com
//           dc2-m21-r101-prod-hv-01 -> dc2-m.example.com
//           ams01-r101-pdu-left-01 -> ams01.example.com
//           txdr-iah13a-r301-lab-dev-hv-01 -> txdr-lab-dev.example.com
func (c *Categorizer) extractZone(deviceName string) string {
	parts := strings.Split(deviceName, "-")
	if len(parts) < 2 {
		return c.domainSuffix
	}

	loc := parts[0]

	// Special case: lab devices with pattern {location}-{site}-{rack}-lab-{environment}
	// e.g., txdr-iah13a-r301-lab-dev-hv-01 -> txdr-lab-dev.example.com
	for i, part := range parts {
		if part == "lab" && i+1 < len(parts) {
			env := parts[i+1]
			return loc + "-lab-" + env + "." + c.domainSuffix
		}
	}

	site := parts[1]

	// Strip trailing digits and letters from site component
	// e.g., "site13a" -> "site", "m21" -> "m", "cu2a" -> "cu"
	cleanSite := strings.TrimRight(site, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if cleanSite == "" {
		// All characters were stripped, try a different approach
		// Keep only the alphabetic prefix
		for i, c := range site {
			if c >= '0' && c <= '9' {
				cleanSite = site[:i]
				break
			}
		}
	}

	// If cleanSite is just a single letter, check if it's a rack number or site code
	// - If parts[2] looks like a rack (e.g., "r101"), then cleanSite is a valid site code
	// - Otherwise (e.g., "ams01-r101-..."), the single letter is the rack, use location only
	if len(cleanSite) == 1 {
		// Check if there's a third part that looks like a rack number
		if len(parts) > 2 && len(parts[2]) > 1 && parts[2][0] == 'r' {
			// Third part looks like "r101", so cleanSite is a valid site code
			// Continue to use loc-cleanSite pattern
		} else {
			// Single letter without a rack in third position - it IS the rack, use location only
			return loc + "." + c.domainSuffix
		}
	}

	if cleanSite != "" {
		return loc + "-" + cleanSite + "." + c.domainSuffix
	}

	return loc + "-" + site + "." + c.domainSuffix
}

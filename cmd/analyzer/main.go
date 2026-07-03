package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/logging"
	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

func main() {
	slog.SetDefault(slog.New(logging.NewGCPHandler(os.Stderr, nil)))

	// Command line flags
	filePath := flag.String("file", "", "Path to all_ips.json file")
	showAll := flag.Bool("all", false, "Show all IPs for each device")
	showStats := flag.Bool("stats", false, "Show statistics about the data")
	showDNSNames := flag.Bool("show-dns-names", false, "Show records sourced from dns_name field (service VIPs, UFM cluster IPs, etc.)")
	filterDevice := flag.String("device", "", "Filter to a specific device name (substring match)")
	outputFormat := flag.String("format", "summary", "Output format: summary, detailed, csv")

	// Pattern overrides
	bmcPattern := flag.String("bmc-pattern", "(?i)bmc|ipmi|ilo|idrac", "Regex pattern for BMC interfaces")
	loopbackPattern := flag.String("loopback-pattern", "^lo$|^lo0|^Loopback", "Regex pattern for loopback interfaces")
	dataplanePattern := flag.String("dataplane-pattern", "(?i)storage|vtep|vsan", "Regex pattern for dataplane interfaces")
	mgmtVRFPattern := flag.String("mgmt-vrf-pattern", "(?i)mgmt|oob", "Regex pattern for management VRFs")
	mgmtIfacePattern := flag.String("mgmt-iface-pattern", "(?i)mgmt|Management|fxp0|eth[01]|mgt|NET", "Regex pattern for management interfaces")

	// Domain configuration
	domainSuffix := flag.String("domain", "example.com", "Domain suffix for DNS zones")

	// Reverse zone configuration
	enableReverseZones := flag.Bool("enable-reverse-zones", true, "Enable PTR record preview")
	ipv4Zones := flag.String("ipv4-zones", "10.in-addr.arpa,172.16.in-addr.arpa", "Comma-separated list of IPv4 reverse zones")
	ipv6Zones := flag.String("ipv6-zones", "", "Comma-separated list of IPv6 reverse zones")

	// Name template settings (flags override env vars for dry-run validation)
	var nameParsers, nameAliases multiFlag
	flag.Var(&nameParsers, "name-parser", "Device name parser regex with named groups (repeatable; default: DEVICE_NAME_PARSERS env, newline-separated)")
	flag.Var(&nameAliases, "name-alias", "Alias FQDN template (repeatable; default: NAME_FORMAT_ALIASES env, newline-separated)")
	nameCanonical := flag.String("name-canonical", "", "Canonical FQDN template (default: NAME_FORMAT_CANONICAL env)")
	nameZone := flag.String("name-zone", "", "Optional zone sub-template (default: NAME_FORMAT_ZONE env)")
	validateNameFormats := flag.Bool("validate-name-formats", false, "Report parser match rates, rendered samples, zones and collisions, then exit")

	flag.Parse()

	if *filePath == "" {
		fmt.Println("Usage: analyzer -file <path-to-all_ips.json>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Read the JSON file
	data, err := os.ReadFile(*filePath)
	if err != nil {
		slog.Error("failed to read file", "path", *filePath, "err", err)
		os.Exit(1)
	}

	// Parse the Netbox API response format
	var apiRecords []struct {
		Address string `json:"address"`
		DNSName string `json:"dns_name"`
		VRF     *struct {
			Name string `json:"name"`
		} `json:"vrf"`
		AssignedObject *struct {
			Name   string `json:"name"`
			Device *struct {
				Name string `json:"name"`
			} `json:"device"`
			VirtualMachine *struct {
				Name string `json:"name"`
			} `json:"virtual_machine"`
		} `json:"assigned_object"`
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
	}

	if err := json.Unmarshal(data, &apiRecords); err != nil {
		slog.Error("failed to parse JSON", "err", err)
		os.Exit(1)
	}

	// Convert to IPRecord format
	var records []netboxclient.IPRecord
	for _, apiRec := range apiRecords {
		// Skip non-active records
		if apiRec.Status.Value != "active" {
			continue
		}

		// Extract device and interface info
		deviceName := ""
		interfaceName := ""
		if apiRec.AssignedObject != nil {
			interfaceName = apiRec.AssignedObject.Name
			if apiRec.AssignedObject.Device != nil {
				deviceName = apiRec.AssignedObject.Device.Name
			} else if apiRec.AssignedObject.VirtualMachine != nil {
				deviceName = apiRec.AssignedObject.VirtualMachine.Name
			}
		}

		// Extract VRF
		vrf := ""
		if apiRec.VRF != nil {
			vrf = apiRec.VRF.Name
		}

		// Strip CIDR
		addr := apiRec.Address
		if idx := strings.Index(addr, "/"); idx != -1 {
			addr = addr[:idx]
		}

		// Determine family
		family := 4
		if strings.Contains(addr, ":") {
			family = 6
		}

		records = append(records, netboxclient.IPRecord{
			DNSName:       apiRec.DNSName,
			Address:       addr,
			Family:        family,
			DeviceName:    deviceName,
			InterfaceName: interfaceName,
			VRF:           vrf,
		})
	}

	// Qualify dns_name records — same logic as the sidecar's enrichRecordsWithDeviceNames.
	// Records with a dns_name that lacks the domain suffix get it appended automatically.
	for i, r := range records {
		if r.DNSName != "" {
			records[i].DNSName = zonediscovery.QualifyDNSName(r.DNSName, *domainSuffix)
		}
	}

	// Create categorizer
	cat, err := ipcategorizer.NewCategorizer(*bmcPattern, *loopbackPattern, *dataplanePattern, *mgmtVRFPattern, *mgmtIfacePattern, *domainSuffix)
	if err != nil {
		slog.Error("failed to create categorizer", "err", err)
		os.Exit(1)
	}

	// Build formatter from flags (falling back to env vars)
	parsers := []string(nameParsers)
	if len(parsers) == 0 {
		parsers = nameformat.SplitLines(os.Getenv("DEVICE_NAME_PARSERS"))
	}
	aliases := []string(nameAliases)
	if len(aliases) == 0 {
		aliases = nameformat.SplitLines(os.Getenv("NAME_FORMAT_ALIASES"))
	}
	canonical := *nameCanonical
	if canonical == "" {
		canonical = os.Getenv("NAME_FORMAT_CANONICAL")
	}
	zoneTmpl := *nameZone
	if zoneTmpl == "" {
		zoneTmpl = os.Getenv("NAME_FORMAT_ZONE")
	}
	formatter, err := nameformat.New(parsers, canonical, aliases, zoneTmpl, *domainSuffix)
	if err != nil {
		slog.Error("invalid name format configuration", "err", err)
		os.Exit(1)
	}

	if *showStats {
		// For stats mode, discover reverse zones from raw records
		var reverseZones zonediscovery.ZoneMap
		if *enableReverseZones {
			ipv4ZoneList := parseZoneList(*ipv4Zones)
			ipv6ZoneList := parseZoneList(*ipv6Zones)
			disc := zonediscovery.NewReverseZoneDiscoverer(ipv4ZoneList, ipv6ZoneList)
			reverseZones, err = disc.Discover(records)
			if err != nil {
				slog.Error("failed to discover reverse zones", "err", err)
				os.Exit(1)
			}
		}
		showStatistics(records, cat, reverseZones)
		return
	}

	// Split records: those with dns_name set vs those needing device-based generation
	var withDNSName []netboxclient.IPRecord
	var withoutDNSName []netboxclient.IPRecord
	for _, r := range records {
		if r.DNSName != "" {
			withDNSName = append(withDNSName, r)
		} else {
			withoutDNSName = append(withoutDNSName, r)
		}
	}

	// Show dns_name records if requested
	if *showDNSNames {
		fmt.Printf("Records sourced from dns_name field (%d total):\n\n", len(withDNSName))
		dnsSorted := make([]netboxclient.IPRecord, len(withDNSName))
		copy(dnsSorted, withDNSName)
		sort.Slice(dnsSorted, func(i, j int) bool { return dnsSorted[i].DNSName < dnsSorted[j].DNSName })
		for _, r := range dnsSorted {
			if *filterDevice == "" || strings.Contains(r.DNSName, *filterDevice) {
				fmt.Printf("  %-60s %s\n", r.DNSName, r.Address)
			}
		}
		return
	}

	// Select device IPs (creates FQDNs from device names)
	deviceDNS := cat.SelectDeviceIPs(withoutDNSName)

	// Filter if requested (before discovering reverse zones)
	if *filterDevice != "" {
		filtered := make(map[string]*ipcategorizer.DeviceDNSRecords)
		for name, dns := range deviceDNS {
			if strings.Contains(name, *filterDevice) {
				filtered[name] = dns
			}
		}
		deviceDNS = filtered
	}

	if *validateNameFormats {
		if formatter == nil {
			fmt.Println("no name parsers configured — set -name-parser/-name-canonical or the DEVICE_NAME_PARSERS/NAME_FORMAT_CANONICAL env vars")
			os.Exit(1)
		}
		dnsNameSet := make(map[string]bool, len(withDNSName))
		for _, r := range withDNSName {
			dnsNameSet[strings.ToLower(strings.TrimSuffix(r.DNSName, "."))] = true
		}
		reportNameFormats(deviceDNS, dnsNameSet, formatter, len(parsers))
		return
	}

	// Generate enriched records (A/AAAA + CNAME aliases from formatter)
	genRecords := ipcategorizer.DeviceDNSToRecords(deviceDNS, formatter)

	// Discover reverse zones from selected (and possibly filtered) device IPs
	// This ensures PTR records match the forward A/AAAA records we're creating
	var reverseZones zonediscovery.ZoneMap
	if *enableReverseZones {
		ipv4ZoneList := parseZoneList(*ipv4Zones)
		ipv6ZoneList := parseZoneList(*ipv6Zones)
		disc := zonediscovery.NewReverseZoneDiscoverer(ipv4ZoneList, ipv6ZoneList)
		reverseZones, err = disc.Discover(genRecords)
		if err != nil {
			slog.Error("failed to discover reverse zones", "err", err)
			os.Exit(1)
		}
	}

	// Output results
	switch *outputFormat {
	case "csv":
		outputCSV(deviceDNS, genRecords, reverseZones)
	case "detailed":
		outputDetailed(deviceDNS, records, genRecords, cat, *showAll, reverseZones)
	default:
		outputSummary(deviceDNS, reverseZones)
	}
}

// multiFlag collects repeated occurrences of a string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, "\n") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseZoneList parses a comma-separated list of zones.
func parseZoneList(s string) []string {
	if s == "" {
		return nil
	}
	var zones []string
	for _, z := range strings.Split(s, ",") {
		z = strings.TrimSpace(z)
		if z != "" {
			zones = append(zones, z)
		}
	}
	return zones
}



func showStatistics(records []netboxclient.IPRecord, cat *ipcategorizer.Categorizer, reverseZones zonediscovery.ZoneMap) {
	totalRecords := len(records)
	withDevice := 0
	withDNSName := 0
	categoryCount := make(map[ipcategorizer.InterfaceCategory]int)
	vrfCount := make(map[string]int)

	for _, rec := range records {
		if rec.DeviceName != "" {
			withDevice++
		}
		if rec.DNSName != "" {
			withDNSName++
		}

		category := cat.Categorize(rec)
		categoryCount[category]++

		vrf := rec.VRF
		if vrf == "" {
			vrf = "no-vrf"
		}
		vrfCount[vrf]++
	}

	fmt.Printf("Total active IP records: %d\n", totalRecords)
	fmt.Printf("Records with device assigned: %d (%.1f%%)\n", withDevice, float64(withDevice)/float64(totalRecords)*100)
	fmt.Printf("Records with DNS name: %d (%.1f%%)\n", withDNSName, float64(withDNSName)/float64(totalRecords)*100)
	fmt.Println()

	fmt.Println("Interface Categories:")
	for cat, count := range categoryCount {
		fmt.Printf("  %-20s: %6d (%.1f%%)\n", cat.String(), count, float64(count)/float64(totalRecords)*100)
	}
	fmt.Println()

	fmt.Println("Top 10 VRFs:")
	type vrfStat struct {
		name  string
		count int
	}
	var vrfs []vrfStat
	for name, count := range vrfCount {
		vrfs = append(vrfs, vrfStat{name, count})
	}
	sort.Slice(vrfs, func(i, j int) bool {
		return vrfs[i].count > vrfs[j].count
	})
	for i := 0; i < 10 && i < len(vrfs); i++ {
		fmt.Printf("  %-30s: %6d\n", vrfs[i].name, vrfs[i].count)
	}

	// Reverse zone statistics
	if len(reverseZones) > 0 {
		fmt.Println()
		totalPTRs := 0
		for _, recs := range reverseZones {
			totalPTRs += len(recs)
		}

		fmt.Printf("Reverse DNS Zones: %d\n", len(reverseZones))
		fmt.Printf("Total PTR records: %d\n", totalPTRs)
		fmt.Println()

		fmt.Println("Top 10 Reverse Zones:")
		type zoneStat struct {
			name  string
			count int
		}
		var zones []zoneStat
		for name, recs := range reverseZones {
			zones = append(zones, zoneStat{name, len(recs)})
		}
		sort.Slice(zones, func(i, j int) bool {
			return zones[i].count > zones[j].count
		})
		for i := 0; i < 10 && i < len(zones); i++ {
			fmt.Printf("  %-40s: %6d PTR records\n", zones[i].name, zones[i].count)
		}
	}
}

func outputSummary(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, reverseZones zonediscovery.ZoneMap) {
	// Sort device names
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	totalDevices := len(devices)
	withPrimary := 0
	withBMC := 0
	zoneCount := make(map[string]int)

	for _, name := range devices {
		dns := deviceDNS[name]
		if dns.PrimaryIP != nil {
			withPrimary++
		}
		if dns.BMCIP != nil {
			withBMC++
		}
		zoneCount[dns.Zone]++
	}

	fmt.Printf("Total devices with DNS records: %d\n", totalDevices)
	fmt.Printf("Devices with primary management IP: %d\n", withPrimary)
	fmt.Printf("Devices with BMC IP: %d\n", withBMC)
	fmt.Println()

	fmt.Println("Forward DNS Records to be created:")
	fmt.Printf("  Primary hostnames: %d\n", withPrimary)
	fmt.Printf("  BMC hostnames (-bmc suffix): %d\n", withBMC)
	fmt.Printf("  Total DNS A/AAAA records: %d\n", withPrimary+withBMC)
	fmt.Println()

	// Reverse DNS summary
	if len(reverseZones) > 0 {
		totalPTRs := 0
		for _, recs := range reverseZones {
			totalPTRs += len(recs)
		}
		fmt.Println("Reverse DNS Records to be created:")
		fmt.Printf("  Reverse zones: %d\n", len(reverseZones))
		fmt.Printf("  Total PTR records: %d\n", totalPTRs)
		fmt.Println()
	}

	fmt.Println("Top 10 Forward Zones:")
	type zoneStat struct {
		name  string
		count int
	}
	var zones []zoneStat
	for name, count := range zoneCount {
		zones = append(zones, zoneStat{name, count})
	}
	sort.Slice(zones, func(i, j int) bool {
		return zones[i].count > zones[j].count
	})
	for i := 0; i < 10 && i < len(zones); i++ {
		fmt.Printf("  %-30s: %6d devices\n", zones[i].name, zones[i].count)
	}
	fmt.Println()

	fmt.Println("Sample Forward DNS records (first 10 devices):")
	for i := 0; i < 10 && i < len(devices); i++ {
		name := devices[i]
		dns := deviceDNS[name]
		if dns.PrimaryIP != nil {
			fmt.Printf("  %s.%s. → %s\n", name, dns.Zone, dns.PrimaryIP.Address)
		}
		if dns.BMCIP != nil {
			fmt.Printf("  %s-bmc.%s. → %s\n", name, dns.Zone, dns.BMCIP.Address)
		}
	}

	// Sample PTR records
	if len(reverseZones) > 0 {
		fmt.Println()
		fmt.Println("Sample Reverse DNS (PTR) records (first 10):")

		// Get first reverse zone
		var sortedZones []string
		for zone := range reverseZones {
			sortedZones = append(sortedZones, zone)
		}
		sort.Strings(sortedZones)

		shown := 0
		for _, zone := range sortedZones {
			if shown >= 10 {
				break
			}
			recs := reverseZones[zone]

			// Sort records in zone
			sort.Slice(recs, func(i, j int) bool {
				return recs[i].Address < recs[j].Address
			})

			for _, rec := range recs {
				if shown >= 10 {
					break
				}
				// For PTR records, Address contains the PTR name, DNSName contains the target
				if rec.Address != "" {
					fmt.Printf("  %s.%s. → %s.\n", rec.Address, zone, rec.DNSName)
				}
				shown++
			}
		}
	}
}

func outputDetailed(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, allRecords, genRecords []netboxclient.IPRecord, cat *ipcategorizer.Categorizer, showAll bool, reverseZones zonediscovery.ZoneMap) {
	// Sort device names
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	// Build alias index for fast per-device lookup
	aliasesByDevice := map[string][]netboxclient.IPRecord{}
	for _, r := range genRecords {
		if r.Type == netboxclient.RecordTypeCNAME {
			aliasesByDevice[r.DeviceName] = append(aliasesByDevice[r.DeviceName], r)
		}
	}

	for _, name := range devices {
		dns := deviceDNS[name]
		fmt.Printf("\nDevice: %s\n", name)
		fmt.Printf("  Zone: %s\n", dns.Zone)

		if dns.PrimaryIP != nil {
			fmt.Printf("  Primary: %s.%s. → %s (interface: %s, vrf: %s)\n",
				name, dns.Zone, dns.PrimaryIP.Address, dns.PrimaryIP.InterfaceName, dns.PrimaryIP.VRF)
		}
		if dns.BMCIP != nil {
			fmt.Printf("  BMC:     %s-bmc.%s. → %s (interface: %s, vrf: %s)\n",
				name, dns.Zone, dns.BMCIP.Address, dns.BMCIP.InterfaceName, dns.BMCIP.VRF)
		}
		for _, a := range aliasesByDevice[name] {
			fmt.Printf("  Alias:   %s. → CNAME %s.\n", a.DNSName, a.CNAMETarget)
		}

		if showAll {
			fmt.Println("  All IPs:")
			for _, rec := range allRecords {
				if rec.DeviceName == name {
					category := cat.Categorize(rec)
					fmt.Printf("    %-20s %-15s %-25s %-15s %s\n",
						rec.InterfaceName, rec.Address, rec.VRF, category.String(), rec.DNSName)
				}
			}
		}
	}

	// Show reverse zone summary if available
	if len(reverseZones) > 0 {
		totalPTRs := 0
		for _, recs := range reverseZones {
			totalPTRs += len(recs)
		}
		fmt.Printf("\nReverse DNS: %d PTR records across %d reverse zones\n", totalPTRs, len(reverseZones))
	}
}

func outputCSV(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, genRecords []netboxclient.IPRecord, reverseZones zonediscovery.ZoneMap) {
	fmt.Println("record_type,zone,name,value,device,interface,vrf")

	// Forward DNS records
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	for _, name := range devices {
		dns := deviceDNS[name]
		if dns.PrimaryIP != nil {
			fmt.Printf("A,%s,%s,%s,%s,%s,%s\n",
				dns.Zone, name, dns.PrimaryIP.Address, name,
				dns.PrimaryIP.InterfaceName, dns.PrimaryIP.VRF)
		}
		if dns.BMCIP != nil {
			fmt.Printf("A,%s,%s-bmc,%s,%s,%s,%s\n",
				dns.Zone, name, dns.BMCIP.Address, name,
				dns.BMCIP.InterfaceName, dns.BMCIP.VRF)
		}
	}

	// CNAME alias records (name templates). Zone is printed as "-" because
	// aliases may land in template-defined zones the analyzer cannot know.
	for _, r := range genRecords {
		if r.Type == netboxclient.RecordTypeCNAME {
			fmt.Printf("CNAME,-,%s,%s,%s,,\n", r.DNSName, r.CNAMETarget, r.DeviceName)
		}
	}

	// Reverse DNS (PTR) records
	if len(reverseZones) > 0 {
		var sortedZones []string
		for zone := range reverseZones {
			sortedZones = append(sortedZones, zone)
		}
		sort.Strings(sortedZones)

		for _, zone := range sortedZones {
			recs := reverseZones[zone]

			// Sort records in zone
			sort.Slice(recs, func(i, j int) bool {
				return recs[i].Address < recs[j].Address
			})

			for _, rec := range recs {
				// For PTR records: Address contains PTR name, DNSName contains target
				fmt.Printf("PTR,%s,%s,%s,%s,%s,%s\n",
					zone, rec.Address, rec.DNSName, rec.DeviceName,
					rec.InterfaceName, rec.VRF)
			}
		}
	}
}

// renderedDevice holds the formatter output for one device after pass 1.
type renderedDevice struct {
	name  string
	names nameformat.Names
}

// reportNameFormats dry-run validates a parser/template set: per-parser match
// counts, top unmatched name shapes, sample renders, resulting zones, and
// collision detection (alias-vs-alias, alias-vs-canonical, alias-vs-dns_name).
func reportNameFormats(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, dnsNameSet map[string]bool, formatter *nameformat.Formatter, numParsers int) {
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	digits := regexp.MustCompile(`[0-9]+`)
	perParser := make([]int, numParsers)
	unmatchedShapes := map[string]int{}
	canonicalOwner := map[string]string{}
	aliasOwner := map[string]string{}
	zones := map[string]int{}
	var samples []string
	var collisions []string

	// Pass 1: match index bookkeeping, Format, canonical-collision check,
	// fill canonicalOwner, zones, samples. Store results for pass 2.
	var rendered []renderedDevice
	for _, name := range devices {
		idx := formatter.MatchIndex(name)
		if idx < 0 {
			unmatchedShapes[digits.ReplaceAllString(strings.ToLower(name), "N")]++
			continue
		}
		perParser[idx]++

		names, ok := formatter.Format(name)
		if !ok {
			continue
		}
		if prev, dup := canonicalOwner[strings.ToLower(names.Canonical)]; dup && prev != name {
			collisions = append(collisions, fmt.Sprintf("CANONICAL %s from %q and %q", names.Canonical, prev, name))
		}
		canonicalOwner[strings.ToLower(names.Canonical)] = name
		if i := strings.Index(names.Canonical, "."); i >= 0 {
			zones[names.Canonical[i+1:]]++
		}
		if len(samples) < 10 {
			samples = append(samples, fmt.Sprintf("%-40s -> %s (aliases: %s)", name, names.Canonical, strings.Join(names.Aliases, ", ")))
		}
		rendered = append(rendered, renderedDevice{name: name, names: names})
	}

	// Pass 2: check aliases against each other and against the now-COMPLETE
	// canonicalOwner map. Checking aliases in the same pass as pass 1 would
	// miss collisions with later devices' canonicals.
	for _, rd := range rendered {
		for _, alias := range rd.names.Aliases {
			key := strings.ToLower(alias)
			if prev, dup := aliasOwner[key]; dup && prev != rd.name {
				collisions = append(collisions, fmt.Sprintf("ALIAS %s from %q and %q", alias, prev, rd.name))
			}
			aliasOwner[key] = rd.name
			if _, hit := canonicalOwner[key]; hit {
				collisions = append(collisions, fmt.Sprintf("ALIAS-vs-CANONICAL %s (device %q)", alias, rd.name))
			}
			if dnsNameSet[key] {
				collisions = append(collisions, fmt.Sprintf("ALIAS-vs-dns_name %s (device %q)", alias, rd.name))
			}
		}
	}

	matched := 0
	for i, n := range perParser {
		fmt.Printf("parser %d matched %d devices\n", i, n)
		matched += n
	}
	fmt.Printf("total: %d/%d devices matched (%.1f%%), %d fall back to legacy naming\n\n",
		matched, len(devices), 100*float64(matched)/float64(max(len(devices), 1)), len(devices)-matched)

	type shapeCount struct {
		shape string
		n     int
	}
	var shapes []shapeCount
	for s, n := range unmatchedShapes {
		shapes = append(shapes, shapeCount{s, n})
	}
	sort.Slice(shapes, func(i, j int) bool { return shapes[i].n > shapes[j].n })
	fmt.Println("top unmatched name shapes (digits collapsed to N):")
	for i, s := range shapes {
		if i >= 15 {
			break
		}
		fmt.Printf("  %6d  %s\n", s.n, s.shape)
	}

	fmt.Println("\nsample renders:")
	for _, s := range samples {
		fmt.Printf("  %s\n", s)
	}

	fmt.Printf("\nzones produced (%d):\n", len(zones))
	var zoneNames []string
	for z := range zones {
		zoneNames = append(zoneNames, z)
	}
	sort.Strings(zoneNames)
	for _, z := range zoneNames {
		fmt.Printf("  %6d  %s\n", zones[z], z)
	}

	fmt.Printf("\ncollisions (%d) — these CNAMEs would be DROPPED by the generator:\n", len(collisions))
	for i, c := range collisions {
		if i >= 30 {
			fmt.Printf("  ... and %d more\n", len(collisions)-30)
			break
		}
		fmt.Printf("  %s\n", c)
	}
	if len(collisions) == 0 {
		fmt.Println("  none")
	}
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/logging"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

func main() {
	slog.SetDefault(slog.New(logging.NewGCPHandler(os.Stderr, nil)))

	// Command line flags
	filePath := flag.String("file", "", "Path to all_ips.json file")
	showAll := flag.Bool("all", false, "Show all IPs for each device")
	showStats := flag.Bool("stats", false, "Show statistics about the data")
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

	// Create categorizer
	cat, err := ipcategorizer.NewCategorizer(*bmcPattern, *loopbackPattern, *dataplanePattern, *mgmtVRFPattern, *mgmtIfacePattern, *domainSuffix)
	if err != nil {
		slog.Error("failed to create categorizer", "err", err)
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

	// Select device IPs (creates FQDNs from device names)
	deviceDNS := cat.SelectDeviceIPs(records)

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

	// Discover reverse zones from selected (and possibly filtered) device IPs
	// This ensures PTR records match the forward A/AAAA records we're creating
	var reverseZones zonediscovery.ZoneMap
	if *enableReverseZones {
		enrichedRecords := deviceDNSToRecords(deviceDNS)
		ipv4ZoneList := parseZoneList(*ipv4Zones)
		ipv6ZoneList := parseZoneList(*ipv6Zones)
		disc := zonediscovery.NewReverseZoneDiscoverer(ipv4ZoneList, ipv6ZoneList)
		reverseZones, err = disc.Discover(enrichedRecords)
		if err != nil {
			slog.Error("failed to discover reverse zones", "err", err)
			os.Exit(1)
		}
	}

	// Output results
	switch *outputFormat {
	case "csv":
		outputCSV(deviceDNS, reverseZones)
	case "detailed":
		outputDetailed(deviceDNS, records, cat, *showAll, reverseZones)
	default:
		outputSummary(deviceDNS, reverseZones)
	}
}

// parseZoneList parses a comma-separated list of zones
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

// deviceDNSToRecords converts selected device DNS records back to IPRecord format
// so we can discover reverse zones from the actual IPs we're creating forward records for
func deviceDNSToRecords(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords) []netboxclient.IPRecord {
	var records []netboxclient.IPRecord

	for deviceName, dns := range deviceDNS {
		// Primary management IP
		if dns.PrimaryIP != nil {
			fqdn := deviceName + "." + dns.Zone
			records = append(records, netboxclient.IPRecord{
				DNSName:       fqdn,
				Address:       dns.PrimaryIP.Address,
				Family:        dns.PrimaryIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.PrimaryIP.InterfaceName,
				VRF:           dns.PrimaryIP.VRF,
			})
		}

		// BMC IP
		if dns.BMCIP != nil {
			fqdn := deviceName + "-bmc." + dns.Zone
			records = append(records, netboxclient.IPRecord{
				DNSName:       fqdn,
				Address:       dns.BMCIP.Address,
				Family:        dns.BMCIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.BMCIP.InterfaceName,
				VRF:           dns.BMCIP.VRF,
			})
		}
	}

	return records
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

func outputDetailed(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, allRecords []netboxclient.IPRecord, cat *ipcategorizer.Categorizer, showAll bool, reverseZones zonediscovery.ZoneMap) {
	// Sort device names
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

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

func outputCSV(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, reverseZones zonediscovery.ZoneMap) {
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

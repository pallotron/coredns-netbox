package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

func main() {
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

	flag.Parse()

	if *filePath == "" {
		fmt.Println("Usage: analyzer -file <path-to-all_ips.json>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Read the JSON file
	data, err := os.ReadFile(*filePath)
	if err != nil {
		log.Fatalf("Failed to read file: %v", err)
	}

	// Parse the Netbox API response format
	var apiRecords []struct {
		Address        string `json:"address"`
		DNSName        string `json:"dns_name"`
		VRF            *struct {
			Name string `json:"name"`
		} `json:"vrf"`
		AssignedObject *struct {
			Name   string `json:"name"`
			Device *struct {
				Name string `json:"name"`
			} `json:"device"`
		} `json:"assigned_object"`
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
	}

	if err := json.Unmarshal(data, &apiRecords); err != nil {
		log.Fatalf("Failed to parse JSON: %v", err)
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
		log.Fatalf("Failed to create categorizer: %v", err)
	}

	if *showStats {
		showStatistics(records, cat)
		return
	}

	// Select device IPs
	deviceDNS := cat.SelectDeviceIPs(records)

	// Filter if requested
	if *filterDevice != "" {
		filtered := make(map[string]*ipcategorizer.DeviceDNSRecords)
		for name, dns := range deviceDNS {
			if strings.Contains(name, *filterDevice) {
				filtered[name] = dns
			}
		}
		deviceDNS = filtered
	}

	// Output results
	switch *outputFormat {
	case "csv":
		outputCSV(deviceDNS)
	case "detailed":
		outputDetailed(deviceDNS, records, cat, *showAll)
	default:
		outputSummary(deviceDNS)
	}
}

func showStatistics(records []netboxclient.IPRecord, cat *ipcategorizer.Categorizer) {
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
}

func outputSummary(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords) {
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

	fmt.Println("DNS Records to be created:")
	fmt.Printf("  Primary hostnames: %d\n", withPrimary)
	fmt.Printf("  BMC hostnames (-bmc suffix): %d\n", withBMC)
	fmt.Printf("  Total DNS A/AAAA records: %d\n", withPrimary+withBMC)
	fmt.Println()

	fmt.Println("Top 10 Zones:")
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

	fmt.Println("Sample DNS records (first 10 devices):")
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
}

func outputDetailed(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords, allRecords []netboxclient.IPRecord, cat *ipcategorizer.Categorizer, showAll bool) {
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
}

func outputCSV(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords) {
	fmt.Println("device_name,dns_hostname,ip_address,record_type,interface,vrf,zone")

	// Sort device names
	var devices []string
	for name := range deviceDNS {
		devices = append(devices, name)
	}
	sort.Strings(devices)

	for _, name := range devices {
		dns := deviceDNS[name]
		if dns.PrimaryIP != nil {
			fmt.Printf("%s,%s.%s,%s,primary,%s,%s,%s\n",
				name, name, dns.Zone, dns.PrimaryIP.Address,
				dns.PrimaryIP.InterfaceName, dns.PrimaryIP.VRF, dns.Zone)
		}
		if dns.BMCIP != nil {
			fmt.Printf("%s,%s-bmc.%s,%s,bmc,%s,%s,%s\n",
				name, name, dns.Zone, dns.BMCIP.Address,
				dns.BMCIP.InterfaceName, dns.BMCIP.VRF, dns.Zone)
		}
	}
}

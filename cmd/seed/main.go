// The seed tool uses go-netbox/v4 directly because it needs write operations
// (IpamIpAddressesCreate) that are only used in dev. The production client
// (internal/netboxclient) uses raw HTTP for Netbox v3+v4 compatibility.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	netboxclient "github.com/netbox-community/go-netbox/v4"
)

type seedRecord struct {
	Address string
	DNSName string
}

var testRecords = []seedRecord{
	// A records
	{Address: "10.0.0.1/24", DNSName: "host1.example.org"},
	{Address: "10.0.0.2/24", DNSName: "host2.example.org"},
	{Address: "10.0.0.3/24", DNSName: "host3.example.org"},
	{Address: "10.0.0.4/24", DNSName: "host4.example.org"},
	{Address: "10.0.0.5/24", DNSName: "host5.example.org"},
	{Address: "192.168.1.1/24", DNSName: "web.example.org"},
	{Address: "192.168.1.2/24", DNSName: "db.example.org"},
	{Address: "192.168.1.3/24", DNSName: "cache.example.org"},
	// AAAA records
	{Address: "2001:db8::1/64", DNSName: "host1.example.org"},
	{Address: "2001:db8::2/64", DNSName: "host2.example.org"},
}

func main() {
	netboxURL := os.Getenv("NETBOX_URL")
	if netboxURL == "" {
		netboxURL = "http://localhost:8080"
	}
	token := os.Getenv("NETBOX_TOKEN")
	if token == "" {
		log.Fatal("NETBOX_TOKEN is required")
	}

	// Netbox 4.x v2 tokens start with "nbt_" and use Bearer auth
	authHeader := fmt.Sprintf("Token %s", token)
	if strings.HasPrefix(token, "nbt_") {
		authHeader = fmt.Sprintf("Bearer %s", token)
	}

	cfg := netboxclient.NewConfiguration()
	cfg.Servers = netboxclient.ServerConfigurations{
		{URL: netboxURL},
	}
	cfg.AddDefaultHeader("Authorization", authHeader)

	client := netboxclient.NewAPIClient(cfg)
	ctx := context.Background()

	log.Printf("Seeding Netbox at %s with %d records...", netboxURL, len(testRecords))

	for _, r := range testRecords {
		status := netboxclient.PatchedWritableIPAddressRequestStatus("active")
		req := netboxclient.WritableIPAddressRequest{
			Address: r.Address,
			DnsName: &r.DNSName,
			Status:  &status,
		}

		ip, resp, err := client.IpamAPI.IpamIpAddressesCreate(ctx).
			WritableIPAddressRequest(req).
			Execute()
		if err != nil {
			if resp != nil {
				log.Printf("Warning: failed to create %s (%s): %v (HTTP %d)", r.DNSName, r.Address, err, resp.StatusCode)
			} else {
				log.Printf("Warning: failed to create %s (%s): %v", r.DNSName, r.Address, err)
			}
			continue
		}

		log.Printf("Created IP %d: %s -> %s", ip.GetId(), r.Address, r.DNSName)
	}

	log.Println("Seed complete!")
}

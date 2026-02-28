package zonediscovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// NetboxDNSDiscoverer queries the Netbox DNS plugin API for zone definitions
// and assigns each record to its longest-matching zone.
type NetboxDNSDiscoverer struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewNetboxDNSDiscoverer(baseURL, token string) *NetboxDNSDiscoverer {
	return &NetboxDNSDiscoverer{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type netboxDNSZone struct {
	Name   string `json:"name"`
	Status struct {
		Value string `json:"value"`
	} `json:"status"`
}

type netboxDNSZoneList struct {
	Count   int             `json:"count"`
	Next    *string         `json:"next"`
	Results []netboxDNSZone `json:"results"`
}

// FetchZones retrieves active zones from the Netbox DNS plugin.
// It follows pagination links until all pages have been fetched.
func (d *NetboxDNSDiscoverer) FetchZones(ctx context.Context) ([]string, error) {
	url := d.baseURL + "/api/plugins/netbox-dns/zones/?status=active&limit=1000"

	var zones []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Authorization", netboxclient.AuthHeader(d.token))
		req.Header.Set("Accept", "application/json")

		resp, err := d.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch zones: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("netbox-dns zones API returned %d: %s", resp.StatusCode, string(body))
		}

		var result netboxDNSZoneList
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode zones response: %w", err)
		}
		resp.Body.Close()

		for _, z := range result.Results {
			if z.Name != "" {
				zones = append(zones, z.Name)
			}
		}

		if result.Next != nil {
			url = *result.Next
		} else {
			url = ""
		}
	}
	return zones, nil
}

func (d *NetboxDNSDiscoverer) Discover(records []netboxclient.IPRecord) (ZoneMap, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	zones, err := d.FetchZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch netbox-dns zones: %w", err)
	}

	zm := make(ZoneMap)
	for _, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		zone := longestMatchingZone(name, zones)
		if zone == "" {
			continue // no matching zone
		}
		zm[zone] = append(zm[zone], r)
	}
	return zm, nil
}

// longestMatchingZone finds the longest zone name that is a suffix of the given FQDN.
func longestMatchingZone(fqdn string, zones []string) string {
	fqdn = strings.TrimSuffix(fqdn, ".")
	best := ""
	for _, z := range zones {
		z = strings.TrimSuffix(z, ".")
		if fqdn == z || strings.HasSuffix(fqdn, "."+z) {
			if len(z) > len(best) {
				best = z
			}
		}
	}
	return best
}

// Package netboxclient provides a Netbox IPAM client that uses raw HTTP against the
// stable Netbox REST API (/api/ipam/ip-addresses/). This works with both Netbox 3.x
// and 4.x — the REST API and JSON schema are identical across versions. Only the
// auto-generated Go SDKs (go-netbox/v3 vs go-netbox/v4) are incompatible.
package netboxclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IPRecord is a simplified representation of a Netbox IP address with DNS info.
type IPRecord struct {
	DNSName       string
	Address       string // IP address without CIDR prefix
	Family        int    // 4 or 6
	DeviceName    string // Device name from assigned_object.device.name
	InterfaceName string // Interface name from assigned_object.name
	VRF           string // VRF name
}

// Client queries the Netbox IPAM API with parallel paginated fetching.
type Client struct {
	baseURL        string
	token          string
	httpClient     *http.Client
	pageSize       int
	maxConcurrency int
}

// ipListResponse is the paginated response from /api/ipam/ip-addresses/.
type ipListResponse struct {
	Count   int      `json:"count"`
	Results []ipItem `json:"results"`
}

// ipItem represents a single IP address object in the Netbox API response.
type ipItem struct {
	Address        string          `json:"address"`
	DNSName        string          `json:"dns_name"`
	VRF            *vrfInfo        `json:"vrf"`
	AssignedObject *assignedObject `json:"assigned_object"`
}

type vrfInfo struct {
	Name string `json:"name"`
}

type assignedObject struct {
	Name   string      `json:"name"`
	Device *deviceInfo `json:"device"`
}

type deviceInfo struct {
	Name string `json:"name"`
}

// New creates a new Netbox client.
func New(baseURL, token string, pageSize, maxConcurrency int) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid netbox URL: %w", err)
	}

	return &Client{
		baseURL:        strings.TrimRight(u.String(), "/"),
		token:          token,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		pageSize:       pageSize,
		maxConcurrency: maxConcurrency,
	}, nil
}

// FetchIPAddresses retrieves all active IP addresses from Netbox
// using parallel paginated requests.
func (c *Client) FetchIPAddresses(ctx context.Context) ([]IPRecord, error) {
	// Probe request to get total count
	probeResp, err := c.fetchPage(ctx, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}

	total := probeResp.Count
	if total == 0 {
		return nil, nil
	}

	totalPages := int(math.Ceil(float64(total) / float64(c.pageSize)))

	// Fan-out with bounded concurrency
	sem := make(chan struct{}, c.maxConcurrency)
	resultsCh := make(chan []IPRecord, totalPages)
	errsCh := make(chan error, totalPages)

	var wg sync.WaitGroup
	for page := 0; page < totalPages; page++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := c.fetchPage(ctx, offset, c.pageSize)
			if err != nil {
				errsCh <- fmt.Errorf("fetch page offset=%d: %w", offset, err)
				return
			}

			records := make([]IPRecord, 0, len(resp.Results))
			for _, ip := range resp.Results {
				addr := stripCIDR(ip.Address)
				family := 4
				if strings.Contains(addr, ":") {
					family = 6
				}

				// Extract device and interface info
				deviceName := ""
				interfaceName := ""
				if ip.AssignedObject != nil {
					interfaceName = ip.AssignedObject.Name
					if ip.AssignedObject.Device != nil {
						deviceName = ip.AssignedObject.Device.Name
					}
				}

				// Extract VRF
				vrf := ""
				if ip.VRF != nil {
					vrf = ip.VRF.Name
				}

				records = append(records, IPRecord{
					DNSName:       ip.DNSName,
					Address:       addr,
					Family:        family,
					DeviceName:    deviceName,
					InterfaceName: interfaceName,
					VRF:           vrf,
				})
			}
			resultsCh <- records
		}(page * c.pageSize)
	}

	wg.Wait()
	close(resultsCh)
	close(errsCh)

	// Check for errors
	if err := <-errsCh; err != nil {
		return nil, err
	}

	// Collect results
	var all []IPRecord
	for records := range resultsCh {
		all = append(all, records...)
	}

	return all, nil
}

// fetchPage retrieves a single page of IP addresses from the Netbox API.
func (c *Client) fetchPage(ctx context.Context, offset, limit int) (*ipListResponse, error) {
	reqURL := fmt.Sprintf("%s/api/ipam/ip-addresses/?status=active&limit=%d&offset=%d", c.baseURL, limit, offset)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", AuthHeader(c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("netbox API returned %d: %s", resp.StatusCode, string(body))
	}

	var result ipListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// stripCIDR removes the /prefix from an address like "10.0.0.1/24".
func stripCIDR(addr string) string {
	if idx := strings.Index(addr, "/"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

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
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RecordTypeCNAME marks an IPRecord as a CNAME alias rather than an
// address record. The zero value of Type means A/AAAA (from Family).
const RecordTypeCNAME = "CNAME"

// IPRecord is a simplified representation of a Netbox IP address with DNS info.
type IPRecord struct {
	DNSName       string `json:"dns_name"`
	Address       string `json:"address"` // IP address without CIDR prefix
	Family        int    `json:"family"` // 4 or 6
	DeviceName    string `json:"device_name,omitempty"` // Device name from assigned_object.device.name
	InterfaceName string `json:"interface_name,omitempty"` // Interface name from assigned_object.name
	VRF           string `json:"vrf,omitempty"` // VRF name
	TTL           uint32 `json:"ttl,omitempty"`
	// Type is "" for address records (A/AAAA per Family) or RecordTypeCNAME
	// for aliases. CNAME records leave Address/Family empty and carry the
	// canonical FQDN in CNAMETarget.
	Type        string `json:"type,omitempty"`
	CNAMETarget string `json:"cname_target,omitempty"`
}

// Client queries the Netbox IPAM API with parallel paginated fetching.
type Client struct {
	baseURL        string
	token          string
	httpClient     *http.Client
	pageSize       int
	maxConcurrency int
	retryCount     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	// RetryCounter is an optional Prometheus counter incremented on each retry attempt.
	// Set after construction. Safe to leave nil (no-op).
	RetryCounter prometheus.Counter
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
	Name           string      `json:"name"`
	Device         *deviceInfo `json:"device"`
	VirtualMachine *deviceInfo `json:"virtual_machine"`
}

type deviceInfo struct {
	Name string `json:"name"`
}

// New creates a new Netbox client.
func New(baseURL, token string, pageSize, maxConcurrency, retryCount int, retryBaseDelay, retryMaxDelay time.Duration) (*Client, error) {
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
		retryCount:     retryCount,
		retryBaseDelay: retryBaseDelay,
		retryMaxDelay:  retryMaxDelay,
	}, nil
}

// FetchIPAddresses retrieves all active IP addresses from Netbox,
// retrying on transient errors with exponential backoff.
func (c *Client) FetchIPAddresses(ctx context.Context) ([]IPRecord, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retryCount; attempt++ {
		if attempt > 0 {
			delay := c.retryDelay(attempt)
			slog.Warn("retrying Netbox fetch",
				"attempt", attempt,
				"max_retries", c.retryCount,
				"delay", delay,
				"err", lastErr,
			)
			if c.RetryCounter != nil {
				c.RetryCounter.Inc()
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		records, err := c.fetchIPAddressesOnce(ctx)
		if err == nil {
			return records, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// retryDelay returns the backoff duration for the given attempt number (1-based).
// Uses exponential backoff with ±25% jitter, capped at retryMaxDelay.
func (c *Client) retryDelay(attempt int) time.Duration {
	// Cap the shift to prevent int64 overflow for large attempt counts.
	shift := attempt - 1
	if shift > 62 {
		shift = 62
	}
	exp := c.retryBaseDelay * time.Duration(1<<uint(shift))
	if exp > c.retryMaxDelay {
		exp = c.retryMaxDelay
	}
	// ±25% jitter — guard against zero to avoid rand.Int63n(0) panic
	if int64(exp/2) > 0 {
		jitter := time.Duration(rand.Int63n(int64(exp/2))) - exp/4
		exp += jitter
	}
	if exp < 0 {
		exp = 0
	}
	return exp
}

// RecordFromJSON parses a single Netbox IP address object into an IPRecord.
// It accepts the same JSON shape as one element of /api/ipam/ip-addresses/'s
// "results" array, which is also the shape of a Netbox webhook's "data" field
// for an ipam.ipaddress event (verified against a Netbox v4.6.0 webhook capture).
// Fields present in a webhook payload but not read here (e.g. "id", "family",
// "status") are intentionally ignored — family is derived from the address
// string, exactly as fetchIPAddressesOnce already does.
func RecordFromJSON(raw []byte) (IPRecord, error) {
	var item ipItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return IPRecord{}, fmt.Errorf("decode ip address object: %w", err)
	}
	return ipItemToRecord(item), nil
}

// ipItemToRecord maps a decoded Netbox IP address object to an IPRecord.
func ipItemToRecord(ip ipItem) IPRecord {
	addr := stripCIDR(ip.Address)
	family := 4
	if strings.Contains(addr, ":") {
		family = 6
	}

	deviceName := ""
	interfaceName := ""
	if ip.AssignedObject != nil {
		interfaceName = ip.AssignedObject.Name
		if ip.AssignedObject.Device != nil {
			deviceName = ip.AssignedObject.Device.Name
		} else if ip.AssignedObject.VirtualMachine != nil {
			deviceName = ip.AssignedObject.VirtualMachine.Name
		}
	}

	vrf := ""
	if ip.VRF != nil {
		vrf = ip.VRF.Name
	}

	return IPRecord{
		DNSName:       ip.DNSName,
		Address:       addr,
		Family:        family,
		DeviceName:    deviceName,
		InterfaceName: interfaceName,
		VRF:           vrf,
	}
}

// fetchIPAddressesOnce retrieves all active IP addresses from Netbox
// using parallel paginated requests (single attempt, no retry).
func (c *Client) fetchIPAddressesOnce(ctx context.Context) ([]IPRecord, error) {
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
	// Use indexed results to ensure deterministic ordering
	sem := make(chan struct{}, c.maxConcurrency)
	results := make([][]IPRecord, totalPages)
	errsCh := make(chan error, totalPages)

	var wg sync.WaitGroup
	for page := 0; page < totalPages; page++ {
		wg.Add(1)
		go func(pageNum int, offset int) {
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
				records = append(records, ipItemToRecord(ip))
			}
			results[pageNum] = records
		}(page, page*c.pageSize)
	}

	wg.Wait()
	close(errsCh)

	// Check for errors
	if err := <-errsCh; err != nil {
		return nil, err
	}

	// Collect results in deterministic page order
	var all []IPRecord
	for _, records := range results {
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
	defer func() { _ = resp.Body.Close() }()

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

// Package netboxclient provides a Netbox IPAM client that uses raw HTTP against the
// stable Netbox REST API (/api/ipam/ip-addresses/). This works with both Netbox 3.x
// and 4.x — the REST API and JSON schema are identical across versions. Only the
// auto-generated Go SDKs (go-netbox/v3 vs go-netbox/v4) are incompatible.
package netboxclient

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RecordTypeCNAME marks an IPRecord as a CNAME alias rather than an
// address record. The zero value of Type means A/AAAA (from Family).
const RecordTypeCNAME = "CNAME"

// SourceWebhook marks an IPRecord as written by the Netbox webhook trigger
// (internal/netboxwebhook), as opposed to the zero value "" which covers
// both Netbox-fetched records and records added manually via the
// DynamicZoneService gRPC API. Only dynamicstore-held records ever set this;
// it is never present on Netbox-fetched records.
const SourceWebhook = "webhook"

// Kind classifies how a record was generated, used by reverse-zone PTR
// selection. The zero value "" means explicit: the dns_name came from an
// operator (NetBox dns_name field, dynamicstore, webhook) and wins PTR
// priority over all generated kinds.
const (
	KindDevice    = "device"
	KindBMC       = "bmc"
	KindInterface = "interface"
)

// IPRecord is a simplified representation of a Netbox IP address with DNS info.
type IPRecord struct {
	DNSName       string `json:"dns_name"`
	Address       string `json:"address"`                  // IP address without CIDR prefix
	Family        int    `json:"family"`                   // 4 or 6
	DeviceName    string `json:"device_name,omitempty"`    // Device name from assigned_object.device.name
	InterfaceName string `json:"interface_name,omitempty"` // Interface name from assigned_object.name
	VRF           string `json:"vrf,omitempty"`            // VRF name
	TTL           uint32 `json:"ttl,omitempty"`
	// Type is "" for address records (A/AAAA per Family) or RecordTypeCNAME
	// for aliases. CNAME records leave Address/Family empty and carry the
	// canonical FQDN in CNAMETarget.
	Type        string `json:"type,omitempty"`
	CNAMETarget string `json:"cname_target,omitempty"`
	// Source and AppliedAt are set only on dynamicstore-held records written
	// by the Netbox webhook trigger (see internal/netboxwebhook). They are
	// never read by zone generation or hashing (internal/zonegen) — only by
	// the webhook handler's ordering guard and the periodic-poll
	// reconciliation in cmd/sidecar/main.go.
	Source    string    `json:"source,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitempty"`
	// ID is the NetBox IP object id (0 when unknown), the join key against
	// DeviceRecord.PrimaryIP4ID / OOBIPID.
	ID int `json:"id,omitempty"`
	// Kind is "" for explicit/operator-named records, or one of the Kind*
	// constants for generated ones. Reverse-zone PTR selection keys on it.
	Kind string `json:"kind,omitempty"`
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
	// publishStatuses are the NetBox IP statuses requested from the API
	// (one status= query param each — NetBox ORs repeated filters). This is
	// the poll path's half of the publish-status policy; defaults to active
	// only. See SetPublishStatuses.
	publishStatuses []string
	// RetryCounter is an optional Prometheus counter incremented on each retry attempt.
	// Set after construction. Safe to leave nil (no-op).
	RetryCounter prometheus.Counter
	// Caps on how much of a response body is read (see fetch.go).
	maxErrorBodyBytes int64
	maxPageBodyBytes  int64
	// sem bounds requests in flight across every fetch on this client, so
	// fetches running concurrently never put more than maxConcurrency
	// requests on NetBox at once.
	sem chan struct{}
}

// ipListResponse is the paginated response from /api/ipam/ip-addresses/.
type ipListResponse struct {
	Count   int      `json:"count"`
	Results []ipItem `json:"results"`
}

// ipItem represents a single IP address object in the Netbox API response.
type ipItem struct {
	ID             int             `json:"id"`
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
		baseURL:         strings.TrimRight(u.String(), "/"),
		token:           token,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		pageSize:        pageSize,
		maxConcurrency:  maxConcurrency,
		retryCount:      retryCount,
		retryBaseDelay:  retryBaseDelay,
		retryMaxDelay:   retryMaxDelay,
		publishStatuses: []string{"active"},

		maxErrorBodyBytes: DefaultMaxErrorBodyBytes,
		maxPageBodyBytes:  DefaultMaxPageBodyBytes,
		sem:               make(chan struct{}, max(maxConcurrency, 1)),
	}, nil
}

// SetPublishStatuses replaces the set of NetBox IP statuses the fetch
// requests (config.ParsePublishStatuses validates the values). Set once
// after construction, before the first fetch; an empty slice is ignored so
// the active-only default can never be accidentally cleared.
func (c *Client) SetPublishStatuses(statuses []string) {
	if len(statuses) == 0 {
		return
	}
	c.publishStatuses = statuses
}

// FetchIPAddresses retrieves all active IP addresses from Netbox,
// retrying on transient errors with exponential backoff.
func (c *Client) FetchIPAddresses(ctx context.Context) ([]IPRecord, error) {
	return withRetry(ctx, c, "Netbox fetch", c.fetchIPAddressesOnce)
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
		ID:            ip.ID,
	}
}

// fetchIPAddressesOnce retrieves all published IP addresses from Netbox
// using parallel paginated requests (single attempt, no retry).
func (c *Client) fetchIPAddressesOnce(ctx context.Context) ([]IPRecord, error) {
	return fetchAllPages(ctx, c, c.fetchIPPage)
}

// fetchIPPage retrieves a single page of IP addresses from the Netbox API.
// Status filtering is server-side: one status= param per published status
// (NetBox ORs repeated filters). This query is the only thing keeping
// non-published IPs (reserved/deprecated/...) out of the zones on the poll
// path — pinned by N1 in the acceptance matrix.
func (c *Client) fetchIPPage(ctx context.Context, offset, limit int) (int, []IPRecord, error) {
	q := url.Values{}
	for _, st := range c.publishStatuses {
		q.Add("status", st)
	}
	q.Set("limit", fmt.Sprint(limit))
	q.Set("offset", fmt.Sprint(offset))

	var resp ipListResponse
	if err := c.getPage(ctx, ipAddressesPath, q, &resp); err != nil {
		return 0, nil, err
	}
	records := make([]IPRecord, 0, len(resp.Results))
	for _, ip := range resp.Results {
		records = append(records, ipItemToRecord(ip))
	}
	return resp.Count, records, nil
}

// stripCIDR removes the /prefix from an address like "10.0.0.1/24".
func stripCIDR(addr string) string {
	if idx := strings.Index(addr, "/"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

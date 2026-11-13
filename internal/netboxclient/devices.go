package netboxclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DeviceRecord is the slice of a NetBox device (or virtual machine) that
// device-record generation needs: the name and the ids of its primary and
// out-of-band IPs. Status is informational — nothing filters on it, so a
// racked-but-reachable device still resolves (spec §1).
type DeviceRecord struct {
	Name         string
	Status       string
	PrimaryIP4ID int // 0 = unset
	OOBIPID      int // 0 = unset; devices only, VMs have no oob_ip
}

type deviceListResponse struct {
	Count   int          `json:"count"`
	Results []deviceItem `json:"results"`
}

type deviceItem struct {
	Name      string     `json:"name"`
	Status    *valueInfo `json:"status"`
	PrimaryIP *idInfo    `json:"primary_ip4"`
	OOBIP     *idInfo    `json:"oob_ip"`
}

type valueInfo struct {
	Value string `json:"value"`
}

type idInfo struct {
	ID int `json:"id"`
}

func (d deviceItem) toRecord() DeviceRecord {
	rec := DeviceRecord{Name: d.Name}
	if d.Status != nil {
		rec.Status = d.Status.Value
	}
	if d.PrimaryIP != nil {
		rec.PrimaryIP4ID = d.PrimaryIP.ID
	}
	if d.OOBIP != nil {
		rec.OOBIPID = d.OOBIP.ID
	}
	return rec
}

// FetchDevices retrieves all devices and virtual machines from NetBox with
// the same retry/backoff semantics as FetchIPAddresses. No status filter is
// applied — device status must never gate DNS (the IP publish-status policy
// on the ip-addresses fetch is the only record gate).
func (c *Client) FetchDevices(ctx context.Context) ([]DeviceRecord, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retryCount; attempt++ {
		if attempt > 0 {
			delay := c.retryDelay(attempt)
			slog.Warn("retrying Netbox device fetch",
				"attempt", attempt, "max_retries", c.retryCount, "delay", delay, "err", lastErr)
			if c.RetryCounter != nil {
				c.RetryCounter.Inc()
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		devices, err := c.fetchDevicesOnce(ctx)
		if err == nil {
			return devices, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) fetchDevicesOnce(ctx context.Context) ([]DeviceRecord, error) {
	devices, err := c.fetchAllDevicePages(ctx, "/api/dcim/devices/")
	if err != nil {
		return nil, fmt.Errorf("fetch devices: %w", err)
	}
	vms, err := c.fetchAllDevicePages(ctx, "/api/virtualization/virtual-machines/")
	if err != nil {
		return nil, fmt.Errorf("fetch virtual machines: %w", err)
	}
	return append(devices, vms...), nil
}

// fetchAllDevicePages mirrors fetchIPAddressesOnce: a probe for the count,
// then bounded-concurrency page fan-out with deterministic page order.
func (c *Client) fetchAllDevicePages(ctx context.Context, apiPath string) ([]DeviceRecord, error) {
	probe, err := c.fetchDevicePage(ctx, apiPath, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	total := probe.Count
	if total == 0 {
		return nil, nil
	}
	totalPages := int(math.Ceil(float64(total) / float64(c.pageSize)))

	sem := make(chan struct{}, c.maxConcurrency)
	results := make([][]DeviceRecord, totalPages)
	errsCh := make(chan error, totalPages)

	var wg sync.WaitGroup
	for page := 0; page < totalPages; page++ {
		wg.Add(1)
		go func(pageNum, offset int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := c.fetchDevicePage(ctx, apiPath, offset, c.pageSize)
			if err != nil {
				errsCh <- fmt.Errorf("fetch page offset=%d: %w", offset, err)
				return
			}
			records := make([]DeviceRecord, 0, len(resp.Results))
			for _, d := range resp.Results {
				records = append(records, d.toRecord())
			}
			results[pageNum] = records
		}(page, page*c.pageSize)
	}
	wg.Wait()
	close(errsCh)

	if err := <-errsCh; err != nil {
		return nil, err
	}

	var all []DeviceRecord
	for _, records := range results {
		all = append(all, records...)
	}
	return all, nil
}

func (c *Client) fetchDevicePage(ctx context.Context, apiPath string, offset, limit int) (*deviceListResponse, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(limit))
	q.Set("offset", fmt.Sprint(offset))
	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, apiPath, q.Encode())

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

	var result deviceListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

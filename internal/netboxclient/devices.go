package netboxclient

import (
	"context"
	"fmt"
	"net/url"
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
	return withRetry(ctx, c, "Netbox device fetch", c.fetchDevicesOnce)
}

func (c *Client) fetchDevicesOnce(ctx context.Context) ([]DeviceRecord, error) {
	devices, err := fetchAllPages(ctx, c, c.devicePager(devicesPath))
	if err != nil {
		return nil, fmt.Errorf("fetch devices: %w", err)
	}
	vms, err := fetchAllPages(ctx, c, c.devicePager(virtualMachinesPath))
	if err != nil {
		return nil, fmt.Errorf("fetch virtual machines: %w", err)
	}
	return append(devices, vms...), nil
}

// devicePager returns the page fetcher for a device-shaped list endpoint
// (dcim devices or virtualization virtual machines).
func (c *Client) devicePager(apiPath string) pageFunc[DeviceRecord] {
	return func(ctx context.Context, offset, limit int) (int, []DeviceRecord, error) {
		q := url.Values{}
		q.Set("limit", fmt.Sprint(limit))
		q.Set("offset", fmt.Sprint(offset))

		var resp deviceListResponse
		if err := c.getPage(ctx, apiPath, q, &resp); err != nil {
			return 0, nil, err
		}
		records := make([]DeviceRecord, 0, len(resp.Results))
		for _, d := range resp.Results {
			records = append(records, d.toRecord())
		}
		return resp.Count, records, nil
	}
}

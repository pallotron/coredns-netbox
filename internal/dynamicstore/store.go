package dynamicstore

import "github.com/pallotron/coredns-netbox/internal/netboxclient"

// DynamicStore persists and retrieves dynamically provisioned DNS zones and records.
type DynamicStore interface {
	CreateZone(zone string) error
	DeleteZone(zone string) error
	ListZones() []string
	GetRecords(zone string) []netboxclient.IPRecord
	UpsertRecords(zone string, records []netboxclient.IPRecord) error
	DeleteRecords(zone string, names []string) error
	BatchUpsert(zoneRecords map[string][]netboxclient.IPRecord) error
	BatchDelete(zone string, names []string) error
}

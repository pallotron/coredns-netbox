package dynamicstore

import (
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

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
	// ReconcileWebhookSourced removes records with Source == netboxclient.SourceWebhook
	// whose AppliedAt predates cutoff. Called after a full Netbox poll completes:
	// the fresh fetch has already proven to include (or supersede) any such
	// record, so the webhook overlay entry is now redundant. Records with
	// Source == "" (manually added, or never touched by the webhook) are
	// never affected.
	ReconcileWebhookSourced(cutoff time.Time) error
}

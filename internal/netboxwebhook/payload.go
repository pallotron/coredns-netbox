package netboxwebhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// snapshot is the subset of Netbox's snapshots.prechange/postchange objects
// this package reads. Unlike the top-level "data" object, snapshot fields are
// flat (e.g. assigned_object_id, not a nested assigned_object) — confirmed
// against a real Netbox v4.6.0 capture — but only dns_name is needed here.
type snapshot struct {
	DNSName string `json:"dns_name"`
}

// payload is a Netbox event-rule webhook delivery for an ipam.ipaddress
// create/update/delete event.
type payload struct {
	Event      string          `json:"event"` // "created", "updated", "deleted"
	Timestamp  time.Time       `json:"timestamp"`
	ObjectType string          `json:"object_type"`
	Data       json.RawMessage `json:"data"` // null on "deleted" events
	Snapshots  struct {
		PreChange *snapshot `json:"prechange"`
	} `json:"snapshots"`
}

func parsePayload(body []byte) (payload, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return payload{}, fmt.Errorf("decode webhook payload: %w", err)
	}
	return p, nil
}

// recordFromPayload builds an IPRecord from a created/updated payload's data
// field. Callers must not call this for "deleted" events (data is null).
func recordFromPayload(p payload) (netboxclient.IPRecord, error) {
	rec, err := netboxclient.RecordFromJSON(p.Data)
	if err != nil {
		return netboxclient.IPRecord{}, err
	}
	rec.Source = netboxclient.SourceWebhook
	rec.AppliedAt = p.Timestamp
	return rec, nil
}

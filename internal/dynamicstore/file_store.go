package dynamicstore

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// storeData is the on-disk JSON structure.
type storeData struct {
	Zones map[string][]netboxclient.IPRecord `json:"zones"`
}

// FileStore is a thread-safe, file-backed implementation of DynamicStore.
// All mutations are persisted atomically via a temp-file rename.
type FileStore struct {
	mu   sync.RWMutex
	path string
	data storeData
}

// NewFileStore opens (or creates) the JSON file at path and loads its contents.
// A missing or corrupt file is not fatal — the store starts empty.
func NewFileStore(path string) (*FileStore, error) {
	fs := &FileStore{
		path: path,
		data: storeData{Zones: make(map[string][]netboxclient.IPRecord)},
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fs, nil
		}
		slog.Warn("dynamicstore: cannot read store file, starting empty", "path", path, "err", err)
		return fs, nil
	}

	var loaded storeData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		slog.Warn("dynamicstore: corrupt store file, starting empty", "path", path, "err", err)
		return fs, nil
	}
	if loaded.Zones == nil {
		loaded.Zones = make(map[string][]netboxclient.IPRecord)
	}
	fs.data = loaded
	return fs, nil
}

// CreateZone adds zone to the store if it does not already exist.
func (f *FileStore) CreateZone(zone string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data.Zones[zone]; !ok {
		f.data.Zones[zone] = []netboxclient.IPRecord{}
	}
	return f.persist()
}

// DeleteZone removes zone and all its records from the store.
func (f *FileStore) DeleteZone(zone string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data.Zones, zone)
	return f.persist()
}

// ListZones returns a snapshot of all zone names.
func (f *FileStore) ListZones() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	zones := make([]string, 0, len(f.data.Zones))
	for z := range f.data.Zones {
		zones = append(zones, z)
	}
	return zones
}

// GetRecords returns a copy of the records for zone.
// Returns an empty slice if the zone does not exist.
func (f *FileStore) GetRecords(zone string) []netboxclient.IPRecord {
	f.mu.RLock()
	defer f.mu.RUnlock()
	src := f.data.Zones[zone]
	out := make([]netboxclient.IPRecord, len(src))
	copy(out, src)
	return out
}

// UpsertRecords inserts or updates records in zone (keyed by DNSName).
// If zone does not exist it is created automatically.
func (f *FileStore) UpsertRecords(zone string, records []netboxclient.IPRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsert(zone, records)
	return f.persist()
}

// DeleteRecords removes records identified by name from zone.
func (f *FileStore) DeleteRecords(zone string, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteByNames(zone, names)
	return f.persist()
}

// BatchUpsert performs UpsertRecords for each zone in the map atomically.
func (f *FileStore) BatchUpsert(zoneRecords map[string][]netboxclient.IPRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for zone, records := range zoneRecords {
		f.upsert(zone, records)
	}
	return f.persist()
}

// BatchDelete removes records identified by names from zone atomically.
func (f *FileStore) BatchDelete(zone string, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteByNames(zone, names)
	return f.persist()
}

// ReconcileWebhookSourced removes records with Source == netboxclient.SourceWebhook
// whose AppliedAt is before cutoff, across all zones. Caller-visible zones with
// no matching records are left untouched (no-op, no persist).
func (f *FileStore) ReconcileWebhookSourced(cutoff time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	changed := false
	for zone, recs := range f.data.Zones {
		out := recs[:0:0]
		for _, r := range recs {
			if r.Source == netboxclient.SourceWebhook && r.AppliedAt.Before(cutoff) {
				changed = true
				continue
			}
			out = append(out, r)
		}
		f.data.Zones[zone] = out
	}

	if !changed {
		return nil
	}
	return f.persist()
}

// upsert merges records into zone by DNSName. Caller must hold f.mu.
func (f *FileStore) upsert(zone string, records []netboxclient.IPRecord) {
	existing := f.data.Zones[zone]
	// Build index by DNSName for O(n) merge.
	idx := make(map[string]int, len(existing))
	for i, r := range existing {
		idx[r.DNSName] = i
	}
	for _, r := range records {
		if i, ok := idx[r.DNSName]; ok {
			existing[i] = r
		} else {
			idx[r.DNSName] = len(existing)
			existing = append(existing, r)
		}
	}
	f.data.Zones[zone] = existing
}

// deleteByNames removes records with matching DNSName from zone. Caller must hold f.mu.
func (f *FileStore) deleteByNames(zone string, names []string) {
	remove := make(map[string]struct{}, len(names))
	for _, n := range names {
		remove[n] = struct{}{}
	}
	src := f.data.Zones[zone]
	out := make([]netboxclient.IPRecord, 0, len(src))
	for _, r := range src {
		if _, skip := remove[r.DNSName]; !skip {
			out = append(out, r)
		}
	}
	f.data.Zones[zone] = out
}

// persist atomically writes f.data to disk. Caller must hold f.mu (write).
func (f *FileStore) persist() error {
	raw, err := json.MarshalIndent(f.data, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".dynamic-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, f.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

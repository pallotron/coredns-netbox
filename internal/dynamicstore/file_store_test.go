package dynamicstore_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *dynamicstore.FileStore {
	t.Helper()
	s, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)
	return s
}

func TestCreateAndListZones(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.CreateZone("example.org"))
	require.NoError(t, s.CreateZone("k8s.example.org"))
	assert.ElementsMatch(t, []string{"example.org", "k8s.example.org"}, s.ListZones())
}

func TestUpsertAndGetRecords(t *testing.T) {
	s := newStore(t)
	records := []netboxclient.IPRecord{
		{DNSName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4, TTL: 60},
	}
	require.NoError(t, s.UpsertRecords("k8s.example.org", records))
	got := s.GetRecords("k8s.example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "node1.k8s.example.org", got[0].DNSName)
	assert.Equal(t, uint32(60), got[0].TTL)
}

func TestUpsertRecords_Idempotent(t *testing.T) {
	s := newStore(t)
	r := netboxclient.IPRecord{DNSName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4}
	require.NoError(t, s.UpsertRecords("k8s.example.org", []netboxclient.IPRecord{r}))
	r.Address = "10.0.0.2"
	require.NoError(t, s.UpsertRecords("k8s.example.org", []netboxclient.IPRecord{r}))
	got := s.GetRecords("k8s.example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "10.0.0.2", got[0].Address)
}

func TestDeleteRecords(t *testing.T) {
	s := newStore(t)
	records := []netboxclient.IPRecord{
		{DNSName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "node2.k8s.example.org", Address: "10.0.0.2", Family: 4},
	}
	require.NoError(t, s.UpsertRecords("k8s.example.org", records))
	require.NoError(t, s.DeleteRecords("k8s.example.org", []string{"node1.k8s.example.org"}))
	got := s.GetRecords("k8s.example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "node2.k8s.example.org", got[0].DNSName)
}

func TestDeleteZone(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.CreateZone("example.org"))
	require.NoError(t, s.DeleteZone("example.org"))
	assert.Empty(t, s.ListZones())
}

func TestBatchUpsert(t *testing.T) {
	s := newStore(t)
	batch := map[string][]netboxclient.IPRecord{
		"a.example.org": {{DNSName: "h1.a.example.org", Address: "10.0.1.1", Family: 4}},
		"b.example.org": {{DNSName: "h1.b.example.org", Address: "10.0.2.1", Family: 4}},
	}
	require.NoError(t, s.BatchUpsert(batch))
	assert.Len(t, s.GetRecords("a.example.org"), 1)
	assert.Len(t, s.GetRecords("b.example.org"), 1)
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dynamic.json")

	s1, err := dynamicstore.NewFileStore(path)
	require.NoError(t, err)
	require.NoError(t, s1.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
	}))

	// reload from disk
	s2, err := dynamicstore.NewFileStore(path)
	require.NoError(t, err)
	got := s2.GetRecords("example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "host1.example.org", got[0].DNSName)
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dynamic.json")
	require.NoError(t, os.WriteFile(path, []byte("not-json{{{"), 0o644))
	s, err := dynamicstore.NewFileStore(path)
	require.NoError(t, err) // corrupt file is not fatal
	assert.Empty(t, s.ListZones())
}

func TestBatchDelete(t *testing.T) {
	s := newStore(t)
	records := []netboxclient.IPRecord{
		{DNSName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "node2.k8s.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "node3.k8s.example.org", Address: "10.0.0.3", Family: 4},
	}
	require.NoError(t, s.UpsertRecords("k8s.example.org", records))
	require.NoError(t, s.BatchDelete("k8s.example.org", []string{"node1.k8s.example.org", "node3.k8s.example.org"}))
	got := s.GetRecords("k8s.example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "node2.k8s.example.org", got[0].DNSName)
}

func TestReconcileWebhookSourced_DropsOnlyStaleWebhookRecords(t *testing.T) {
	s := newStore(t)
	cutoff := time.Now()

	require.NoError(t, s.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "stale-webhook.example.org", Address: "10.0.0.1", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: cutoff.Add(-time.Minute)},
		{DNSName: "fresh-webhook.example.org", Address: "10.0.0.2", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: cutoff.Add(time.Minute)},
		{DNSName: "manual.example.org", Address: "10.0.0.3", Family: 4},
	}))

	require.NoError(t, s.ReconcileWebhookSourced(cutoff))

	got := s.GetRecords("example.org")
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.DNSName)
	}
	assert.ElementsMatch(t, []string{"fresh-webhook.example.org", "manual.example.org"}, names)
}

func TestReconcileWebhookSourced_NoOpWhenNothingStale(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "manual.example.org", Address: "10.0.0.1", Family: 4},
	}))
	require.NoError(t, s.ReconcileWebhookSourced(time.Now()))
	assert.Len(t, s.GetRecords("example.org"), 1)
}

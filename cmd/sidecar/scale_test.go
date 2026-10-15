package main

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonegen"
)

// N26 (acceptance-test matrix): large-scale headroom for the record
// pipeline — enrichment (device categorization + name templating), zone
// discovery, and zone generation.
//
// Why a pure-Go guard rather than a scaled-up cluster run: a deployment's
// own metrics answer "does this scale fit the budget?" directly (see
// docs/scale.md), and timing assertions on a shared CI runner measure the
// runner more than the code. What a cluster test cannot give
// cheaply, and what this does give, is a deterministic tripwire for an
// algorithmic regression (an accidental O(n²) over devices or zone
// records): such a change blows past the ceiling by orders of magnitude,
// while ordinary runner noise does not come close to it.
//
// Shape matters more than volume here. Where DNS records come mostly from
// DEVICES, the expensive path runs: SelectDeviceIPs compares all of a
// device's interfaces, the formatter renders a canonical plus aliases per
// device, and CNAME collision resolution runs across the whole zone. The
// e2e seed has the opposite shape (18k dns_name records, 2 devices) and
// barely touches any of it, so this corpus is deliberately
// device-dominated.
const (
	scaleDevices        = 25_000
	scaleDNSNameRecords = 1_000
	scaleDomain         = "mycompany.com"
)

// scaleCeiling is deliberately generous: the pipeline runs in a few
// seconds, and the point is to catch a complexity-class change, not to
// police seconds. Override with SCALE_CEILING for local profiling.
func scaleCeiling(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("SCALE_CEILING"); v != "" {
		d, err := time.ParseDuration(v)
		require.NoError(t, err, "invalid SCALE_CEILING")
		return d
	}
	return 90 * time.Second
}

// buildScaleCorpus returns a device-dominated record set: two interfaces
// (mgmt + BMC) per device with no dns_name, plus a slice of records that
// carry an explicit dns_name. Device names match the parser in
// dev/coredns-netbox-values.yaml so templating and alias generation run
// for real rather than falling through to legacy naming.
func buildScaleCorpus() []netboxclient.IPRecord {
	records := make([]netboxclient.IPRecord, 0, scaleDevices*2+scaleDNSNameRecords)
	for i := range scaleDevices {
		dc := fmt.Sprintf("dc%d", i%3+1)
		hall := fmt.Sprintf("h%d%s", i%9+1, string(rune('a'+i%4)))
		device := fmt.Sprintf("%s-%s-r%d-prod-hv-%d", dc, hall, i%400+1, i)
		records = append(records,
			netboxclient.IPRecord{
				Address:       fmt.Sprintf("10.%d.%d.%d", 20+i/65024, (i/254)%254, i%254+1),
				Family:        4,
				DeviceName:    device,
				InterfaceName: "mgmt0",
			},
			netboxclient.IPRecord{
				Address:       fmt.Sprintf("10.%d.%d.%d", 120+i/65024, (i/254)%254, i%254+1),
				Family:        4,
				DeviceName:    device,
				InterfaceName: "bmc0",
			},
		)
	}
	for i := range scaleDNSNameRecords {
		records = append(records, netboxclient.IPRecord{
			DNSName: fmt.Sprintf("host%d.%s", i, scaleDomain),
			Address: fmt.Sprintf("10.250.%d.%d", i/254, i%254+1),
			Family:  4,
		})
	}
	return records
}

func scalePipelineDeps(t testing.TB) (*ipcategorizer.Categorizer, *nameformat.Formatter, zonediscovery.Discoverer, zonediscovery.Discoverer) {
	t.Helper()
	categorizer, err := ipcategorizer.NewCategorizer(
		"(?i)bmc|ipmi|ilo|idrac",
		"^lo$|^lo0|^Loopback",
		"(?i)storage|vtep|vsan",
		"(?i)mgmt|oob",
		"(?i)mgmt|Management|fxp0|eth[01]|mgt|NET|^ens[0-9]|^eno[0-9]|^nic[0-9]",
		scaleDomain,
	)
	require.NoError(t, err)

	formatter, err := nameformat.New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<hall>[a-z]+[0-9][a-z0-9]*)-r(?P<rack>[0-9]+)-(?P<env>prod|mgmt|staging)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$`},
		`{{.name}}.{{template "zone" .}}`,
		[]string{`{{.role}}{{.idx}}-{{.dc}}-{{.hall}}-r{{.rack}}.{{template "zone" .}}`},
		`{{.domain}}`,
		scaleDomain,
	)
	require.NoError(t, err)
	require.NotNil(t, formatter, "name templating must be active for this corpus")

	forwardDisc, err := zonediscovery.NewDiscoverer(zonediscovery.DiscoveryMode("zone-depth"),
		map[string]string{"depth": strconv.Itoa(2)})
	require.NoError(t, err)

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	return categorizer, formatter, forwardDisc, reverseDisc
}

// runScalePipeline performs the per-poll work that scales with NetBox size
// and returns the number of records emitted into zone files.
func runScalePipeline(t testing.TB, records []netboxclient.IPRecord,
	categorizer *ipcategorizer.Categorizer, formatter *nameformat.Formatter,
	forwardDisc, reverseDisc zonediscovery.Discoverer,
) int {
	t.Helper()
	enriched := enrichRecordsWithDeviceNames(records, categorizer, scaleDomain, formatter)

	forwardZones, err := forwardDisc.Discover(enriched)
	require.NoError(t, err)
	reverseZones, err := reverseDisc.Discover(enriched)
	require.NoError(t, err)

	emitted := 0
	for zones, zoneType := range map[*zonediscovery.ZoneMap]zonegen.ZoneType{
		&forwardZones: zonegen.ZoneTypeForward,
		&reverseZones: zonegen.ZoneTypeReverse,
	} {
		for zone, zoneRecords := range *zones {
			g := zonegen.NewGenerator(zonegen.ZoneConfig{
				Origin:     zone,
				PrimaryNS:  "ns1." + scaleDomain + ".",
				AdminEmail: "admin." + scaleDomain + ".",
				TTL:        300,
				Type:       zoneType,
			}, "")
			_, _, count, err := g.Generate(zoneRecords)
			require.NoError(t, err)
			emitted += count
		}
	}
	return emitted
}

func TestScale_DeviceDominatedPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("scale guard skipped in -short mode")
	}
	categorizer, formatter, forwardDisc, reverseDisc := scalePipelineDeps(t)

	buildStart := time.Now()
	records := buildScaleCorpus()
	t.Logf("N26 corpus: %d records (%d devices × 2 interfaces + %d dns_name) built in %s",
		len(records), scaleDevices, scaleDNSNameRecords, time.Since(buildStart).Round(time.Millisecond))

	start := time.Now()
	emitted := runScalePipeline(t, records, categorizer, formatter, forwardDisc, reverseDisc)
	elapsed := time.Since(start)

	t.Logf("N26 pipeline (enrich + discover + generate): %s for %d emitted records",
		elapsed.Round(time.Millisecond), emitted)
	require.Positive(t, emitted, "pipeline must emit records")
	require.Less(t, elapsed, scaleCeiling(t),
		"device-dominated pipeline must stay far inside the poll interval; a breach this large means a complexity regression, not slow hardware")
}

// BenchmarkScaleDeviceDominatedPipeline is for profiling the device path
// (go test -bench . -run '^$' ./cmd/sidecar/ -cpuprofile cpu.out).
func BenchmarkScaleDeviceDominatedPipeline(b *testing.B) {
	categorizer, formatter, forwardDisc, reverseDisc := scalePipelineDeps(b)
	records := buildScaleCorpus()
	b.ResetTimer()
	for range b.N {
		runScalePipeline(b, records, categorizer, formatter, forwardDisc, reverseDisc)
	}
}

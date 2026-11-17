//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sidecarMetricsURL() string {
	if v := os.Getenv("SIDECAR_METRICS_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:18082/metrics"
}

// fetchMetricsErr retrieves the /metrics page, returning errors instead of
// failing the test — safe to call from retry loops and goroutines.
func fetchMetricsErr() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(sidecarMetricsURL())
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", sidecarMetricsURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", sidecarMetricsURL(), resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}

// fetchMetrics retrieves the /metrics page and returns the body as a string.
func fetchMetrics(t *testing.T) string {
	t.Helper()
	body, err := fetchMetricsErr()
	require.NoError(t, err)
	return body
}

// TestMetricsEndpointReachable verifies that the /metrics endpoint returns HTTP 200.
func TestMetricsEndpointReachable(t *testing.T) {
	fetchMetrics(t) // fatals on any error
}

// TestMetricsContainExpectedSeries verifies that the netbox_sidecar_* metric
// families a dashboard or alert depends on appear in the scrape output.
func TestMetricsContainExpectedSeries(t *testing.T) {
	body := fetchMetrics(t)

	want := []string{
		// The shrink guard's series exist from startup (the refusal counter
		// is pre-created for every scope), so an alert reads 0 rather than
		// a missing series until something goes wrong.
		"netbox_sidecar_shrink_guard_blocked",
		"netbox_sidecar_shrink_guard_refusals_total",
		"netbox_sidecar_shrink_guard_overrides_total",
		"netbox_sidecar_poll_total",
		"netbox_sidecar_poll_duration_seconds",
		"netbox_sidecar_last_successful_poll_timestamp_seconds",
		"netbox_sidecar_netbox_fetch_duration_seconds",
		"netbox_sidecar_netbox_records_fetched",
		"netbox_sidecar_netbox_empty_response_total",
		"netbox_sidecar_zones_active",
		"netbox_sidecar_zone_records",
		"netbox_sidecar_zone_writes_total",
		"netbox_sidecar_zone_write_errors_total",
		"netbox_sidecar_netbox_fetch_retries_total",
		"netbox_sidecar_zone_staleness_seconds",
	}

	for _, name := range want {
		assert.Contains(t, body, name, "metric %q not found in /metrics output", name)
	}
}

// TestMetricsSuccessfulPoll verifies that at least one successful poll has been
// recorded (i.e., the sidecar has completed its first sync with Netbox).
func TestMetricsSuccessfulPoll(t *testing.T) {
	body := fetchMetrics(t)

	// The text exposition format includes lines like:
	//   netbox_sidecar_poll_total{result="success"} 1
	assert.Contains(t, body, `netbox_sidecar_poll_total{result="success"}`,
		`expected netbox_sidecar_poll_total{result="success"} in /metrics output`)
}

// TestMetricsZonesActive verifies that the sidecar is managing at least one zone.
func TestMetricsZonesActive(t *testing.T) {
	body := fetchMetrics(t)

	// Find the zones_active line and confirm the value is non-zero.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "netbox_sidecar_zones_active ") {
			parts := strings.Fields(line)
			require.GreaterOrEqual(t, len(parts), 2, "unexpected format for zones_active line: %q", line)
			assert.NotEqual(t, "0", parts[1], "expected netbox_sidecar_zones_active > 0")
			return
		}
	}
	require.Fail(t, "netbox_sidecar_zones_active line not found in /metrics output")
}

// zoneRecordsLineRe matches one netbox_sidecar_zone_records sample in the
// Prometheus text exposition format. Labels are emitted alphabetically, so
// type precedes zone.
var zoneRecordsLineRe = regexp.MustCompile(`^netbox_sidecar_zone_records\{type="([^"]+)",zone="([^"]+)"\} ([0-9.e+]+)$`)

// scrapeZoneRecords parses the gauge into zone -> type -> count. It returns
// an error rather than failing the test — safe to call from retry loops and
// goroutines.
func scrapeZoneRecords() (map[string]map[string]int, error) {
	body, err := fetchMetricsErr()
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]int)
	for _, line := range strings.Split(body, "\n") {
		matches := zoneRecordsLineRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		rtype, zone := matches[1], matches[2]
		v, err := strconv.ParseFloat(matches[3], 64)
		if err != nil {
			return nil, fmt.Errorf("parse value in %q: %w", line, err)
		}
		if out[zone] == nil {
			out[zone] = make(map[string]int)
		}
		out[zone][rtype] = int(v)
	}
	return out, nil
}

// axfrTypeCounts counts the data records a zone transfer serves, excluding
// zone infrastructure: the SOA (AXFR envelopes carry it twice), the apex NS,
// and the G3 primary-NS glue address — an A record owned by the SOA MNAME
// that zonegen emits from PRIMARY_NS_IP and that the gauge does not count.
// (Fixture assumption: NetBox does not itself hold a record named like the
// primary NS; if it ever does, zonegen suppresses the glue and this exclusion
// would undercount A by one.)
func axfrTypeCounts(zone string) (map[string]int, error) {
	rrs, err := axfrRecordsErr(zone)
	if err != nil {
		return nil, err
	}

	var mname string
	for _, rr := range rrs {
		if soa, ok := rr.(*dns.SOA); ok {
			mname = soa.Ns
			break
		}
	}

	counts := make(map[string]int)
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.SOA, *dns.NS:
			continue
		case *dns.A:
			if v.Hdr.Name == mname {
				continue // primary-NS glue
			}
			counts["A"]++
		case *dns.AAAA:
			if v.Hdr.Name == mname {
				continue
			}
			counts["AAAA"]++
		case *dns.CNAME:
			counts["CNAME"]++
		case *dns.PTR:
			counts["PTR"]++
		}
	}
	return counts, nil
}

// TestZoneRecordsMatchAXFR asserts the invariant issue #111 exists for: the
// published-record gauge agrees, per zone and per type, with what an AXFR of
// that zone actually serves. Eventually-wrapped because a background poll can
// land between the metrics scrape and the zone transfer.
func TestZoneRecordsMatchAXFR(t *testing.T) {
	waitForPrimary(t)

	var lastDiff string
	ok := assert.Eventually(t, func() bool {
		metricCounts, err := scrapeZoneRecords()
		if err != nil {
			lastDiff = err.Error()
			return false
		}
		if len(metricCounts) == 0 {
			lastDiff = "no netbox_sidecar_zone_records series scraped yet"
			return false
		}
		for zone, want := range metricCounts {
			got, err := axfrTypeCounts(zone)
			if err != nil {
				lastDiff = err.Error()
				return false
			}
			if !maps.Equal(want, got) {
				lastDiff = fmt.Sprintf("zone %s: metrics=%v axfr=%v", zone, want, got)
				return false
			}
		}
		return true
	}, 60*time.Second, 2*time.Second, "gauge and AXFR disagree")
	if !ok {
		t.Logf("last disagreement: %s", lastDiff)
	}
}

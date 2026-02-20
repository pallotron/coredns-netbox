//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func sidecarMetricsURL() string {
	if v := os.Getenv("SIDECAR_METRICS_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:18082/metrics"
}

// fetchMetrics retrieves the /metrics page and returns the body as a string.
func fetchMetrics(t *testing.T) string {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(sidecarMetricsURL())
	if err != nil {
		t.Fatalf("GET %s failed: %v", sidecarMetricsURL(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// TestMetricsEndpointReachable verifies that the /metrics endpoint returns HTTP 200.
func TestMetricsEndpointReachable(t *testing.T) {
	fetchMetrics(t) // fatals on any error
}

// TestMetricsContainExpectedSeries verifies that all 9 netbox_sidecar_* metric
// families appear in the scrape output.
func TestMetricsContainExpectedSeries(t *testing.T) {
	body := fetchMetrics(t)

	want := []string{
		"netbox_sidecar_poll_total",
		"netbox_sidecar_poll_duration_seconds",
		"netbox_sidecar_last_successful_poll_timestamp_seconds",
		"netbox_sidecar_netbox_fetch_duration_seconds",
		"netbox_sidecar_netbox_records_fetched",
		"netbox_sidecar_netbox_empty_response_total",
		"netbox_sidecar_zones_active",
		"netbox_sidecar_zone_writes_total",
		"netbox_sidecar_zone_write_errors_total",
	}

	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}
}

// TestMetricsSuccessfulPoll verifies that at least one successful poll has been
// recorded (i.e., the sidecar has completed its first sync with Netbox).
func TestMetricsSuccessfulPoll(t *testing.T) {
	body := fetchMetrics(t)

	// The text exposition format includes lines like:
	//   netbox_sidecar_poll_total{result="success"} 1
	if !strings.Contains(body, `netbox_sidecar_poll_total{result="success"}`) {
		t.Error(`expected netbox_sidecar_poll_total{result="success"} in /metrics output`)
	}
}

// TestMetricsZonesActive verifies that the sidecar is managing at least one zone.
func TestMetricsZonesActive(t *testing.T) {
	body := fetchMetrics(t)

	// Find the zones_active line and confirm the value is non-zero.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "netbox_sidecar_zones_active ") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				t.Fatalf("unexpected format for zones_active line: %q", line)
			}
			if parts[1] == "0" {
				t.Errorf("expected netbox_sidecar_zones_active > 0, got 0")
			}
			return
		}
	}
	t.Error("netbox_sidecar_zones_active line not found in /metrics output")
}

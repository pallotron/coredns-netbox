package zonefetch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// httpClient has a per-request timeout to prevent hung connections in init containers.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// FetchZones fetches all zone files from baseURL/zones/ and writes them to dir.
// baseURL should not have a trailing slash (e.g. "http://coredns-netbox-sidecar:8082").
func FetchZones(baseURL, dir string) error {
	resp, err := httpClient.Get(baseURL + "/zones/")
	if err != nil {
		return fmt.Errorf("list zones: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list zones: unexpected status %d", resp.StatusCode)
	}

	var files []string
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return fmt.Errorf("decode zone list: %w", err)
	}

	for _, name := range files {
		if !strings.HasPrefix(name, "db.") {
			continue
		}
		if err := fetchFile(baseURL+"/zones/"+name, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
	}
	return nil
}

func fetchFile(url, dest string) error {
	resp, err := httpClient.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o644)
}

// WaitForSidecar polls baseURL/healthz until it returns 200 or timeout elapses.
// Connection errors (server not yet up) are retried silently — this handles the
// case where the sidecar pod is not yet scheduled when zone-init starts.
func WaitForSidecar(baseURL string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(baseURL + "/healthz") //nolint:gosec
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		// Connection refused, DNS not ready, or non-200 — all retried silently.
		time.Sleep(interval)
	}
	return fmt.Errorf("sidecar at %s not ready after %s", baseURL, timeout)
}

package netboxreload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpFetchClient has a per-request timeout for zone file fetches.
var httpFetchClient = &http.Client{Timeout: 30 * time.Second}

// fetchZonesFromURL fetches zone files from baseURL/zones/ and parses them.
func fetchZonesFromURL(baseURL string) (map[string]*zone, error) {
	resp, err := httpFetchClient.Get(baseURL + "/zones/") //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("list zones from %s: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list zones: status %d", resp.StatusCode)
	}

	var filenames []string
	if err := json.NewDecoder(resp.Body).Decode(&filenames); err != nil {
		return nil, fmt.Errorf("decode zone list: %w", err)
	}

	zones := make(map[string]*zone)
	for _, name := range filenames {
		if !strings.HasPrefix(name, "db.") {
			continue
		}
		z, err := fetchZoneFile(baseURL+"/zones/"+name, name)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		zones[z.origin] = z
	}
	return zones, nil
}

func fetchZoneFile(url, filename string) (*zone, error) {
	resp, err := httpFetchClient.Get(url) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseZoneContent(filename, data)
}

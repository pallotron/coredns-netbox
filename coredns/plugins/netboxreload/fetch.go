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
// Files with an entry in prev (keyed by filename) are fetched conditionally
// via If-Modified-Since; on 304 the already-parsed zone is reused. Returns
// the zones keyed by origin plus the cache to pass as prev on the next fetch.
func fetchZonesFromURL(baseURL string, prev map[string]cachedZone) (map[string]*zone, map[string]cachedZone, error) {
	resp, err := httpFetchClient.Get(baseURL + "/zones/") //nolint:noctx
	if err != nil {
		return nil, nil, fmt.Errorf("list zones from %s: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("list zones: status %d", resp.StatusCode)
	}

	var filenames []string
	if err := json.NewDecoder(resp.Body).Decode(&filenames); err != nil {
		return nil, nil, fmt.Errorf("decode zone list: %w", err)
	}

	zones := make(map[string]*zone)
	cache := make(map[string]cachedZone)
	for _, name := range filenames {
		if !strings.HasPrefix(name, "db.") {
			continue
		}
		c, err := fetchZoneFile(baseURL+"/zones/"+name, name, prev[name])
		if err != nil {
			return nil, nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		zones[c.z.origin] = c.z
		cache[name] = c
	}
	return zones, cache, nil
}

// fetchZoneFile fetches and parses one zone file. When prev holds a parsed
// zone and a Last-Modified validator, the request is conditional and a 304
// response returns prev unchanged. A server that sends no Last-Modified
// yields an empty validator, which degrades to unconditional full fetches.
func fetchZoneFile(url, filename string, prev cachedZone) (cachedZone, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil) //nolint:noctx
	if err != nil {
		return cachedZone{}, err
	}
	if prev.z != nil && prev.validator != "" {
		req.Header.Set("If-Modified-Since", prev.validator)
	}
	resp, err := httpFetchClient.Do(req)
	if err != nil {
		return cachedZone{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return prev, nil
	}
	if resp.StatusCode != http.StatusOK {
		return cachedZone{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return cachedZone{}, err
	}
	z, err := parseZoneContent(filename, data)
	if err != nil {
		return cachedZone{}, err
	}
	return cachedZone{validator: resp.Header.Get("Last-Modified"), z: z}, nil
}

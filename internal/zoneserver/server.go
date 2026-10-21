package zoneserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Register adds /zones/ and /zones/{filename} handlers to mux.
// Only files matching db.* are served; path traversal is rejected.
func Register(mux *http.ServeMux, dir string) {
	mux.HandleFunc("/zones/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/zones/")

		if name == "" {
			// List zone files
			entries, err := os.ReadDir(dir)
			if err != nil {
				http.Error(w, "cannot read zone dir", http.StatusInternalServerError)
				return
			}
			files := []string{}
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "db.") {
					files = append(files, e.Name())
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(files)
			return
		}

		// Reject path traversal and non-zone filenames
		if strings.Contains(name, "/") || strings.Contains(name, "..") || !strings.HasPrefix(name, "db.") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		path := filepath.Join(dir, filepath.Base(name))
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Validate on content, never on time.
		//
		// This used to be http.ServeFile, which validates with
		// Last-Modified/If-Modified-Since. HTTP dates carry whole seconds,
		// and Go's checkIfModifiedSince truncates the file's mtime to a
		// second before comparing, returning 304 when modtime <= the
		// supplied time. So a zone rewritten twice inside one wall-clock
		// second served 304 for the second write, and the client kept
		// serving the previous zone -- the update was not late, it was
		// invisible until some unrelated later write in a different second.
		//
		// That is not a theoretical race. The webhook fast path exists to
		// write the moment an event arrives, which is exactly when it is
		// most likely to land in the same second as a poll-driven write and
		// be swallowed. Nothing alerted: the sidecar had written the file,
		// so staleness stayed healthy while DNS answered NXDOMAIN.
		//
		// An ETag is compared for equality, so there is no granularity to
		// get wrong. Passing a zero modtime keeps ServeContent from emitting
		// Last-Modified at all, so the unsafe validator cannot come back by
		// way of a client that prefers it.
		data, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("ETag", fmt.Sprintf("%q", hex.EncodeToString(sum[:])))
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}

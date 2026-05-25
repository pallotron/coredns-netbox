package zoneserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		http.ServeFile(w, r, path)
	})
}

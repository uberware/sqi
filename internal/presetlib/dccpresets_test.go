// SPDX-License-Identifier: AGPL-3.0-or-later

package presetlib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The DCC reference presets must round-trip through the real library install
// path (index → fetch → fingerprint verify → parsed product).
func TestDCCReferencePresetsInstallLoop(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "presets", "sqi", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob presets/sqi: %v (%d files)", err, len(paths))
	}
	files := map[string][]byte{}
	var entries []IndexEntry
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		sum := sha256.Sum256(data)
		files["/"+filepath.Base(path)] = data
		entries = append(entries, IndexEntry{
			Name:       name,
			Category:   "Rendering",
			Definition: filepath.Base(path),
			Sha256:     hex.EncodeToString(sum[:]),
		})
	}
	index, err := json.Marshal(map[string]any{"presets": entries})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			_, _ = w.Write(index) //nolint:errcheck // response write errors cannot be handled after headers are sent
			return
		}
		if data, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(data) //nolint:errcheck // response write errors cannot be handled after headers are sent
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	svc := New(srv.URL+"/index.json", time.Minute)
	got, err := svc.FetchIndex(context.Background(), true)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if len(got) != len(paths) {
		t.Fatalf("index entries = %d, want %d", len(got), len(paths))
	}
	for _, entry := range got {
		if _, err := svc.FetchDefinition(context.Background(), entry); err != nil {
			t.Errorf("FetchDefinition(%s): %v", entry.Name, err)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// presetDef is a minimal product definition that satisfies ParseDefinition:
// it has a name, title, and a valid inline OpenJD template.
const presetDef = `name: studio/maya
title: Maya Render
template:
  specificationVersion: jobtemplate-2023-09
  name: Maya Render
  steps:
    - name: Run
      script:
        actions:
          onRun: { command: echo, args: ["render"] }
`

// TestPresetInstall_RoundTrip verifies the full preset-install flow against a
// fixture HTTP server acting as the community library:
//
//  1. Spin up an httptest.Server serving /index.json and /maya.yaml.
//  2. Open a temp SQLite store and wire presetlib.Service + product.Catalog.
//  3. FetchIndex → FetchDefinition → Install.
//  4. Assert the returned product carries Source=installed, correct fingerprint,
//     and OriginRef; then verify the row is retrievable by name from the store.
func TestPresetInstall_RoundTrip(t *testing.T) {
	sum := sha256.Sum256([]byte(presetDef))
	sha := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"presets":[{"name":"studio/maya","title":"Maya Render","category":"Rendering","version":"1.0.0","definition":"maya.yaml","sha256":"` + sha + `"}]}`)) //nolint:errcheck // test HTTP handler; write errors are not actionable
	})
	mux.HandleFunc("/maya.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(presetDef)) //nolint:errcheck // test HTTP handler; write errors are not actionable
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"
	st, err := sqlite.Open(ctx, dbPath, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	lib := presetlib.New(srv.URL+"/index.json", time.Minute)
	cat := product.NewCatalog(st)

	entries, err := lib.FetchIndex(ctx, false)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("FetchIndex: got 0 entries, want 1")
	}

	def, err := lib.FetchDefinition(ctx, entries[0])
	if err != nil {
		t.Fatalf("FetchDefinition: %v", err)
	}

	got, created, err := cat.Install(ctx, def, entries[0].Name, entries[0].Sha256)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !created {
		t.Fatalf("Install: created=false, want true")
	}
	if got.Source != store.SourceInstalled {
		t.Errorf("Source: got %q, want %q", got.Source, store.SourceInstalled)
	}
	if got.OriginFingerprint != sha {
		t.Errorf("OriginFingerprint: got %q, want %q", got.OriginFingerprint, sha)
	}

	stored, err := st.GetProductByName(ctx, "studio/maya")
	if err != nil {
		t.Fatalf("GetProductByName: %v", err)
	}
	if stored.OriginRef != "studio/maya" {
		t.Errorf("OriginRef: got %q, want %q", stored.OriginRef, "studio/maya")
	}
}

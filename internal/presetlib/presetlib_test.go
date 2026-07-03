// SPDX-License-Identifier: AGPL-3.0-or-later

package presetlib_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/presetlib"
)

const validDef = `name: studio/maya
title: Maya
template:
  specificationVersion: jobtemplate-2023-09
  name: Maya
  steps:
    - name: Run
      script:
        actions:
          onRun:
            command: echo
            args: ["hi"]
`

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newLibrary serves an index at /index.json and the definition at /maya.yaml.
// hits counts index requests so we can assert caching.
func newLibrary(t *testing.T, defBody, defSha string) (url string, hits *int64) {
	t.Helper()
	var n int64
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"presets":[{"name":"studio/maya","title":"Maya","category":"Rendering","version":"1.0.0","definition":"maya.yaml","sha256":"` + defSha + `"}]}`)) //nolint:errcheck // response write errors cannot be handled after headers are sent
	})
	mux.HandleFunc("/maya.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(defBody)) //nolint:errcheck // response write errors cannot be handled after headers are sent
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/index.json", &n
}

func TestConfigured(t *testing.T) {
	if presetlib.New("", presetlib.DefaultCacheTTL).Configured() {
		t.Fatal("empty URL should be unconfigured")
	}
	if !presetlib.New("http://x/i.json", presetlib.DefaultCacheTTL).Configured() {
		t.Fatal("non-empty URL should be configured")
	}
}

func TestFetchIndex_CachesUntilTTL(t *testing.T) {
	url, hits := newLibrary(t, validDef, sha256hex(validDef))
	s := presetlib.New(url, time.Hour)
	ctx := context.Background()
	if _, err := s.FetchIndex(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchIndex(ctx, false); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("expected 1 index fetch (cached), got %d", *hits)
	}
	if _, err := s.FetchIndex(ctx, true); err != nil { // force refresh
		t.Fatal(err)
	}
	if *hits != 2 {
		t.Fatalf("force refresh should re-fetch, got %d hits", *hits)
	}
}

func TestFetchDefinition_VerifiesFingerprint(t *testing.T) {
	sha := sha256hex(validDef)
	url, _ := newLibrary(t, validDef, sha)
	s := presetlib.New(url, time.Hour)
	entries, err := s.FetchIndex(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.FetchDefinition(context.Background(), entries[0])
	if err != nil {
		t.Fatalf("valid definition: %v", err)
	}
	if p.Name != "studio/maya" {
		t.Fatalf("parsed wrong product: %+v", p)
	}
}

func TestFetchDefinition_FingerprintMismatch(t *testing.T) {
	url, _ := newLibrary(t, validDef, sha256hex("something-else"))
	s := presetlib.New(url, time.Hour)
	entries, err := s.FetchIndex(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.FetchDefinition(context.Background(), entries[0])
	if err == nil {
		t.Fatal("expected fingerprint mismatch error")
	}
}

func TestFetchIndex_NotConfigured(t *testing.T) {
	_, err := presetlib.New("", time.Hour).FetchIndex(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when not configured")
	}
}

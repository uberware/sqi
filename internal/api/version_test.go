// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/version"
)

func TestGetVersion(t *testing.T) {
	t.Parallel()

	info := version.Info{
		Version:   "v1.2.3",
		Commit:    "abc1234",
		BuildDate: "2026-06-22T00:00:00Z",
		GoVersion: "go1.26.3",
	}
	h := newVersionHandler(info)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()

	h.getVersion(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got version.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got != info {
		t.Errorf("response = %+v, want %+v", got, info)
	}
}

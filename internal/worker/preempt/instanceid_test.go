// SPDX-License-Identifier: AGPL-3.0-or-later

package preempt_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/preempt"
)

func TestInstanceID_AWS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				http.Error(w, "no ttl", http.StatusBadRequest)
				return
			}
			reply(w, "tok-1")
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/instance-id":
			if r.Header.Get("X-aws-ec2-metadata-token") != "tok-1" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			reply(w, "i-0abc123\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := preempt.InstanceID(context.Background(), preempt.VendorAWS, srv.URL, srv.Client())
	if err != nil || got != "i-0abc123" {
		t.Fatalf("InstanceID = %q, %v; want i-0abc123", got, err)
	}
}

func TestInstanceID_GCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/name" || r.Header.Get("Metadata-Flavor") != "Google" {
			http.NotFound(w, r)
			return
		}
		reply(w, "render-node-7")
	}))
	defer srv.Close()

	got, err := preempt.InstanceID(context.Background(), preempt.VendorGCP, srv.URL+"/", srv.Client())
	if err != nil || got != "render-node-7" {
		t.Fatalf("InstanceID = %q, %v; want render-node-7", got, err)
	}
}

func TestInstanceID_Azure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata/instance/compute/resourceId" ||
			r.URL.Query().Get("api-version") != "2021-02-01" ||
			r.URL.Query().Get("format") != "text" ||
			r.Header.Get("Metadata") != "true" {
			http.NotFound(w, r)
			return
		}
		reply(w, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/3")
	}))
	defer srv.Close()

	got, err := preempt.InstanceID(context.Background(), preempt.VendorAzure, srv.URL, srv.Client())
	if err != nil || got != "3" {
		t.Fatalf("InstanceID = %q, %v; want 3", got, err)
	}
}

func TestInstanceID_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	for _, vendor := range []string{preempt.VendorAWS, preempt.VendorGCP, preempt.VendorAzure} {
		t.Run(vendor, func(t *testing.T) {
			start := time.Now()
			got, err := preempt.InstanceID(context.Background(), vendor, url, nil)
			if err == nil || got != "" {
				t.Fatalf("InstanceID = %q, %v; want an error and no ID", got, err)
			}
			if time.Since(start) > 5*time.Second {
				t.Errorf("took %s, want it bounded by the 2s per-request timeout", time.Since(start))
			}
		})
	}
}

func TestInstanceID_BadAnswers(t *testing.T) {
	tests := []struct {
		name    string
		vendor  string
		status  int
		body    string
		wantErr string
	}{
		{"gcp non-200", preempt.VendorGCP, http.StatusForbidden, "no", "unexpected status 403"},
		{"gcp empty", preempt.VendorGCP, http.StatusOK, "  \n", "empty"},
		{"azure empty segment", preempt.VendorAzure, http.StatusOK, "/a/b/", "empty"},
		{"unknown vendor", "oracle", http.StatusOK, "x", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				reply(w, tt.body)
			}))
			defer srv.Close()
			_, err := preempt.InstanceID(context.Background(), tt.vendor, srv.URL, srv.Client())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("InstanceID error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

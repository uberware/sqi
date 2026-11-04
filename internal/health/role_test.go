// SPDX-License-Identifier: AGPL-3.0-or-later

package health_test

// Tests for the HA role on /readyz and for /leaderz.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/health"
)

// serve runs h once for path and returns the status and body.
func serve(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestReadiness_NoRoleProviderBodyUnchanged pins the body a server without HA
// serves, byte for byte: no role, leader or epoch keys.
func TestReadiness_NoRoleProviderBodyUnchanged(t *testing.T) {
	reg := health.NewRegistry()
	reg.Register("sqlite", health.CheckerFunc(func(context.Context) error { return nil }))
	reg.Register("nats", health.CheckerFunc(func(context.Context) error { return nil }))

	code, body := serve(t, reg.ReadinessHandler(), "/readyz")
	const want = `{"status":"ok","checks":{"nats":{"status":"ok"},"sqlite":{"status":"ok"}}}`
	if code != http.StatusOK || body != want {
		t.Fatalf("/readyz = %d %s; want 200 %s", code, body, want)
	}

	// A nil provider is the same as none.
	reg.SetRoleProvider(nil)
	if _, body := serve(t, reg.ReadinessHandler(), "/readyz"); body != want {
		t.Fatalf("/readyz with a nil role provider = %s; want %s", body, want)
	}
}

func TestReadiness_CarriesRole(t *testing.T) {
	tests := []struct {
		name     string
		info     health.RoleInfo
		checkErr error
		wantCode int
		want     string
	}{
		{
			name:     "warm standby is ready",
			info:     health.RoleInfo{Role: "standby", Leader: "alpha", Epoch: 7},
			wantCode: http.StatusOK,
			want:     `{"status":"ok","checks":{"postgres":{"status":"ok"}},"role":"standby","leader":"alpha","epoch":7}`,
		},
		{
			name:     "leader",
			info:     health.RoleInfo{Role: "leader", IsLeader: true, Leader: "beta", Epoch: 8},
			wantCode: http.StatusOK,
			want:     `{"status":"ok","checks":{"postgres":{"status":"ok"}},"role":"leader","leader":"beta","epoch":8}`,
		},
		{
			name:     "no leader yet keeps the keys",
			info:     health.RoleInfo{Role: "standby"},
			wantCode: http.StatusOK,
			want:     `{"status":"ok","checks":{"postgres":{"status":"ok"}},"role":"standby","leader":"","epoch":0}`,
		},
		{
			name:     "a degraded standby is not ready",
			info:     health.RoleInfo{Role: "standby", Leader: "alpha", Epoch: 7},
			checkErr: errors.New("down"),
			wantCode: http.StatusServiceUnavailable,
			want:     `{"status":"degraded","checks":{"postgres":{"status":"error","error":"down"}},"role":"standby","leader":"alpha","epoch":7}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := health.NewRegistry()
			reg.Register("postgres", health.CheckerFunc(func(context.Context) error { return tt.checkErr }))
			reg.SetRoleProvider(func() health.RoleInfo { return tt.info })

			code, body := serve(t, reg.ReadinessHandler(), "/readyz")
			if code != tt.wantCode || body != tt.want {
				t.Fatalf("/readyz = %d %s; want %d %s", code, body, tt.wantCode, tt.want)
			}
		})
	}
}

func TestLeaderHandler(t *testing.T) {
	tests := []struct {
		name     string
		info     health.RoleInfo
		wantCode int
		want     string
	}{
		{
			name:     "leader answers 200",
			info:     health.RoleInfo{Role: "leader", IsLeader: true, Leader: "beta", Epoch: 3},
			wantCode: http.StatusOK,
			want:     `{"status":"ok","role":"leader","leader":"beta","epoch":3}`,
		},
		{
			name:     "standby answers 503",
			info:     health.RoleInfo{Role: "standby", Leader: "beta", Epoch: 3},
			wantCode: http.StatusServiceUnavailable,
			want:     `{"status":"standby","role":"standby","leader":"beta","epoch":3}`,
		},
		{
			name:     "standby with no leader",
			info:     health.RoleInfo{Role: "standby"},
			wantCode: http.StatusServiceUnavailable,
			want:     `{"status":"standby","role":"standby","leader":"","epoch":0}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := health.NewRegistry()
			// A failing checker must not matter: /leaderz is about the role.
			reg.Register("nats", health.CheckerFunc(func(context.Context) error { return errors.New("down") }))
			reg.SetRoleProvider(func() health.RoleInfo { return tt.info })

			code, body := serve(t, reg.LeaderHandler(), "/leaderz")
			if code != tt.wantCode || body != tt.want {
				t.Fatalf("/leaderz = %d %s; want %d %s", code, body, tt.wantCode, tt.want)
			}
		})
	}
}

// TestLeaderHandler_NoRoleProvider: /leaderz on a registry with no role is
// not a leader. The router does not mount it without HA; this is the
// defensive answer if it ever is.
func TestLeaderHandler_NoRoleProvider(t *testing.T) {
	code, body := serve(t, health.NewRegistry().LeaderHandler(), "/leaderz")
	const want = `{"status":"standby","role":"","leader":"","epoch":0}`
	if code != http.StatusServiceUnavailable || body != want {
		t.Fatalf("/leaderz = %d %s; want 503 %s", code, body, want)
	}
}

// TestRoleProvider_ReadEachRequest: the provider is consulted per request, so
// a promotion shows without rebuilding anything.
func TestRoleProvider_ReadEachRequest(t *testing.T) {
	reg := health.NewRegistry()
	info := health.RoleInfo{Role: "standby", Leader: "a", Epoch: 1}
	reg.SetRoleProvider(func() health.RoleInfo { return info })
	if code, _ := serve(t, reg.LeaderHandler(), "/leaderz"); code != http.StatusServiceUnavailable {
		t.Fatalf("standby /leaderz = %d, want 503", code)
	}
	info.Role, info.IsLeader = "leader", true
	if code, _ := serve(t, reg.LeaderHandler(), "/leaderz"); code != http.StatusOK {
		t.Fatalf("leader /leaderz = %d, want 200", code)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the servers REST handler.
//
// Route coverage:
//   GET /api/v1/servers — listServers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

const serversNow = int64(1_000_000_000_000)

func newServersRouter(st store.LeadershipStore, cfg ServersConfig) chi.Router {
	h := newServersHandler(st, cfg, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/servers", h.listServers)
	return r
}

func getServers(t *testing.T, r http.Handler) (*httptest.ResponseRecorder, serversResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newReq(t, http.MethodGet, "/api/v1/servers", nil))
	var resp serversResponse
	if rr.Code == http.StatusOK {
		if err := json.NewDecoder(strings.NewReader(rr.Body.String())).Decode(&resp); err != nil {
			t.Fatalf("decode: %v — body: %s", err, rr.Body)
		}
	}
	return rr, resp
}

func TestServers_HAListsRoles(t *testing.T) {
	ctx := t.Context()
	st := fake.New()
	now := serversNow
	st.SetClock(func() int64 { return now })
	started := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	for _, m := range []store.ServerMember{
		{InstanceID: "ic", Name: "sched-c", Host: "hc", PID: 3, Version: "v1", StartedAt: started},
		{InstanceID: "ia", Name: "sched-a", Host: "ha", PID: 4242, Version: "v2", StartedAt: started, BrokerOK: true},
	} {
		if err := st.UpsertServerMember(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	now += 3_600_000_000 // c was last seen an hour ago
	if err := st.UpsertServerMember(ctx, store.ServerMember{InstanceID: "ib", Name: "sched-b", Version: "v2", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertServerMember(ctx, store.ServerMember{InstanceID: "ia", Name: "sched-a", Host: "ha", PID: 4242, Version: "v2", StartedAt: started, BrokerOK: true}); err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := st.AcquireLeadership(ctx, "ia", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: %d %v %v", epoch, ok, err)
	}
	now += 2_100_000 // 2.1 s on

	rr, resp := getServers(t, newServersRouter(st, ServersConfig{HA: true, LeaseTTL: 15 * time.Second}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !resp.HA || resp.Epoch != epoch {
		t.Errorf("ha/epoch = %v/%d, want true/%d", resp.HA, resp.Epoch, epoch)
	}
	if len(resp.Servers) != 3 {
		t.Fatalf("servers = %+v, want 3", resp.Servers)
	}
	for i, want := range []struct{ id, name, role string }{
		{"ia", "sched-a", "leader"}, {"ib", "sched-b", "standby"}, {"ic", "sched-c", "missing"},
	} {
		got := resp.Servers[i]
		if got.InstanceID != want.id || got.Name != want.name || got.Role != want.role {
			t.Errorf("servers[%d] = %s/%s/%s, want %s/%s/%s", i, got.InstanceID, got.Name, got.Role, want.id, want.name, want.role)
		}
	}
	a := resp.Servers[0]
	if a.Host != "ha" || a.PID != 4242 || a.Version != "v2" || !a.StartedAt.Equal(started) {
		t.Errorf("leader = %+v", a)
	}
	if a.LastSeenSecondsAgo != 2.1 {
		t.Errorf("last_seen_seconds_ago = %v, want 2.1", a.LastSeenSecondsAgo)
	}
	if !a.BrokerOK {
		t.Error("leader broker_ok = false, want true")
	}
	if resp.Servers[1].BrokerOK {
		t.Error("standby that never saw a broker: broker_ok = true, want false")
	}
	if resp.Servers[2].LastSeenSecondsAgo < 3600 {
		t.Errorf("missing server last_seen_seconds_ago = %v, want >= 3600", resp.Servers[2].LastSeenSecondsAgo)
	}
	for _, key := range []string{`"instance_id"`, `"last_seen_seconds_ago"`, `"started_at":"2026-11-03T09:00:00Z"`, `"broker_ok"`} {
		if !strings.Contains(rr.Body.String(), key) {
			t.Errorf("body lacks %s: %s", key, rr.Body)
		}
	}
}

func TestServers_HAEmptyEncodesAnArray(t *testing.T) {
	rr, _ := getServers(t, newServersRouter(fake.New(), ServersConfig{HA: true, LeaseTTL: 15 * time.Second}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), `"servers":[]`) {
		t.Errorf("body = %s, want \"servers\":[]", rr.Body)
	}
}

func TestServers_NonHA(t *testing.T) {
	started := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	self := ServerSelf{InstanceID: "i1", Name: "farm-1", Host: "box", PID: 77, Version: "v0.4.0", StartedAt: started}
	// The store must not be consulted: nothing has registered a member.
	rr, resp := getServers(t, newServersRouter(fake.New(), ServersConfig{Self: self}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rr.Code, rr.Body)
	}
	if resp.HA || resp.Epoch != 0 || len(resp.Servers) != 1 {
		t.Fatalf("resp = %+v, want ha false, epoch 0, one server", resp)
	}
	s := resp.Servers[0]
	if s.InstanceID != "i1" || s.Name != "farm-1" || s.Host != "box" || s.PID != 77 || s.Version != "v0.4.0" ||
		s.Role != "leader" || !s.StartedAt.Equal(started) || !s.BrokerOK {
		t.Errorf("server = %+v", s)
	}
	if s.LastSeenSecondsAgo != 0 {
		t.Errorf("last_seen_seconds_ago = %v, want 0", s.LastSeenSecondsAgo)
	}
	if !strings.Contains(rr.Body.String(), `"ha":false`) || !strings.Contains(rr.Body.String(), `"epoch":0`) {
		t.Errorf("body = %s", rr.Body)
	}
}

// brokenLeadership fails every read.
type brokenLeadership struct{ store.LeadershipStore }

func (brokenLeadership) GetLeadership(context.Context) (store.Lease, error) {
	return store.Lease{}, errors.New("db down")
}

func TestServers_StoreErrorIs500(t *testing.T) {
	rr, _ := getServers(t, newServersRouter(brokenLeadership{fake.New()}, ServersConfig{HA: true, LeaseTTL: 15 * time.Second}))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "db down") {
		t.Errorf("body leaks the store error: %s", rr.Body)
	}
}

func TestServers_RequiresDiagnosticsRead(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{"read-only", http.StatusForbidden},
		{"user", http.StatusForbidden},
		{"operator", http.StatusOK},
		{"admin", http.StatusOK},
	} {
		t.Run(tc.role, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "u", "pw-secret-1", tc.role)
			srv := newAuthTestServer(t, st)
			cookie := loginCookie(t, srv, "u", "pw-secret-1")
			resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/servers", nil, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("GET /servers as %s = %d, want %d", tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

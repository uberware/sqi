// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
)

// readyz is the /readyz body of a server in an HA group.
type readyz struct {
	Status string `json:"status"`
	Checks map[string]struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"checks"`
	Role   string `json:"role"`
	Leader string `json:"leader"`
	Epoch  int64  `json:"epoch"`
}

// readyzNow fetches /readyz and decodes it.
func (h *haHarness) readyzNow() (int, readyz) {
	h.t.Helper()
	code, _, body := h.get("/readyz")
	var got readyz
	if code == 0 { // the listener is not up yet
		return 0, got
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		h.t.Fatalf("/readyz = %d %q: not JSON: %v", code, body, err)
	}
	return code, got
}

// waitReadyz waits until /readyz answers code, and returns its body.
func (h *haHarness) waitReadyz(code int) readyz {
	h.t.Helper()
	var got readyz
	waitFor(h.t, 30*time.Second, "/readyz answering", func() bool {
		var c int
		c, got = h.readyzNow()
		return c == code
	})
	return got
}

// requireCheckError checks that name failed with an error containing want.
func requireCheckError(t *testing.T, got readyz, name, want string) {
	t.Helper()
	c, ok := got.Checks[name]
	if !ok || c.Status != "error" || !strings.Contains(strings.ToLower(c.Error), strings.ToLower(want)) {
		t.Errorf("check %q = %+v (present %v); want an error containing %q", name, c, ok, want)
	}
	if got.Status != "degraded" {
		t.Errorf("status = %q, want degraded", got.Status)
	}
}

// requireReady checks a standby's healthy /readyz: both checks ok, role
// standby, naming the leader.
func requireReady(t *testing.T, got readyz) {
	t.Helper()
	if got.Status != "ok" || got.Role != "standby" || got.Leader != "alpha" {
		t.Errorf("/readyz = %+v; want ok, standby behind alpha", got)
	}
	for _, name := range []string{"nats", "postgres"} {
		if c := got.Checks[name]; c.Status != "ok" {
			t.Errorf("check %q = %+v, want ok", name, c)
		}
	}
}

// TestStandbyReadiness_BrokerAndStore is the readiness contract of a warm
// standby: ready exactly when its store answers and its broker is connected
// with the standby checks passed, so a balancer or a rollout waits for a
// standby that could take over.
func TestStandbyReadiness_BrokerAndStore(t *testing.T) {
	t.Run("broker and store up is ready", func(t *testing.T) {
		h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})
		requireReady(t, h.waitReadyz(http.StatusOK))
	})

	t.Run("broker down at boot is not ready, then ready once it connects", func(t *testing.T) {
		h := startHarness(t, haSetup{ha: true, brokerDown: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})

		// The probe-only listener answers while the bus does not exist yet.
		got := h.waitReadyz(http.StatusServiceUnavailable)
		requireCheckError(t, got, "nats", "not connected")
		if c := got.Checks["postgres"]; c.Status != "ok" {
			t.Errorf("a broker outage reported the store as %+v", c)
		}
		if got.Role != "standby" {
			t.Errorf("role = %q, want standby", got.Role)
		}

		h.br.bringUp()
		requireReady(t, h.waitReadyz(http.StatusOK))
	})

	t.Run("broker lost after warm-up is not ready, then ready again", func(t *testing.T) {
		h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})
		requireReady(t, h.waitReadyz(http.StatusOK))

		h.br.setUp(false)
		requireCheckError(t, h.waitReadyz(http.StatusServiceUnavailable), "nats", "not connected")

		h.br.setUp(true)
		requireReady(t, h.waitReadyz(http.StatusOK))
	})

	t.Run("standby checks pending is not ready until they pass", func(t *testing.T) {
		h := startHarness(t, haSetup{
			ha:       true,
			seed:     func(st *haStore) { holdLease(t, st, "alpha", "") },
			mutateBr: (*fakeHABroker).holdChecks,
		})
		got := h.waitReadyz(http.StatusServiceUnavailable)
		requireCheckError(t, got, "nats", bus.ErrStandbyUnchecked.Error())
		if c := got.Checks["postgres"]; c.Status != "ok" {
			t.Errorf("pending broker checks reported the store as %+v", c)
		}
		if h.rec.count("AcquireAttempt") != 0 {
			t.Error("a standby with its checks pending campaigned")
		}

		h.br.passChecks()
		requireReady(t, h.waitReadyz(http.StatusOK))
	})

	t.Run("a standby whose checks refused it is not ready", func(t *testing.T) {
		refusal := errors.New("the account has no JetStream")
		h := startHarness(t, haSetup{
			ha:   true,
			seed: func(st *haStore) { holdLease(t, st, "alpha", "") },
			mutateBr: func(b *fakeHABroker) {
				b.holdChecks()
				b.readyErr = refusal
			},
		})
		requireCheckError(t, h.waitReadyz(http.StatusServiceUnavailable), "nats", refusal.Error())
	})

	t.Run("store ping failing is not ready, naming the store", func(t *testing.T) {
		h := startHarness(t, haSetup{ha: true, seed: func(st *haStore) { holdLease(t, st, "alpha", "") }})
		requireReady(t, h.waitReadyz(http.StatusOK))

		h.st.failPing(errors.New("connection refused"))
		got := h.waitReadyz(http.StatusServiceUnavailable)
		requireCheckError(t, got, "postgres", "connection refused")
		if c := got.Checks["nats"]; c.Status != "ok" {
			t.Errorf("a store outage reported the broker as %+v", c)
		}

		h.st.failPing(nil)
		requireReady(t, h.waitReadyz(http.StatusOK))
	})
}

// TestLeaderReadiness_GuardLostDegraded pins that a leader's /readyz checks
// the broker guard: a guard that is not healthy makes it not ready, until
// the demotion that follows a lost guard exits the process.
func TestLeaderReadiness_GuardLostDegraded(t *testing.T) {
	h := startHarness(t, haSetup{ha: true})
	h.waitLeader()

	got := h.waitReadyz(http.StatusOK)
	if got.Role != "leader" || got.Leader != "self" || got.Checks["nats"].Status != "ok" {
		t.Fatalf("/readyz on the leader = %+v; want ok, leader self", got)
	}

	h.br.failGuard(errors.New("guard: key held by another instance"))
	got = h.waitReadyz(http.StatusServiceUnavailable)
	requireCheckError(t, got, "nats", "guard")
	if got.Role != "leader" {
		t.Errorf("role = %q while the guard is unhealthy, want leader", got.Role)
	}

	h.br.failGuard(nil)
	if got = h.waitReadyz(http.StatusOK); got.Checks["nats"].Status != "ok" {
		t.Errorf("/readyz = %+v after the guard recovered", got)
	}
}

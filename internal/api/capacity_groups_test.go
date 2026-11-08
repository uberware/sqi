// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Tests for the capacity-group REST handlers.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/capacity"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// fakeCapacityStatus serves canned evaluations.
type fakeCapacityStatus map[string]capacity.Status

func (f fakeCapacityStatus) CapacityStatus(id string) (capacity.Status, bool) {
	s, ok := f[id]
	return s, ok
}

type capacityEnv struct {
	t      *testing.T
	st     *fake.Store
	status fakeCapacityStatus
	router chi.Router
	farmID string
}

// newCapacityEnv wires the handler to a fake store holding one farm. natsAuth
// is whether the broker requires credentials.
func newCapacityEnv(t *testing.T, natsAuth bool) *capacityEnv {
	t.Helper()
	st := fake.New()
	t.Cleanup(func() { st.Close() })
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "f"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	env := &capacityEnv{t: t, st: st, status: fakeCapacityStatus{}, farmID: farm.ID}
	h := newCapacityGroupHandler(st, env.status, natsAuth, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/capacity-groups", h.list)
	r.Post("/api/v1/capacity-groups", h.create)
	r.Get("/api/v1/capacity-groups/{id}", h.get)
	r.Put("/api/v1/capacity-groups/{id}", h.update)
	r.Delete("/api/v1/capacity-groups/{id}", h.delete)
	r.Get("/api/v1/capacity-groups/{id}/members", h.listMembers)
	r.Put("/api/v1/capacity-groups/{id}/members/{workerID}", h.addMember)
	r.Delete("/api/v1/capacity-groups/{id}/members/{workerID}", h.removeMember)
	r.Get("/api/v1/capacity-groups/{id}/tokens", h.listTokens)
	r.Post("/api/v1/capacity-groups/{id}/tokens", h.createToken)
	r.Delete("/api/v1/capacity-groups/{id}/tokens/{tokenID}", h.revokeToken)
	env.router = r
	return env
}

// do sends a request and returns the recorder. A nil body sends none; a string
// body is sent as is.
func (e *capacityEnv) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			e.t.Fatalf("encode body: %v", err)
		}
	}
	req := newReq(e.t, method, path, &buf)
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	return rr
}

// groupBody is a valid sized-group request body; mutate edits it.
func (e *capacityEnv) groupBody(mutate func(map[string]any)) map[string]any {
	b := map[string]any{
		"farm_id":      e.farmID,
		"name":         "cloud",
		"kind":         "sized",
		"max_machines": 10,
		"machine_profile": map[string]any{
			"cpu_count": 16,
		},
	}
	if mutate != nil {
		mutate(b)
	}
	return b
}

// createGroupID creates a group and returns its ID.
func (e *capacityEnv) createGroupID(mutate func(map[string]any)) string {
	e.t.Helper()
	return idOf(e.t, e.createGroup(mutate))
}

func strOf(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s = %v, want a string", key, m[key])
	}
	return s
}

func idOf(t *testing.T, m map[string]any) string {
	t.Helper()
	return strOf(t, m, "id")
}

func (e *capacityEnv) worker(id string) store.Worker {
	e.t.Helper()
	w, err := e.st.GetWorker(e.t.Context(), id)
	if err != nil {
		e.t.Fatalf("GetWorker(%q): %v", id, err)
	}
	return w
}

func (e *capacityEnv) tokens(groupID string) []store.WorkerJoinToken {
	e.t.Helper()
	toks, err := e.st.ListCapacityGroupTokens(e.t.Context(), groupID)
	if err != nil {
		e.t.Fatalf("ListCapacityGroupTokens: %v", err)
	}
	return toks
}

func mustParseTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("time %v is not a string", v)
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time %v: %v", v, err)
	}
	return ts
}

func decodeMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return m
}

func (e *capacityEnv) createGroup(mutate func(map[string]any)) map[string]any {
	e.t.Helper()
	rr := e.do(http.MethodPost, "/api/v1/capacity-groups", e.groupBody(mutate))
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("create group: %d %s", rr.Code, rr.Body.String())
	}
	return decodeMap(e.t, rr)
}

func (e *capacityEnv) seedWorkerIn(farmID string) store.Worker {
	e.t.Helper()
	now := time.Now().UTC()
	w, _, err := e.st.RegisterWorker(e.t.Context(), store.Worker{
		ID:     "w-" + strings.ReplaceAll(e.t.Name(), "/", "-") + "-" + now.Format("150405.000000"),
		FarmID: farmID, Hostname: "node-" + now.Format("150405.000000"), Status: store.WorkerStatusOnline,
		RegisteredAt: now, UpdatedAt: now,
	})
	if err != nil {
		e.t.Fatalf("RegisterWorker: %v", err)
	}
	return w
}

func TestCapacityGroup_Create(t *testing.T) {
	t.Run("happy path with defaults", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		g := e.createGroup(nil)
		if g["id"] == "" || g["id"] == nil {
			t.Fatal("no id")
		}
		want := map[string]any{
			"name": "cloud", "kind": "sized", "provider": "external", "enabled": true,
			"retire_action": "sleep", "min_machines": float64(0), "max_machines": float64(10),
			"max_step_up": float64(1), "priority": float64(0),
			"scale_up_cooldown_seconds": float64(60), "idle_timeout_seconds": float64(600),
			"boot_timeout_seconds": float64(600),
		}
		for k, v := range want {
			if g[k] != v {
				t.Errorf("%s = %v, want %v", k, g[k], v)
			}
		}
		if g["status"] != nil {
			t.Errorf("status = %v, want null before the first evaluation", g["status"])
		}
		if _, present := g["status"]; !present {
			t.Error("status key must be present (null), not omitted")
		}
		if prof, ok := g["machine_profile"].(map[string]any); !ok || prof["cpu_count"] != float64(16) {
			t.Errorf("machine_profile = %v", g["machine_profile"])
		}
	})

	t.Run("explicit durations in seconds", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		g := e.createGroup(func(b map[string]any) {
			b["scale_up_cooldown_seconds"] = 0
			b["idle_timeout_seconds"] = 120
			b["boot_timeout_seconds"] = 300
			b["enabled"] = false
		})
		if g["scale_up_cooldown_seconds"] != float64(0) || g["idle_timeout_seconds"] != float64(120) ||
			g["boot_timeout_seconds"] != float64(300) || g["enabled"] != false {
			t.Errorf("got %v", g)
		}
	})

	t.Run("queue ids in the farm are accepted", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		q, err := e.st.CreateQueue(t.Context(), store.Queue{ID: "q1", FarmID: e.farmID, Name: "q"})
		if err != nil {
			t.Fatalf("CreateQueue: %v", err)
		}
		g := e.createGroup(func(b map[string]any) { b["queue_ids"] = []string{q.ID} })
		if ids, ok := g["queue_ids"].([]any); !ok || len(ids) != 1 || ids[0] != q.ID {
			t.Errorf("queue_ids = %v", g["queue_ids"])
		}
	})

	tooMany := make([]string, maxCapacityGroupQueues+1)
	for i := range tooMany {
		tooMany[i] = "q"
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		raw    string
		want   int
		detail string
	}{
		{name: "invalid json", raw: "{bad", want: 400, detail: "invalid JSON"},
		{name: "unknown field", mutate: func(b map[string]any) { b["idle_timeout"] = 5 }, want: 400, detail: "unknown field"},
		{name: "missing farm", mutate: func(b map[string]any) { delete(b, "farm_id") }, want: 400, detail: "farm_id is required"},
		{name: "farm not found", mutate: func(b map[string]any) { b["farm_id"] = "nope" }, want: 400, detail: "farm not found"},
		{name: "bad name", mutate: func(b map[string]any) { b["name"] = "-x" }, want: 400, detail: "name"},
		{name: "bad kind", mutate: func(b map[string]any) { b["kind"] = "huge" }, want: 400, detail: "kind"},
		{name: "sized without cpu_count", mutate: func(b map[string]any) { delete(b, "machine_profile") }, want: 400, detail: "cpu_count"},
		{name: "min over max", mutate: func(b map[string]any) { b["min_machines"] = 11 }, want: 400, detail: "min_machines"},
		{name: "idle timeout too short", mutate: func(b map[string]any) { b["idle_timeout_seconds"] = 30 }, want: 400, detail: "idle_timeout"},
		{name: "negative cooldown", mutate: func(b map[string]any) { b["scale_up_cooldown_seconds"] = -1 }, want: 400, detail: "scale_up_cooldown_seconds"},
		{name: "absurd timeout", mutate: func(b map[string]any) { b["boot_timeout_seconds"] = int64(1) << 60 }, want: 400, detail: "boot_timeout_seconds"},
		{name: "bad retire action", mutate: func(b map[string]any) { b["retire_action"] = "explode" }, want: 400, detail: "retire_action"},
		{name: "wake_on_lan not available", mutate: func(b map[string]any) { b["provider"] = "wake_on_lan" }, want: 400, detail: `provider "wake_on_lan" is not available yet`},
		{name: "script provider not available", mutate: func(b map[string]any) { b["provider"] = "script:aws" }, want: 400, detail: `provider "script:aws" is not available yet`},
		{name: "unknown provider", mutate: func(b map[string]any) { b["provider"] = "magic" }, want: 400, detail: "provider"},
		{name: "unknown queue", mutate: func(b map[string]any) { b["queue_ids"] = []string{"nope"} }, want: 400, detail: "queue"},
		{name: "too many queues", mutate: func(b map[string]any) { b["queue_ids"] = tooMany }, want: 400, detail: "queue_ids"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false)
			var body any = e.groupBody(tc.mutate)
			if tc.raw != "" {
				body = tc.raw
			}
			rr := e.do(http.MethodPost, "/api/v1/capacity-groups", body)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
			if d := strOf(t, decodeMap(t, rr), "detail"); !strings.Contains(d, tc.detail) {
				t.Errorf("detail %q does not mention %q", d, tc.detail)
			}
		})
	}

	t.Run("queue in another farm is rejected", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if _, err := e.st.CreateFarm(t.Context(), store.Farm{ID: "farm-2", Name: "g"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.CreateQueue(t.Context(), store.Queue{ID: "q2", FarmID: "farm-2", Name: "q"}); err != nil {
			t.Fatal(err)
		}
		rr := e.do(http.MethodPost, "/api/v1/capacity-groups", e.groupBody(func(b map[string]any) { b["queue_ids"] = []string{"q2"} }))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("duplicate name is a 409", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		e.createGroup(nil)
		rr := e.do(http.MethodPost, "/api/v1/capacity-groups", e.groupBody(nil))
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})
}

func TestCapacityGroup_ListAndGet(t *testing.T) {
	t.Run("status is null before an evaluation and filled after", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		g := e.createGroup(nil)
		id := idOf(t, g)

		rr := e.do(http.MethodGet, "/api/v1/capacity-groups/"+id, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get: %d", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"status":null`) {
			t.Errorf("get body lacks status:null: %s", rr.Body.String())
		}

		at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		e.status[id] = capacity.Status{
			GroupID: id, DemandCores: 64, DesiredMachines: 4, OnlineMembers: 2, OfflineMembers: 1,
			RetireCandidates: []string{"w9"},
			Mismatches:       []capacity.Mismatch{{WorkerID: "w3", Reason: "cpu_count 8 < 16"}},
			EvaluatedAt:      at,
		}
		rr = e.do(http.MethodGet, "/api/v1/capacity-groups/"+id, nil)
		st, ok := decodeMap(t, rr)["status"].(map[string]any)
		if !ok {
			t.Fatalf("status = %v", decodeMap(t, rr)["status"])
		}
		if st["demand_cores"] != float64(64) || st["desired_machines"] != float64(4) ||
			st["online_members"] != float64(2) || st["offline_members"] != float64(1) {
			t.Errorf("status = %v", st)
		}
		if rc, ok := st["retire_candidates"].([]any); !ok || len(rc) != 1 || rc[0] != "w9" {
			t.Errorf("retire_candidates = %v", st["retire_candidates"])
		}
		mm, ok := st["mismatches"].([]any)
		if !ok || len(mm) != 1 {
			t.Fatalf("mismatches = %v", st["mismatches"])
		}
		m, ok := mm[0].(map[string]any)
		if !ok || m["worker_id"] != "w3" || m["reason"] != "cpu_count 8 < 16" {
			t.Errorf("mismatches = %v", st["mismatches"])
		}
		if st["evaluated_at"] != at.Format(time.RFC3339) {
			t.Errorf("evaluated_at = %v", st["evaluated_at"])
		}

		rr = e.do(http.MethodGet, "/api/v1/capacity-groups", nil)
		var list []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list) != 1 {
			t.Fatalf("list = %s (%v)", rr.Body.String(), err)
		}
		if list[0]["status"] == nil {
			t.Error("list entry lacks its status")
		}
	})

	t.Run("empty status slices serialize as arrays", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		g := e.createGroup(nil)
		id := idOf(t, g)
		e.status[id] = capacity.Status{GroupID: id, EvaluatedAt: time.Now()}
		rr := e.do(http.MethodGet, "/api/v1/capacity-groups/"+id, nil)
		if !strings.Contains(rr.Body.String(), `"retire_candidates":[]`) || !strings.Contains(rr.Body.String(), `"mismatches":[]`) {
			t.Errorf("body: %s", rr.Body.String())
		}
	})

	t.Run("a nil status source means null", func(t *testing.T) {
		st := fake.New()
		defer st.Close()
		if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "f"}); err != nil {
			t.Fatal(err)
		}
		g, err := st.CreateCapacityGroup(t.Context(), store.CapacityGroup{
			ID: "g1", FarmID: "farm-1", Name: "n", Kind: store.CapacityGroupNamed, Provider: store.CapacityProviderExternal,
			Enabled: true, MaxMachines: 1, MaxStepUp: 1, IdleTimeout: time.Minute, BootTimeout: time.Minute, RetireAction: "sleep",
		})
		if err != nil {
			t.Fatal(err)
		}
		h := newCapacityGroupHandler(st, nil, false, newTestLogger())
		r := chi.NewRouter()
		r.Get("/api/v1/capacity-groups/{id}", h.get)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, newReq(t, http.MethodGet, "/api/v1/capacity-groups/"+g.ID, nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":null`) {
			t.Errorf("%d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("get unknown is 404", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if rr := e.do(http.MethodGet, "/api/v1/capacity-groups/zzz", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})

	t.Run("list is empty array not null", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		rr := e.do(http.MethodGet, "/api/v1/capacity-groups", nil)
		if strings.TrimSpace(rr.Body.String()) != "[]" {
			t.Errorf("body = %q", rr.Body.String())
		}
	})
}

func TestCapacityGroup_Update(t *testing.T) {
	t.Run("happy path replaces fields", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(func(b map[string]any) {
			b["max_machines"] = 20
			b["priority"] = 5
		}))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		g := decodeMap(t, rr)
		if g["max_machines"] != float64(20) || g["priority"] != float64(5) || g["id"] != id {
			t.Errorf("got %v", g)
		}
	})

	t.Run("omitting farm_id keeps the farm", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(func(b map[string]any) { delete(b, "farm_id") }))
		if rr.Code != http.StatusOK || decodeMap(t, rr)["farm_id"] != e.farmID {
			t.Errorf("%d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("changing kind with no members is allowed", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(func(b map[string]any) { b["kind"] = "named" }))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("changing kind with members is a 409", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(func(b map[string]any) { b["kind"] = "named" })
		w := e.seedWorkerIn(e.farmID)
		if rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil); rr.Code != http.StatusOK {
			t.Fatalf("add member: %d %s", rr.Code, rr.Body.String())
		}
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(func(b map[string]any) { b["kind"] = "sized" }))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "members") {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("keeping kind with members is allowed", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(func(b map[string]any) { b["kind"] = "named" })
		w := e.seedWorkerIn(e.farmID)
		e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil)
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(func(b map[string]any) { b["kind"] = "named"; b["priority"] = 3 }))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	tests := []struct {
		name   string
		mutate func(map[string]any)
		raw    string
		want   int
		detail string
	}{
		{name: "invalid json", raw: "{bad", want: 400, detail: "invalid JSON"},
		{name: "validation failure", mutate: func(b map[string]any) { b["max_machines"] = 0 }, want: 400, detail: "max_machines"},
		{name: "provider not available", mutate: func(b map[string]any) { b["provider"] = "script:x" }, want: 400, detail: "is not available yet"},
		{name: "farm cannot change", mutate: func(b map[string]any) { b["farm_id"] = "other" }, want: 400, detail: "farm_id"},
		{name: "unknown queue", mutate: func(b map[string]any) { b["queue_ids"] = []string{"nope"} }, want: 400, detail: "queue"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false)
			id := e.createGroupID(nil)
			var body any = e.groupBody(tc.mutate)
			if tc.raw != "" {
				body = tc.raw
			}
			rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, body)
			if rr.Code != tc.want || !strings.Contains(rr.Body.String(), tc.detail) {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}
		})
	}

	t.Run("unknown group is 404", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if rr := e.do(http.MethodPut, "/api/v1/capacity-groups/zzz", e.groupBody(nil)); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})

	t.Run("renaming onto another group is a 409", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		e.createGroup(nil)
		id := e.createGroupID(func(b map[string]any) { b["name"] = "other" })
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id, e.groupBody(nil))
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})
}

func TestCapacityGroup_Delete(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		if rr := e.do(http.MethodDelete, "/api/v1/capacity-groups/"+id, nil); rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rr.Code)
		}
		if rr := e.do(http.MethodGet, "/api/v1/capacity-groups/"+id, nil); rr.Code != http.StatusNotFound {
			t.Errorf("after delete, get = %d", rr.Code)
		}
	})
	t.Run("unknown is 404", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if rr := e.do(http.MethodDelete, "/api/v1/capacity-groups/zzz", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})
	t.Run("members exist is a 409", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(func(b map[string]any) { b["kind"] = "named" })
		w := e.seedWorkerIn(e.farmID)
		e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil)
		rr := e.do(http.MethodDelete, "/api/v1/capacity-groups/"+id, nil)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "members") {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})
}

func TestCapacityGroup_Members(t *testing.T) {
	named := func(b map[string]any) { b["kind"] = "named" }

	t.Run("add, list and remove", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(named)
		w := e.seedWorkerIn(e.farmID)

		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
		}
		if got := decodeMap(t, rr); got["id"] != w.ID || got["hostname"] != w.Hostname {
			t.Errorf("add response = %v", got)
		}
		stored := e.worker(w.ID)
		if stored.CapacityGroupID != id || stored.Ephemeral {
			t.Errorf("stored group=%q ephemeral=%v", stored.CapacityGroupID, stored.Ephemeral)
		}

		rr = e.do(http.MethodGet, "/api/v1/capacity-groups/"+id+"/members", nil)
		var list []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0]["id"] != w.ID {
			t.Fatalf("members = %s (%v)", rr.Body.String(), err)
		}

		if rr = e.do(http.MethodDelete, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil); rr.Code != http.StatusNoContent {
			t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
		}
		stored = e.worker(w.ID)
		if stored.CapacityGroupID != "" {
			t.Errorf("membership not cleared: %q", stored.CapacityGroupID)
		}
	})

	t.Run("members of an unknown group is 404", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if rr := e.do(http.MethodGet, "/api/v1/capacity-groups/zzz/members", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})

	t.Run("empty member list is an array", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(named)
		rr := e.do(http.MethodGet, "/api/v1/capacity-groups/"+id+"/members", nil)
		if strings.TrimSpace(rr.Body.String()) != "[]" {
			t.Errorf("body = %q", rr.Body.String())
		}
	})

	t.Run("a sized group refuses PUT membership", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		w := e.seedWorkerIn(e.farmID)
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "sized") {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("unknown group is 404 on PUT", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		w := e.seedWorkerIn(e.farmID)
		if rr := e.do(http.MethodPut, "/api/v1/capacity-groups/zzz/members/"+w.ID, nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})

	t.Run("unknown worker is 404 on PUT", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(named)
		if rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/nope", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d", rr.Code)
		}
	})

	t.Run("a worker of another farm is a 409", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		if _, err := e.st.CreateFarm(t.Context(), store.Farm{ID: "farm-2", Name: "g"}); err != nil {
			t.Fatal(err)
		}
		id := e.createGroupID(named)
		w := e.seedWorkerIn("farm-2")
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+id+"/members/"+w.ID, nil)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "farm") {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("a worker already in another group is a 409 that does not claim only that", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		a := e.createGroupID(named)
		b := e.createGroupID(func(m map[string]any) { named(m); m["name"] = "second" })
		w := e.seedWorkerIn(e.farmID)
		if rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+a+"/members/"+w.ID, nil); rr.Code != http.StatusOK {
			t.Fatalf("first add: %d", rr.Code)
		}
		rr := e.do(http.MethodPut, "/api/v1/capacity-groups/"+b+"/members/"+w.ID, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "another group") || !strings.Contains(rr.Body.String(), "concurrently") {
			t.Errorf("message must name both causes: %s", rr.Body.String())
		}
	})

	t.Run("remove: a worker not in this group is 404", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		a := e.createGroupID(named)
		b := e.createGroupID(func(m map[string]any) { named(m); m["name"] = "second" })
		w := e.seedWorkerIn(e.farmID)
		e.do(http.MethodPut, "/api/v1/capacity-groups/"+a+"/members/"+w.ID, nil)
		if rr := e.do(http.MethodDelete, "/api/v1/capacity-groups/"+b+"/members/"+w.ID, nil); rr.Code != http.StatusNotFound {
			t.Errorf("wrong group: %d", rr.Code)
		}
		if rr := e.do(http.MethodDelete, "/api/v1/capacity-groups/"+a+"/members/nope", nil); rr.Code != http.StatusNotFound {
			t.Errorf("unknown worker: %d", rr.Code)
		}
		if stored := e.worker(w.ID); stored.CapacityGroupID != a {
			t.Error("a refused removal changed the membership")
		}
	})
}

func TestCapacityGroup_Tokens(t *testing.T) {
	t.Run("create returns the raw token once and never the hash", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		id := e.createGroupID(nil)

		rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", map[string]any{"name": "autoscaler"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		created := decodeMap(t, rr)
		raw := strOf(t, created, "token")
		if !strings.HasPrefix(raw, "sqiw_") || !strings.HasPrefix(raw, strOf(t, created, "prefix")) {
			t.Fatalf("token = %q prefix = %v", raw, created["prefix"])
		}
		if created["name"] != "autoscaler" || created["id"] == "" {
			t.Errorf("created = %v", created)
		}

		// The stored token binds to the group, expires in 30 days, and its
		// hash is what the store holds.
		tokens := e.tokens(id)
		if len(tokens) != 1 {
			t.Fatalf("stored tokens = %v", tokens)
		}
		tok := tokens[0]
		if tok.CapacityGroupID != id || tok.TokenHash == "" || tok.TokenHash == raw {
			t.Fatalf("stored token = %+v", tok)
		}
		exp := mustParseTime(t, created["expires_at"])
		if d := time.Until(exp); d < 719*time.Hour || d > 721*time.Hour {
			t.Errorf("default expiry in %s, want ~720h", d)
		}
		if strings.Contains(rr.Body.String(), tok.TokenHash) {
			t.Error("create response contains the token hash")
		}

		// The list shows neither the raw token nor the hash, nor a field that
		// could hold one.
		rr = e.do(http.MethodGet, "/api/v1/capacity-groups/"+id+"/tokens", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: %d", rr.Code)
		}
		body := rr.Body.String()
		if strings.Contains(body, raw) {
			t.Error("list response contains the raw token")
		}
		if strings.Contains(body, tok.TokenHash) {
			t.Error("list response contains the token hash")
		}
		var list []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list) != 1 {
			t.Fatalf("list = %s (%v)", body, err)
		}
		for k := range list[0] {
			switch k {
			case "id", "name", "prefix", "expires_at", "revoked_at", "created_by", "created_at":
			default:
				t.Errorf("unexpected list field %q", k)
			}
		}
		if list[0]["prefix"] != created["prefix"] || list[0]["revoked_at"] != nil {
			t.Errorf("list entry = %v", list[0])
		}
	})

	t.Run("expires_in", func(t *testing.T) {
		tests := []struct {
			name, in string
			want     int
			approx   time.Duration
		}{
			{"explicit", "48h", 201, 48 * time.Hour},
			{"the ceiling", "2160h", 201, 2160 * time.Hour},
			{"over the ceiling", "2161h", 400, 0},
			{"not a duration", "soon", 400, 0},
			{"zero", "0s", 400, 0},
			{"negative", "-1h", 400, 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e := newCapacityEnv(t, true)
				id := e.createGroupID(nil)
				rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", map[string]any{"expires_in": tc.in})
				if rr.Code != tc.want {
					t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
				}
				if tc.want == 201 {
					exp := mustParseTime(t, decodeMap(t, rr)["expires_at"])
					if d := time.Until(exp); d < tc.approx-time.Minute || d > tc.approx+time.Minute {
						t.Errorf("expires in %s, want ~%s", d, tc.approx)
					}
				}
			})
		}
	})

	t.Run("an empty body mints a default token", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		id := e.createGroupID(nil)
		if rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", nil); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("invalid json is a 400", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		id := e.createGroupID(nil)
		if rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", "{bad"); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rr.Code)
		}
	})

	t.Run("broker auth off is a 409 with the documented message", func(t *testing.T) {
		e := newCapacityEnv(t, false)
		id := e.createGroupID(nil)
		rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", map[string]any{})
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		want := "group tokens need nats.auth.enabled; workers on an auth-off farm join with worker.capacity_group"
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("body %q lacks %q", rr.Body.String(), want)
		}
		if len(e.tokens(id)) != 0 {
			t.Error("a token was minted anyway")
		}
	})

	t.Run("a named group refuses tokens", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		id := e.createGroupID(func(b map[string]any) { b["kind"] = "named" })
		rr := e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", map[string]any{})
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "named") {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("unknown group is 404", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for _, m := range []string{http.MethodPost, http.MethodGet} {
			if rr := e.do(m, "/api/v1/capacity-groups/zzz/tokens", nil); rr.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d", m, rr.Code)
			}
		}
	})

	t.Run("revoke marks the token and is idempotent", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		id := e.createGroupID(nil)
		tokID := idOf(t, decodeMap(t, e.do(http.MethodPost, "/api/v1/capacity-groups/"+id+"/tokens", map[string]any{})))

		path := "/api/v1/capacity-groups/" + id + "/tokens/" + tokID
		for range 2 {
			if rr := e.do(http.MethodDelete, path, nil); rr.Code != http.StatusNoContent {
				t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
			}
		}
		var list []map[string]any
		if err := json.Unmarshal(e.do(http.MethodGet, "/api/v1/capacity-groups/"+id+"/tokens", nil).Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0]["revoked_at"] == nil {
			t.Errorf("list after revoke = %v", list)
		}
	})

	t.Run("revoke refuses a token of another group or an ordinary one", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		a := e.createGroupID(nil)
		b := e.createGroupID(func(m map[string]any) { m["name"] = "second" })
		tokA := idOf(t, decodeMap(t, e.do(http.MethodPost, "/api/v1/capacity-groups/"+a+"/tokens", map[string]any{})))
		plain, err := e.st.CreateWorkerJoinToken(t.Context(), store.WorkerJoinToken{
			ID: "plain", TokenHash: "h", Prefix: "sqiw_x", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{
			"/api/v1/capacity-groups/" + b + "/tokens/" + tokA,
			"/api/v1/capacity-groups/" + a + "/tokens/" + plain.ID,
			"/api/v1/capacity-groups/" + a + "/tokens/zzz",
			"/api/v1/capacity-groups/zzz/tokens/" + tokA,
		} {
			if rr := e.do(http.MethodDelete, p, nil); rr.Code != http.StatusNotFound {
				t.Errorf("DELETE %s = %d, want 404", p, rr.Code)
			}
		}
		if toks := e.tokens(a); len(toks) != 1 || toks[0].RevokedAt != nil {
			t.Error("a refused revoke changed the token")
		}
	})
}

// TestCapacityGroup_PermissionGate drives the real router with auth on: the
// reads need infra.read, everything else (token listing included) needs
// infra.manage.
func TestCapacityGroup_PermissionGate(t *testing.T) {
	const base = "/api/v1/capacity-groups"
	tests := []struct {
		name, role, method, path string
		body                     any
		want                     int
	}{
		{"user lists groups", "user", http.MethodGet, base, nil, http.StatusOK},
		{"user gets a group", "user", http.MethodGet, base + "/g1", nil, http.StatusNotFound},
		{"user lists members", "user", http.MethodGet, base + "/g1/members", nil, http.StatusNotFound},
		{"user cannot create", "user", http.MethodPost, base, map[string]any{}, http.StatusForbidden},
		{"user cannot update", "user", http.MethodPut, base + "/g1", map[string]any{}, http.StatusForbidden},
		{"user cannot delete", "user", http.MethodDelete, base + "/g1", nil, http.StatusForbidden},
		{"user cannot add a member", "user", http.MethodPut, base + "/g1/members/w1", nil, http.StatusForbidden},
		{"user cannot remove a member", "user", http.MethodDelete, base + "/g1/members/w1", nil, http.StatusForbidden},
		{"user cannot list tokens", "user", http.MethodGet, base + "/g1/tokens", nil, http.StatusForbidden},
		{"user cannot mint a token", "user", http.MethodPost, base + "/g1/tokens", map[string]any{}, http.StatusForbidden},
		{"user cannot revoke a token", "user", http.MethodDelete, base + "/g1/tokens/t1", nil, http.StatusForbidden},
		{"read-only lists groups", "read-only", http.MethodGet, base, nil, http.StatusOK},
		{"read-only cannot create", "read-only", http.MethodPost, base, map[string]any{}, http.StatusForbidden},
		{"operator can create", "operator", http.MethodPost, base, map[string]any{}, http.StatusBadRequest},
		{"operator can list tokens", "operator", http.MethodGet, base + "/g1/tokens", nil, http.StatusNotFound},
		{"admin can mint a token", "admin", http.MethodPost, base + "/g1/tokens", map[string]any{}, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "u", "pw-secret-1", tc.role)
			srv := httptest.NewServer(authRouterWith(st, func(d *Deps) { d.NATSAuthEnabled = true }))
			t.Cleanup(srv.Close)
			cookie := loginCookie(t, srv, "u", "pw-secret-1")
			resp := doRequest(t, tc.method, srv.URL+tc.path, tc.body, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s as %s: status = %d, want %d", tc.method, tc.path, tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestCapacityGroup_TokenMintingNeedsRESTAuth: like POST /workers/join-tokens,
// minting is not mounted at all while REST auth is off, since an anonymous
// superuser could otherwise attach arbitrary compute.
func TestCapacityGroup_TokenMintingNeedsRESTAuth(t *testing.T) {
	st := fake.New()
	t.Cleanup(func() { st.Close() })
	r := NewRouter(Config{DisableRateLimit: true}, Deps{Store: st, NATSAuthEnabled: true, WorkerRevoker: storeRevoker{store: st}},
		newTestLogger(), metrics.New(), health.NewRegistry())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, newReq(t, http.MethodPost, "/api/v1/capacity-groups/g1/tokens", strings.NewReader("{}")))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (route not mounted)", rr.Code)
	}
	// The reads and the group CRUD stay available, as every other infra route.
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, newReq(t, http.MethodGet, "/api/v1/capacity-groups", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("list = %d", rr.Code)
	}
}

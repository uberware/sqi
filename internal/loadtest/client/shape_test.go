// SPDX-License-Identifier: AGPL-3.0-or-later

package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/loadtest/client"
)

// recorded is one request as the stub server saw it.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// stub answers each request from handler and records it.
type stub struct {
	mu   sync.Mutex
	reqs []recorded
}

func (s *stub) last() recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

func (s *stub) start(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test handler
		s.mu.Lock()
		s.reqs = append(s.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		n := len(s.reqs)
		s.mu.Unlock()
		handler(w, r, n)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body)) //nolint:errcheck // test handler
}

func TestClient_RequestShapes(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		replyCode  int
		call       func(c *client.Client) error
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
	}{
		{
			name: "CreateQueue", reply: `{"id":"q1"}`, replyCode: 201,
			call: func(c *client.Client) error {
				id, err := c.CreateQueue(t.Context(), "f1", "q", 4)
				if err == nil && id != "q1" {
					return errors.New("id not decoded")
				}
				return err
			},
			wantMethod: "POST", wantPath: "/api/v1/queues",
			wantBody: `{"farm_id":"f1","max_concurrent_tasks":4,"name":"q"}`,
		},
		{
			name: "CreateUsagePool", reply: `{}`, replyCode: 201,
			call:       func(c *client.Client) error { return c.CreateUsagePool(t.Context(), "p", 2) },
			wantMethod: "POST", wantPath: "/api/v1/usage-pools",
			wantBody: `{"max_concurrent":2,"name":"p"}`,
		},
		{
			name: "DeleteFarm", replyCode: 204,
			call:       func(c *client.Client) error { return c.DeleteFarm(t.Context(), "f/1") },
			wantMethod: "DELETE", wantPath: "/api/v1/farms/f/1",
		},
		{
			name: "ListJobs paging", reply: `{"items":[],"total":0,"limit":5,"offset":10}`, replyCode: 200,
			call: func(c *client.Client) error {
				p, err := c.ListJobs(t.Context(), "f1", 5, 10)
				if err == nil && (p.Limit != 5 || p.Offset != 10) {
					return errors.New("page not decoded")
				}
				return err
			},
			wantMethod: "GET", wantPath: "/api/v1/jobs", wantQuery: "farm_id=f1&limit=5&offset=10",
		},
		{
			name: "ListWorkers no farm", reply: `{"items":[{"id":"w1","status":"online"}],"total":1}`, replyCode: 200,
			call: func(c *client.Client) error {
				p, err := c.ListWorkers(t.Context(), "", 0, 0)
				if err == nil && (len(p.Items) != 1 || p.Items[0].Status != "online") {
					return errors.New("workers not decoded")
				}
				return err
			},
			wantMethod: "GET", wantPath: "/api/v1/workers",
		},
		{
			name: "TaskLogs cursor", reply: `{"items":[{"nats_seq":7,"data":"x"}],"after_nats_seq":7,"limit":50}`, replyCode: 200,
			call: func(c *client.Client) error {
				lines, next, err := c.TaskLogs(t.Context(), "t1", 3, 50)
				if err == nil && (next != 7 || len(lines) != 1 || lines[0].Data != "x") {
					return errors.New("logs not decoded")
				}
				return err
			},
			wantMethod: "GET", wantPath: "/api/v1/tasks/t1/logs", wantQuery: "after_nats_seq=3&limit=50",
		},
		{
			name: "ListAttempts", reply: `{"items":[{"attempt_number":2,"status":"failed","worker_id":"w1","started_at":"2026-01-01T00:00:00Z"}]}`, replyCode: 200,
			call: func(c *client.Client) error {
				a, err := c.ListAttempts(t.Context(), "t1")
				if err == nil && (len(a) != 1 || a[0].ID != "2" || a[0].WorkerID != "w1" || a[0].StartedAt == nil || a[0].EndedAt != nil) {
					return errors.New("attempts not decoded")
				}
				return err
			},
			wantMethod: "GET", wantPath: "/api/v1/tasks/t1/attempts",
		},
		{
			name: "disable worker", reply: `{}`, replyCode: 200,
			call:       func(c *client.Client) error { return c.SetWorkerDisabled(t.Context(), "w1", true) },
			wantMethod: "POST", wantPath: "/api/v1/workers/w1/disable",
		},
		{
			name: "enable worker", reply: `{}`, replyCode: 200,
			call:       func(c *client.Client) error { return c.SetWorkerDisabled(t.Context(), "w1", false) },
			wantMethod: "POST", wantPath: "/api/v1/workers/w1/enable",
		},
		{
			name: "SubmitJob query and JSON content type", reply: `{"id":"j1"}`, replyCode: 201,
			call: func(c *client.Client) error {
				id, st, err := c.SubmitJob(t.Context(), client.SubmitRequest{
					FarmID: "f", QueueID: "q", Template: []byte(` {"a":1}`), DependsOn: []string{"u1", "u2"}, Priority: 70,
				})
				if err == nil && (id != "j1" || st != 201) {
					return errors.New("submit result wrong")
				}
				return err
			},
			wantMethod: "POST", wantPath: "/api/v1/jobs",
			wantQuery: "depends_on=u1&depends_on=u2&farm_id=f&priority=70&queue_id=q",
			wantBody:  ` {"a":1}`,
		},
		{
			name: "Enroll", reply: `{}`, replyCode: 201,
			call:       func(c *client.Client) error { return c.Enroll(t.Context(), "tok", "w1", "UKEY", "n") },
			wantMethod: "POST", wantPath: "/api/v1/workers/enroll",
			wantBody: `{"join_token":"tok","name":"n","public_key":"UKEY","worker_id":"w1"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s stub
			ts := s.start(t, func(w http.ResponseWriter, _ *http.Request, _ int) { reply(w, tc.replyCode, tc.reply) })
			c := client.New(ts.URL+"/", client.WithAPIKey("key"))
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			got := s.last()
			if got.Method != tc.wantMethod || got.Path != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", got.Method, got.Path, tc.wantMethod, tc.wantPath)
			}
			if got.Query != tc.wantQuery {
				t.Errorf("query = %q, want %q", got.Query, tc.wantQuery)
			}
			if tc.wantBody != "" && got.Body != tc.wantBody {
				t.Errorf("body = %s, want %s", got.Body, tc.wantBody)
			}
			wantAuth := "Bearer key"
			if tc.name == "Enroll" {
				wantAuth = "" // the join token is the credential
			}
			if a := got.Header.Get("Authorization"); a != wantAuth {
				t.Errorf("Authorization = %q, want %q", a, wantAuth)
			}
		})
	}
}

func TestClient_SubmitContentType(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{"yaml", "name: x\n", "application/x-yaml"},
		{"json", `{"name":"x"}`, "application/json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s stub
			ts := s.start(t, func(w http.ResponseWriter, _ *http.Request, _ int) { reply(w, 201, `{"id":"j"}`) })
			if _, _, err := client.New(ts.URL).SubmitJob(t.Context(), client.SubmitRequest{Template: []byte(tc.template)}); err != nil {
				t.Fatalf("SubmitJob: %v", err)
			}
			if got := s.last().Header.Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClient_ErrorDetail(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantDetail string
	}{
		{"problem detail", 422, `{"type":"about:blank","title":"Unprocessable Entity","status":422,"detail":"bad step"}`, "bad step"},
		{"title only", 429, `{"title":"Too Many Requests"}`, "Too Many Requests"},
		{"plain body", 503, "upstream gone\n", "upstream gone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s stub
			ts := s.start(t, func(w http.ResponseWriter, _ *http.Request, _ int) { reply(w, tc.status, tc.body) })
			id, status, err := client.New(ts.URL).SubmitJob(t.Context(), client.SubmitRequest{Template: []byte("x")})
			if id != "" || status != tc.status {
				t.Errorf("got (%q, %d), want (\"\", %d)", id, status, tc.status)
			}
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v is not an *APIError", err)
			}
			if apiErr.Detail != tc.wantDetail || !strings.Contains(err.Error(), tc.wantDetail) {
				t.Errorf("detail = %q (error %q), want %q", apiErr.Detail, err, tc.wantDetail)
			}
		})
	}
}

func TestClient_TransportErrorStatusZero(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	id, status, err := client.New(url).SubmitJob(t.Context(), client.SubmitRequest{Template: []byte("x")})
	if err == nil || status != 0 || id != "" {
		t.Errorf("got (%q, %d, %v), want a status-0 transport error", id, status, err)
	}
}

func TestClient_PauseQueueReadsThenWrites(t *testing.T) {
	var s stub
	ts := s.start(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Method == http.MethodGet {
			reply(w, 200, `{"id":"q1","farm_id":"f1","name":"main","priority":3,"max_concurrent_tasks":8,"paused":false,"max_attempts":4,"run_as_user":"render"}`)
			return
		}
		reply(w, 200, `{}`)
	})
	if err := client.New(ts.URL).PauseQueue(t.Context(), "q1", true); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	put := s.last()
	if put.Method != http.MethodPut || put.Path != "/api/v1/queues/q1" {
		t.Fatalf("last request = %s %s, want PUT /api/v1/queues/q1", put.Method, put.Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(put.Body), &body); err != nil {
		t.Fatalf("PUT body: %v", err)
	}
	want := map[string]any{"farm_id": "f1", "name": "main", "priority": 3.0, "max_concurrent_tasks": 8.0, "paused": true, "max_attempts": 4.0}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("PUT body[%s] = %v, want %v", k, body[k], v)
		}
	}
	if _, ok := body["run_as_user"]; ok {
		t.Error("PUT body carries run_as_user; omitting it is what preserves the stored identity")
	}
}

func TestClient_LoginThenCreateAPIKey(t *testing.T) {
	var s stub
	ts := s.start(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "sqi_session", Value: "tok123", Path: "/", HttpOnly: true})
			reply(w, 200, `{"id":"u1"}`)
		case "/api/v1/api-keys":
			reply(w, 201, `{"id":"k1","secret":"sqi_secret"}`)
		default:
			reply(w, 404, `{"detail":"nope"}`)
		}
	})
	c := client.New(ts.URL, client.WithAPIKey("ignored-on-login"))

	session, err := c.Login(t.Context(), "admin", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session != "tok123" {
		t.Errorf("session = %q, want tok123", session)
	}
	if login := s.last(); login.Header.Get("Authorization") != "" || login.Header.Get("Cookie") != "" {
		t.Errorf("login request carried credentials: %v", login.Header)
	}

	secret, err := c.CreateAPIKey(t.Context(), "load")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if secret != "sqi_secret" {
		t.Errorf("secret = %q", secret)
	}
	req := s.last()
	if got := req.Header.Get("Cookie"); got != "sqi_session=tok123" {
		t.Errorf("Cookie = %q, want sqi_session=tok123", got)
	}
	if got := req.Header.Get("Origin"); got != ts.URL {
		t.Errorf("Origin = %q, want %q (the CSRF guard needs one on a cookie-bearing POST)", got, ts.URL)
	}
}

func TestClient_LoginWithoutCookie(t *testing.T) {
	var s stub
	ts := s.start(t, func(w http.ResponseWriter, _ *http.Request, _ int) { reply(w, 200, `{}`) })
	if _, err := client.New(ts.URL).Login(t.Context(), "u", "p"); err == nil {
		t.Fatal("Login succeeded with no session cookie in the response")
	}
}

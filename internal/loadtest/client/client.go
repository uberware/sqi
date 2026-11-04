// SPDX-License-Identifier: AGPL-3.0-or-later

// Package client is the load harness's REST and WebSocket client for a running
// sqi-server. Field and route names follow internal/api/openapi.yaml; each
// struct decodes only the fields the harness reads.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"
)

// requestTimeout bounds every call except SubmitJob. It is a variable so a
// test can shorten it.
var requestTimeout = 30 * time.Second

// The pauses between retries of a throttled call (see [WithRetry]) when the
// server sends no Retry-After: they start at retryBase and double up to
// retryMax. Variables so a test can shorten them.
var (
	retryBase = 250 * time.Millisecond
	retryMax  = 10 * time.Second
)

// DefaultRetryBudget is the longest one call spends retrying 429 and 503
// answers on a client made with WithRetry(DefaultRetryBudget).
const DefaultRetryBudget = 2 * time.Minute

const (
	apiPrefix        = "/api/v1"
	maxResponseBytes = 64 << 20
	maxDetailBytes   = 512
	defaultCookie    = "sqi_session"
)

// Client talks to one sqi-server. It is safe for concurrent use.
type Client struct {
	base   string
	origin string
	apiKey string
	hc     *http.Client
	// retryBudget bounds the time one call spends retrying 429 and 503
	// answers; 0 never retries.
	retryBudget time.Duration
	// reads, set on a client from [Client.RetryingReads], also retries a
	// GET's transport error or other 5xx, within the same budget.
	reads *readRetry

	mu      sync.RWMutex
	session *http.Cookie
}

// Option configures New.
type Option func(*Client)

// WithAPIKey sends "Authorization: Bearer <key>" on every request.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithRetry makes every call but SubmitJob and Ready retry a 429 or 503
// answer, waiting as the server's Retry-After says (or backing off from
// 250 ms to 10 s without one), until the call succeeds, fails otherwise, or
// would wait past budget in all. A server's rate limiter answers 429, and an
// attached server keeps its limiter on, so the harness's own setup, actions,
// checks and teardown must ride it out. Submissions never retry: a 429 or
// 503 answer to one is what the run measures. A budget of 0 turns retries
// off, as for the UI pollers, whose errors are counted.
func WithRetry(budget time.Duration) Option {
	return func(c *Client) { c.retryBudget = max(budget, 0) }
}

// WithHTTPClient replaces the default client. Per-request deadlines are applied
// by the Client itself (30 s, none for SubmitJob), so the http.Client should
// not set a Timeout of its own.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.hc = hc }
}

// HTTPClient returns the client's underlying *http.Client, for dialing a
// WebSocket that trusts the same CA.
func (c *Client) HTTPClient() *http.Client { return c.hc }

// WithoutRetry returns a client for the same server, with c's HTTP client and
// its credentials as they are now, that never retries a throttled call (see
// [WithRetry]).
func (c *Client) WithoutRetry() *Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &Client{base: c.base, origin: c.origin, apiKey: c.apiKey, hc: c.hc, session: c.session}
}

// readRetry is the state the reads of one [Client.RetryingReads] copy share.
type readRetry struct {
	// down is set once a read has retried a transport error or 5xx for the
	// whole budget: the server is taken to be down, and later reads fail such
	// an answer at once instead of each spending the budget again.
	down atomic.Bool
}

// RetryingReads returns a client like c, with its credentials as they are
// now, that within c's retry budget also retries a GET that fails with a
// transport error or any 5xx: a read is safe to repeat, so a reader that must
// not give up on a passing fault (the verifier) needs no retry of its own.
// Once one read has retried such a failure for the whole budget, the copy
// stops retrying them, so a server that is down fails the remaining reads
// promptly; take a new copy for each pass that should start afresh. Other
// calls keep c's policy, and a call that is never retried still is not.
func (c *Client) RetryingReads() *Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &Client{
		base: c.base, origin: c.origin, apiKey: c.apiKey, hc: c.hc, session: c.session,
		retryBudget: c.retryBudget, reads: &readRetry{},
	}
}

// New returns a client for the server at baseURL (for example
// http://127.0.0.1:8080).
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base: strings.TrimRight(baseURL, "/"),
		hc:   &http.Client{},
	}
	if u, err := url.Parse(c.base); err == nil && u.Host != "" {
		c.origin = u.Scheme + "://" + u.Host
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Method string
	Path   string
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Page is one page of a paginated list.
type Page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Job is a job summary or detail.
type Job struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	TaskCounts map[string]int `json:"task_counts"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// Task is a task row.
type Task struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	AssignedWorkerID string    `json:"assigned_worker_id"`
	FailureReason    string    `json:"failure_reason"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Attempt is one execution attempt of a task. The API identifies an attempt by
// its number within the task and has no attempt id, so ID is that number in
// decimal.
type Attempt struct {
	ID            string
	AttemptNumber int
	WorkerID      string
	Status        string
	StartedAt     *time.Time
	EndedAt       *time.Time
}

// LogLine is one persisted task-log chunk.
type LogLine struct {
	ID        string    `json:"id"`
	AttemptID string    `json:"attempt_id"`
	SeqNum    int64     `json:"seq_num"`
	NatsSeq   int64     `json:"nats_seq"`
	Stream    string    `json:"stream"`
	Data      string    `json:"data"`
	At        time.Time `json:"at"`
}

// Farm is a farm.
type Farm struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Queue is a queue.
type Queue struct {
	ID     string `json:"id"`
	FarmID string `json:"farm_id"`
	Name   string `json:"name"`
}

// UsagePool is a usage pool.
type UsagePool struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// Worker is a registered worker.
type Worker struct {
	ID       string `json:"id"`
	FarmID   string `json:"farm_id"`
	QueueID  string `json:"queue_id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
}

// SubmitRequest describes a job submission. A zero Priority leaves the server
// default.
type SubmitRequest struct {
	FarmID    string
	QueueID   string
	Template  []byte
	DependsOn []string
	Priority  int
}

// call is one HTTP request.
type call struct {
	method      string
	path        string // relative to /api/v1, or to the server root when root is set
	root        bool
	query       url.Values
	body        []byte
	contentType string
	noAuth      bool  // send no API key and no session
	noDeadline  bool  // bounded only by the caller's context
	noRetry     bool  // never retried, whatever WithRetry says
	raw         bool  // the body is not JSON: send no Accept header
	maxBytes    int64 // the most of the body read; 0 is maxResponseBytes
}

// reply is a 2xx response.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func escape(id string) string { return url.PathEscape(id) }

// send performs cl, retrying a failure [Client.retryable] allows within the
// client's retry budget (see [WithRetry]). A non-2xx response returns its
// status with an *APIError; a transport error returns status 0.
func (c *Client) send(ctx context.Context, cl call) (reply, int, error) {
	r, status, wait, err := c.sendOnce(ctx, cl)
	if c.retryBudget <= 0 || cl.noRetry {
		return r, status, err
	}
	deadline := time.Now().Add(c.retryBudget)
	backoff := retryBase
	for err != nil && c.retryable(ctx, cl, status) {
		if wait <= 0 {
			wait = backoff
		}
		if time.Until(deadline) < wait {
			if c.readFailure(cl, status) {
				c.reads.down.Store(true)
			}
			return r, status, err
		}
		if !sleep.For(ctx, wait) {
			return r, status, err
		}
		backoff = min(2*backoff, retryMax)
		r, status, wait, err = c.sendOnce(ctx, cl)
	}
	return r, status, err
}

// retryable reports whether a call that failed with status (0 for a
// transport error) may be tried again: a 429 or 503 always, and on a client
// from [Client.RetryingReads] a GET's transport error, while ctx is live, or
// any other 5xx.
func (c *Client) retryable(ctx context.Context, cl call, status int) bool {
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable:
		return true
	case !c.readFailure(cl, status) || c.reads.down.Load():
		return false
	default:
		return status != 0 || ctx.Err() == nil
	}
}

// readFailure reports whether a call that failed with status is a read only a
// [Client.RetryingReads] copy retries: a GET's transport error (status 0) or
// a 5xx other than 503.
func (c *Client) readFailure(cl call, status int) bool {
	return c.reads != nil && cl.method == http.MethodGet &&
		(status == 0 || status >= http.StatusInternalServerError && status != http.StatusServiceUnavailable)
}

// retryAfter reads a Retry-After header: delta seconds or an HTTP date. It
// returns 0 when there is none, or none that parses.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// sendOnce performs cl once. A non-2xx response also returns the wait its
// Retry-After header asks for.
func (c *Client) sendOnce(ctx context.Context, cl call) (rep reply, status int, wait time.Duration, err error) {
	if !cl.noDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	full := c.base + apiPrefix + cl.path
	if cl.root {
		full = c.base + cl.path
	}
	if len(cl.query) > 0 {
		full += "?" + cl.query.Encode()
	}
	var body io.Reader
	if cl.body != nil {
		body = bytes.NewReader(cl.body)
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, full, body)
	if err != nil {
		return reply{}, 0, 0, fmt.Errorf("%s %s: %w", cl.method, cl.path, err)
	}
	if cl.contentType != "" {
		req.Header.Set("Content-Type", cl.contentType)
	}
	if !cl.raw {
		req.Header.Set("Accept", "application/json")
	}
	c.authorize(req, cl)

	resp, err := c.hc.Do(req)
	if err != nil {
		return reply{}, 0, 0, fmt.Errorf("%s %s: %w", cl.method, cl.path, err)
	}
	defer resp.Body.Close()
	limit := int64(maxResponseBytes)
	if cl.maxBytes > 0 {
		limit = cl.maxBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return reply{}, resp.StatusCode, 0, fmt.Errorf("%s %s: read body: %w", cl.method, cl.path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return reply{}, resp.StatusCode, retryAfter(resp.Header), &APIError{
			Status: resp.StatusCode, Method: cl.method, Path: cl.path, Detail: problemDetail(data),
		}
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}, resp.StatusCode, 0, nil
}

// authorize attaches the API key and, when a session exists, its cookie. The
// server's CSRF guard requires an Origin on a cookie-bearing unsafe request.
func (c *Client) authorize(req *http.Request, cl call) {
	if cl.noAuth {
		return
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess == nil {
		return
	}
	req.AddCookie(sess)
	switch cl.method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
	}
}

// problemDetail extracts the RFC 7807 detail (or title) from a body, falling
// back to the body's leading bytes.
func problemDetail(body []byte) string {
	var p struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(body, &p) == nil {
		if p.Detail != "" {
			return p.Detail
		}
		if p.Title != "" {
			return p.Title
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes]
	}
	return s
}

// GetRaw fetches path, relative to the server root and with any query (for
// example /metrics), and returns its body read up to maxBytes. It sends the
// client's credentials, is bounded only by ctx and is never retried, so a
// caller with a deadline of its own, such as a profile request, sets it. A
// non-2xx answer is an *APIError.
func (c *Client) GetRaw(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	r, _, err := c.send(ctx, call{
		method: http.MethodGet, path: path, root: true,
		raw: true, noDeadline: true, noRetry: true, maxBytes: maxBytes,
	})
	if err != nil {
		return nil, err
	}
	return r.body, nil
}

// do performs cl and decodes the response body into out when out is non-nil.
func (c *Client) do(ctx context.Context, cl call, out any) error {
	r, _, err := c.send(ctx, cl)
	if err != nil {
		return err
	}
	if out == nil || len(r.body) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", cl.method, cl.path, err)
	}
	return nil
}

func jsonCall(method, path string, body any) (call, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return call{}, fmt.Errorf("%s %s: encode request: %w", method, path, err)
	}
	return call{method: method, path: path, body: data, contentType: "application/json"}, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	cl, err := jsonCall(method, path, body)
	if err != nil {
		return err
	}
	return c.do(ctx, cl, out)
}

// EachPage reads a paged listing: it calls fetch for successive pages of up
// to limit items, from offset zero, and fn with each page, until a page comes
// back empty or the pages read cover the Total the server reports. It returns
// fetch's first error.
func EachPage[T any](limit int, fetch func(limit, offset int) (Page[T], error), fn func(Page[T])) error {
	for offset := 0; ; {
		p, err := fetch(limit, offset)
		if err != nil {
			return err
		}
		fn(p)
		offset += len(p.Items)
		if len(p.Items) == 0 || offset >= p.Total {
			return nil
		}
	}
}

func pageQuery(q url.Values, limit, offset int) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	return q
}

// ── Setup ────────────────────────────────────────────────────────────────────

// Ready returns nil when the server's readiness probe (GET /readyz) answers 200.
func (c *Client) Ready(ctx context.Context) error {
	// Never retried: a caller polling for readiness keeps its own pace.
	return c.do(ctx, call{method: http.MethodGet, path: "/readyz", root: true, noRetry: true}, nil)
}

// CreateFarm creates a farm (POST /farms) and returns its id.
func (c *Client) CreateFarm(ctx context.Context, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/farms", map[string]any{"name": name}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// CreateQueue creates a queue (POST /queues) with the given concurrency cap
// (0 is unlimited) and returns its id.
func (c *Client) CreateQueue(ctx context.Context, farmID, name string, maxConcurrent int) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{"farm_id": farmID, "name": name, "max_concurrent_tasks": maxConcurrent}
	if err := c.doJSON(ctx, http.MethodPost, "/queues", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// CreateUsagePool creates a usage pool (POST /usage-pools) and returns its id.
func (c *Client) CreateUsagePool(ctx context.Context, name string, limit int) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{"name": name, "max_concurrent": limit}
	if err := c.doJSON(ctx, http.MethodPost, "/usage-pools", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ListUsagePools lists every usage pool (GET /usage-pools). Pools are global,
// not per farm.
func (c *Client) ListUsagePools(ctx context.Context) ([]UsagePool, error) {
	var out []UsagePool
	err := c.do(ctx, call{method: http.MethodGet, path: "/usage-pools"}, &out)
	return out, err
}

// DeleteUsagePool deletes a usage pool (DELETE /usage-pools/{id}).
func (c *Client) DeleteUsagePool(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/usage-pools/" + escape(id)}, nil)
}

// ListFarms lists every farm (GET /farms).
func (c *Client) ListFarms(ctx context.Context) ([]Farm, error) {
	var out []Farm
	err := c.do(ctx, call{method: http.MethodGet, path: "/farms"}, &out)
	return out, err
}

// GetFarm returns a farm (GET /farms/{id}).
func (c *Client) GetFarm(ctx context.Context, id string) (Farm, error) {
	var f Farm
	err := c.do(ctx, call{method: http.MethodGet, path: "/farms/" + escape(id)}, &f)
	return f, err
}

// DeleteFarm deletes a farm (DELETE /farms/{id}). The server refuses while
// the farm still has queues, jobs or workers.
func (c *Client) DeleteFarm(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/farms/" + escape(id)}, nil)
}

// maxQueuePage is the largest page GET /queues serves.
const maxQueuePage = 1000

// ListQueues returns every queue of a farm (GET /queues), reading page by page.
func (c *Client) ListQueues(ctx context.Context, farmID string) ([]Queue, error) {
	var all []Queue
	err := EachPage(maxQueuePage, func(limit, offset int) (Page[Queue], error) {
		var p Page[Queue]
		q := pageQuery(url.Values{"farm_id": {farmID}}, limit, offset)
		err := c.do(ctx, call{method: http.MethodGet, path: "/queues", query: q}, &p)
		return p, err
	}, func(p Page[Queue]) { all = append(all, p.Items...) })
	if err != nil {
		return nil, err
	}
	return all, nil
}

// DeleteQueue deletes a queue (DELETE /queues/{id}).
func (c *Client) DeleteQueue(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/queues/" + escape(id)}, nil)
}

// PauseQueue sets a queue's paused flag. The API has no partial update, so this
// reads the queue and writes it back whole (GET then PUT /queues/{id}); the
// run-as identity keys are left out of the PUT, which preserves them.
func (c *Client) PauseQueue(ctx context.Context, id string, paused bool) error {
	path := "/queues/" + escape(id)
	var q struct {
		FarmID             string `json:"farm_id"`
		Name               string `json:"name"`
		Description        string `json:"description,omitempty"`
		Priority           int    `json:"priority"`
		MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
		Paused             bool   `json:"paused"`
		MaxAttempts        *int   `json:"max_attempts,omitempty"`
		RetryDelaySeconds  *int   `json:"retry_delay_seconds,omitempty"`
		FailureLimit       *int   `json:"failure_limit,omitempty"`
	}
	if err := c.do(ctx, call{method: http.MethodGet, path: path}, &q); err != nil {
		return err
	}
	q.Paused = paused
	return c.doJSON(ctx, http.MethodPut, path, q, nil)
}

// ── Auth ─────────────────────────────────────────────────────────────────────

// Login authenticates with a local account (POST /auth/login). The server
// answers with a session cookie, which the client keeps and sends on later
// requests; Login returns the cookie's value.
func (c *Client) Login(ctx context.Context, user, pass string) (string, error) {
	cl, err := jsonCall(http.MethodPost, "/auth/login", map[string]string{"username": user, "password": pass})
	if err != nil {
		return "", err
	}
	cl.noAuth = true
	r, _, err := c.send(ctx, cl)
	if err != nil {
		return "", err
	}
	sess := sessionCookie((&http.Response{Header: r.header}).Cookies())
	if sess == nil {
		return "", errors.New("POST /auth/login: response set no session cookie")
	}
	c.mu.Lock()
	c.session = &http.Cookie{Name: sess.Name, Value: sess.Value} //nolint:gosec // G124: replayed in requests, never set on a response
	c.mu.Unlock()
	return sess.Value, nil
}

func sessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, ck := range cookies {
		if ck.Name == defaultCookie && ck.Value != "" {
			return ck
		}
	}
	for _, ck := range cookies {
		if ck.Value != "" {
			return ck
		}
	}
	return nil
}

// CreateAPIKey issues an API key for the logged-in account (POST /api-keys) and
// returns its secret, which the server shows once.
func (c *Client) CreateAPIKey(ctx context.Context, name string) (string, error) {
	var out struct {
		Secret string `json:"secret"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api-keys", map[string]string{"name": name}, &out); err != nil {
		return "", err
	}
	if out.Secret == "" {
		return "", errors.New("POST /api-keys: response carried no secret")
	}
	return out.Secret, nil
}

// ── Jobs ─────────────────────────────────────────────────────────────────────

// SubmitJob submits a job (POST /jobs). It is never retried, so a 429 or 503
// answer reaches the caller to be counted. It has no deadline of its own: a large
// job takes the server a long time to create, and giving up would orphan it. The
// caller's ctx bounds it.
// It returns the HTTP status of every
// response so callers can count 422, 429 and 503; a non-2xx status comes with an
// error carrying the server's message, and a transport error returns status 0.
func (c *Client) SubmitJob(ctx context.Context, r SubmitRequest) (jobID string, status int, err error) {
	q := url.Values{"farm_id": {r.FarmID}, "queue_id": {r.QueueID}}
	if r.Priority > 0 {
		q.Set("priority", strconv.Itoa(r.Priority))
	}
	for _, d := range r.DependsOn {
		q.Add("depends_on", d)
	}
	ct := "application/x-yaml"
	if t := bytes.TrimSpace(r.Template); len(t) > 0 && t[0] == '{' {
		ct = "application/json"
	}
	rep, status, err := c.send(ctx, call{method: http.MethodPost, path: "/jobs", query: q, body: r.Template, contentType: ct, noDeadline: true, noRetry: true})
	if err != nil {
		return "", status, err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		return "", status, fmt.Errorf("POST /jobs: decode response: %w", err)
	}
	return out.ID, status, nil
}

// GetJob returns a job with its task counts (GET /jobs/{id}).
func (c *Client) GetJob(ctx context.Context, id string) (Job, error) {
	var j Job
	err := c.do(ctx, call{method: http.MethodGet, path: "/jobs/" + escape(id)}, &j)
	return j, err
}

// ListJobs lists jobs (GET /jobs), optionally of one farm.
func (c *Client) ListJobs(ctx context.Context, farmID string, limit, offset int) (Page[Job], error) {
	q := url.Values{}
	if farmID != "" {
		q.Set("farm_id", farmID)
	}
	var p Page[Job]
	err := c.do(ctx, call{method: http.MethodGet, path: "/jobs", query: pageQuery(q, limit, offset)}, &p)
	return p, err
}

// CancelJob cancels a job (POST /jobs/{id}/cancel).
func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodPost, path: "/jobs/" + escape(id) + "/cancel"}, nil)
}

// DeleteJob deletes a job (DELETE /jobs/{id}), canceling its unfinished tasks
// first.
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/jobs/" + escape(id)}, nil)
}

// RetryJob revives a job's failed and canceled tasks (POST /jobs/{id}/retry).
func (c *Client) RetryJob(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodPost, path: "/jobs/" + escape(id) + "/retry"}, nil)
}

// ── Tasks ────────────────────────────────────────────────────────────────────

// ListTasks lists a job's tasks (GET /jobs/{id}/tasks).
func (c *Client) ListTasks(ctx context.Context, jobID string, limit, offset int) (Page[Task], error) {
	var p Page[Task]
	err := c.do(ctx, call{method: http.MethodGet, path: "/jobs/" + escape(jobID) + "/tasks", query: pageQuery(nil, limit, offset)}, &p)
	return p, err
}

// ListAttempts returns a task's attempts, oldest first (GET /tasks/{id}/attempts).
func (c *Client) ListAttempts(ctx context.Context, taskID string) ([]Attempt, error) {
	var out struct {
		Items []struct {
			AttemptNumber int        `json:"attempt_number"`
			Status        string     `json:"status"`
			WorkerID      string     `json:"worker_id"`
			StartedAt     *time.Time `json:"started_at"`
			EndedAt       *time.Time `json:"ended_at"`
		} `json:"items"`
	}
	if err := c.do(ctx, call{method: http.MethodGet, path: "/tasks/" + escape(taskID) + "/attempts"}, &out); err != nil {
		return nil, err
	}
	attempts := make([]Attempt, len(out.Items))
	for i, it := range out.Items {
		attempts[i] = Attempt{
			ID:            strconv.Itoa(it.AttemptNumber),
			AttemptNumber: it.AttemptNumber,
			WorkerID:      it.WorkerID,
			Status:        it.Status,
			StartedAt:     it.StartedAt,
			EndedAt:       it.EndedAt,
		}
	}
	return attempts, nil
}

// TaskLogs returns the log chunks of a task's latest attempt with nats_seq
// greater than after (GET /tasks/{id}/logs). The returned cursor is the value
// to pass as after next time; on an empty page it equals after.
func (c *Client) TaskLogs(ctx context.Context, taskID string, after int64, limit int) ([]LogLine, int64, error) {
	q := url.Values{"after_nats_seq": {strconv.FormatInt(after, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Items        []LogLine `json:"items"`
		AfterNatsSeq int64     `json:"after_nats_seq"`
	}
	if err := c.do(ctx, call{method: http.MethodGet, path: "/tasks/" + escape(taskID) + "/logs", query: q}, &out); err != nil {
		return nil, after, err
	}
	return out.Items, out.AfterNatsSeq, nil
}

// ── Workers ──────────────────────────────────────────────────────────────────

// ListWorkers lists workers (GET /workers), optionally of one farm.
func (c *Client) ListWorkers(ctx context.Context, farmID string, limit, offset int) (Page[Worker], error) {
	q := url.Values{}
	if farmID != "" {
		q.Set("farm_id", farmID)
	}
	var p Page[Worker]
	err := c.do(ctx, call{method: http.MethodGet, path: "/workers", query: pageQuery(q, limit, offset)}, &p)
	return p, err
}

// RemoveWorker deletes a worker (DELETE /workers/{id}). The server refuses a
// worker that is not offline, or that still has a task assigned or running.
func (c *Client) RemoveWorker(ctx context.Context, id string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/workers/" + escape(id)}, nil)
}

// SetWorkerDisabled disables or enables a worker
// (POST /workers/{id}/disable or /enable).
func (c *Client) SetWorkerDisabled(ctx context.Context, id string, disabled bool) error {
	action := "enable"
	if disabled {
		action = "disable"
	}
	return c.do(ctx, call{method: http.MethodPost, path: "/workers/" + escape(id) + "/" + action}, nil)
}

// Enroll exchanges a join token for a broker credential (POST /workers/enroll).
// The route is authenticated by the token alone, so the client sends no API key
// or session.
func (c *Client) Enroll(ctx context.Context, joinToken, workerID, publicKey, name string) error {
	cl, err := jsonCall(http.MethodPost, "/workers/enroll", map[string]string{
		"join_token": joinToken, "worker_id": workerID, "public_key": publicKey, "name": name,
	})
	if err != nil {
		return err
	}
	cl.noAuth = true
	return c.do(ctx, cl, nil)
}

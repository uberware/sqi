# Diagnostic Logs & Production Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `sqi-server` and `sqi-worker` diagnostic logs visible in production — a bounded, server-held ring buffer surfaced in the web UI (with a task-detail fallback for failures that produce no task output), plus documentation for plugging into external log stacks.

**Architecture:** A fan-out `slog.Handler` writes to stderr as today *and* feeds a sink. On the worker the sink publishes a `DiagLogMsg` to the core-NATS subject `worker.diag.<workerID>` (no JetStream stream — ephemeral, zero storage). The server subscribes to `worker.diag.>` and also feeds its own logs in-process into an in-memory per-component ring buffer (`internal/diag`). A REST endpoint queries the buffer and a WebSocket subject live-tails it. The web UI shows a worker Diagnostics panel, an extensible Admin page with the server log, and a fallback on failed tasks with empty logs.

**Tech Stack:** Go 1.23 (`log/slog`, `nats.go` core pub/sub), chi REST router, existing `internal/ws` hub, React 19 + TypeScript + Vite, TanStack Query, Vitest + React Testing Library.

**Reference spec:** `docs/superpowers/specs/2026-06-17-diagnostic-logs-observability-design.md`

**Conventions reminder (apply to every task):**
- SPDX header `// SPDX-License-Identifier: AGPL-3.0-or-later` (or `/* */` for CSS) as the first line of every new source file, before package/imports.
- Conventional Commits for every commit message.
- Go tests are table-driven (`[]struct{...}` + `t.Run`), run with `-race`. SQLite tests use `t.TempDir()` (not relevant here — no schema changes).
- Never run bare `go test ./...` or lint `./...` from repo root. Use the explicit package path shown in each task, or `make test` / `make lint`.
- Web: strict TS, no `any`, `@/` alias, CSS Modules co-located, colors/spacing only from `src/styles/tokens.css`, data only through `src/api/` + `useWebSocket`.

---

## File structure

**New Go files**
- `internal/worker/protocol/diag.go` — `DiagLogMsg` wire type (worker → server).
- `internal/log/sink.go` — `Sink` interface, `SinkRecord`, fan-out handler, `NewWithSink`.
- `internal/diag/buffer.go` — `Record`, `Filter`, bounded per-component ring `Buffer`.
- `internal/diag/buffer_test.go`
- `internal/diag/server_sink.go` — `Sink` adapter that appends to a `Buffer` for a fixed component.
- `internal/bus/diag.go` — `worker.diag.<id>` subject helper + core-NATS publish/subscribe on `Client`.
- `internal/scheduler/diagingest.go` — server subscriber decoding `DiagLogMsg` into the buffer.
- `internal/api/diagnostics.go` — `GET /api/v1/diagnostics/logs` handler.
- `internal/worker/diaglog/diaglog.go` — worker `Sink` publishing `DiagLogMsg` to NATS.

**Modified Go files**
- `internal/log/log.go` — package doc note (now shared by both binaries).
- `internal/ws/event.go` — `DiagEvent`, `NotifyDiag` on `Notifier`, `NoopNotifier`.
- `internal/ws/hub.go` — `DiagnosticsPush`, `NotifyDiag`, `isValidSubject`.
- `internal/ws/message.go` — `SubjectDiagnostics` constant.
- `internal/config/config.go` + loader/defaults/validate — `DiagnosticsConfig` (server).
- `internal/worker/config/config.go` + loader/defaults/validate — `DiagnosticsConfig` (worker).
- `internal/api/router.go` — register diagnostics route.
- `internal/api/openapi.yaml` — diagnostics endpoint + schemas.
- `internal/server/server.go` (+ `cmd/sqi-server/serve.go`) — build buffer, wire server sink + subscriber + notifier.
- `cmd/sqi-worker/start.go` — build worker sink, gate on config.

**New web files**
- `web/src/api/diagnostics.ts` — `DiagRecord` type, fetch fn, query hook.
- `web/src/ws/diagnostics.ts` — `WsDiagEvent` type + guard.
- `web/src/components/DiagnosticsPanel.tsx` + `.module.css` + test — reusable panel.
- `web/src/pages/Admin.tsx` + `.module.css` + test — extensible admin container, server-log section.

**Modified web files**
- `web/src/api/types.ts` — re-export diagnostics types if the project centralizes them (otherwise skip).
- `web/src/ws/events.ts` — export the diag guard (or keep in `diagnostics.ts`).
- `web/src/routes.tsx` — `/admin` route.
- `web/src/components/layout/Sidebar.tsx` — promote "Admin" from deferred stub to nav item.
- `web/src/pages/WorkerDetail.tsx` (+ test) — embed `DiagnosticsPanel`.
- `web/src/pages/TaskLogPage.tsx` (+ test) — empty-log + failed fallback.

**Docs**
- `docs/observability.md` (new), cross-linked from `docs/operations.md` and `docs/worker-deployment.md`.

---

## Phase A — Shared plumbing

### Task 1: `DiagLogMsg` wire type

**Files:**
- Create: `internal/worker/protocol/diag.go`
- Test: `internal/worker/protocol/diag_test.go`

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestDiagLogMsg_JSONRoundTrip(t *testing.T) {
	in := protocol.DiagLogMsg{
		Ts:    time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
		Level: "ERROR",
		Msg:   "executor: task process error",
		Attrs: map[string]string{"task_id": "t1", "attempt_id": "a1"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out protocol.DiagLogMsg
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Level != in.Level || out.Msg != in.Msg || out.Attrs["task_id"] != "t1" {
		t.Fatalf("round trip mismatch: got %+v", out)
	}
	if !out.Ts.Equal(in.Ts) {
		t.Fatalf("ts mismatch: got %v want %v", out.Ts, in.Ts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/protocol/ -run TestDiagLogMsg_JSONRoundTrip -v`
Expected: FAIL — `undefined: protocol.DiagLogMsg`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import "time"

// DiagLogMsg is a single diagnostic (operational) log record published by
// sqi-worker to the core-NATS subject worker.diag.<workerID>.  It carries the
// worker's own slog output — distinct from task process output, which flows via
// LogChunkMsg on task.logs.<taskID>.
//
// Published with core NATS (not JetStream): delivery is best-effort and nothing
// is retained on the broker.  The server holds a bounded in-memory ring buffer.
type DiagLogMsg struct {
	// Ts is the time the log record was emitted.
	Ts time.Time `json:"ts"`
	// Level is the slog level string: "DEBUG", "INFO", "WARN", or "ERROR".
	Level string `json:"level"`
	// Msg is the log message.
	Msg string `json:"msg"`
	// Attrs holds flattened slog attributes (including correlation keys such as
	// task_id, attempt_id, job_id, session_id) stringified for transport.
	Attrs map[string]string `json:"attrs,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/worker/protocol/ -run TestDiagLogMsg_JSONRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/protocol/diag.go internal/worker/protocol/diag_test.go
git commit -m "feat(protocol): add DiagLogMsg wire type for worker diagnostic logs"
```

---

### Task 2: Fan-out `slog` handler + sink

**Files:**
- Create: `internal/log/sink.go`
- Test: `internal/log/sink_test.go`
- Modify: `internal/log/log.go:14` (package doc note)

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	sqilog "github.com/uberware/sqi/internal/log"
)

type captureSink struct {
	mu   sync.Mutex
	recs []sqilog.SinkRecord
}

func (s *captureSink) Emit(r sqilog.SinkRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
}

func TestNewWithSink_WritesStderrAndSink(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	logger, err := sqilog.NewWithSink("info", "json", &buf, sink)
	if err != nil {
		t.Fatalf("NewWithSink: %v", err)
	}

	logger.With(slog.String("task_id", "t1")).
		ErrorContext(context.Background(), "boom", slog.String("attempt_id", "a1"))

	if !strings.Contains(buf.String(), "boom") {
		t.Fatalf("stderr output missing message: %q", buf.String())
	}
	if len(sink.recs) != 1 {
		t.Fatalf("sink got %d records, want 1", len(sink.recs))
	}
	got := sink.recs[0]
	if got.Level != "ERROR" || got.Msg != "boom" {
		t.Fatalf("sink record = %+v", got)
	}
	if got.Attrs["task_id"] != "t1" || got.Attrs["attempt_id"] != "a1" {
		t.Fatalf("sink attrs = %+v (want With + inline attrs merged)", got.Attrs)
	}
}

func TestNewWithSink_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	sink := &captureSink{}
	logger, _ := sqilog.NewWithSink("warn", "json", &buf, sink)

	logger.InfoContext(context.Background(), "ignored")

	if len(sink.recs) != 0 {
		t.Fatalf("info record reached sink under warn level: %+v", sink.recs)
	}
}

func TestNewWithSink_NilSinkBehavesLikeNew(t *testing.T) {
	var buf bytes.Buffer
	logger, err := sqilog.NewWithSink("info", "text", &buf, nil)
	if err != nil {
		t.Fatalf("NewWithSink nil sink: %v", err)
	}
	logger.InfoContext(context.Background(), "hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("stderr output missing: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/log/ -run TestNewWithSink -v`
Expected: FAIL — `undefined: sqilog.NewWithSink` / `sqilog.SinkRecord`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"context"
	"io"
	"log/slog"
)

// SinkRecord is the flattened form of a slog record delivered to a [Sink].
type SinkRecord struct {
	// Ts is the record time (slog.Record.Time).
	Ts string // RFC3339Nano; see Emit caller. Kept as string to avoid time import churn.
	// Level is the slog level string: "DEBUG", "INFO", "WARN", "ERROR".
	Level string
	// Msg is the log message.
	Msg string
	// Attrs holds all attributes (WithAttrs-accumulated + per-record), stringified.
	Attrs map[string]string
}

// Sink receives every log record that passes the configured level, in addition
// to the normal stderr output.
//
// IMPORTANT: a Sink implementation MUST NOT emit slog records itself (directly
// or indirectly).  Doing so re-enters the fan-out handler and creates an
// infinite logging loop.  Sink errors must be dropped or written straight to a
// raw io.Writer, never routed back through slog.
type Sink interface {
	Emit(r SinkRecord)
}

// fanoutHandler wraps a base slog.Handler, delegating to it for stderr output
// and additionally forwarding each handled record to a Sink.
type fanoutHandler struct {
	base  slog.Handler
	sink  Sink
	attrs []slog.Attr
}

func (h *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.base.Handle(ctx, r)

	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.sink.Emit(SinkRecord{
		Ts:    r.Time.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		Level: r.Level.String(),
		Msg:   r.Message,
		Attrs: attrs,
	})
	return err
}

func (h *fanoutHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(as))
	merged = append(merged, h.attrs...)
	merged = append(merged, as...)
	return &fanoutHandler{base: h.base.WithAttrs(as), sink: h.sink, attrs: merged}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	// Groups are preserved for stderr output but not flattened into sink attrs;
	// diagnostic records are flat key/value for the UI. This is intentional.
	return &fanoutHandler{base: h.base.WithGroup(name), sink: h.sink, attrs: h.attrs}
}

// NewWithSink behaves like [New] (same level/format/destination semantics and
// SetDefault side effect) but, when sink is non-nil, also forwards every record
// that passes the level filter to sink.  A nil sink is identical to [New].
func NewWithSink(level, format string, w io.Writer, sink Sink) (*slog.Logger, error) {
	logger, err := New(level, format, w)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		return logger, nil
	}
	wrapped := slog.New(&fanoutHandler{base: logger.Handler(), sink: sink})
	slog.SetDefault(wrapped)
	return wrapped, nil
}
```

Then update the package doc in `internal/log/log.go` (line 3 area) — change "for sqi-server" to note it is shared:

```go
// Package log provides slog-based structured logging for the sqi binaries.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/log/ -v`
Expected: PASS (all `TestNewWithSink*` and existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/log/sink.go internal/log/sink_test.go internal/log/log.go
git commit -m "feat(log): add fan-out slog handler with sink for diagnostic capture"
```

---

## Phase B — Server-side buffer & ingestion

### Task 3: Diagnostic ring buffer

**Files:**
- Create: `internal/diag/buffer.go`
- Test: `internal/diag/buffer_test.go`

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package diag_test

import (
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/diag"
)

func rec(comp, level, msg string, attrs map[string]string) diag.Record {
	return diag.Record{Ts: time.Now().UTC(), Component: comp, Level: level, Msg: msg, Attrs: attrs}
}

func TestBuffer_EvictsOldestPerComponent(t *testing.T) {
	b := diag.NewBuffer(2, nil)
	b.Append(rec("server", "INFO", "a", nil))
	b.Append(rec("server", "INFO", "b", nil))
	b.Append(rec("server", "INFO", "c", nil))

	got := b.Query(diag.Filter{Component: "server", Limit: 10})
	if len(got) != 2 || got[0].Msg != "b" || got[1].Msg != "c" {
		t.Fatalf("eviction wrong: %+v", got)
	}
}

func TestBuffer_KeysPerComponent(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	b.Append(rec("server", "INFO", "s1", nil))
	b.Append(rec("worker:w1", "INFO", "w1msg", nil))

	if got := b.Query(diag.Filter{Component: "worker:w1", Limit: 10}); len(got) != 1 || got[0].Msg != "w1msg" {
		t.Fatalf("component keying wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{Limit: 10}); len(got) != 2 {
		t.Fatalf("unfiltered query should return all components: %+v", got)
	}
}

func TestBuffer_FiltersByLevelTaskIDAndSince(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	old := diag.Record{Ts: time.Now().Add(-time.Hour), Component: "server", Level: "INFO", Msg: "old"}
	b.Append(old)
	b.Append(rec("server", "ERROR", "err", map[string]string{"task_id": "t1"}))
	b.Append(rec("server", "DEBUG", "dbg", map[string]string{"task_id": "t2"}))

	if got := b.Query(diag.Filter{MinLevel: "WARN", Limit: 10}); len(got) != 1 || got[0].Msg != "err" {
		t.Fatalf("level filter wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{TaskID: "t1", Limit: 10}); len(got) != 1 || got[0].Msg != "err" {
		t.Fatalf("task_id filter wrong: %+v", got)
	}
	if got := b.Query(diag.Filter{Since: time.Now().Add(-time.Minute), Limit: 10}); len(got) != 2 {
		t.Fatalf("since filter wrong: %+v", got)
	}
}

func TestBuffer_LimitReturnsNewest(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	for _, m := range []string{"1", "2", "3"} {
		b.Append(rec("server", "INFO", m, nil))
	}
	got := b.Query(diag.Filter{Component: "server", Limit: 2})
	if len(got) != 2 || got[0].Msg != "2" || got[1].Msg != "3" {
		t.Fatalf("limit should return newest in chronological order: %+v", got)
	}
}

func TestBuffer_AppendInvokesNotify(t *testing.T) {
	var mu sync.Mutex
	var seen []diag.Record
	b := diag.NewBuffer(10, func(r diag.Record) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	})
	b.Append(rec("server", "INFO", "x", nil))
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Msg != "x" {
		t.Fatalf("notify not invoked: %+v", seen)
	}
}

func TestBuffer_ConcurrentAppend(t *testing.T) {
	b := diag.NewBuffer(100, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Append(rec("server", "INFO", "x", nil))
		}()
	}
	wg.Wait()
	if got := b.Query(diag.Filter{Component: "server", Limit: 1000}); len(got) != 50 {
		t.Fatalf("concurrent append lost records: got %d", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/diag/ -v`
Expected: FAIL — `undefined: diag.NewBuffer` etc.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package diag holds the server-side in-memory ring buffer of diagnostic
// (operational) log records gathered from sqi-server itself and from connected
// workers.  It is deliberately bounded and ephemeral: it provides a "recent
// glance" in the web UI, not a durable searchable archive.  The buffer is lost
// on server restart by design (see the design spec, transport choice A1).
package diag

import (
	"sort"
	"sync"
	"time"
)

// Record is one diagnostic log entry held in the buffer.
type Record struct {
	Ts        time.Time         `json:"ts"`
	Component string            `json:"component"` // "server" or "worker:<id>"
	Level     string            `json:"level"`     // DEBUG|INFO|WARN|ERROR
	Msg       string            `json:"msg"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

// Filter selects records in a [Buffer.Query].  Zero-valued fields are ignored.
type Filter struct {
	Component string    // exact component match; empty = all components
	MinLevel  string    // minimum level (DEBUG|INFO|WARN|ERROR); empty = all
	TaskID    string    // match records whose Attrs["task_id"] equals this
	Since     time.Time // only records with Ts after this; zero = no lower bound
	Limit     int       // max records returned (newest kept); <=0 = default 200
}

const defaultLimit = 200

// levelRank maps a level string to an ordinal for MinLevel comparison.
func levelRank(level string) int {
	switch level {
	case "DEBUG":
		return 0
	case "INFO":
		return 1
	case "WARN":
		return 2
	case "ERROR":
		return 3
	default:
		return 1 // treat unknown as INFO
	}
}

// Buffer is a concurrency-safe, per-component bounded ring buffer of [Record].
type Buffer struct {
	perComponent int
	notify       func(Record)

	mu   sync.RWMutex
	rings map[string][]Record // component → chronological slice (len ≤ perComponent)
}

// NewBuffer creates a Buffer retaining up to perComponent records per component.
// notify, if non-nil, is called synchronously for every appended record (used
// to fan out to the WebSocket hub).  notify MUST NOT emit slog records.
func NewBuffer(perComponent int, notify func(Record)) *Buffer {
	if perComponent <= 0 {
		perComponent = 1
	}
	return &Buffer{
		perComponent: perComponent,
		notify:       notify,
		rings:        make(map[string][]Record),
	}
}

// Append stores r under its component, evicting the oldest record for that
// component when the per-component cap is exceeded, then invokes notify.
func (b *Buffer) Append(r Record) {
	b.mu.Lock()
	ring := b.rings[r.Component]
	ring = append(ring, r)
	if len(ring) > b.perComponent {
		ring = ring[len(ring)-b.perComponent:]
	}
	b.rings[r.Component] = ring
	b.mu.Unlock()

	if b.notify != nil {
		b.notify(r)
	}
}

// Query returns matching records in chronological (oldest-first) order, capped
// to Filter.Limit (newest retained when the cap truncates).
func (b *Buffer) Query(f Filter) []Record {
	b.mu.RLock()
	var all []Record
	if f.Component != "" {
		all = append(all, b.rings[f.Component]...)
	} else {
		for _, ring := range b.rings {
			all = append(all, ring...)
		}
	}
	b.mu.RUnlock()

	minRank := -1
	if f.MinLevel != "" {
		minRank = levelRank(f.MinLevel)
	}

	out := all[:0:0]
	for _, r := range all {
		if minRank >= 0 && levelRank(r.Level) < minRank {
			continue
		}
		if f.TaskID != "" && r.Attrs["task_id"] != f.TaskID {
			continue
		}
		if !f.Since.IsZero() && !r.Ts.After(f.Since) {
			continue
		}
		out = append(out, r)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/diag/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/diag/buffer.go internal/diag/buffer_test.go
git commit -m "feat(diag): add bounded per-component diagnostic ring buffer"
```

---

### Task 4: Server-side sink adapter

**Files:**
- Create: `internal/diag/server_sink.go`
- Test: `internal/diag/server_sink_test.go`

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package diag_test

import (
	"testing"

	"github.com/uberware/sqi/internal/diag"
	sqilog "github.com/uberware/sqi/internal/log"
)

func TestServerSink_AppendsWithComponentAndParsesTs(t *testing.T) {
	b := diag.NewBuffer(10, nil)
	sink := diag.NewServerSink(b)

	sink.Emit(sqilog.SinkRecord{
		Ts:    "2026-06-17T12:00:00.000000000Z",
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	})

	got := b.Query(diag.Filter{Component: "server", Limit: 10})
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	if got[0].Level != "ERROR" || got[0].Msg != "boom" || got[0].Attrs["task_id"] != "t1" {
		t.Fatalf("record = %+v", got[0])
	}
	if got[0].Ts.IsZero() {
		t.Fatalf("timestamp not parsed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/diag/ -run TestServerSink -v`
Expected: FAIL — `undefined: diag.NewServerSink`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package diag

import (
	"time"

	sqilog "github.com/uberware/sqi/internal/log"
)

// ServerComponent is the component label for the sqi-server process's own logs.
const ServerComponent = "server"

// serverSink adapts a [sqilog.Sink] to append records to a [Buffer] under the
// fixed "server" component.  It performs no slog calls (loop-safe).
type serverSink struct {
	buf *Buffer
}

// NewServerSink returns a [sqilog.Sink] that appends sqi-server's own log
// records into buf under [ServerComponent].
func NewServerSink(buf *Buffer) sqilog.Sink {
	return &serverSink{buf: buf}
}

func (s *serverSink) Emit(r sqilog.SinkRecord) {
	ts, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", r.Ts)
	if err != nil {
		ts = time.Now().UTC()
	}
	s.buf.Append(Record{
		Ts:        ts,
		Component: ServerComponent,
		Level:     r.Level,
		Msg:       r.Msg,
		Attrs:     r.Attrs,
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/diag/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/diag/server_sink.go internal/diag/server_sink_test.go
git commit -m "feat(diag): add server sink adapter feeding the diagnostic buffer"
```

---

### Task 5: core-NATS diag subject + bus publish/subscribe

**Files:**
- Create: `internal/bus/diag.go`
- Test: `internal/bus/diag_test.go`

Note: `Client.publish` (client.go) uses JetStream. Diagnostics are ephemeral, so this task adds **core-NATS** publish/subscribe via `Client.nc` directly.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package bus_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
)

func TestWorkerDiagSubject(t *testing.T) {
	if got := bus.WorkerDiagSubject("w1"); got != "worker.diag.w1" {
		t.Fatalf("WorkerDiagSubject = %q", got)
	}
}

func TestClient_PublishSubscribeWorkerDiag(t *testing.T) {
	br := bus.NewTestBroker(t) // existing helper used by broker_test.go
	client := br.Client(t)

	var (
		mu      sync.Mutex
		gotSubj string
		gotData []byte
	)
	sub, err := client.SubscribeWorkerDiag(func(subject string, data []byte) {
		mu.Lock()
		gotSubj, gotData = subject, data
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	if err := client.PublishWorkerDiag(context.Background(), "w1", []byte(`{"msg":"hi"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		done := gotData != nil
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("did not receive diag message")
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSubj != "worker.diag.w1" || string(gotData) != `{"msg":"hi"}` {
		t.Fatalf("got subj=%q data=%q", gotSubj, gotData)
	}
}
```

> Before writing the implementation, open `internal/bus/broker_test.go` and confirm the exact names of the test-broker/client helpers (e.g. `NewTestBroker`, `Client`). If they differ, adjust the test above to match the existing helper names — do not invent new helpers.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/bus/ -run 'TestWorkerDiag|TestClient_PublishSubscribeWorkerDiag' -v`
Expected: FAIL — `undefined: bus.WorkerDiagSubject` / `SubscribeWorkerDiag`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"

	nats "github.com/nats-io/nats.go"
)

// SubjectWorkerDiagPrefix is the prefix for worker diagnostic-log subjects.
// Full subject: SubjectWorkerDiagPrefix + "." + workerID.
//
// Unlike task/work/status subjects, diagnostic logs use CORE NATS (not
// JetStream): they are best-effort and never retained on the broker.  The
// server keeps a bounded in-memory ring buffer instead.
const SubjectWorkerDiagPrefix = "worker.diag"

// WorkerDiagSubject returns the full core-NATS subject a worker publishes its
// diagnostic log records to.
func WorkerDiagSubject(workerID string) string {
	return SubjectWorkerDiagPrefix + "." + workerID
}

// PublishWorkerDiag publishes a diagnostic log record to worker.diag.<workerID>
// over core NATS (fire-and-forget, no JetStream ack).  Errors are returned but
// callers should treat them as non-fatal: diagnostics are best-effort.
func (c *Client) PublishWorkerDiag(_ context.Context, workerID string, data []byte) error {
	return c.nc.Publish(WorkerDiagSubject(workerID), data)
}

// SubscribeWorkerDiag subscribes to all worker diagnostic subjects
// (worker.diag.>) over core NATS and invokes handler for each message with the
// concrete subject and raw payload.  The returned subscription must be
// unsubscribed/drained by the caller (the server does this at shutdown).
func (c *Client) SubscribeWorkerDiag(handler func(subject string, data []byte)) (*nats.Subscription, error) {
	return c.nc.Subscribe(SubjectWorkerDiagPrefix+".>", func(m *nats.Msg) {
		handler(m.Subject, m.Data)
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/bus/ -run 'TestWorkerDiag|TestClient_PublishSubscribeWorkerDiag' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bus/diag.go internal/bus/diag_test.go
git commit -m "feat(bus): add core-NATS worker.diag publish/subscribe"
```

---

### Task 6: WebSocket hub diagnostics subject

**Files:**
- Modify: `internal/ws/message.go:88` (add subject constant)
- Modify: `internal/ws/event.go` (`DiagEvent`, `NotifyDiag`, `NoopNotifier`)
- Modify: `internal/ws/hub.go` (`DiagnosticsPush`, `NotifyDiag`, `isValidSubject`)
- Test: `internal/ws/hub_test.go` (add cases)

- [ ] **Step 1: Write the failing test** (append to `internal/ws/hub_test.go`)

```go
func TestHub_NotifyDiag_FansToDiagnosticsSubscribers(t *testing.T) {
	h := ws.NewHub(slog.New(slog.DiscardHandler))
	ch := h.Register("c1")
	if err := h.Subscribe("c1", ws.SubjectDiagnostics, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h.NotifyDiag(ws.DiagEvent{
		Component: "worker:w1",
		Level:     "ERROR",
		Msg:       "boom",
		Attrs:     map[string]string{"task_id": "t1"},
		At:        time.Now().UTC(),
	})

	select {
	case env := <-ch:
		if env.Type != ws.TypePush || env.Subject != ws.SubjectDiagnostics {
			t.Fatalf("envelope = %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("no diagnostics push received")
	}
}

func TestHub_DiagnosticsIsValidSubject(t *testing.T) {
	h := ws.NewHub(slog.New(slog.DiscardHandler))
	h.Register("c1")
	if err := h.Subscribe("c1", ws.SubjectDiagnostics, 0); err != nil {
		t.Fatalf("diagnostics should be a valid subject: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ws/ -run 'TestHub_NotifyDiag|TestHub_DiagnosticsIsValidSubject' -v`
Expected: FAIL — `undefined: ws.SubjectDiagnostics` / `ws.DiagEvent` / `NotifyDiag`.

- [ ] **Step 3: Write minimal implementation**

In `internal/ws/message.go`, add to the Subject constants block:

```go
	// SubjectDiagnostics streams diagnostic (operational) log records from the
	// server and all workers.  Payload is a [DiagnosticsPush]; subscribers
	// filter by component client-side.
	SubjectDiagnostics = "diagnostics"
```

In `internal/ws/event.go`, add the event type and extend the `Notifier` interface + `NoopNotifier`:

```go
// DiagEvent is published when a diagnostic log record is ingested (from the
// server's own logger or a worker's worker.diag stream).  The Hub fans this out
// to clients subscribed to [SubjectDiagnostics].
type DiagEvent struct {
	Component string // "server" or "worker:<id>"
	Level     string // DEBUG|INFO|WARN|ERROR
	Msg       string
	Attrs     map[string]string
	At        time.Time
}
```

Add to the `Notifier` interface:

```go
	NotifyDiag(e DiagEvent)
```

Add to `NoopNotifier`:

```go
// NotifyDiag discards the event.
func (NoopNotifier) NotifyDiag(DiagEvent) {}
```

In `internal/ws/hub.go`, add the push payload near the other `*Push` types:

```go
// DiagnosticsPush is the TypePush payload for [SubjectDiagnostics] subscriptions.
type DiagnosticsPush struct {
	Component string            `json:"component"`
	Level     string            `json:"level"`
	Msg       string            `json:"msg"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	At        time.Time         `json:"at"`
}
```

Add the `NotifyDiag` method near the other `Notify*` methods:

```go
// NotifyDiag fans a diagnostic log record to all SubjectDiagnostics subscribers.
func (h *Hub) NotifyDiag(e DiagEvent) {
	if !h.hasSubscribers(SubjectDiagnostics) {
		return
	}
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	env, err := buildEnvelope(SubjectDiagnostics, DiagnosticsPush{
		Component: e.Component,
		Level:     e.Level,
		Msg:       e.Msg,
		Attrs:     e.Attrs,
		At:        at,
	})
	if err != nil {
		h.logger.WarnContext(context.Background(), "ws: hub: NotifyDiag envelope", slog.Any("error", err))
		return
	}
	h.fanout(SubjectDiagnostics, env)
}
```

In `isValidSubject`, add `SubjectDiagnostics` to the exact-match switch:

```go
	case SubjectJobs, SubjectWorkers, SubjectDiagnostics:
		return true
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ws/ -v`
Expected: PASS (new cases + existing). The `Notifier` interface change will break any other implementers — `NoopNotifier` is updated here; the `Hub` now satisfies it.

- [ ] **Step 5: Commit**

```bash
git add internal/ws/message.go internal/ws/event.go internal/ws/hub.go internal/ws/hub_test.go
git commit -m "feat(ws): add diagnostics subject and NotifyDiag fan-out"
```

---

### Task 7: Server-side diag ingestion (NATS → buffer)

**Files:**
- Create: `internal/scheduler/diagingest.go`
- Test: `internal/scheduler/diagingest_test.go`

This mirrors `logingest.go`. The scheduler already holds `s.bus`, `s.logger`, `s.ctx`. The diag buffer is injected into the scheduler (see Task 8 for wiring; this task adds the field + handler and a focused unit test of the decode path).

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestHandleDiagMessage_AppendsWithWorkerComponent(t *testing.T) {
	buf := diag.NewBuffer(10, nil)
	s := &Scheduler{diagBuf: buf}

	msg := protocol.DiagLogMsg{
		Ts:    time.Now().UTC(),
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	}
	data, _ := json.Marshal(msg)

	s.handleDiagMessage("worker.diag.w1", data)

	got := buf.Query(diag.Filter{Component: "worker:w1", Limit: 10})
	if len(got) != 1 || got[0].Msg != "boom" || got[0].Attrs["task_id"] != "t1" {
		t.Fatalf("record = %+v", got)
	}
}

func TestHandleDiagMessage_IgnoresMalformed(t *testing.T) {
	buf := diag.NewBuffer(10, nil)
	s := &Scheduler{diagBuf: buf}
	s.handleDiagMessage("worker.diag.w1", []byte("not json"))
	if got := buf.Query(diag.Filter{Limit: 10}); len(got) != 0 {
		t.Fatalf("malformed message should be dropped: %+v", got)
	}
}

func TestHandleDiagMessage_NilBufferNoPanic(t *testing.T) {
	s := &Scheduler{} // diagnostics disabled → diagBuf nil
	s.handleDiagMessage("worker.diag.w1", []byte(`{"msg":"x"}`))
}
```

> Before writing, open `internal/scheduler/scheduler.go` and confirm the `Scheduler` struct field names. Add a `diagBuf *diag.Buffer` field (nil when diagnostics disabled). If the package uses a constructor with options, add `diagBuf` there too.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scheduler/ -run TestHandleDiagMessage -v`
Expected: FAIL — `s.diagBuf undefined` / `s.handleDiagMessage undefined`.

- [ ] **Step 3: Write minimal implementation**

Add a `diagBuf *diag.Buffer` field to the `Scheduler` struct in `scheduler.go` (import `"github.com/uberware/sqi/internal/diag"`). Then create `diagingest.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Diagnostic-log ingestion: decodes worker.diag.<id> core-NATS messages into
// the server's in-memory diagnostic ring buffer.  No persistence, no ack — this
// is best-effort recent-history telemetry (design transport choice A1).

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// startDiagConsumer subscribes to worker.diag.> and feeds the buffer.  It is a
// no-op when diagnostics are disabled (diagBuf nil).  Called once from Run.
func (s *Scheduler) startDiagConsumer() error {
	if s.diagBuf == nil {
		return nil
	}
	sub, err := s.bus.SubscribeWorkerDiag(s.handleDiagMessage)
	if err != nil {
		return err
	}
	s.diagSub = sub
	return nil
}

// handleDiagMessage decodes a worker diagnostic record and appends it to the
// buffer under the "worker:<id>" component derived from the subject.
func (s *Scheduler) handleDiagMessage(subject string, data []byte) {
	if s.diagBuf == nil {
		return
	}
	var m protocol.DiagLogMsg
	if err := json.Unmarshal(data, &m); err != nil {
		if s.logger != nil {
			s.logger.DebugContext(s.ctx, "scheduler: malformed worker.diag message", slog.Any("error", err))
		}
		return
	}
	workerID := strings.TrimPrefix(subject, "worker.diag.")
	s.diagBuf.Append(diag.Record{
		Ts:        m.Ts,
		Component: "worker:" + workerID,
		Level:     m.Level,
		Msg:       m.Msg,
		Attrs:     m.Attrs,
	})
}
```

Add the `diagSub *nats.Subscription` field to the `Scheduler` struct (import `nats "github.com/nats-io/nats.go"`), call `s.startDiagConsumer()` alongside `s.startTaskLogsConsumer(ctx)` in `Run`, and unsubscribe `s.diagSub` in the scheduler's shutdown path (where `s.bus` consumers are drained). If the scheduler's `bus` field is a concrete `*bus.Client`, the method set already includes `SubscribeWorkerDiag`; if it is an interface, add `SubscribeWorkerDiag(func(string, []byte)) (*nats.Subscription, error)` to that interface.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/scheduler/ -run TestHandleDiagMessage -v`
Expected: PASS. Then `go test -race ./internal/scheduler/` to confirm nothing else broke.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/diagingest.go internal/scheduler/diagingest_test.go internal/scheduler/scheduler.go
git commit -m "feat(scheduler): ingest worker.diag messages into diagnostic buffer"
```

---

## Phase C — Config & REST

### Task 8: Server diagnostics config

**Files:**
- Modify: `internal/config/config.go` (add `DiagnosticsConfig` + field on root `Config`)
- Modify: `internal/config/loader.go` (defaults + file merge + env)
- Modify: `internal/config/validate.go` (validation)
- Test: `internal/config/config_test.go` (add cases)

- [ ] **Step 1: Write the failing test** (add to `config_test.go`)

```go
func TestLoad_DiagnosticsDefaults(t *testing.T) {
	cfg, err := config.Load(config.Options{}) // match existing Load signature
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Diagnostics.Enabled {
		t.Fatal("diagnostics should default to enabled")
	}
	if cfg.Diagnostics.BufferSize <= 0 {
		t.Fatalf("buffer size default should be positive, got %d", cfg.Diagnostics.BufferSize)
	}
}

func TestLoad_DiagnosticsEnvOverride(t *testing.T) {
	t.Setenv("SQI_DIAGNOSTICS_ENABLED", "false")
	t.Setenv("SQI_DIAGNOSTICS_BUFFER_SIZE", "500")
	cfg, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Diagnostics.Enabled {
		t.Fatal("env should disable diagnostics")
	}
	if cfg.Diagnostics.BufferSize != 500 {
		t.Fatalf("buffer size = %d", cfg.Diagnostics.BufferSize)
	}
}
```

> Open `internal/config/config_test.go` first and match the existing `config.Load` call convention used by the other tests (e.g. how they pass file path / args). Adjust the two calls above accordingly.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad_Diagnostics -v`
Expected: FAIL — `cfg.Diagnostics undefined`.

- [ ] **Step 3: Write minimal implementation**

In `config.go`, add the struct and a field on the root `Config` (place the field next to `Log LogConfig`):

```go
// DiagnosticsConfig controls the in-memory diagnostic-log ring buffer surfaced
// in the web UI.  Disable it to avoid spending memory on something accessed
// out-of-band (journald/Docker/Loki/etc.).
type DiagnosticsConfig struct {
	// Enabled turns the server-side ring buffer and worker.diag subscription
	// on or off.  Default: true.
	// Env: SQI_DIAGNOSTICS_ENABLED
	Enabled bool `yaml:"enabled"`

	// BufferSize is the maximum diagnostic records retained per component
	// (server + each worker).  Must be > 0.  Default: 1000.
	// Env: SQI_DIAGNOSTICS_BUFFER_SIZE
	BufferSize int `yaml:"buffer_size"`
}
```

```go
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`
```

In `loader.go`: locate where `Log` defaults are set and add diagnostics defaults `Enabled: true, BufferSize: 1000`; locate `mergeLogFile` and add an analogous `mergeDiagnosticsFile` (only overriding when the file specifies values); locate the env-binding section (where `SQI_LOG_LEVEL` is read) and bind `SQI_DIAGNOSTICS_ENABLED` (parse bool) and `SQI_DIAGNOSTICS_BUFFER_SIZE` (parse int). Follow the exact mechanism the file already uses for `Log` — read it before editing.

In `validate.go`: add `if cfg.Diagnostics.Enabled && cfg.Diagnostics.BufferSize <= 0 { return fmt.Errorf("diagnostics.buffer_size must be > 0") }`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): add server diagnostics buffer configuration"
```

---

### Task 9: Diagnostics REST endpoint

**Files:**
- Create: `internal/api/diagnostics.go`
- Test: `internal/api/diagnostics_test.go`
- Modify: `internal/api/router.go` (route + handler wiring)
- Modify: `internal/api/openapi.yaml`

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/diag"
)

func TestDiagnosticsLogs_FiltersAndReturnsJSON(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	buf.Append(diag.Record{Ts: time.Now().UTC(), Component: "server", Level: "INFO", Msg: "s"})
	buf.Append(diag.Record{Ts: time.Now().UTC(), Component: "worker:w1", Level: "ERROR", Msg: "e", Attrs: map[string]string{"task_id": "t1"}})

	h := api.NewDiagnosticsHandler(buf) // returns http.Handler / chi-mountable

	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/logs?component=worker:w1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Records []diag.Record `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Records) != 1 || body.Records[0].Msg != "e" {
		t.Fatalf("records = %+v", body.Records)
	}
}

func TestDiagnosticsLogs_TaskIDFilter(t *testing.T) {
	buf := diag.NewBuffer(100, nil)
	buf.Append(diag.Record{Ts: time.Now().UTC(), Component: "worker:w1", Level: "ERROR", Msg: "match", Attrs: map[string]string{"task_id": "t1"}})
	buf.Append(diag.Record{Ts: time.Now().UTC(), Component: "worker:w1", Level: "ERROR", Msg: "nope", Attrs: map[string]string{"task_id": "t2"}})

	h := api.NewDiagnosticsHandler(buf)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/logs?task_id=t1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body struct {
		Records []diag.Record `json:"records"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Records) != 1 || body.Records[0].Msg != "match" {
		t.Fatalf("task_id filter records = %+v", body.Records)
	}
}
```

> Open one existing handler test (e.g. `internal/api/workers_test.go`) to match how handlers are constructed/mounted and how JSON responses are written in this codebase. Mirror that style; the `NewDiagnosticsHandler(buf)` shape below is the target but adapt to the project's actual handler/router idiom (it may attach to a central `Handlers` struct rather than a standalone constructor).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestDiagnosticsLogs -v`
Expected: FAIL — `undefined: api.NewDiagnosticsHandler`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/uberware/sqi/internal/diag"
)

// DiagReader is the read surface the diagnostics handler needs.  *diag.Buffer
// satisfies it; tests can substitute a fake.
type DiagReader interface {
	Query(diag.Filter) []diag.Record
}

type diagnosticsHandler struct {
	reader DiagReader
}

// NewDiagnosticsHandler returns an http.Handler serving GET
// /api/v1/diagnostics/logs from the given diagnostic buffer.  Pass nil when
// diagnostics are disabled — the handler then returns 503.
func NewDiagnosticsHandler(reader DiagReader) http.Handler {
	return &diagnosticsHandler{reader: reader}
}

const maxDiagLimit = 1000

func (h *diagnosticsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.reader == nil {
		http.Error(w, "diagnostics disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()

	f := diag.Filter{
		Component: q.Get("component"),
		MinLevel:  q.Get("level"),
		TaskID:    q.Get("task_id"),
		Limit:     maxDiagLimit,
	}
	if v := q.Get("since"); v != "" {
		if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
			f.Since = ts
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxDiagLimit {
				n = maxDiagLimit
			}
			f.Limit = n
		}
	}

	records := h.reader.Query(f)
	if records == nil {
		records = []diag.Record{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Records []diag.Record `json:"records"`
	}{Records: records})
}
```

In `router.go`, mount it under the v1 group (matching the existing mounting idiom), passing the buffer through from server boot (Task 10). When diagnostics are disabled the server passes `nil` and the handler returns 503.

In `openapi.yaml`, add:
- path `/diagnostics/logs` (GET) with query params `component`, `level`, `task_id`, `since`, `limit`;
- a `DiagnosticRecord` schema (`ts`, `component`, `level`, `msg`, `attrs`) and a `DiagnosticLogsResponse` schema (`records: [DiagnosticRecord]`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestDiagnosticsLogs -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/diagnostics.go internal/api/diagnostics_test.go internal/api/router.go internal/api/openapi.yaml
git commit -m "feat(api): add GET /diagnostics/logs endpoint"
```

---

## Phase D — Wiring

### Task 10: Wire server boot

**Files:**
- Modify: `cmd/sqi-server/serve.go:75` (build logger with sink)
- Modify: `internal/server/server.go` (construct buffer, pass notifier callback, inject into scheduler + diagnostics handler, drain subscription)

This task has no new unit test of its own logic (it is wiring); it is validated by the smoke test and existing server boot tests. Build + `make smoke` are the gates.

- [ ] **Step 1: Build the buffer and wire it (server.go)**

In server boot, after the `Hub` is created and before the scheduler is started:

```go
var diagBuf *diag.Buffer
if cfg.Diagnostics.Enabled {
	diagBuf = diag.NewBuffer(cfg.Diagnostics.BufferSize, func(r diag.Record) {
		hub.NotifyDiag(ws.DiagEvent{
			Component: r.Component,
			Level:     r.Level,
			Msg:       r.Msg,
			Attrs:     r.Attrs,
			At:        r.Ts,
		})
	})
}
```

Pass `diagBuf` into the scheduler constructor (the `diagBuf` field added in Task 7; nil when disabled) and into the diagnostics route (Task 9): `NewDiagnosticsHandler(diagBuf)` — a nil `*diag.Buffer` satisfies `DiagReader` but the handler nil-checks the interface, so pass `nil` explicitly when disabled:

```go
var diagReader api.DiagReader
if diagBuf != nil {
	diagReader = diagBuf
}
// ... pass diagReader to the router/handlers
```

- [ ] **Step 2: Build the logger with the server sink (serve.go)**

Replace the logger construction at `cmd/sqi-server/serve.go:75`. The buffer must exist before the logger so the sink can feed it. Restructure so the buffer is created in `serve.go` (or returned from an early server-setup step) and passed both to `NewWithSink` and into server boot. Concretely:

```go
// Build the diagnostic buffer first (nil when disabled) so the logger sink can
// feed it from the very first log line.
var diagBuf *diag.Buffer
if cfg.Diagnostics.Enabled {
	diagBuf = diag.NewBuffer(cfg.Diagnostics.BufferSize, nil) // notify wired after hub exists
}

var sink sqilog.Sink
if diagBuf != nil {
	sink = diag.NewServerSink(diagBuf)
}
logger, err := sqilog.NewWithSink(cfg.Log.Level, cfg.Log.Format, os.Stderr, sink)
```

Because the hub does not exist yet at logger-construction time, set the buffer's notify callback after the hub is built. Add a setter to `diag.Buffer`:

```go
// SetNotify sets (or replaces) the per-append notify callback.  Safe to call
// once during boot before heavy traffic; it is guarded by the buffer mutex.
func (b *Buffer) SetNotify(fn func(Record)) {
	b.mu.Lock()
	b.notify = fn
	b.mu.Unlock()
}
```

(Add a small test for `SetNotify` to `internal/diag/buffer_test.go` following the `TestBuffer_AppendInvokesNotify` pattern, then commit it with Task 3's file — or include here.) Then in server boot call `diagBuf.SetNotify(func(r diag.Record){ hub.NotifyDiag(...) })`.

- [ ] **Step 3: Drain the subscription on shutdown**

Ensure the scheduler's shutdown unsubscribes `s.diagSub` (added in Task 7). No separate action if Task 7 already wired it into the drain path; verify by reading the scheduler shutdown code.

- [ ] **Step 4: Build and smoke-test**

Run:
```bash
make build
make smoke
```
Expected: build succeeds; smoke passes. Manually verify (optional): start the server, `curl localhost:8080/api/v1/diagnostics/logs` returns `{"records":[...]}` containing server log lines.

- [ ] **Step 5: Commit**

```bash
git add cmd/sqi-server/serve.go internal/server/server.go internal/diag/buffer.go internal/diag/buffer_test.go
git commit -m "feat(server): wire diagnostic buffer, server sink, and notifier"
```

---

### Task 11: Worker diag sink + config + wiring

**Files:**
- Create: `internal/worker/diaglog/diaglog.go`
- Test: `internal/worker/diaglog/diaglog_test.go`
- Modify: `internal/worker/config/config.go` (+ loader/defaults/validate) — `DiagnosticsConfig{Enabled bool}`
- Modify: `cmd/sqi-worker/start.go:87` (build logger with sink when enabled)

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package diaglog_test

import (
	"encoding/json"
	"sync"
	"testing"

	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/diaglog"
	"github.com/uberware/sqi/internal/worker/protocol"
)

type fakePublisher struct {
	mu   sync.Mutex
	subj string
	data []byte
	err  error
}

func (f *fakePublisher) Publish(subj string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subj, f.data = subj, data
	return f.err
}

func TestPublisher_Emit_PublishesDiagLogMsg(t *testing.T) {
	fp := &fakePublisher{}
	p := diaglog.New(fp, "w1")

	p.Emit(sqilog.SinkRecord{
		Ts:    "2026-06-17T12:00:00.000000000Z",
		Level: "ERROR",
		Msg:   "boom",
		Attrs: map[string]string{"task_id": "t1"},
	})

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.subj != "worker.diag.w1" {
		t.Fatalf("subject = %q", fp.subj)
	}
	var msg protocol.DiagLogMsg
	if err := json.Unmarshal(fp.data, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Level != "ERROR" || msg.Msg != "boom" || msg.Attrs["task_id"] != "t1" {
		t.Fatalf("msg = %+v", msg)
	}
}

func TestPublisher_Emit_PublishErrorDropped(t *testing.T) {
	fp := &fakePublisher{err: assertErr{}}
	p := diaglog.New(fp, "w1")
	// Must not panic or block; error is swallowed (no slog calls → no recursion).
	p.Emit(sqilog.SinkRecord{Ts: "2026-06-17T12:00:00.000000000Z", Level: "INFO", Msg: "x"})
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/worker/diaglog/ -v`
Expected: FAIL — `undefined: diaglog.New`.

- [ ] **Step 3: Write minimal implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package diaglog implements the sqi-worker diagnostic-log sink.  It adapts a
// [sqilog.Sink] to publish each of the worker's own slog records as a
// [protocol.DiagLogMsg] on the core-NATS subject worker.diag.<workerID>.
//
// All publishing is best-effort: marshal or publish errors are dropped.  The
// sink performs NO slog calls — doing so would re-enter the fan-out handler and
// loop.  Diagnostics are recent-history telemetry, not a guaranteed channel.
package diaglog

import (
	"encoding/json"
	"time"

	"github.com/uberware/sqi/internal/bus"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// natsPublisher is the minimal subset of *nats.Conn the Publisher uses.
type natsPublisher interface {
	Publish(subject string, data []byte) error
}

// Publisher is a [sqilog.Sink] that ships worker diagnostic logs over NATS.
type Publisher struct {
	nc      natsPublisher
	subject string
}

// New returns a Publisher that publishes to worker.diag.<workerID>.
func New(nc natsPublisher, workerID string) *Publisher {
	return &Publisher{nc: nc, subject: bus.WorkerDiagSubject(workerID)}
}

// Emit serializes r and publishes it.  Errors are intentionally dropped.
func (p *Publisher) Emit(r sqilog.SinkRecord) {
	ts, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", r.Ts)
	if err != nil {
		ts = time.Now().UTC()
	}
	data, err := json.Marshal(protocol.DiagLogMsg{
		Ts:    ts,
		Level: r.Level,
		Msg:   r.Msg,
		Attrs: r.Attrs,
	})
	if err != nil {
		return
	}
	_ = p.nc.Publish(p.subject, data)
}
```

Add worker `DiagnosticsConfig{Enabled bool}` to `internal/worker/config/config.go` (field `Diagnostics`, env `SQI_DIAGNOSTICS_ENABLED`, default true) following the existing worker `LogConfig` pattern (defaults/file/env/validate). Add a config test mirroring `internal/worker/config/config_test.go` style.

In `cmd/sqi-worker/start.go`, the worker must connect to NATS before building the diag sink (the sink needs `nc`). Read the current ordering: `sqilog.New(...)` is at line 87, likely before `natsclient.Connect`. Restructure so:
1. Build a bootstrap logger first (no sink) for early startup messages, OR
2. Build the NATS connection, then rebuild the logger with the sink.

Recommended minimal approach: build the logger without a sink initially (as today), connect to NATS, then if `cfg.Diagnostics.Enabled`, call `sqilog.NewWithSink(cfg.Log.Level, cfg.Log.Format, os.Stderr, diaglog.New(nc, workerID))` and reassign the logger used by the rest of the worker. Ensure the `workerID` is known at that point (it is established during registration; if the ID is assigned later, use the worker's stable configured name/ID available at connect time — read `registration` to confirm the ID source and use the same value the worker reports).

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
go test -race ./internal/worker/diaglog/ ./internal/worker/config/ -v
make build
```
Expected: tests PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/diaglog/ internal/worker/config/ cmd/sqi-worker/start.go
git commit -m "feat(worker): publish diagnostic logs to worker.diag over NATS"
```

---

## Phase E — Web UI

> All web tasks must pass the gate before committing:
> `cd web && npm run format:check && npm run typecheck && npm run lint && npm run test:coverage`
> Verify each new component renders correctly in **both** light and dark themes (toggle via the existing `ThemeToggle`; tests should not hardcode colors — use only `tokens.css` variables in CSS).

### Task 12: Diagnostics API client + WS event types

**Files:**
- Create: `web/src/api/diagnostics.ts`
- Create: `web/src/ws/diagnostics.ts`
- Test: `web/src/api/diagnostics.test.ts`

- [ ] **Step 1: Write the failing test**

```ts
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi, afterEach } from 'vitest'
import { fetchDiagnosticsLogs } from '@/api/diagnostics'

afterEach(() => vi.restoreAllMocks())

describe('fetchDiagnosticsLogs', () => {
  it('builds the query string and returns records', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ records: [{ ts: '2026-06-17T12:00:00Z', component: 'server', level: 'INFO', msg: 'hi' }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const out = await fetchDiagnosticsLogs({ component: 'worker:w1', level: 'WARN', limit: 50 })

    expect(out.records).toHaveLength(1)
    expect(out.records[0]?.msg).toBe('hi')
    const url = spy.mock.calls[0]?.[0] as string
    expect(url).toContain('/api/v1/diagnostics/logs?')
    expect(url).toContain('component=worker%3Aw1')
    expect(url).toContain('level=WARN')
    expect(url).toContain('limit=50')
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/api/diagnostics.test.ts`
Expected: FAIL — cannot resolve `@/api/diagnostics`.

- [ ] **Step 3: Write minimal implementation**

`web/src/api/diagnostics.ts`:

```ts
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'

/** A single diagnostic log record (mirrors internal/diag.Record). */
export interface DiagRecord {
  ts: string
  component: string
  level: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR'
  msg: string
  attrs?: Record<string, string>
}

export interface DiagnosticLogsResponse {
  records: DiagRecord[]
}

export interface DiagnosticsParams {
  component?: string
  level?: string
  task_id?: string
  since?: string
  limit?: number
}

function buildQuery(params: DiagnosticsParams): string {
  const qs = new URLSearchParams()
  if (params.component) qs.set('component', params.component)
  if (params.level) qs.set('level', params.level)
  if (params.task_id) qs.set('task_id', params.task_id)
  if (params.since) qs.set('since', params.since)
  if (params.limit != null) qs.set('limit', String(params.limit))
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export function fetchDiagnosticsLogs(params: DiagnosticsParams = {}): Promise<DiagnosticLogsResponse> {
  return apiFetch<DiagnosticLogsResponse>(`/diagnostics/logs${buildQuery(params)}`)
}

export const diagnosticsQueryKey = (params: DiagnosticsParams) => ['diagnostics', params] as const

export function useDiagnosticsLogs(params: DiagnosticsParams = {}) {
  return useQuery({
    queryKey: diagnosticsQueryKey(params),
    queryFn: () => fetchDiagnosticsLogs(params),
  })
}
```

`web/src/ws/diagnostics.ts`:

```ts
// SPDX-License-Identifier: AGPL-3.0-or-later

import type { DiagRecord } from '@/api/diagnostics'

/** Payload received on the "diagnostics" subject for each diagnostic record. */
export interface WsDiagEvent {
  component: string
  level: DiagRecord['level']
  msg: string
  attrs?: Record<string, string>
  at: string
}

/** Returns true when payload is a WsDiagEvent (has component, level, msg). */
export function isDiagEvent(payload: unknown): payload is WsDiagEvent {
  return (
    typeof payload === 'object' &&
    payload !== null &&
    'component' in payload &&
    'level' in payload &&
    'msg' in payload
  )
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npx vitest run src/api/diagnostics.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/api/diagnostics.ts web/src/ws/diagnostics.ts web/src/api/diagnostics.test.ts
git commit -m "feat(web): add diagnostics API client and WS event types"
```

---

### Task 13: Reusable DiagnosticsPanel component

**Files:**
- Create: `web/src/components/DiagnosticsPanel.tsx`
- Create: `web/src/components/DiagnosticsPanel.module.css`
- Test: `web/src/components/DiagnosticsPanel.test.tsx`

- [ ] **Step 1: Write the failing test**

```tsx
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import * as api from '@/api/diagnostics'

vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))

function renderPanel(component: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <DiagnosticsPanel component={component} title="Server log" />
    </QueryClientProvider>,
  )
}

describe('DiagnosticsPanel', () => {
  it('renders fetched records with level and message', async () => {
    vi.spyOn(api, 'fetchDiagnosticsLogs').mockResolvedValue({
      records: [
        { ts: '2026-06-17T12:00:00Z', component: 'server', level: 'ERROR', msg: 'process not found' },
      ],
    })
    renderPanel('server')
    expect(await screen.findByText('process not found')).toBeInTheDocument()
    expect(screen.getByText('ERROR')).toBeInTheDocument()
  })

  it('shows an empty state when there are no records', async () => {
    vi.spyOn(api, 'fetchDiagnosticsLogs').mockResolvedValue({ records: [] })
    renderPanel('server')
    expect(await screen.findByText(/no diagnostic logs/i)).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/components/DiagnosticsPanel.test.tsx`
Expected: FAIL — cannot resolve `@/components/DiagnosticsPanel`.

- [ ] **Step 3: Write minimal implementation**

`DiagnosticsPanel.tsx`:

```tsx
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { useDiagnosticsLogs, type DiagRecord } from '@/api/diagnostics'
import { useWebSocket } from '@/ws/context'
import { isDiagEvent } from '@/ws/diagnostics'
import styles from './DiagnosticsPanel.module.css'

interface Props {
  /** Component filter, e.g. "server" or "worker:w1". */
  component: string
  /** Heading shown above the log lines. */
  title: string
  /** Optional task_id filter (used by the task-detail fallback). */
  taskId?: string
}

export default function DiagnosticsPanel({ component, title, taskId }: Props) {
  const params = taskId ? { component, task_id: taskId } : { component }
  const { data, isLoading, isError } = useDiagnosticsLogs(params)
  const [live, setLive] = useState<DiagRecord[]>([])

  const onMessage = useCallback(
    (payload: unknown) => {
      if (!isDiagEvent(payload)) return
      if (payload.component !== component) return
      if (taskId && payload.attrs?.['task_id'] !== taskId) return
      setLive((prev) => [
        ...prev.slice(-499),
        { ts: payload.at, component: payload.component, level: payload.level, msg: payload.msg, attrs: payload.attrs },
      ])
    },
    [component, taskId],
  )
  useWebSocket('diagnostics', onMessage)

  const records = [...(data?.records ?? []), ...live]

  return (
    <section className={styles.panel} aria-label={title}>
      <h3 className={styles.title}>{title}</h3>
      {isLoading && <p className={styles.muted}>Loading…</p>}
      {isError && <p className={styles.muted}>Diagnostics unavailable.</p>}
      {!isLoading && !isError && records.length === 0 && (
        <p className={styles.muted}>No diagnostic logs.</p>
      )}
      {records.length > 0 && (
        <ol className={styles.lines} role="log">
          {records.map((r, i) => (
            <li key={`${r.ts}-${i}`} className={styles.line} data-level={r.level}>
              <time className={styles.ts} dateTime={r.ts}>
                {new Date(r.ts).toLocaleTimeString()}
              </time>
              <span className={styles.level} data-level={r.level}>
                {r.level}
              </span>
              <span className={styles.msg}>{r.msg}</span>
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}
```

`DiagnosticsPanel.module.css` — colors strictly from tokens so both themes work:

```css
/* SPDX-License-Identifier: AGPL-3.0-or-later */

.panel {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md, 6px);
  padding: var(--space-3);
}

.title {
  font-size: var(--font-size-lg);
  margin: 0 0 var(--space-2);
  color: var(--color-text-primary);
}

.muted {
  color: var(--color-text-muted);
  font-size: var(--font-size-sm);
}

.lines {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 24rem;
  overflow-y: auto;
  font-family: var(--font-family-mono);
  font-size: var(--font-size-xs);
}

.line {
  display: grid;
  grid-template-columns: auto 4rem 1fr;
  gap: var(--space-2);
  padding: var(--space-1) var(--space-2);
  border-bottom: 1px solid var(--color-border-subtle);
  color: var(--color-text-primary);
}

.ts {
  color: var(--color-text-muted);
}

.level {
  font-weight: var(--font-weight-semibold);
}
.level[data-level='ERROR'] {
  color: var(--color-error);
}
.level[data-level='WARN'] {
  color: var(--color-warning);
}
.level[data-level='INFO'] {
  color: var(--color-info);
}
.level[data-level='DEBUG'] {
  color: var(--color-text-muted);
}

.msg {
  white-space: pre-wrap;
  word-break: break-word;
}
```

> If `--radius-md` does not exist in `tokens.css`, read the file and use the actual radius token (or drop the fallback). Do not introduce new token names.

- [ ] **Step 4: Run test + gate**

Run: `cd web && npx vitest run src/components/DiagnosticsPanel.test.tsx`
Expected: PASS. Then run the full web gate.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/DiagnosticsPanel.tsx web/src/components/DiagnosticsPanel.module.css web/src/components/DiagnosticsPanel.test.tsx
git commit -m "feat(web): add reusable DiagnosticsPanel component"
```

---

### Task 14: Worker detail Diagnostics panel

**Files:**
- Modify: `web/src/pages/WorkerDetail.tsx`
- Modify: `web/src/pages/WorkerDetail.test.tsx`

- [ ] **Step 1: Write the failing test** (add to `WorkerDetail.test.tsx`)

```tsx
it('renders a diagnostics panel for the worker', async () => {
  // Reuse the file's existing render helper + worker fixture (worker id "w1").
  // Mock fetchDiagnosticsLogs to return one record.
  vi.spyOn(diagApi, 'fetchDiagnosticsLogs').mockResolvedValue({
    records: [{ ts: '2026-06-17T12:00:00Z', component: 'worker:w1', level: 'INFO', msg: 'worker diag line' }],
  })
  renderWorkerDetail('w1') // use the file's existing helper name
  expect(await screen.findByText('worker diag line')).toBeInTheDocument()
})
```

> Open `WorkerDetail.test.tsx` and reuse its existing render helper and worker fixture; import `* as diagApi from '@/api/diagnostics'`. Adjust the worker id to match the fixture.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/pages/WorkerDetail.test.tsx`
Expected: FAIL — no diagnostics text rendered.

- [ ] **Step 3: Write minimal implementation**

In `WorkerDetail.tsx`, import the panel and render it within the page, keyed to the worker:

```tsx
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
// ... inside the component, where the worker id is available as `id`:
<DiagnosticsPanel component={`worker:${id}`} title="Worker diagnostics" />
```

- [ ] **Step 4: Run test + gate**

Run: `cd web && npx vitest run src/pages/WorkerDetail.test.tsx` then the full web gate.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/WorkerDetail.tsx web/src/pages/WorkerDetail.test.tsx
git commit -m "feat(web): show diagnostics panel on worker detail page"
```

---

### Task 15: Extensible Admin page + nav + route

**Files:**
- Create: `web/src/pages/Admin.tsx`
- Create: `web/src/pages/Admin.module.css`
- Create: `web/src/pages/Admin.test.tsx`
- Modify: `web/src/routes.tsx`
- Modify: `web/src/components/layout/Sidebar.tsx`

- [ ] **Step 1: Write the failing test**

```tsx
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import Admin from '@/pages/Admin'
import * as api from '@/api/diagnostics'

vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))

describe('Admin page', () => {
  it('renders the server log section', async () => {
    vi.spyOn(api, 'fetchDiagnosticsLogs').mockResolvedValue({
      records: [{ ts: '2026-06-17T12:00:00Z', component: 'server', level: 'INFO', msg: 'server up' }],
    })
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <Admin />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(screen.getByRole('heading', { name: /admin/i })).toBeInTheDocument()
    expect(await screen.findByText('server up')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/pages/Admin.test.tsx`
Expected: FAIL — cannot resolve `@/pages/Admin`.

- [ ] **Step 3: Write minimal implementation**

`Admin.tsx` — built as a section registry so future admin tools slot in without restructuring:

```tsx
// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import styles from './Admin.module.css'

interface AdminSection {
  id: string
  label: string
  render: () => ReactNode
}

// Extensible registry — add future admin tools (settings, licensing, etc.) here.
const SECTIONS: AdminSection[] = [
  {
    id: 'server-log',
    label: 'Server log',
    render: () => <DiagnosticsPanel component="server" title="Server log" />,
  },
]

export default function Admin() {
  return (
    <div className={styles.page}>
      <h1 className={styles.heading}>Admin</h1>
      {SECTIONS.map((s) => (
        <section key={s.id} className={styles.section} aria-label={s.label}>
          {s.render()}
        </section>
      ))}
    </div>
  )
}
```

`Admin.module.css`:

```css
/* SPDX-License-Identifier: AGPL-3.0-or-later */

.page {
  padding: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.heading {
  font-size: var(--font-size-2xl);
  color: var(--color-text-primary);
  margin: 0;
}

.section {
  width: 100%;
}
```

In `routes.tsx`, add the import and route:

```tsx
import Admin from '@/pages/Admin'
// ...
<Route path="/admin" element={<Admin />} />
```

In `Sidebar.tsx`, remove `'Admin'` from `DEFERRED_LABELS` and add it to `PHASE1_NAV`:

```tsx
const DEFERRED_LABELS = ['Presets', 'Products']
// ...
{ label: 'Storage', to: '/storage-locations' },
{ label: 'Admin', to: '/admin' },
```

Update `Sidebar.test.tsx` if it asserts the exact deferred/active label sets (read it first; adjust the expected arrays so "Admin" is now an active nav link, not a "coming soon" stub).

- [ ] **Step 4: Run test + gate**

Run: `cd web && npx vitest run src/pages/Admin.test.tsx src/components/layout/Sidebar.test.tsx` then full web gate.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/Admin.tsx web/src/pages/Admin.module.css web/src/pages/Admin.test.tsx web/src/routes.tsx web/src/components/layout/Sidebar.tsx web/src/components/layout/Sidebar.test.tsx
git commit -m "feat(web): add extensible Admin page with server log section"
```

---

### Task 16: Task-detail fallback (empty log + failed)

**Files:**
- Modify: `web/src/pages/TaskLogPage.tsx`
- Modify: `web/src/pages/TaskLogPage.test.tsx`

The fallback: when a task is `failed` and has no task-log output, render a `DiagnosticsPanel` filtered by `task_id` (worker diagnostics for that attempt) plus the terminal failure reason.

- [ ] **Step 1: Write the failing test** (add to `TaskLogPage.test.tsx`)

```tsx
it('falls back to worker diagnostics when a failed task has no logs', async () => {
  // Use the file's existing helpers to render a failed task whose log fetch
  // returns zero chunks. Mock diagnostics to return a worker record for the task.
  vi.spyOn(diagApi, 'fetchDiagnosticsLogs').mockResolvedValue({
    records: [{ ts: '2026-06-17T12:00:00Z', component: 'worker:w1', level: 'ERROR', msg: 'executable file not found', attrs: { task_id: 't1' } }],
  })
  renderTaskLogPage({ taskId: 't1', status: 'failed', logChunks: [] }) // adapt to existing helper
  expect(await screen.findByText(/no task output/i)).toBeInTheDocument()
  expect(await screen.findByText('executable file not found')).toBeInTheDocument()
})
```

> Read `TaskLogPage.test.tsx` and `TaskLogPage.tsx` first to match the existing data hooks (how the task status and log chunks are obtained) and render helper. Import `* as diagApi from '@/api/diagnostics'`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/pages/TaskLogPage.test.tsx`
Expected: FAIL — fallback text/diagnostics not rendered.

- [ ] **Step 3: Write minimal implementation**

In `TaskLogPage.tsx`, after the existing log rendering, add the fallback. Use the task status and whether any log chunks exist (both already available in this page; read the file to use the real variable names):

```tsx
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
// ...
const hasNoLogs = logChunks.length === 0 // use the page's real "no chunks" condition
const isFailed = task?.status === 'failed' // use the page's real status accessor

{hasNoLogs && isFailed && (
  <div>
    <p>No task output was produced. Showing worker diagnostics for this task.</p>
    {task?.failure_reason && <p><strong>Reason:</strong> {task.failure_reason}</p>}
    <DiagnosticsPanel component={`worker:${task.worker_id}`} title="Worker diagnostics" taskId={taskId} />
  </div>
)}
```

> Confirm the actual field names for the failure reason and worker id on the task type in `web/src/api/types.ts` (e.g. `failure_reason`/`reason`, `worker_id`). Use the real names. If the worker id is not present on the task, omit the `component` narrowing and pass `component=""`? No — `DiagnosticsPanel` requires a component; instead filter by task_id across all components by passing the panel a component of the worker if known, else read the task's `worker_id`. If `worker_id` may be empty for a pre-exec failure, change `DiagnosticsPanel` to treat an empty `component` as "all components" (and update its test) — verify against the task fixture which field is reliably set.

- [ ] **Step 4: Run test + gate**

Run: `cd web && npx vitest run src/pages/TaskLogPage.test.tsx` then full web gate.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/TaskLogPage.tsx web/src/pages/TaskLogPage.test.tsx
git commit -m "feat(web): show worker diagnostics fallback for failed tasks with no logs"
```

---

## Phase F — Docs & correlation polish

### Task 17: Correlation-field audit

**Files:**
- Modify: various `slog` call sites under `internal/` (additive only)

- [ ] **Step 1: Inventory correlation gaps**

Run:
```bash
grep -rn 'ErrorContext\|WarnContext\|InfoContext' internal/scheduler internal/api internal/worker/executor internal/worker/session | grep -v _test.go > /tmp/diag-logsites.txt
wc -l /tmp/diag-logsites.txt
```
Review for task/job/worker/attempt/session log lines that omit the relevant correlation key (`task_id`, `attempt_id`, `job_id`, `worker_id`, `session_id`).

- [ ] **Step 2: Add missing keys**

For each identified line, add the missing `slog.String("<key>", value)` attribute where the value is in scope. Keep keys consistently named (snake_case as already used). This is additive — no behavior change.

- [ ] **Step 3: Verify nothing breaks**

Run: `make test` and `make lint`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/
git commit -m "chore(log): ensure consistent correlation keys across diagnostic logs"
```

---

### Task 18: Observability documentation

**Files:**
- Create: `docs/observability.md`
- Modify: `docs/operations.md` (cross-link)
- Modify: `docs/worker-deployment.md` (cross-link)

- [ ] **Step 1: Write `docs/observability.md`**

Cover, with worked examples:
1. The two log channels: **task logs** (process stdout/stderr, durable, per-task in the UI) vs **diagnostic logs** (server/worker `slog`, ephemeral ring buffer + stderr).
2. **In-UI diagnostics**: worker Diagnostics panel, Admin → Server log, the failed-task fallback. How to disable (`SQI_DIAGNOSTICS_ENABLED=false`) and what that costs you (no in-UI history; rely on OOB).
3. **JSON log schema**: field/level reference; the correlation keys (`task_id`, `attempt_id`, `job_id`, `worker_id`, `session_id`).
4. **Out-of-band wiring** (the advanced path): examples for journald (`systemd` unit + `journalctl -u sqi-server -o json`), Docker (`--log-driver`), Loki (Promtail scrape of JSON stdout), and ELK (Filebeat). Note `SQI_LOG_FORMAT=json` is the production default; `text` is for local dev.
5. **Security note:** do not log secret values; diagnostic lines carry attribute keys/paths/commands, not secrets; the diagnostics endpoint sits under the same read authorization as other endpoints.

- [ ] **Step 2: Cross-link**

Add a "See `docs/observability.md`" pointer to the operations guide and worker deployment guide where logging/monitoring is discussed.

- [ ] **Step 3: Commit**

```bash
git add docs/observability.md docs/operations.md docs/worker-deployment.md
git commit -m "docs: add observability guide for diagnostic logs and external wiring"
```

---

## Final verification

- [ ] Run the full Go CI gate: `make ci`
- [ ] Run the full web gate: `cd web && npm run format:check && npm run typecheck && npm run lint && npm run test:coverage`
- [ ] Run the smoke test: `make smoke`
- [ ] Manual: start server + a worker, submit a job with a bad command (e.g. a nonexistent executable), open the task in the UI, confirm the failed task with no output shows the worker diagnostics + reason. Open Admin → Server log and the worker's Diagnostics panel; confirm live updates. Toggle dark/light theme and confirm both panels render correctly.
- [ ] Set `SQI_DIAGNOSTICS_ENABLED=false` on the server; confirm `/api/v1/diagnostics/logs` returns 503 and the UI panels show "Diagnostics unavailable" gracefully.

---

## Self-review notes (author)

- **Spec coverage:** fan-out handler (T2), worker push over core NATS (T1/T5/T11), server ring buffer + disable switch (T3/T4/T8/T10), REST + WS (T6/T9), worker panel + Admin server log + task fallback all in both themes (T13–T16), docs + correlation polish (T17/T18). A1 ephemeral (no JetStream stream — T5 uses core NATS). All spec sections map to a task.
- **Type consistency:** `SinkRecord.Ts` is a formatted string end-to-end (T2 emits, T4/T11 parse with the same layout). `diag.Record`/`diag.Filter` used identically in T3/T4/T7/T9. `ws.DiagEvent`/`DiagnosticsPush` fields match `WsDiagEvent`/`DiagRecord` on the web side (T6/T12). `bus.WorkerDiagSubject` shared by publish (T11) and subject-prefix subscribe (T5/T7).
- **Adapt-to-codebase callouts:** several tasks instruct reading the existing file first (config `Load` signature, scheduler struct/constructor, api handler idiom, web test helpers, task type field names) because those exact signatures were not all read during planning. These are explicit, not placeholders.

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

	mu     sync.RWMutex
	notify func(Record)
	rings  map[string][]Record // component → chronological slice (len ≤ perComponent)
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

// SetNotify sets (or replaces) the per-append notify callback.  Intended to be
// called once during boot (e.g. after the WebSocket hub exists) before heavy
// traffic.  Safe for concurrent use.
func (b *Buffer) SetNotify(fn func(Record)) {
	b.mu.Lock()
	b.notify = fn
	b.mu.Unlock()
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
	notify := b.notify
	b.mu.Unlock()

	if notify != nil {
		notify(r)
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

	out := make([]Record, 0, len(all))
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

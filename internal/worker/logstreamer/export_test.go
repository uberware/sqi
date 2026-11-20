// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer

import "time"

// Knobs are the delivery settings tests shorten. Zero keeps the default.
type Knobs struct {
	AckTimeout      time.Duration
	RetryBase       time.Duration
	RetryMax        time.Duration
	FlushWait       time.Duration
	MaxPendingBytes int
	Window          int
}

// WithKnobs returns c with k's delivery settings.
func WithKnobs(c Config, k Knobs) Config {
	c.ackTimeout = k.AckTimeout
	c.retryBase = k.RetryBase
	c.retryMax = k.RetryMax
	c.flushWait = k.FlushWait
	c.maxPendingBytes = k.MaxPendingBytes
	c.window = k.Window
	return c
}

// SenderIdle returns a channel closed while no sender goroutine runs.
func (p *Publisher) SenderIdle() <-chan struct{} {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return p.out.idle
}

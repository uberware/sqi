// SPDX-License-Identifier: AGPL-3.0-or-later

// Package netfault is a TCP proxy that a load or chaos run puts between a
// client and a server so it can cut, blackhole or shape the connection
// without touching either end.
package netfault

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"
)

const (
	bufSize     = 32 * 1024
	queueDepth  = 64
	dialTimeout = 5 * time.Second
)

type mode int

const (
	modePass mode = iota
	modeCut
	modeBlackhole
)

// shaping is the delay and bandwidth applied to every forwarded chunk.
type shaping struct {
	delay, jitter time.Duration
	bytesPerSec   int
}

// Proxy forwards TCP connections from a loopback listener to a target and can
// cut, blackhole or shape them.
type Proxy struct {
	ln     net.Listener
	target string
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	mode    mode
	shape   shaping
	changed chan struct{} // closed and replaced on every mode change
	links   map[*link]struct{}
	rng     *rand.Rand
}

// link is one accepted client connection and, once dialed, its target side.
type link struct {
	p      *Proxy
	client net.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	target net.Conn
}

// Listen starts a proxy on 127.0.0.1:0 forwarding to target. seed drives the
// jitter.
func Listen(target string, seed uint64) (*Proxy, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		ln:      ln,
		target:  target,
		changed: make(chan struct{}),
		links:   map[*link]struct{}{},
		rng:     rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: jitter must be reproducible
	}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

// Addr is the proxy's host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Cut closes every connection the proxy carries and refuses new ones until
// Heal. Data a blackhole was holding is dropped: each link is canceled before
// the mode changes, so nothing parked on the blackhole wakes to forward it.
func (p *Proxy) Cut() {
	p.mu.Lock()
	links := p.takeLinksLocked()
	for _, l := range links {
		l.cancel()
	}
	p.setModeLocked(modeCut)
	p.mu.Unlock()
	for _, l := range links {
		l.close()
	}
}

// Blackhole keeps connections open but forwards nothing in either direction,
// and accepts new connections without dialing the target, until Heal.
func (p *Proxy) Blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setModeLocked(modeBlackhole)
}

// Shape delays each forwarded chunk by delay plus or minus jitter (order
// preserved) and, when bytesPerSec is positive, caps each direction's
// throughput.
func (p *Proxy) Shape(delay, jitter time.Duration, bytesPerSec int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shape = shaping{delay: delay, jitter: jitter, bytesPerSec: bytesPerSec}
}

// Heal returns to pass-through: refused connections are accepted again,
// blackholed data flows and shaping is removed.
func (p *Proxy) Heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shape = shaping{}
	p.setModeLocked(modePass)
}

// Close stops the listener, closes every connection and waits for the
// proxy's goroutines to finish.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	links := p.takeLinksLocked()
	p.mu.Unlock()
	err := p.ln.Close()
	for _, l := range links {
		l.close()
	}
	p.wg.Wait()
	return err
}

// setModeLocked changes the mode and wakes everything parked on it.
func (p *Proxy) setModeLocked(m mode) {
	p.mode = m
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Proxy) takeLinksLocked() []*link {
	links := make([]*link, 0, len(p.links))
	for l := range p.links {
		links = append(links, l)
	}
	p.links = map[*link]struct{}{}
	return links
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		l := p.register(c)
		if l == nil {
			_ = c.Close()
			continue
		}
		p.wg.Add(1)
		go l.serve()
	}
}

// register tracks a new connection, or returns nil when the proxy is cut or
// closed and the connection must be refused.
func (p *Proxy) register(c net.Conn) *link {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.mode == modeCut {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &link{p: p, client: c, ctx: ctx, cancel: cancel}
	p.links[l] = struct{}{}
	return l
}

func (p *Proxy) unregister(l *link) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.links, l)
}

// currentShape is the shaping a chunk is handled under.
func (p *Proxy) currentShape() shaping {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shape
}

// jittered is delay plus a uniform draw from [-jitter, +jitter], never below zero.
func (p *Proxy) jittered(s shaping) time.Duration {
	d := s.delay
	if s.jitter > 0 {
		p.mu.Lock()
		d += time.Duration(p.rng.Int64N(2*int64(s.jitter)+1)) - s.jitter
		p.mu.Unlock()
	}
	return max(d, 0)
}

// close ends both sides of the link and releases everything waiting on it.
func (l *link) close() {
	l.cancel()
	_ = l.client.Close()
	l.mu.Lock()
	t := l.target
	l.mu.Unlock()
	if t != nil {
		_ = t.Close()
	}
}

// waitPass parks until the proxy is not blackholed. It reports false when the
// link was closed first, or the proxy is cut.
func (l *link) waitPass() bool {
	for {
		l.p.mu.Lock()
		m := l.p.mode
		changed := l.p.changed
		l.p.mu.Unlock()
		switch m {
		case modeCut:
			return false
		case modeBlackhole:
		default:
			return l.ctx.Err() == nil
		}
		select {
		case <-changed:
		case <-l.ctx.Done():
			return false
		}
	}
}

func (l *link) serve() {
	defer l.p.wg.Done()
	defer l.p.unregister(l)
	defer l.close()

	// A connection accepted while blackholed is not dialed until Heal.
	if !l.waitPass() {
		return
	}
	d, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(l.ctx, "tcp", l.p.target)
	if err != nil {
		return
	}
	l.mu.Lock()
	l.target = d
	l.mu.Unlock()
	if l.ctx.Err() != nil { // closed while dialing; close() may have missed d
		_ = d.Close()
		return
	}

	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { defer pumps.Done(); l.pump(l.client, d) }()
	go func() { defer pumps.Done(); l.pump(d, l.client) }()
	pumps.Wait()
}

// sourceEnd is how a pump's source ended: err is nil for a clean end.
type sourceEnd struct{ err error }

type chunk struct {
	data      []byte
	deliverAt time.Time
}

// pump forwards src to dst: a reader stamps each chunk with the time it may be
// delivered, and a writer delivers them in order, so the delay is latency and
// does not throttle a stream.
func (l *link) pump(src, dst net.Conn) {
	q := make(chan chunk, queueDepth)
	var rw sync.WaitGroup
	var end sourceEnd // set before q is closed, so the writer reads it safely after the drain
	rw.Go(func() { l.readLoop(src, q, &end) })
	ok := l.writeLoop(dst, q, &end)
	if !ok {
		l.close()
	}
	rw.Wait()
}

func (l *link) readLoop(src net.Conn, q chan<- chunk, end *sourceEnd) {
	defer close(q)
	buf := make([]byte, bufSize)
	var prev time.Time
	for {
		n, err := src.Read(buf)
		if n > 0 {
			at := time.Now().Add(l.p.jittered(l.p.currentShape()))
			if at.Before(prev) {
				at = prev
			}
			prev = at
			c := chunk{data: append([]byte(nil), buf[:n]...), deliverAt: at}
			select {
			case q <- c:
			case <-l.ctx.Done():
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				end.err = err
			}
			return
		}
	}
}

// writeLoop delivers queued chunks. It reports false when the link failed or
// was closed, and true when the source ended cleanly after everything it sent
// was written (the write side is then half-closed). The source ending is
// passed on only when the proxy is not blackholed, so a partition hides a
// teardown as well as data: a read error closes the link, and a clean end
// half-closes the peer, once the blackhole lifts.
func (l *link) writeLoop(dst net.Conn, q <-chan chunk, end *sourceEnd) bool {
	for c := range q {
		if !sleep.Until(l.ctx, c.deliverAt) || !l.waitPass() {
			return false
		}
		if _, err := dst.Write(c.data); err != nil {
			return false
		}
		if s := l.p.currentShape(); s.bytesPerSec > 0 {
			pause := time.Duration(int64(len(c.data)) * int64(time.Second) / int64(s.bytesPerSec))
			if !sleep.For(l.ctx, pause) {
				return false
			}
		}
	}
	if !l.waitPass() || end.err != nil {
		return false
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite() //nolint:errcheck // the connection may already be closed
		return true
	}
	return false
}

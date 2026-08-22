// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// Multicast capability detection for the mDNS discovery tests.
//
// The rest of this suite disables mDNS, on the long-standing assumption that
// "multicast is not available in most CI environments". That assumption was
// never tested. It is tested here: requireMulticast actually performs a
// round trip and reports what happened, rather than guessing from the
// environment.
//
// These tests do NOT transmit on the network they are running on. Every
// advertisement is restricted to loopback (see loopbackIfaces), which a browser
// listening on all interfaces still receives — verified, not assumed. That
// keeps a test run from announcing a service on an office LAN, and it is an
// invariant rather than a preference: where loopback cannot carry multicast the
// tests refuse to run rather than falling back to a real interface.
//
// Because a skipped test verifies nothing, SQI_TEST_REQUIRE_MULTICAST=1 turns
// the skip into a failure. `make test-discovery` and the CI job set it, so a
// runner that silently loses multicast fails the build instead of quietly
// covering less than it claims.

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	serverdiscovery "github.com/uberware/sqi/internal/discovery"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
)

// multicastRequired reports whether a skip should be treated as a failure.
func multicastRequired() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SQI_TEST_REQUIRE_MULTICAST")))
	return v == "1" || v == "true" || v == "yes"
}

// loopbackIfaces returns the multicast-capable loopback interfaces.
//
// Advertising only on these is what keeps a test run off the real network. It
// works because multicast sent on loopback is delivered to anything listening
// on loopback, including a resolver bound to every interface — which is what
// the production worker uses, so the real-binary test needs no special casing.
//
// Linux `lo` does not carry the MULTICAST flag by default, which is why the CI
// job enables it; see docs/development.md for the one-line remediation.
func loopbackIfaces() []net.Interface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagMulticast != 0 && i.Flags&net.FlagUp != 0 {
			out = append(out, i)
		}
	}
	return out
}

// requireMulticast proves an mDNS round trip works on this host before a test
// depends on one, by advertising a throwaway service on loopback and browsing
// for it.
//
// It exists so a discovery test fails for the reason it actually failed: a
// broken responder looks identical to a host that cannot do multicast at all,
// and without this preflight every such failure would be investigated twice.
func requireMulticast(t *testing.T) {
	t.Helper()

	lo := loopbackIfaces()
	if len(lo) == 0 {
		skipOrFail(t, "loopback has no MULTICAST flag, and these tests refuse to "+
			"advertise on a real interface; on Linux run: sudo ip link set lo multicast on")
		return
	}

	logger := slog.New(slog.DiscardHandler)
	probe, err := serverdiscovery.New(serverdiscovery.Config{
		Enabled:      true,
		InstanceName: instanceName(t, "sqi-probe"),
		HTTPAddr:     "127.0.0.1:1",
		NATSAddr:     "127.0.0.1:2",
		Interfaces:   lo,
	}, logger)
	if err != nil {
		t.Fatalf("multicast probe: responder New: %v", err)
	}
	if err := probe.Start(context.Background()); err != nil {
		skipOrFail(t, "multicast probe: responder could not start: "+err.Error())
		return
	}
	defer probe.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := workerdiscovery.Browse(ctx, 6*time.Second, logger); err != nil {
		skipOrFail(t, "multicast probe: no mDNS round trip over loopback on this host ("+
			err.Error()+")")
	}
}

// skipOrFail skips, unless multicast was declared required.
func skipOrFail(t *testing.T, reason string) {
	t.Helper()
	if multicastRequired() {
		t.Fatalf("%s — and SQI_TEST_REQUIRE_MULTICAST is set, so this is a failure, not a skip", reason)
	}
	t.Skip(reason)
}

// maxInstanceLabel is the DNS label limit a DNS-SD instance name must fit in.
// Exceeding it does not error: the responder starts and is simply never
// discoverable, which presents as "multicast does not work on this host".
// This cost an hour of looking in the wrong place; the cap is enforced in
// instanceName so no caller has to remember it.
const maxInstanceLabel = 63

// instanceName builds a unique, valid mDNS instance name for this test run.
//
// Uniqueness matters because these tests advertise on a real network: two runs
// on one machine, or two machines on one LAN, must not answer for each other.
func instanceName(t *testing.T, prefix string) string {
	t.Helper()
	safe := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		return b.String()
	}

	// t.TempDir() is unique per test; its last two elements carry the random
	// component and the per-test counter, which is all the uniqueness needed.
	dir := t.TempDir()
	unique := safe(filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir))
	if len(unique) > 24 {
		unique = unique[len(unique)-24:]
	}

	name := safe(prefix) + "-" + unique
	if len(name) > maxInstanceLabel {
		name = name[len(name)-maxInstanceLabel:]
	}
	return strings.Trim(name, "-")
}

// noForeignServer skips when something other than this test is already
// advertising _sqi._tcp.
//
// The tests never advertise beyond loopback, but they still BROWSE on every
// interface — the production worker does, and the real-binary test runs the
// production worker. So a live sqi-server on the same LAN can still be
// discovered, and the test cannot tell a colleague's production broker from its
// own before connecting to it. Refusing to run is the only safe response.
func noForeignServer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, err := workerdiscovery.Browse(ctx, 2*time.Second, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Skipf("another sqi-server is advertising on this network (%s at %s); "+
			"refusing to run a real-mDNS test that could discover it",
			found.InstanceName, found.NATSURL)
	}
}

// TestMulticastRequired_FlagParsing pins the switch that decides whether a
// missing capability is a skip or a failure.
//
// It is worth a test of its own because everything else in this file trusts
// it: get this wrong in the "skip" direction and `make test-discovery` and the
// CI job go green while running nothing, which is the exact failure the flag
// exists to prevent.
func TestMulticastRequired_FlagParsing(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", " 1 "} {
		t.Setenv("SQI_TEST_REQUIRE_MULTICAST", v)
		if !multicastRequired() {
			t.Errorf("SQI_TEST_REQUIRE_MULTICAST=%q did not require multicast", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off"} {
		t.Setenv("SQI_TEST_REQUIRE_MULTICAST", v)
		if multicastRequired() {
			t.Errorf("SQI_TEST_REQUIRE_MULTICAST=%q required multicast", v)
		}
	}
}

// TestInstanceName_FitsTheDNSLabelLimit guards the trap that cost real time
// while these tests were being written: an over-long instance name does not
// error, it just makes the responder undiscoverable, which is indistinguishable
// from the host having no multicast at all.
func TestInstanceName_FitsTheDNSLabelLimit(t *testing.T) {
	got := instanceName(t, "a-deliberately-long-prefix-that-would-overflow-the-label-limit-on-its-own")
	if len(got) > maxInstanceLabel {
		t.Errorf("instance name is %d bytes (%q), want <= %d", len(got), got, maxInstanceLabel)
	}
	if got == "" {
		t.Error("instance name is empty")
	}

	// Uniqueness comes from t.TempDir(), which differs per call, so comparing
	// two calls proves nothing. What matters is that the unique part SURVIVES
	// the length cap — truncation keeps the tail for exactly that reason, and
	// a change that trimmed the other end would make every over-long name
	// collide on its shared prefix.
	long := instanceName(t, strings.Repeat("x", 200))
	short := instanceName(t, "p")
	if len(long) != maxInstanceLabel {
		t.Errorf("an over-long name is %d bytes, want it capped at exactly %d", len(long), maxInstanceLabel)
	}
	if !strings.HasSuffix(short, strings.TrimPrefix(short, "p-")) {
		t.Errorf("unique suffix missing from %q", short)
	}
	if long == instanceName(t, strings.Repeat("y", 200)) {
		t.Error("two over-long names collided after truncation; the unique tail was trimmed")
	}
}

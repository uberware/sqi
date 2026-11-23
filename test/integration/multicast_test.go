// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// Multicast capability detection for the mDNS discovery tests.
//
// The rest of this suite disables mDNS. Here, requireMulticast performs a
// round trip and reports what happened, rather than guessing from the
// environment whether multicast is available.
//
// These tests do not transmit on the network they run on. Every advertisement
// is restricted to loopback (see loopbackIfaces), which a browser listening on
// all interfaces still receives. That keeps a test run from announcing a
// service on the LAN: where loopback cannot carry multicast the tests refuse
// to run rather than fall back to a real interface.
//
// A skipped test verifies nothing, so SQI_TEST_REQUIRE_MULTICAST=1 turns the
// skip into a failure. `make test-discovery` and the CI job set it, so a
// runner without multicast fails the build.

import (
	"context"
	"fmt"
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
// Advertising only on these keeps a test run off the real network. Multicast
// sent on loopback is delivered to anything listening on loopback, including a
// resolver bound to every interface, which is what the production worker
// uses, so the real-binary test needs no special case.
//
// Linux `lo` does not carry the MULTICAST flag by default, so the CI job
// enables it. The flag is necessary but not sufficient; see advertisable.
// docs/development.md gives both remediations.
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

// requireMulticast checks that an mDNS round trip works on this host before a
// test depends on one, by advertising a throwaway service on loopback and
// browsing for it. Without it, a broken responder and a host that cannot do
// multicast fail the same way.
func requireMulticast(t *testing.T) {
	t.Helper()

	lo := loopbackIfaces()
	if len(lo) == 0 {
		skipOrFail(t, "loopback has no MULTICAST flag, and these tests refuse to "+
			"advertise on a real interface; on Linux run: sudo ip link set lo multicast on")
		return
	}

	if !anyAdvertisable(lo) {
		skipOrFail(t, "loopback carries loopback addresses only, and zeroconf "+
			"discards those when it builds the advertisement's address records, "+
			"so a loopback-only advertisement cannot register at all; on Linux "+
			"run: sudo ip -6 addr add fe80::1/64 dev lo")
		return
	}

	logger := slog.New(slog.DiscardHandler)
	probe, err := serverdiscovery.New(serverdiscovery.Config{
		Enabled:      true,
		InstanceName: instanceName(t, "sqi-probe"),
		HTTPAddr:     "127.0.0.1:1",
		NATSURL:      "nats://127.0.0.1:2",
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

// anyAdvertisable reports whether any of these interfaces can carry an
// advertisement.
func anyAdvertisable(ifaces []net.Interface) bool {
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		if advertisable(addrs) {
			return true
		}
	}
	return false
}

// advertisable mirrors the address filter zeroconf applies when it builds an
// advertisement, so a host that cannot register says so in its own terms.
//
// zeroconf fills the A/AAAA records from the interface's own addresses and
// drops every loopback one (addrsForInterface: `!ipnet.IP.IsLoopback()`), then
// fails with "Could not determine host IP addresses" if nothing is left. Linux
// `lo` carries only 127.0.0.1 and ::1, so registering on loopback alone fails
// there; macOS lo0 also carries fe80::1, so it succeeds on a Mac. Without this
// check the failure is an error from deep inside zeroconf that does not
// mention loopback.
func advertisable(addrs []net.Addr) bool {
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			return true
		}
	}
	return false
}

// skipOrFail skips, unless multicast was declared required.
func skipOrFail(t *testing.T, reason string) {
	t.Helper()
	if multicastRequired() {
		t.Fatalf("%s; SQI_TEST_REQUIRE_MULTICAST is set, so this is a failure, not a skip", reason)
	}
	t.Skip(reason)
}

// maxInstanceLabel is the DNS label limit a DNS-SD instance name must fit in.
// Exceeding it does not error: the responder starts and is never
// discoverable, which looks like "multicast does not work on this host".
// instanceName enforces the cap so no caller has to.
const maxInstanceLabel = 63

// instanceName builds a unique, valid mDNS instance name for this test run.
//
// Two runs on one machine share loopback, so they must not answer for each
// other.
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
	// component and the per-test counter.
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
// The tests never advertise beyond loopback, but they browse on every
// interface, as the production worker does and the real-binary test runs the
// production worker. A live sqi-server on the same LAN can therefore be
// discovered, and the test cannot tell another farm's broker from its own
// before connecting to it, so it does not run.
func noForeignServer(t *testing.T) {
	t.Helper()
	if reason := foreignServerReason(); reason != "" {
		t.Skip(reason)
	}
}

// noForeignServerOrFail is [noForeignServer] for a caller that must not lose
// its mDNS coverage: when multicast was declared required, a foreign server
// fails the test instead of skipping it.
func noForeignServerOrFail(t *testing.T) {
	t.Helper()
	if reason := foreignServerReason(); reason != "" {
		skipOrFail(t, reason)
	}
}

// foreignServerReason returns why a real-mDNS test must not run, or "" when
// no other sqi-server is advertising.
func foreignServerReason() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, err := workerdiscovery.Browse(ctx, 2*time.Second, slog.New(slog.DiscardHandler))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("another sqi-server is advertising on this network (%s at %s); "+
		"refusing to run a real-mDNS test that could discover it",
		found.InstanceName, found.NATSURL)
}

// TestMulticastRequired_FlagParsing pins the switch that decides whether a
// missing capability is a skip or a failure. Wrong in the "skip" direction,
// `make test-discovery` and the CI job would pass while running nothing.
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

// TestInstanceName_FitsTheDNSLabelLimit guards against an over-long instance
// name, which does not error but makes the responder undiscoverable, as if the
// host had no multicast.
func TestInstanceName_FitsTheDNSLabelLimit(t *testing.T) {
	got := instanceName(t, "a-deliberately-long-prefix-that-would-overflow-the-label-limit-on-its-own")
	if len(got) > maxInstanceLabel {
		t.Errorf("instance name is %d bytes (%q), want <= %d", len(got), got, maxInstanceLabel)
	}
	if got == "" {
		t.Error("instance name is empty")
	}

	// Uniqueness comes from t.TempDir(), which differs per call, so two calls
	// always differ. The unique part must survive the length cap: truncation
	// keeps the tail, and trimming the other end would make every over-long
	// name collide on its shared prefix.
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

// TestAdvertisable_MatchesZeroconfAddressFilter pins the check that tells a
// Linux host why a loopback-only advertisement cannot register.
//
// The two loopback rows are what macOS and Linux put on their loopback
// interface; only the macOS one can carry an advertisement.
func TestAdvertisable_MatchesZeroconfAddressFilter(t *testing.T) {
	addr := func(cidr string) net.Addr {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", cidr, err)
		}
		return &net.IPNet{IP: ip, Mask: n.Mask}
	}

	for _, tc := range []struct {
		name  string
		addrs []net.Addr
		want  bool
	}{
		{"linux lo", []net.Addr{addr("127.0.0.1/8"), addr("::1/128")}, false},
		{"macos lo0", []net.Addr{addr("127.0.0.1/8"), addr("::1/128"), addr("fe80::1/64")}, true},
		{"no addresses", nil, false},
		{"routable interface", []net.Addr{addr("192.168.1.10/24")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := advertisable(tc.addrs); got != tc.want {
				t.Errorf("advertisable() = %v, want %v", got, tc.want)
			}
		})
	}
}

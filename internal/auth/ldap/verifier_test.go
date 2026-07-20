// SPDX-License-Identifier: AGPL-3.0-or-later

package ldap

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// A sub-second timeout truncates to 0 under the naive int(d.Seconds()), and
// RFC 4511 gives 0 the meaning "no time limit" — the opposite of what an
// operator asking for 500ms wrote. Config validation only requires timeout >
// 0, so those values are reachable and must floor to the smallest limit the
// protocol can express.
func TestSearchTimeLimit(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want int
	}{
		{"whole seconds", 10 * time.Second, 10},
		{"truncates fractional part", 5500 * time.Millisecond, 5},
		{"sub-second floors to 1 rather than unlimited", 500 * time.Millisecond, 1},
		{"one nanosecond floors to 1", time.Nanosecond, 1},
		{"exactly one second", time.Second, 1},
		{"unset means no limit", 0, 0},
		{"negative means no limit", -time.Second, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := searchTimeLimit(tc.in); got != tc.want {
				t.Errorf("searchTimeLimit(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// New selects the mode from the config rather than from a caller-supplied
// flag, so a config that sets UserDNTemplate can never get a service-account
// verifier.
func TestNewSelectsModeFromConfig(t *testing.T) {
	tmpl, err := New(Config{URL: "ldap://dc.example.com:389", UserDNTemplate: "uid=%s,dc=example,dc=com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := tmpl.(*templateBindVerifier); !ok {
		t.Errorf("template config produced %T, want *templateBindVerifier", tmpl)
	}

	search, err := New(Config{URL: "ldap://dc.example.com:389", BaseDN: "dc=example,dc=com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := search.(*searchBindVerifier); !ok {
		t.Errorf("search config produced %T, want *searchBindVerifier", search)
	}
}

// A CA file that cannot be read or parsed must fail at construction, not on a
// user's first login attempt.
func TestNewRejectsBadCAFile(t *testing.T) {
	if _, err := New(Config{URL: "ldap://dc.example.com:389", BaseDN: "dc=example,dc=com", CAFile: t.TempDir() + "/absent.pem"}); err == nil {
		t.Error("expected an error for a missing ca_file")
	}

	bad := t.TempDir() + "/bad.pem"
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := New(Config{URL: "ldap://dc.example.com:389", BaseDN: "dc=example,dc=com", CAFile: bad}); err == nil {
		t.Error("expected an error for a ca_file with no usable certificates")
	}
}

// TestRealDialer_StartTLSIsBoundedByTimeout proves that the dialer built by
// realDialer cannot hang forever on a StartTLS round trip against a server
// that completes the TCP handshake and then goes silent.
//
// c.SetTimeout(cfg.Timeout) has to run BEFORE the StartTLS branch: go-ldap
// only arms a per-request timer when its internal requestTimeout is already >
// 0 at send time, and SetTimeout is the only thing that sets it. Moved after
// StartTLS, the extended request goes out with no timer at all. A listener
// that accepts and never replies is exactly the server this guards against —
// no live directory, no DNS, nothing outside this test's own loopback
// listener.
func TestRealDialer_StartTLSIsBoundedByTimeout(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
		// Hold the connection open and never write a reply: the server
		// completed the TCP handshake and then went silent, which is the
		// scenario an unarmed request timer cannot survive.
		<-make(chan struct{})
	}()

	cfg := Config{
		URL:      "ldap://" + ln.Addr().String(),
		StartTLS: true,
		Timeout:  200 * time.Millisecond,
	}
	dial, err := realDialer(cfg)
	if err != nil {
		t.Fatalf("realDialer: %v", err)
	}

	type result struct {
		c   conn
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		c, err := dial(context.Background())
		done <- result{c, err}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		t.Cleanup(func() {
			select {
			case ac := <-accepted:
				_ = ac.Close()
			default:
			}
		})
		if r.err == nil {
			if r.c != nil {
				_ = r.c.Close()
			}
			t.Fatal("expected an error from a StartTLS round trip against a silent server, got none")
		}
		// Well inside a second: the configured timeout is 200ms. 2s leaves
		// generous CI headroom while still catching the unbounded-hang case,
		// which would sit on this select until the test's own deadline.
		if elapsed >= 2*time.Second {
			t.Errorf("dial took %v, want well under 2s (timeout was %v)", elapsed, cfg.Timeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not return within 2s: SetTimeout is not bounding the StartTLS round trip")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package wol_test

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/wol"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

func TestMagicPacket(t *testing.T) {
	mac := mustMAC(t, "01:02:03:04:05:06")
	pkt, err := wol.MagicPacket(mac)
	if err != nil {
		t.Fatalf("MagicPacket: %v", err)
	}
	if len(pkt) != 102 {
		t.Fatalf("len = %d, want 102", len(pkt))
	}
	want := bytes.Repeat([]byte{0xFF}, 6)
	for range 16 {
		want = append(want, 1, 2, 3, 4, 5, 6)
	}
	if !bytes.Equal(pkt, want) {
		t.Errorf("packet = %x, want %x", pkt, want)
	}

	tests := []struct {
		name string
		mac  net.HardwareAddr
	}{
		{"eui64", mustMAC(t, "01:02:03:04:05:06:07:08")},
		{"short", net.HardwareAddr{1, 2, 3}},
		{"nil", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if pkt, err := wol.MagicPacket(tt.mac); err == nil || pkt != nil {
				t.Errorf("MagicPacket(%v) = %x, %v; want nil, error", tt.mac, pkt, err)
			}
		})
	}
}

func TestDirectedBroadcast(t *testing.T) {
	tests := []struct {
		name string
		cidr string
		want string
		ok   bool
	}{
		{"slash24", "192.168.1.20/24", "192.168.1.255", true},
		{"slash16", "10.1.2.3/16", "10.1.255.255", true},
		{"slash22", "172.16.5.9/22", "172.16.7.255", true},
		{"slash31", "192.168.1.20/31", "", false},
		{"slash32", "192.168.1.20/32", "", false},
		{"ipv6", "fe80::1/64", "", false},
		{"ipv4MappedIPv6Text", "::ffff:192.168.1.20/120", "", false},
		{"garbage", "not-a-cidr", "", false},
		{"noMask", "192.168.1.20", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wol.DirectedBroadcast(tt.cidr)
			if ok != tt.ok {
				t.Fatalf("DirectedBroadcast(%q) ok = %v, want %v (ip %v)", tt.cidr, ok, tt.ok, got)
			}
			if ok && got.String() != tt.want {
				t.Errorf("DirectedBroadcast(%q) = %v, want %v", tt.cidr, got, tt.want)
			}
		})
	}
}

func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// drain reads packets until n arrive or the deadline passes.
func drain(t *testing.T, conn *net.UDPConn, n int) [][]byte {
	t.Helper()
	var got [][]byte
	buf := make([]byte, 2048)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	for len(got) < n {
		k, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read after %d/%d packets: %v", len(got), n, err)
		}
		got = append(got, append([]byte(nil), buf[:k]...))
	}
	return got
}

func TestSend_DeliversBurstToEachTarget(t *testing.T) {
	macs := []net.HardwareAddr{mustMAC(t, "01:02:03:04:05:06"), mustMAC(t, "0a:0b:0c:0d:0e:0f")}
	a, b := listen(t), listen(t)
	targets := []string{a.LocalAddr().String(), b.LocalAddr().String()}

	if err := wol.Send(context.Background(), macs, targets, 3, time.Millisecond); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, conn := range []*net.UDPConn{a, b} {
		pkts := drain(t, conn, 6)
		counts := map[string]int{}
		for _, p := range pkts {
			if len(p) != 102 {
				t.Fatalf("packet length %d, want 102", len(p))
			}
			var matched bool
			for _, mac := range macs {
				want, err := wol.MagicPacket(mac)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(p, want) {
					counts[mac.String()]++
					matched = true
				}
			}
			if !matched {
				t.Errorf("packet %x matches no MAC", p)
			}
		}
		for _, mac := range macs {
			if counts[mac.String()] != 3 {
				t.Errorf("%v: got %d packets, want 3", mac, counts[mac.String()])
			}
		}
	}
}

func TestSend_ContinuesPastABadTarget(t *testing.T) {
	macs := []net.HardwareAddr{mustMAC(t, "01:02:03:04:05:06")}
	good := listen(t)
	targets := []string{"not a target", good.LocalAddr().String()}

	err := wol.Send(context.Background(), macs, targets, 2, time.Millisecond)
	if err == nil {
		t.Fatal("Send returned nil, want the bad target's error")
	}
	if got := drain(t, good, 2); len(got) != 2 {
		t.Errorf("good target received %d packets, want 2", len(got))
	}
}

func TestSend_BadMACIsAnError(t *testing.T) {
	good := listen(t)
	err := wol.Send(context.Background(), []net.HardwareAddr{{1, 2, 3}}, []string{good.LocalAddr().String()}, 1, 0)
	if err == nil {
		t.Fatal("Send with a 3-byte MAC returned nil")
	}
}

func TestSend_StopsOnCanceledContext(t *testing.T) {
	macs := []net.HardwareAddr{mustMAC(t, "01:02:03:04:05:06")}
	good := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := wol.Send(ctx, macs, []string{good.LocalAddr().String()}, 3, time.Hour)
	if err == nil {
		t.Error("Send on a canceled context returned nil")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Send took %v on a canceled context", time.Since(start))
	}
}

func TestDefaults(t *testing.T) {
	if wol.DefaultPort != 9 || wol.DefaultBurst != 3 || wol.DefaultGap != 100*time.Millisecond {
		t.Errorf("defaults = %d, %d, %v", wol.DefaultPort, wol.DefaultBurst, wol.DefaultGap)
	}
}

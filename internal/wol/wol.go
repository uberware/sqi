// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wol builds and sends Wake-on-LAN magic packets. It imports only the
// standard library so both sqi-server and sqi-worker can use it.
package wol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	// DefaultPort is the conventional Wake-on-LAN UDP port (discard).
	DefaultPort = 9
	// DefaultBurst is how many copies of each packet are sent; UDP gives no
	// delivery guarantee, and a sleeping NIC only needs one to arrive.
	DefaultBurst = 3
	// DefaultGap is the pause between bursts.
	DefaultGap = 100 * time.Millisecond
)

const (
	macLen    = 6
	packetLen = 6 + 16*macLen
)

// MagicPacket returns the 102-byte magic packet for mac: six 0xFF bytes
// followed by the 6-byte MAC repeated 16 times. It is an error unless mac is
// exactly 6 bytes.
func MagicPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != macLen {
		return nil, fmt.Errorf("wol: MAC address %q is %d bytes, want %d", mac.String(), len(mac), macLen)
	}
	pkt := make([]byte, 0, packetLen)
	for range macLen {
		pkt = append(pkt, 0xFF)
	}
	for range 16 {
		pkt = append(pkt, mac...)
	}
	return pkt, nil
}

// DirectedBroadcast returns the IPv4 subnet-directed broadcast address of cidr
// ("192.168.1.20/24" gives 192.168.1.255). ok is false for IPv6, for /31 and
// /32 (which have no broadcast address), and for input that does not parse.
func DirectedBroadcast(cidr string) (net.IP, bool) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, false
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return nil, false
	}
	mask := network.Mask
	out := make(net.IP, 4)
	for i := range out {
		out[i] = ip4[i] | ^mask[i]
	}
	return out, true
}

// Send sends burst copies of each MAC's magic packet to each target
// ("ip:port"), gap apart, over UDP with SO_BROADCAST set. A bad MAC, an
// unresolvable target or a failed write is recorded and Send keeps going; the
// returned error joins every one of them. A canceled ctx stops Send early and
// its error is joined in too.
func Send(ctx context.Context, macs []net.HardwareAddr, targets []string, burst int, gap time.Duration) error {
	if burst < 1 {
		burst = 1
	}
	packets, errs := buildPackets(macs)
	addrs, targetErrs := resolveTargets(targets)
	errs = append(errs, targetErrs...)
	if len(packets) == 0 || len(addrs) == 0 {
		return errors.Join(errs...)
	}

	if err := ctx.Err(); err != nil {
		return errors.Join(append(errs, err)...)
	}

	lc := net.ListenConfig{Control: setBroadcast}
	pc, err := lc.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("wol: open socket: %w", err))...)
	}
	defer pc.Close()

	for i := range burst {
		if i > 0 {
			if err := sleep(ctx, gap); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		for _, addr := range addrs {
			for _, pkt := range packets {
				if _, err := pc.WriteTo(pkt, addr); err != nil {
					errs = append(errs, fmt.Errorf("wol: send to %s: %w", addr, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// sleep waits for d or until ctx is done, returning ctx's error in that case.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func buildPackets(macs []net.HardwareAddr) ([][]byte, []error) {
	var packets [][]byte
	var errs []error
	for _, mac := range macs {
		pkt, err := MagicPacket(mac)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		packets = append(packets, pkt)
	}
	return packets, errs
}

func resolveTargets(targets []string) ([]*net.UDPAddr, []error) {
	var addrs []*net.UDPAddr
	var errs []error
	for _, target := range targets {
		addr, err := net.ResolveUDPAddr("udp4", target)
		if err != nil {
			errs = append(errs, fmt.Errorf("wol: target %q: %w", target, err))
			continue
		}
		addrs = append(addrs, addr)
	}
	return addrs, errs
}

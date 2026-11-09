// SPDX-License-Identifier: AGPL-3.0-or-later

package registration

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func mac(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return hw
}

func cidr(t *testing.T, s string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	n.IP = ip
	return n
}

func TestLocalInterfaces(t *testing.T) {
	up := net.FlagUp | net.FlagBroadcast
	eth0 := net.Interface{Index: 1, Name: "eth0", Flags: up, HardwareAddr: mac(t, "aa:bb:cc:dd:ee:01")}
	eth1 := net.Interface{Index: 2, Name: "eth1", Flags: up, HardwareAddr: mac(t, "AA:BB:CC:DD:EE:02")}
	lo := net.Interface{Index: 3, Name: "lo", Flags: net.FlagUp | net.FlagLoopback}
	loWithMAC := net.Interface{Index: 4, Name: "lo2", Flags: net.FlagUp | net.FlagLoopback, HardwareAddr: mac(t, "aa:bb:cc:dd:ee:03")}
	down := net.Interface{Index: 5, Name: "eth2", Flags: net.FlagBroadcast, HardwareAddr: mac(t, "aa:bb:cc:dd:ee:04")}
	noMAC := net.Interface{Index: 6, Name: "tun0", Flags: up | net.FlagPointToPoint}

	addrsByName := map[string][]net.Addr{
		"eth0": {cidr(t, "192.168.1.20/24"), cidr(t, "fe80::1/64")},
		"eth1": {&net.IPAddr{IP: net.ParseIP("10.0.0.1")}, cidr(t, "10.0.0.2/8")},
	}
	addrs := func(i net.Interface) ([]net.Addr, error) { return addrsByName[i.Name], nil }

	t.Run("keeps up non-loopback interfaces with a MAC", func(t *testing.T) {
		list := func() ([]net.Interface, error) {
			return []net.Interface{lo, eth0, loWithMAC, down, noMAC, eth1}, nil
		}
		got := localInterfaces(list, addrs)
		want := []protocol.NetInterface{
			{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Addrs: []string{"192.168.1.20/24", "fe80::1/64"}},
			// A bare IPAddr has no prefix length and is not CIDR, so it is dropped.
			{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02", Addrs: []string{"10.0.0.2/8"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("localInterfaces = %+v, want %+v", got, want)
		}
	})

	t.Run("a failing address lookup keeps the interface with no addresses", func(t *testing.T) {
		failing := func(net.Interface) ([]net.Addr, error) { return nil, errors.New("boom") }
		got := localInterfaces(func() ([]net.Interface, error) { return []net.Interface{eth0}, nil }, failing)
		want := []protocol.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("localInterfaces = %+v, want %+v", got, want)
		}
	})

	t.Run("a failing interface listing reports none", func(t *testing.T) {
		got := localInterfaces(func() ([]net.Interface, error) { return nil, errors.New("boom") }, addrs)
		if len(got) != 0 {
			t.Fatalf("localInterfaces = %+v, want none", got)
		}
	})

	t.Run("40 interfaces are capped at 32 in OS order", func(t *testing.T) {
		var all []net.Interface
		for i := range 40 {
			all = append(all, net.Interface{
				Index: i + 1, Name: fmt.Sprintf("nic%02d", i), Flags: up,
				HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, byte(i)},
			})
		}
		got := localInterfaces(func() ([]net.Interface, error) { return all, nil },
			func(net.Interface) ([]net.Addr, error) { return nil, nil })
		if len(got) != 32 {
			t.Fatalf("got %d interfaces, want 32", len(got))
		}
		if got[0].Name != "nic00" || got[31].Name != "nic31" {
			t.Errorf("order not kept: first %q last %q", got[0].Name, got[31].Name)
		}
	})

	t.Run("an interface's addresses are capped at 16", func(t *testing.T) {
		var many []net.Addr
		for i := range 20 {
			many = append(many, cidr(t, fmt.Sprintf("10.1.%d.1/24", i)))
		}
		got := localInterfaces(func() ([]net.Interface, error) { return []net.Interface{eth0}, nil },
			func(net.Interface) ([]net.Addr, error) { return many, nil })
		if len(got) != 1 || len(got[0].Addrs) != 16 || got[0].Addrs[15] != "10.1.15.1/24" {
			t.Fatalf("got %+v, want one interface with the first 16 addresses", got)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestWorker_HasWakeAddress: a wake needs an interface with both a 6-byte MAC
// and an IPv4 address, which is what the wake-on-LAN provider addresses a
// packet with. A MAC on an IPv6-only interface, or an address on an interface
// with no usable MAC, is not enough.
func TestWorker_HasWakeAddress(t *testing.T) {
	tests := []struct {
		name string
		ifs  []store.NetInterface
		want bool
	}{
		{"none", nil, false},
		{"MAC and IPv4", []store.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"10.0.0.5/24"}}}, true},
		{"MAC, IPv6 only", []store.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"fe80::1/64"}}}, false},
		{"MAC, no address", []store.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff"}}, false},
		{"IPv4, no MAC", []store.NetInterface{{Name: "tun0", Addrs: []string{"10.8.0.2/24"}}}, false},
		{"IPv4, 8-byte MAC", []store.NetInterface{{Name: "ib0", MAC: "00:11:22:33:44:55:66:77", Addrs: []string{"10.1.0.2/16"}}}, false},
		{"IPv4, bare address", []store.NetInterface{{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"10.0.0.5"}}}, false},
		{
			"MAC on one interface, IPv4 on another",
			[]store.NetInterface{
				{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", Addrs: []string{"fe80::1/64"}},
				{Name: "tun0", Addrs: []string{"10.8.0.2/24"}},
			},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (store.Worker{Interfaces: tc.ifs}).HasWakeAddress(); got != tc.want {
				t.Errorf("HasWakeAddress = %v, want %v", got, tc.want)
			}
		})
	}
}

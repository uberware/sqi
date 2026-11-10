// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

// A simulated worker reports one interface, as a render node does: a valid
// locally administered MAC and a CIDR address, the same for an ID every time
// and different between IDs.
func TestSimInterfaces(t *testing.T) {
	a, again, b := simInterfaces("worker-a"), simInterfaces("worker-a"), simInterfaces("worker-b")
	if !reflect.DeepEqual(a, again) {
		t.Errorf("simInterfaces is not repeatable: %+v then %+v", a, again)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("simInterfaces = %+v and %+v, want one interface each", a, b)
	}
	if a[0].MAC == b[0].MAC {
		t.Errorf("two workers share MAC %s", a[0].MAC)
	}
	mac, err := net.ParseMAC(a[0].MAC)
	if err != nil || len(mac) != 6 || mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Errorf("MAC %q is not a 6-byte locally administered unicast address (%v)", a[0].MAC, err)
	}
	if len(a[0].Addrs) != 1 {
		t.Fatalf("addrs = %v, want one", a[0].Addrs)
	}
	if _, err := netip.ParsePrefix(a[0].Addrs[0]); err != nil {
		t.Errorf("address %q is not CIDR: %v", a[0].Addrs[0], err)
	}
}

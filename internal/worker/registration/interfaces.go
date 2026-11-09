// SPDX-License-Identifier: AGPL-3.0-or-later

package registration

import (
	"net"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// The most a registration reports: the server drops anything beyond these, so
// sending more only costs bytes.
const (
	maxInterfaces        = 32
	maxAddrsPerInterface = 16
)

// localInterfaces lists the host's network interfaces that can be woken by
// address: up, not loopback, and carrying a hardware address. list and addrs
// are net.Interfaces and (net.Interface).Addrs, injected for tests. Order
// follows the operating system, and the maxInterfaces and maxAddrsPerInterface
// caps apply. An address that is not in CIDR form is dropped, and an interface
// whose addresses cannot be read is still reported, since its MAC is the part a
// wake needs.
func localInterfaces(
	list func() ([]net.Interface, error),
	addrs func(net.Interface) ([]net.Addr, error),
) []protocol.NetInterface {
	all, err := list()
	if err != nil {
		return nil
	}
	var out []protocol.NetInterface
	for _, ifc := range all {
		if len(out) == maxInterfaces {
			break
		}
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) == 0 {
			continue
		}
		out = append(out, protocol.NetInterface{
			Name:  ifc.Name,
			MAC:   ifc.HardwareAddr.String(),
			Addrs: cidrAddrs(ifc, addrs),
		})
	}
	return out
}

// cidrAddrs returns ifc's addresses in CIDR form, at most maxAddrsPerInterface.
func cidrAddrs(ifc net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []string {
	list, err := addrs(ifc)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range list {
		if len(out) == maxAddrsPerInterface {
			break
		}
		if _, _, err := net.ParseCIDR(a.String()); err != nil {
			continue
		}
		out = append(out, a.String())
	}
	return out
}

// hostInterfaces is localInterfaces over the host's own interfaces.
func hostInterfaces() []protocol.NetInterface {
	return localInterfaces(net.Interfaces, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() })
}

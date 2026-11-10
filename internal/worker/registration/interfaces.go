// SPDX-License-Identifier: AGPL-3.0-or-later

package registration

import (
	"net"
	"strings"

	"github.com/uberware/sqi/internal/worker/protocol"
)

// virtualPrefixes start the names of the interfaces container runtimes,
// Kubernetes network plugins and desktop hypervisors create on a host for
// their guests. None is the machine's own link, so none can carry a wake
// packet or relay one, and a busy container host has dozens. A plain bridge
// ("br0") and a Hyper-V virtual switch ("vEthernet (...)") are not here: on a
// hypervisor host they hold the machine's own address.
var virtualPrefixes = []string{
	"veth", "docker", "br-", "cni", "flannel", "cali", "vxlan", "virbr", "vmnet", "vboxnet",
	"lxcbr", "lxdbr", "podman", "kube-", "weave",
}

// virtualInterface reports whether name is one of [virtualPrefixes].
func virtualInterface(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// localInterfaces lists the host's network interfaces that can be woken by
// address: up, not loopback, carrying a hardware address, and not the host
// side of container or VM networking ([virtualPrefixes]). list and addrs
// are net.Interfaces and (net.Interface).Addrs, injected for tests. Order
// follows the operating system, and the [protocol.MaxInterfaces] and
// [protocol.MaxAddrsPerInterface] caps apply. An address that is not in CIDR
// form is dropped, and an interface whose addresses cannot be read is still
// reported, since its MAC is the part a wake needs.
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
		if len(out) == protocol.MaxInterfaces {
			break
		}
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) == 0 ||
			virtualInterface(ifc.Name) {
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

// cidrAddrs returns ifc's addresses in CIDR form, at most
// protocol.MaxAddrsPerInterface.
func cidrAddrs(ifc net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []string {
	list, err := addrs(ifc)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range list {
		if len(out) == protocol.MaxAddrsPerInterface {
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

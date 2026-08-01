// Package netutil holds small networking helpers.
package netutil

import (
	"fmt"
	"net"
)

// PrimaryIPv4 returns the host's primary non-loopback IPv4 address. If iface is
// non-empty it is treated as either an interface name or an explicit IP to use.
func PrimaryIPv4(iface string) (net.IP, error) {
	if iface != "" {
		if ip := net.ParseIP(iface); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				return v4, nil
			}
			return nil, fmt.Errorf("configured interface %q is not IPv4", iface)
		}
		ni, err := net.InterfaceByName(iface)
		if err != nil {
			return nil, fmt.Errorf("interface %q: %w", iface, err)
		}
		if ip := firstIPv4(ni); ip != nil {
			return ip, nil
		}
		return nil, fmt.Errorf("interface %q has no IPv4 address", iface)
	}

	// Auto-detect: prefer a UDP dial to a public address, which picks the
	// kernel's default-route source IP without sending anything.
	if conn, err := net.Dial("udp4", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && a.IP.To4() != nil {
			return a.IP.To4(), nil
		}
	}

	// Fall back to the first up, non-loopback interface with an IPv4 address.
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, ni := range ifaces {
		if ni.Flags&net.FlagUp == 0 || ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip := firstIPv4(&ni); ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no suitable IPv4 interface found")
}

// MulticastInterfaces returns the interfaces SSDP should operate on: up,
// multicast-capable, non-loopback, and carrying an IPv4 address.
//
// SSDP must be present on every one of them. Joining the group on a single
// default interface — which is what net.ListenMulticastUDP with a nil interface
// does — silently loses discovery on a NAS with link aggregation, VLANs, or
// Docker bridges, because the kernel's default may not be the one the TVs are on.
//
// If filter is non-empty it selects a single interface by name or by one of its
// IP addresses.
func MulticastInterfaces(filter string) ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	wantIP := net.ParseIP(filter)

	var out []net.Interface
	for _, ni := range all {
		if ni.Flags&net.FlagUp == 0 ||
			ni.Flags&net.FlagMulticast == 0 ||
			ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip := firstIPv4(&ni)
		if ip == nil {
			continue
		}
		switch {
		case filter == "":
		case wantIP != nil:
			if !interfaceHasIP(&ni, wantIP) {
				continue
			}
		default:
			if ni.Name != filter {
				continue
			}
		}
		out = append(out, ni)
	}
	if len(out) == 0 {
		if filter != "" {
			return nil, fmt.Errorf("no up, multicast-capable IPv4 interface matches %q", filter)
		}
		return nil, fmt.Errorf("no up, multicast-capable IPv4 interface found")
	}
	return out, nil
}

// InterfaceIPv4 returns an interface's first usable IPv4 address.
func InterfaceIPv4(ni *net.Interface) net.IP { return firstIPv4(ni) }

func interfaceHasIP(ni *net.Interface, want net.IP) bool {
	addrs, err := ni.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(want) {
			return true
		}
	}
	return false
}

func firstIPv4(ni *net.Interface) net.IP {
	addrs, err := ni.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && !ip.IsLoopback() {
			if v4 := ip.To4(); v4 != nil {
				return v4
			}
		}
	}
	return nil
}

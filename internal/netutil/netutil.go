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

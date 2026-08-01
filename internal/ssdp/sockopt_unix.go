//go:build unix

package ssdp

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePort lets several sockets share UDP/1900.
//
// SSDP's port is a well-known shared one: a NAS often already runs its own
// media server or a discovery daemon on it. Without SO_REUSEADDR/SO_REUSEPORT
// the bind simply fails and Beacon becomes undiscoverable, even though multicast
// delivery to multiple listeners is exactly what the protocol expects.
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			serr = err
			return
		}
		// Not present on every unix; a failure here is not fatal because
		// SO_REUSEADDR alone is enough for multicast on Linux.
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

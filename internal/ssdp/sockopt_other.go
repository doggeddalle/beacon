//go:build !unix

package ssdp

import "syscall"

// reusePort is a no-op away from unix. The deployment target is Linux; on other
// platforms (Windows development hosts) Beacon simply takes the port or fails to
// bind, which Run reports as a non-fatal discovery error.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }

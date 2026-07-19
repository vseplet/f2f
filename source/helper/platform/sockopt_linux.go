//go:build linux

package platform

import "syscall"

// SetReuseAddr sets SO_REUSEADDR on a raw socket fd so a listener can rebind
// its address immediately after a restart drops the old socket (skipping the
// TIME_WAIT hold-down). Deliberately NOT SO_REUSEPORT: two live instances
// should still collide on bind, which is a useful "already running" signal
// rather than silent port-sharing.
func SetReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}

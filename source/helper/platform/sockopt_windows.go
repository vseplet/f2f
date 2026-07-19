//go:build windows

package platform

// SetReuseAddr is a no-op on Windows, by design.
//
// Winsock's SO_REUSEADDR is not the BSD option of the same name: instead of
// merely skipping the TIME_WAIT hold-down, it lets an unrelated process bind
// an address another socket is already listening on and steal its traffic.
// Setting it would therefore break the property the Unix path is after — that
// two live instances collide on bind, giving an "already running" signal.
//
// Windows also doesn't need the option for the restart case: a listening
// socket's address is released as soon as the socket closes, so rebinding
// after a restart already works.
func SetReuseAddr(fd uintptr) error { return nil }

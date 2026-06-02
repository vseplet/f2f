//go:build linux

package platform

import (
	"fmt"
	"net/netip"
	"os/exec"
)

// RouteAddIface installs a route for p, sending matching traffic
// into iface. iproute2 uses the same syntax for host (/32, /128) and
// net routes — the prefix length carries the distinction.
func RouteAddIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("ip", "route", "add", p.String(), "dev", iface).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route add %s dev %s: %w: %s", p, iface, err, out)
	}
	return nil
}

func RouteDeleteIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("ip", "route", "del", p.String(), "dev", iface).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route del %s dev %s: %w: %s", p, iface, err, out)
	}
	return nil
}

// RouteAddReject installs an "unreachable" route: the kernel returns
// ICMP unreachable / ENETUNREACH to senders.
func RouteAddReject(p netip.Prefix) error {
	out, err := exec.Command("ip", "route", "add", "unreachable", p.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route add unreachable %s: %w: %s", p, err, out)
	}
	return nil
}

func RouteDeleteReject(p netip.Prefix) error {
	out, err := exec.Command("ip", "route", "del", "unreachable", p.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route del unreachable %s: %w: %s", p, err, out)
	}
	return nil
}

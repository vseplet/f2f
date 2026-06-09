//go:build linux

package platform

import (
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
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

// routeGetIfaceRE extracts the device name from `ip route get`
// output ("1.2.3.4 via 10.0.0.1 dev tun0 src ...").
var routeGetIfaceRE = regexp.MustCompile(`\bdev\s+(\S+)`)

// RouteGetIface asks the kernel routing table which interface a
// packet to addr would leave through. Used by egress to NAT
// per-target traffic into the right tunnel (e.g. a corporate VPN's
// tun) instead of the default-route interface.
func RouteGetIface(addr netip.Addr) (string, error) {
	out, err := exec.Command("ip", "route", "get", addr.String()).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route get %s: %w: %s", addr, err, out)
	}
	m := routeGetIfaceRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("ip route get %s: no dev in output", addr)
	}
	return string(m[1]), nil
}

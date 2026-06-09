//go:build darwin

package platform

import (
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
)

// routeGetIfaceRE extracts the interface name from `route -n get`
// output ("  interface: utun5").
var routeGetIfaceRE = regexp.MustCompile(`(?m)^\s*interface:\s*(\S+)`)

// RouteGetIface asks the kernel routing table which interface a
// packet to addr would leave through. Used by egress to NAT
// per-target traffic into the right tunnel (e.g. a corporate VPN's
// utun) instead of the default-route interface.
func RouteGetIface(addr netip.Addr) (string, error) {
	family := "-inet"
	if addr.Is6() {
		family = "-inet6"
	}
	out, err := exec.Command("/sbin/route", "-n", "get", family, addr.String()).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("route -n get %s: %w: %s", addr, err, out)
	}
	m := routeGetIfaceRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("route -n get %s: no interface in output", addr)
	}
	return string(m[1]), nil
}

// RouteAddIface installs a route for prefix p, pointing at iface.
// macOS distinguishes -host (single address) from -net (CIDR); we
// pick based on the prefix length.
func RouteAddIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("/sbin/route", routeIfaceArgs("add", p, iface)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add %s -interface %s: %w: %s", p, iface, err, out)
	}
	return nil
}

// RouteDeleteIface removes an interface-bound route previously added
// by RouteAddIface (or by any other tool — kernel finds it by
// destination + family + iface).
func RouteDeleteIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("/sbin/route", routeIfaceArgs("delete", p, iface)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route delete %s -interface %s: %w: %s", p, iface, err, out)
	}
	return nil
}

// RouteAddReject installs a -reject route. The kernel responds with
// ICMP unreachable / ECONNREFUSED to any local sender. Use for
// destinations we want to "appear unreachable" to apps (e.g. IPv6
// targets so browsers' Happy Eyeballs falls back to IPv4 instantly).
func RouteAddReject(p netip.Prefix) error {
	out, err := exec.Command("/sbin/route", routeRejectArgs("add", p)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add -reject %s: %w: %s", p, err, out)
	}
	return nil
}

// RouteDeleteReject removes a -reject route.
func RouteDeleteReject(p netip.Prefix) error {
	out, err := exec.Command("/sbin/route", routeRejectArgs("delete", p)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route delete -reject %s: %w: %s", p, err, out)
	}
	return nil
}

func routeIfaceArgs(action string, p netip.Prefix, iface string) []string {
	family := "-inet"
	if p.Addr().Is6() {
		family = "-inet6"
	}
	if p.Bits() == p.Addr().BitLen() {
		return []string{"-n", action, family, "-host", p.Addr().String(), "-interface", iface}
	}
	return []string{"-n", action, family, "-net", p.String(), "-interface", iface}
}

// routeRejectArgs builds argv for installing/removing a -reject
// route. macOS's `route` requires a syntactic gateway even for
// -reject (otherwise it errors with "Invalid argument") — we pass
// the loopback address (::1 / 127.0.0.1) which is irrelevant to the
// rejected route's semantics (the kernel returns ICMP unreachable,
// packets never actually go anywhere). The delete form does not
// need either the gateway or the -reject flag.
func routeRejectArgs(action string, p netip.Prefix) []string {
	family := "-inet"
	if p.Addr().Is6() {
		family = "-inet6"
	}
	args := []string{"-n", action, family}
	if p.Bits() == p.Addr().BitLen() {
		args = append(args, "-host", p.Addr().String())
	} else {
		args = append(args, "-net", p.String())
	}
	if action == "add" {
		gw := "127.0.0.1"
		if p.Addr().Is6() {
			gw = "::1"
		}
		args = append(args, gw, "-reject")
	}
	return args
}

//go:build windows

package platform

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// RouteAddIface installs a route for p sending matching traffic into iface.
// store=active keeps it out of the persistent config: the route dies with the
// adapter instead of outliving a crashed process.
func RouteAddIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("netsh", "interface", "ipv4", "add", "route",
		"prefix="+p.String(), "interface="+iface, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh add route %s if %s: %w: %s", p, iface, err, out)
	}
	return nil
}

func RouteDeleteIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("netsh", "interface", "ipv4", "delete", "route",
		"prefix="+p.String(), "interface="+iface, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh delete route %s if %s: %w: %s", p, iface, err, out)
	}
	return nil
}

// RouteAddReject has no Windows equivalent. The Linux/macOS versions install an
// "unreachable"/"reject" route so the stack fails senders fast with ICMP
// unreachable; the Windows routing table has no such route type (a blackhole
// would silently drop instead, which is a different, worse behaviour — senders
// hang until timeout rather than failing immediately). Intercepts that rely on
// reject routes are therefore unavailable here.
func RouteAddReject(p netip.Prefix) error    { return ErrUnsupported }
func RouteDeleteReject(p netip.Prefix) error { return ErrUnsupported }

// RouteGetIface asks Windows which interface a packet to addr would leave
// through. Find-NetRoute performs the same best-match lookup the stack itself
// does, and InterfaceAlias is the friendly name netsh and our other calls use.
func RouteGetIface(addr netip.Addr) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("(Find-NetRoute -RemoteIPAddress %s -ErrorAction Stop | Select-Object -First 1).InterfaceAlias", addr),
	).Output()
	if err != nil {
		return "", fmt.Errorf("Find-NetRoute %s: %w", addr, err)
	}
	iface := strings.TrimSpace(string(out))
	if iface == "" {
		return "", fmt.Errorf("Find-NetRoute %s: no interface in output", addr)
	}
	return iface, nil
}

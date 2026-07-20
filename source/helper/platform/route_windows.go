//go:build windows

package platform

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// loopbackIfIndex is the "Loopback Pseudo-Interface 1" index, stable across
// Windows installs. Used to blackhole a prefix — see RouteAddReject.
const loopbackIfIndex = "1"

// family picks the netsh context for a prefix. netsh keeps IPv4 and IPv6
// routing under separate sub-commands, and passing a v6 prefix to the v4
// context just fails.
func family(p netip.Prefix) string {
	if p.Addr().Is4() {
		return "ipv4"
	}
	return "ipv6"
}

// RouteAddIface installs a route for p sending matching traffic into iface.
//
// Delete-then-add rather than a plain add: netsh treats an existing identical
// route as an error, and its messages are localized, so matching the text to
// tell "already there" apart from a real failure isn't portable. Removing first
// makes the call idempotent in any language — and also clears a stale route a
// previously crashed process left pointing at a dead interface.
//
// store=active keeps routes out of the persistent config so they die with the
// process instead of outliving it.
func RouteAddIface(p netip.Prefix, iface string) error {
	fam := family(p)
	_ = exec.Command("netsh", "interface", fam, "delete", "route",
		"prefix="+p.String(), "interface="+iface, "store=active").Run()

	out, err := exec.Command("netsh", "interface", fam, "add", "route",
		"prefix="+p.String(), "interface="+iface, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh add route %s if %s: %w: %s", p, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteDeleteIface removes the route. A prefix that isn't there is not an error
// — callers delete speculatively before adding.
func RouteDeleteIface(p netip.Prefix, iface string) error {
	out, err := exec.Command("netsh", "interface", family(p), "delete", "route",
		"prefix="+p.String(), "interface="+iface, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh delete route %s if %s: %w: %s", p, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteAddReject makes p unreachable, so traffic for an intercepted name can't
// leak around the tunnel — in practice this is the IPv6 side of a domain we
// route over IPv4, and without it the browser happily connects over v6 instead.
//
// Windows has no "unreachable" route type, so point the prefix at the loopback
// pseudo-interface: packets are handed to a stack that has no listener for them
// and fail immediately, which is the fast failure a reject route exists to
// produce. A blackhole would instead hang the caller until it times out.
func RouteAddReject(p netip.Prefix) error {
	fam := family(p)
	_ = exec.Command("netsh", "interface", fam, "delete", "route",
		"prefix="+p.String(), "interface="+loopbackIfIndex, "store=active").Run()

	out, err := exec.Command("netsh", "interface", fam, "add", "route",
		"prefix="+p.String(), "interface="+loopbackIfIndex, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh add reject route %s: %w: %s", p, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func RouteDeleteReject(p netip.Prefix) error {
	out, err := exec.Command("netsh", "interface", family(p), "delete", "route",
		"prefix="+p.String(), "interface="+loopbackIfIndex, "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh delete reject route %s: %w: %s", p, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteGetIface asks Windows which interface a packet to addr would leave
// through. Find-NetRoute runs the same best-match lookup the stack itself does,
// and InterfaceAlias is the friendly name netsh and our other calls take.
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

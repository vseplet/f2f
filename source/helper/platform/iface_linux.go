//go:build linux

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// DefaultEgressInterface returns the interface name of the current
// IPv4 default route. tun*/tap*/f2f* names are rejected so we don't
// pick a tunnel as our egress.
//
// Implementation: `ip -4 route show default` prints lines like
// "default via 192.168.1.1 dev en0 proto dhcp". We grab the token
// after "dev".
func DefaultEgressInterface() (string, error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("ip -4 route show default: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f != "dev" || i+1 >= len(fields) {
			continue
		}
		name := fields[i+1]
		if strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "tap") || strings.HasPrefix(name, "f2f") {
			return "", fmt.Errorf("default route is a tunnel (%s); refusing to nest egress", name)
		}
		return name, nil
	}
	return "", fmt.Errorf("ip -4 route show default: no dev field in output: %s", out)
}

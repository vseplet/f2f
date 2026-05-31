//go:build darwin

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// DefaultEgressInterface returns the interface name of the current
// IPv4 default route. utun* interfaces (any macOS tunnel — ours or
// a third-party VPN's) are rejected so we don't pick a tunnel as
// our egress.
func DefaultEgressInterface() (string, error) {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", fmt.Errorf("route -n get default: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "interface:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		if strings.HasPrefix(name, "utun") {
			return "", fmt.Errorf("default route is a tunnel (%s); refusing to nest egress", name)
		}
		return name, nil
	}
	return "", fmt.Errorf("route -n get default: no interface line in output")
}

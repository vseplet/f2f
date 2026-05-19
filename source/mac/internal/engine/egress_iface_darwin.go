//go:build darwin

package engine

import (
	"os/exec"
	"strings"
)

// detectDefaultRouteIface returns the interface name of the current IPv4
// default route, or "" if it can't be determined. utun* interfaces are
// rejected so we don't pick our own tunnel as egress when another VPN
// owns the default route.
func detectDefaultRouteIface() string {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "interface:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		if strings.HasPrefix(name, "utun") {
			return ""
		}
		return name
	}
	return ""
}

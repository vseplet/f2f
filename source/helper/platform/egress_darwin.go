//go:build darwin

package platform

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// egressAnchor lives under com.apple/* so the default /etc/pf.conf
// evaluates it via its `nat-anchor "com.apple/*"` wildcard. The name
// is unique enough to avoid colliding with Apple's own VPN/NE
// entries.
const egressAnchor = "com.apple/f2f-mac"

const sysctlIPForwarding = "net.inet.ip.forwarding"

// GetIPForwarding reads the current ip-forwarding sysctl as a
// string ("0" or "1") so callers can restore it later without
// caring about which key the OS uses.
func GetIPForwarding() (string, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", sysctlIPForwarding).Output()
	if err != nil {
		return "", fmt.Errorf("sysctl -n %s: %w", sysctlIPForwarding, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SetIPForwarding writes the ip-forwarding sysctl.
func SetIPForwarding(value string) error {
	out, err := exec.Command("/usr/sbin/sysctl", "-w", sysctlIPForwarding+"="+value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl -w %s=%s: %w: %s", sysctlIPForwarding, value, err, out)
	}
	return nil
}

// pfTokenRE extracts the numeric token from `pfctl -E` output, which
// looks like "Token : 1234567890" on one of its lines.
var pfTokenRE = regexp.MustCompile(`Token\s*:\s*(\d+)`)

// EnableFilterEngine turns pf on via the reference-counted -E
// interface: pf stays enabled until every -E token has been released
// via -X, so we can coexist with other tools that also flipped it.
func EnableFilterEngine() (FilterEngineToken, error) {
	out, err := exec.Command("/sbin/pfctl", "-E").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pfctl -E: %w: %s", err, out)
	}
	m := pfTokenRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("pfctl -E: no token in output: %s", out)
	}
	return FilterEngineToken(m[1]), nil
}

// DisableFilterEngine returns a token previously acquired via
// EnableFilterEngine. pf turns off when the last token is released.
func DisableFilterEngine(t FilterEngineToken) error {
	if t == "" {
		return nil
	}
	out, err := exec.Command("/sbin/pfctl", "-X", string(t)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -X %s: %w: %s", t, err, out)
	}
	return nil
}

// InstallNAT loads the egress NAT anchor that translates packets
// leaving Subnet via EgressIface to that interface's address.
func InstallNAT(r NATRules) error {
	rule := fmt.Sprintf("nat on %s from %s to any -> (%s)\n", r.EgressIface, r.Subnet, r.EgressIface)
	cmd := exec.Command("/sbin/pfctl", "-a", egressAnchor, "-f", "-")
	cmd.Stdin = strings.NewReader(rule)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -a %s -f -: %w: %s\nrule: %s", egressAnchor, err, out, rule)
	}
	return nil
}

// RemoveNAT flushes the egress anchor. Idempotent.
func RemoveNAT() error {
	out, err := exec.Command("/sbin/pfctl", "-a", egressAnchor, "-F", "all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -a %s -F all: %w: %s", egressAnchor, err, out)
	}
	return nil
}

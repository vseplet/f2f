//go:build linux

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

const sysctlIPForwarding = "net.ipv4.ip_forward"

func GetIPForwarding() (string, error) {
	out, err := exec.Command("sysctl", "-n", sysctlIPForwarding).Output()
	if err != nil {
		return "", fmt.Errorf("sysctl -n %s: %w", sysctlIPForwarding, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func SetIPForwarding(value string) error {
	out, err := exec.Command("sysctl", "-w", sysctlIPForwarding+"="+value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl -w %s=%s: %w: %s", sysctlIPForwarding, value, err, out)
	}
	return nil
}

// EnableFilterEngine is a no-op on Linux: nftables is always loaded
// and ready to apply rules. Returns an empty token.
func EnableFilterEngine() (FilterEngineToken, error) { return "", nil }

// DisableFilterEngine is a no-op on Linux.
func DisableFilterEngine(FilterEngineToken) error { return nil }

// InstallNAT creates an nftables postrouting masquerade for traffic
// leaving from Subnet via EgressIface. The declare-then-delete
// header makes the install/replace atomic — nft runs the whole
// script as one transaction, so there's no window where masquerade
// is half-installed.
func InstallNAT(r NATRules) error {
	rules := fmt.Sprintf(`table ip f2fnat {}
delete table ip f2fnat
table ip f2fnat {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr %s oif "%s" masquerade
  }
}
`, r.Subnet, r.EgressIface)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft -f -: %w: %s\nruleset:\n%s", err, out, rules)
	}
	return nil
}

func RemoveNAT() error {
	out, err := exec.Command("nft", "delete", "table", "ip", "f2fnat").CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no such") || strings.Contains(msg, "does not exist") {
			return nil
		}
		return fmt.Errorf("nft delete table ip f2fnat: %w: %s", err, out)
	}
	return nil
}

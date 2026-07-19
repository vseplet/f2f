//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Windows Firewall can't scope a rule to an interface name the way nft's
// `iif f2f0` or pf on a device does. What it CAN scope on is the local address,
// and every f2f node has a unique overlay IP — so `localip=<TunnelIP>` selects
// exactly the inbound traffic arriving on the tunnel and nothing else.
//
// Two Windows specifics make this simpler than the nft version:
//   - Rule order is irrelevant: an Allow always wins over a Block, so
//     "allow the listed ports, block everything else to this address" gives
//     default-deny with no priorities to manage.
//   - Stateful inspection is on by default, so replies to connections WE open
//     (the bus dialling out, etc.) come back without an explicit
//     established/related rule — that behaviour is free here.
//
// Outbound is left fully open, matching the policy contract.

// ruleNames are every rule this file may create, listed so RemoveFirewall can
// clean up by name (netsh delete takes a name, not the group).
var ruleNames = []string{"f2f-block-all", "f2f-icmp", "f2f-tcp", "f2f-udp"}

// InstallFirewall replaces the f2f rule set: drop whatever we had, then lay down
// a catch-all block plus ICMP and per-port allows, all scoped to the overlay IP.
// Not atomic like `nft -f` — rules go in one at a time — so the block goes
// FIRST: during the brief rebuild window the address is closed, not open.
func InstallFirewall(p FirewallPolicy) error {
	if p.TunnelIP == "" {
		return fmt.Errorf("firewall: empty TunnelIP")
	}
	if err := RemoveFirewall(); err != nil {
		return err
	}
	if err := netshAddRule("f2f-block-all", "any", nil, p.TunnelIP, "block"); err != nil {
		return err
	}
	// ICMP so a peer's liveness ping is answered (the policy allows it).
	if err := netshAddRule("f2f-icmp", "icmpv4", nil, p.TunnelIP, "allow"); err != nil {
		return err
	}
	if len(p.AllowTCP) > 0 {
		if err := netshAddRule("f2f-tcp", "tcp", p.AllowTCP, p.TunnelIP, "allow"); err != nil {
			return err
		}
	}
	if len(p.AllowUDP) > 0 {
		if err := netshAddRule("f2f-udp", "udp", p.AllowUDP, p.TunnelIP, "allow"); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFirewall deletes every rule we may have created. netsh reports an error
// when a name isn't present; that's expected on first install and on the rules
// a given policy didn't create, so it's swallowed.
func RemoveFirewall() error {
	for _, name := range ruleNames {
		_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name).Run()
	}
	return nil
}

// netshAddRule adds one inbound rule scoped to localAddr. ports applies to
// tcp/udp only; an empty list omits the port match (correct for icmp/any).
func netshAddRule(name, proto string, ports []int, localAddr, action string) error {
	args := []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + name,
		"dir=in",
		"action=" + action,
		"protocol=" + proto,
		"localip=" + localAddr,
		"enable=yes",
	}
	if len(ports) > 0 {
		args = append(args, "localport="+joinPorts(ports))
	}
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh add rule %s (%s): %w: %s", name, proto, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

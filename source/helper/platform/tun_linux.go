//go:build linux

package platform

import (
	"fmt"
	"os/exec"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

// Linux TUN with IFF_NO_PI (wgtun's default) delivers raw IP packets
// with no prefix. wgtun's Read/Write offset parameter is purely for
// the caller's own packet headroom, not an OS-imposed prefix.
const tunAFPrefixLen = 0

// CreateTUN opens /dev/net/tun via TUNSETIFF (wgtun handles the
// ioctl). Linux assigns the next free index for the requested name
// pattern — we ask for "f2f%d", so the kernel picks "f2f0", "f2f1", ...
func CreateTUN(mtu int) (wgtun.Device, string, int, error) {
	dev, err := wgtun.CreateTUN("f2f%d", mtu)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create tun: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", 0, fmt.Errorf("get tun name: %w", err)
	}
	return dev, name, tunAFPrefixLen, nil
}

// IfconfigP2P assigns localIP with peerIP as the point-to-point peer
// and brings the link up via iproute2.
func IfconfigP2P(iface, localIP, peerIP string) error {
	if out, err := exec.Command("ip", "addr", "add", localIP, "peer", peerIP, "dev", iface).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr add %s peer %s dev %s: %w: %s", localIP, peerIP, iface, err, out)
	}
	if out, err := exec.Command("ip", "link", "set", iface, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set %s up: %w: %s", iface, err, out)
	}
	return nil
}

// IfDisableMulticast turns off the MULTICAST flag on the interface.
func IfDisableMulticast(iface string) error {
	if out, err := exec.Command("ip", "link", "set", iface, "multicast", "off").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set %s multicast off: %w: %s", iface, err, out)
	}
	return nil
}

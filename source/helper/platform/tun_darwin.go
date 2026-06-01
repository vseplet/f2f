//go:build darwin

package platform

import (
	"fmt"
	"os/exec"

	wgtun "github.com/amnezia-vpn/amneziawg-go/tun"
)

// macOS utun: the kernel prepends a 4-byte address-family header on
// every read and expects the same on writes. wgtun reserves
// bufs[0][offset-4:offset] for that prefix when the caller passes
// offset=4.
const tunAFPrefixLen = 4

// CreateTUN brings up a virtual interface and returns the wgtun
// device, the kernel-assigned name (e.g. "utun5"), and the AF-prefix
// length the caller must use when calling Read/Write on the device.
func CreateTUN(mtu int) (wgtun.Device, string, int, error) {
	dev, err := wgtun.CreateTUN("utun", mtu)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create utun: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", 0, fmt.Errorf("get utun name: %w", err)
	}
	return dev, name, tunAFPrefixLen, nil
}

// IfconfigP2P brings the interface up as a point-to-point link with
// the given local/peer IPv4 addresses. The peer address is mostly
// cosmetic — macOS utun requires a P2P pair, but nothing on the far
// end actually owns peerIP.
func IfconfigP2P(iface, localIP, peerIP string) error {
	out, err := exec.Command("/sbin/ifconfig", iface, "inet", localIP, peerIP, "up").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s: %w: %s", iface, err, out)
	}
	return nil
}

// IfDisableMulticast drops the MULTICAST flag on the interface.
// macOS otherwise picks every multicast-capable interface for
// SSDP/mDNS/UPnP destinations (239.255.255.250 et al), so the tunnel
// would receive a copy of every local service-discovery query. We
// can't deliver multicast to overlay peers anyway — they're routed
// via per-peer UDP, no group state.
func IfDisableMulticast(iface string) error {
	out, err := exec.Command("/sbin/ifconfig", iface, "-multicast").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s -multicast: %w: %s", iface, err, out)
	}
	return nil
}

// IfDisableOffload is a no-op on macOS — utun does not expose GSO/TSO
// the way Linux TUN does, and packets always arrive ≤ MTU.
func IfDisableOffload(iface string) error { return nil }

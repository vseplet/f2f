//go:build linux

package platform

import (
	"fmt"
	"os/exec"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

// wgtun on Linux enables TUN's VNET_HDR mode (TUNSETOFFLOAD) for GSO
// support: every read returns a 10-byte virtio_net_hdr followed by
// the IP packet, and every write must reserve 10 bytes of headroom
// for wgtun to fill in the same header. So our "offset" is really
// virtioNetHdrLen — wgtun's handleGRO will return "invalid offset"
// for anything smaller.
const tunAFPrefixLen = 10

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
// and brings the link up via iproute2. Peer prefix is pinned to /32
// — older iproute2 builds reject a bare peer address.
func IfconfigP2P(iface, localIP, peerIP string) error {
	if out, err := exec.Command("ip", "addr", "add", localIP, "peer", peerIP+"/32", "dev", iface).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr add %s peer %s/32 dev %s: %w: %s", localIP, peerIP, iface, err, out)
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

// IfDisableOffload caps GSO super-packet size on the tun so the
// kernel never coalesces TCP/UDP segments past MTU. wgtun opens TUN
// with TUNSETOFFLOAD enabling GSO, which otherwise lets the kernel
// hand the tun super-packets up to 64 KiB. Our engine reads one
// packet per Tunnel.Read with a single MTU-sized buffer, so super-
// packets get dropped at qdisc level.
//
// gso_max_size caps the byte size; gso_max_segs caps the segment
// count. Both belong to iproute2 5.10+ / kernel 4.18+, which are
// present on any current distro (NixOS, Debian 11+, etc.).
func IfDisableOffload(iface string) error {
	if out, err := exec.Command("ip", "link", "set", "dev", iface,
		"gso_max_size", "1420",
		"gso_max_segs", "1",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set %s gso_max_size: %w: %s", iface, err, out)
	}
	return nil
}

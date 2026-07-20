//go:build windows

package platform

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	wgtun "github.com/amnezia-vpn/amneziawg-go/tun"
)

// wintun hands us bare IP packets: no AF prefix (macOS) and no virtio_net_hdr
// (Linux GSO mode), so reads and writes need no headroom.
const tunAFPrefixLen = 0

// adapterName is the wintun adapter's friendly name. Unlike Linux there is no
// "%d" pattern — Windows takes the literal name we ask for, and reuses/replaces
// an existing adapter of the same name.
const adapterName = "f2f"

// CreateTUN creates (or reopens) the wintun adapter. Needs wintun.dll beside
// the executable — the driver is loaded from it at runtime — and Administrator
// rights to create the network adapter.
func CreateTUN(mtu int) (wgtun.Device, string, int, error) {
	dev, err := wgtun.CreateTUN(adapterName, mtu)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create wintun adapter (wintun.dll present? running as Administrator?): %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", 0, fmt.Errorf("get tun name: %w", err)
	}
	return dev, name, tunAFPrefixLen, nil
}

// IfconfigP2P assigns localIP to the adapter. Windows has no point-to-point
// address form, so peerIP is unused: the address goes on as a /32 host address
// and reachability comes from the explicit interface routes the caller adds
// afterwards (RouteAddIface). store=active keeps it out of the persistent
// config — the adapter is torn down with the process.
func IfconfigP2P(iface, localIP, peerIP string) error {
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "address",
		"name="+iface, "source=static", "address="+localIP,
		"mask=255.255.255.255", "store=active",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set address %s on %s: %w: %s", localIP, iface, err, out)
	}
	return waitAddrBindable(localIP, 15*time.Second)
}

// waitAddrBindable blocks until addr can actually be bound to.
//
// netsh returns as soon as Windows accepts the configuration, but the address
// stays unusable for a moment while the stack plumbs it through. Services that
// bind the overlay IP the instant the tunnel opens (proxy :80/:443, torrent
// :6881, the QUIC bus) otherwise fail with WSAEADDRNOTAVAIL — "The requested
// address is not valid in its context" — and only the ones that happen to retry
// recover. Binding a throwaway socket is the same operation they perform, so it
// tests exactly the condition that matters.
func waitAddrBindable(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ln, err := net.ListenPacket("udp", net.JoinHostPort(addr, "0"))
		if err == nil {
			_ = ln.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("address %s not bindable after %s: %w", addr, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// IfDisableMulticast is a no-op on Windows: wintun adapters carry no multicast
// flag to clear. Returning nil keeps the caller from logging a warning on every
// start for something that doesn't apply here.
func IfDisableMulticast(iface string) error { return nil }

// IfDisableOffload is a no-op on Windows. The Linux version caps GSO
// super-packets because wgtun opens TUN in VNET_HDR mode there; wintun has no
// such mode and already hands us one IP packet per read.
func IfDisableOffload(iface string) error { return nil }

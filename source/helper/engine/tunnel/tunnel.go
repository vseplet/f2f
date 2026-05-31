//go:build darwin

// Package tunnel owns the lifecycle of a macOS utun virtual network interface.
// Closing a Tunnel removes the interface; the kernel then drops every route
// that pointed at it, so cleanup of stray state is automatic.
package tunnel

import (
	"fmt"
	"log"
	"net/netip"
	"os/exec"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

const (
	// MTU we configure on the utun interface. Conservative — leaves room for
	// the UDP transport + our header that will wrap packets in later milestones.
	MTU = 1420

	// wireguard/tun on darwin reserves 4 bytes at bufs[0][offset-4:offset] for
	// the address-family prefix that the kernel utun socket prepends/expects.
	// Callers pass buffers large enough to hold MTU + this prefix.
	afPrefixLen = 4
)

// Tunnel owns one utun interface. Methods are NOT safe for concurrent use
// from multiple goroutines; the intended pattern is one reader goroutine that
// also performs writes in response to incoming packets.
type Tunnel struct {
	dev  wgtun.Device
	name string

	readBuf   []byte
	readBufs  [][]byte
	readSizes []int

	writeBuf  []byte
	writeBufs [][]byte
}

// Open creates a utun interface (the kernel picks the number) and configures
// it as a point-to-point link with the given local/peer IPv4 addresses.
// peerIP is mostly cosmetic at this stage — nothing on the far end exists yet —
// but a point-to-point address is the conventional way to bring utun up on macOS.
func Open(localIP, peerIP string) (*Tunnel, error) {
	dev, name, err := createUtun()
	if err != nil {
		return nil, err
	}
	if err := ifconfigUp(name, localIP, peerIP); err != nil {
		_ = dev.Close()
		return nil, err
	}
	return newTunnel(dev, name), nil
}

// OpenSubnet brings a utun up that owns an entire IPv4 subnet:
// `ifconfig <name> inet <localIP> <localIP> up` (self-loop point-to-point
// because macOS utun requires a P2P pair) plus a `route add -net <subnet>
// -interface <name>` so every address in the subnet routes through us.
// Used in Camp mode where peer tunnel IPs are assigned from a pool and
// not all of them are known at startup.
func OpenSubnet(localIP string, prefixLen int) (*Tunnel, error) {
	a, err := netip.ParseAddr(localIP)
	if err != nil {
		return nil, fmt.Errorf("parse local %q: %w", localIP, err)
	}
	if !a.Is4() {
		return nil, fmt.Errorf("only IPv4 supported, got %q", localIP)
	}
	subnet := netip.PrefixFrom(a, prefixLen).Masked().String()

	dev, name, err := createUtun()
	if err != nil {
		return nil, err
	}
	if err := ifconfigUp(name, localIP, localIP); err != nil {
		_ = dev.Close()
		return nil, err
	}
	if err := routeAddSubnet(subnet, name); err != nil {
		_ = dev.Close()
		return nil, err
	}
	return newTunnel(dev, name), nil
}

func createUtun() (wgtun.Device, string, error) {
	dev, err := wgtun.CreateTUN("utun", MTU)
	if err != nil {
		return nil, "", fmt.Errorf("create utun: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, "", fmt.Errorf("get utun name: %w", err)
	}
	return dev, name, nil
}

func newTunnel(dev wgtun.Device, name string) *Tunnel {
	return &Tunnel{
		dev:       dev,
		name:      name,
		readBuf:   make([]byte, MTU+afPrefixLen),
		readBufs:  make([][]byte, 1),
		readSizes: make([]int, 1),
		writeBuf:  make([]byte, MTU+afPrefixLen),
		writeBufs: make([][]byte, 1),
	}
}

// Name returns the assigned interface name, e.g. "utun5".
func (t *Tunnel) Name() string { return t.name }

// Read blocks until one IP packet arrives, then returns a slice pointing into
// an internal buffer. The slice is valid only until the next Read call.
// A zero-length slice with nil error means "no packet this round, try again".
func (t *Tunnel) Read() ([]byte, error) {
	t.readBufs[0] = t.readBuf
	t.readSizes[0] = 0
	n, err := t.dev.Read(t.readBufs, t.readSizes, afPrefixLen)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	size := t.readSizes[0]
	return t.readBuf[afPrefixLen : afPrefixLen+size], nil
}

// Write injects one IP packet into the interface. The packet must start with
// the IP header (no AF prefix — Tunnel adds that).
func (t *Tunnel) Write(pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	if len(pkt) > MTU {
		return fmt.Errorf("packet %d bytes exceeds MTU %d", len(pkt), MTU)
	}
	copy(t.writeBuf[afPrefixLen:], pkt)
	t.writeBufs[0] = t.writeBuf[:afPrefixLen+len(pkt)]
	_, err := t.dev.Write(t.writeBufs, afPrefixLen)
	return err
}

// Close tears down the interface.
func (t *Tunnel) Close() error {
	return t.dev.Close()
}

func ifconfigUp(ifname, localIP, peerIP string) error {
	cmd := exec.Command("/sbin/ifconfig", ifname, "inet", localIP, peerIP, "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s: %w: %s", ifname, err, out)
	}
	// Drop the MULTICAST flag: macOS otherwise picks every multicast-
	// capable interface for SSDP/mDNS/UPnP broadcast destinations
	// (239.255.255.250 et al) and our utun gets a copy of every local
	// service-discovery query. We can't deliver multicast to overlay
	// peers anyway — they're routed via per-peer UDP, no group state.
	// Failure is logged but non-fatal: worst case the log gets noisier.
	off := exec.Command("/sbin/ifconfig", ifname, "-multicast")
	if out, err := off.CombinedOutput(); err != nil {
		log.Printf("tunnel: ifconfig %s -multicast: %v: %s", ifname, err, out)
	}
	return nil
}

func routeAddSubnet(subnet, ifname string) error {
	// Delete first: a stale route from a prior crashed process on a
	// different utun would shadow our add and silently send traffic
	// to the zombie interface.
	_ = exec.Command("/sbin/route", "-n", "delete", "-inet", "-net", subnet).Run()
	cmd := exec.Command("/sbin/route", "-n", "add", "-inet", "-net", subnet, "-interface", ifname)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("route add %s -interface %s: %w: %s", subnet, ifname, err, out)
	}
	return nil
}


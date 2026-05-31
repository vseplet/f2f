// Package tunnel owns the lifecycle of one virtual TUN interface.
// Closing a Tunnel removes the interface; the kernel then drops every
// route that pointed at it, so cleanup of stray state is automatic.
package tunnel

import (
	"fmt"
	"log"
	"net/netip"

	"github.com/vseplet/f2f/source/helper/platform"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// MTU we configure on the tunnel. Conservative — leaves room for the
// UDP transport + our header that wraps packets in later milestones.
const MTU = 1420

// Tunnel owns one virtual interface. Methods are NOT safe for
// concurrent use from multiple goroutines; the intended pattern is
// one reader goroutine that also performs writes in response to
// incoming packets.
type Tunnel struct {
	dev      wgtun.Device
	name     string
	afPrefix int // bytes wgtun reserves before the IP header (per-OS)

	readBuf   []byte
	readBufs  [][]byte
	readSizes []int

	writeBuf  []byte
	writeBufs [][]byte
}

// Open creates a TUN interface and configures it as a point-to-point
// link with the given local/peer IPv4 addresses. The peer address is
// mostly cosmetic at this stage — nothing on the far end exists yet —
// but a point-to-point address is the conventional way to bring utun
// up on macOS.
func Open(localIP, peerIP string) (*Tunnel, error) {
	dev, name, afLen, err := platform.CreateTUN(MTU)
	if err != nil {
		return nil, err
	}
	if err := platform.IfconfigP2P(name, localIP, peerIP); err != nil {
		_ = dev.Close()
		return nil, err
	}
	if err := platform.IfDisableMulticast(name); err != nil {
		log.Printf("tunnel: %v", err)
	}
	return newTunnel(dev, name, afLen), nil
}

// OpenSubnet brings a tunnel up that owns an entire IPv4 subnet via
// a self-loop point-to-point (macOS utun requires a P2P pair) plus
// a route covering the whole subnet through this interface. Used in
// Camp mode where peer tunnel IPs are assigned from a pool and not
// all of them are known at startup.
func OpenSubnet(localIP string, prefixLen int) (*Tunnel, error) {
	a, err := netip.ParseAddr(localIP)
	if err != nil {
		return nil, fmt.Errorf("parse local %q: %w", localIP, err)
	}
	if !a.Is4() {
		return nil, fmt.Errorf("only IPv4 supported, got %q", localIP)
	}
	subnet := netip.PrefixFrom(a, prefixLen).Masked()

	dev, name, afLen, err := platform.CreateTUN(MTU)
	if err != nil {
		return nil, err
	}
	if err := platform.IfconfigP2P(name, localIP, localIP); err != nil {
		_ = dev.Close()
		return nil, err
	}
	if err := platform.IfDisableMulticast(name); err != nil {
		log.Printf("tunnel: %v", err)
	}
	// Delete-then-add: a stale route from a prior crashed process on
	// a different tunnel would shadow our add and silently send
	// traffic to the zombie interface. We don't care if the delete
	// fails — it usually means there was nothing to delete.
	_ = platform.RouteDeleteIface(subnet, name)
	if err := platform.RouteAddIface(subnet, name); err != nil {
		_ = dev.Close()
		return nil, err
	}
	return newTunnel(dev, name, afLen), nil
}

func newTunnel(dev wgtun.Device, name string, afLen int) *Tunnel {
	return &Tunnel{
		dev:       dev,
		name:      name,
		afPrefix:  afLen,
		readBuf:   make([]byte, MTU+afLen),
		readBufs:  make([][]byte, 1),
		readSizes: make([]int, 1),
		writeBuf:  make([]byte, MTU+afLen),
		writeBufs: make([][]byte, 1),
	}
}

// Name returns the assigned interface name, e.g. "utun5" on macOS or
// "f2f0" on Linux.
func (t *Tunnel) Name() string { return t.name }

// Read blocks until one IP packet arrives, then returns a slice
// pointing into an internal buffer. The slice is valid only until
// the next Read call. A zero-length slice with nil error means
// "no packet this round, try again".
func (t *Tunnel) Read() ([]byte, error) {
	t.readBufs[0] = t.readBuf
	t.readSizes[0] = 0
	n, err := t.dev.Read(t.readBufs, t.readSizes, t.afPrefix)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	size := t.readSizes[0]
	return t.readBuf[t.afPrefix : t.afPrefix+size], nil
}

// Write injects one IP packet into the interface. The packet must
// start with the IP header (no AF prefix — Tunnel adds that when the
// OS requires one).
func (t *Tunnel) Write(pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	if len(pkt) > MTU {
		return fmt.Errorf("packet %d bytes exceeds MTU %d", len(pkt), MTU)
	}
	copy(t.writeBuf[t.afPrefix:], pkt)
	t.writeBufs[0] = t.writeBuf[:t.afPrefix+len(pkt)]
	_, err := t.dev.Write(t.writeBufs, t.afPrefix)
	return err
}

// Close tears down the interface.
func (t *Tunnel) Close() error {
	return t.dev.Close()
}

//go:build darwin

// Package tunnel owns the lifecycle of a macOS utun virtual network interface.
// Closing a Tunnel removes the interface; the kernel then drops every route
// that pointed at it, so cleanup of stray state is automatic.
package tunnel

import (
	"fmt"
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

// Tunnel owns one utun interface.
type Tunnel struct {
	dev  wgtun.Device
	name string

	readBuf   []byte
	readBufs  [][]byte
	readSizes []int
}

// Open creates a utun interface (the kernel picks the number) and configures
// it as a point-to-point link with the given local/peer IPv4 addresses.
// peerIP is mostly cosmetic at this stage — nothing on the far end exists yet —
// but a point-to-point address is the conventional way to bring utun up on macOS.
func Open(localIP, peerIP string) (*Tunnel, error) {
	dev, err := wgtun.CreateTUN("utun", MTU)
	if err != nil {
		return nil, fmt.Errorf("create utun: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, fmt.Errorf("get utun name: %w", err)
	}
	if err := ifconfigUp(name, localIP, peerIP); err != nil {
		_ = dev.Close()
		return nil, err
	}
	return &Tunnel{
		dev:       dev,
		name:      name,
		readBuf:   make([]byte, MTU+afPrefixLen),
		readBufs:  make([][]byte, 1),
		readSizes: make([]int, 1),
	}, nil
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

// Close tears down the interface.
func (t *Tunnel) Close() error {
	return t.dev.Close()
}

func ifconfigUp(ifname, localIP, peerIP string) error {
	cmd := exec.Command("/sbin/ifconfig", ifname, "inet", localIP, peerIP, "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s: %w: %s", ifname, err, out)
	}
	return nil
}

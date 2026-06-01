// Package awg wires the amneziawg-go device to f2f's existing
// transport: the single UDP socket owned by Engine, the utun device
// owned by the tunnel package, and the verified peer information
// gathered through pair-handshake.
//
// This file implements conn.Bind — the interface amneziawg-go uses to
// talk to "the network". Standard Bind impls (StdNetBind) open their
// own UDP socket; we do NOT — we already own `e.udp` for hole-punch,
// camp announce, and pair-handshake. So our Bind multiplexes on the
// SAME socket: the engine's peerToTunLoop classifies each incoming
// packet by magic-header range, and any packet that lands in the
// AWG slot (H1..H4) gets forwarded into the Bind via Deliver(). The
// AWG Device drains the inbox via the ReceiveFunc returned from Open().
//
// Outbound: Bind.Send writes via the shared UDP socket. AWG packets
// already have their magic header prepended by the device — Bind just
// forwards the bytes verbatim.
//
// Lifecycle: created (but not Open'd) at Engine.Start, Open'd by
// device.NewDevice's internal initialization, Close'd at Engine.Stop.
// The shared UDP socket is owned by the engine, NOT the Bind — closing
// the Bind doesn't close the socket.
package awg

import (
	"errors"
	"net"
	"net/netip"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/conn"
)

// inboxBufferSize is how many AWG packets can be queued between
// peerToTunLoop's Deliver() call and the AWG goroutine's drain via
// ReceiveFunc. 64 is enough for a brief burst — AWG handshake and
// keepalive cadence is low, so deeper queues would just buffer stale
// packets. On overflow, Deliver drops the packet and pair_req's
// keepalive eventually retransmits anyway.
const inboxBufferSize = 64

// inboxItem is one packet handed off from engine to AWG device.
// data is a fresh copy — engine's peerToTunLoop reuses its read buffer
// for every recvfrom syscall, so we must copy before queueing.
type inboxItem struct {
	data []byte
	src  netip.AddrPort
}

// Bind implements conn.Bind. Construct with New(), pass to
// device.NewDevice, and feed incoming AWG packets via Deliver().
type Bind struct {
	udp *net.UDPConn // shared with engine; NOT owned

	mu     sync.Mutex
	open   bool
	closed chan struct{}
	inbox  chan inboxItem
}

var _ conn.Bind = (*Bind)(nil)

// New constructs a Bind that writes to / receives via the given UDP
// socket. The socket must already be open and bound — this Bind never
// calls Listen or closes it.
func New(udp *net.UDPConn) *Bind {
	return &Bind{
		udp:    udp,
		closed: make(chan struct{}),
		inbox:  make(chan inboxItem, inboxBufferSize),
	}
}

// Open puts the Bind into a listening state. We ignore the `port`
// argument: the actual UDP listen happens in the engine; we just
// report whatever local port the engine's socket is bound to so the
// caller (amneziawg-go's UAPI) sees a consistent value.
//
// fns is a single ReceiveFunc that drains the inbox; engine's
// peerToTunLoop populates the inbox via Deliver(). Returning a single
// fn keeps things simple — amneziawg-go's main loop only assumes
// there's at least one.
func (b *Bind) Open(_ uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	b.open = true

	addr, ok := b.udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, 0, errors.New("awg: udp socket has non-UDPAddr local addr")
	}
	actualPort := uint16(addr.Port)

	// amneziawg-go's Device opens, closes and re-opens its Bind during
	// IpcSet + Up() — listen_port assignment and state transitions each
	// reshape the bind. A `closed` channel that was created once at
	// New() would stay closed forever after the first Close, and every
	// subsequent Open would hand out a ReceiveFunc that immediately
	// returns net.ErrClosed. Refresh it here so each Open cycle starts
	// with a live signal.
	b.closed = make(chan struct{})

	closed := b.closed
	inbox := b.inbox
	recv := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		if len(packets) == 0 {
			return 0, nil
		}
		select {
		case <-closed:
			return 0, net.ErrClosed
		case item, ok := <-inbox:
			if !ok {
				return 0, net.ErrClosed
			}
			n := copy(packets[0], item.data)
			sizes[0] = n
			eps[0] = NewEndpoint(item.src)
			return 1, nil
		}
	}
	return []conn.ReceiveFunc{recv}, actualPort, nil
}

// Close marks the Bind as closed and unblocks the ReceiveFunc with
// net.ErrClosed. The shared UDP socket is NOT closed — engine owns it
// and tears it down separately.
func (b *Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return nil
	}
	b.open = false
	close(b.closed)
	return nil
}

// SetMark is a no-op on macOS — Darwin doesn't support SO_MARK.
// amneziawg-go's own stdbind documents this same behavior.
func (b *Bind) SetMark(_ uint32) error { return nil }

// Send transmits AWG-shaped UDP packets to the endpoint. Each packet
// in bufs is already wrapped in AWG's magic header + AEAD by the
// caller; Bind just writes the bytes verbatim.
func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	e, ok := ep.(*Endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	addr := net.UDPAddrFromAddrPort(e.DstAddrPort())
	for _, buf := range bufs {
		if len(buf) == 0 {
			continue
		}
		if _, err := b.udp.WriteToUDP(buf, addr); err != nil {
			return err
		}
	}
	return nil
}

// ParseEndpoint accepts "host:port" (IPv4 or IPv6) and returns an
// Endpoint. Called by amneziawg-go's UAPI when the device config has
// `endpoint=...` lines.
func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	addr, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return NewEndpoint(addr), nil
}

// BatchSize returns the maximum packets-per-call the Bind expects.
// Stay at 1 — keeps Send/Receive simple and matches the engine's
// peerToTunLoop which already processes one UDP datagram at a time.
// AWG performance with batch=1 is fine for the small number of
// peers we have; can raise later if it becomes a bottleneck.
func (b *Bind) BatchSize() int { return 1 }

// Deliver hands off an AWG-shaped packet from the engine into the
// Bind's inbox. Engine's peerToTunLoop calls this when it sees a
// packet whose first uint32 falls into the H1..H4 magic range.
//
// Drops the packet if the inbox is full or the Bind is closed —
// AWG keepalive will retransmit. data is copied before queuing so
// the caller can reuse its read buffer.
func (b *Bind) Deliver(data []byte, src netip.AddrPort) {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case b.inbox <- inboxItem{data: cp, src: src}:
	case <-b.closed:
	default:
		// inbox full — silent drop. AWG will reattempt on next keepalive.
	}
}

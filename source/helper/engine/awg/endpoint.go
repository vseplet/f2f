package awg

import (
	"net/netip"

	"github.com/amnezia-vpn/amneziawg-go/conn"
)

// Endpoint is f2f's implementation of conn.Endpoint for amneziawg-go.
// It wraps the peer's UDP address (host:port) — sufficient for IPv4/IPv6
// in our setup. AmneziaWG also tracks a `src` address for cases where
// the receiving socket binds to a specific local address; on f2f we
// always use the single engine UDP socket, so src stays cleared.
type Endpoint struct {
	dst netip.AddrPort
	src netip.Addr // local source — almost always zero-value for us
}

var _ conn.Endpoint = (*Endpoint)(nil)

// NewEndpoint builds an Endpoint from a remote AddrPort.
func NewEndpoint(dst netip.AddrPort) *Endpoint {
	return &Endpoint{dst: dst}
}

// ClearSrc resets the source field. Called by amneziawg-go when a peer
// roams or NAT-rebinds — the cached src is no longer trusted.
func (e *Endpoint) ClearSrc() { e.src = netip.Addr{} }

// SrcToString returns the local source address (rarely populated in our
// usage). amneziawg-go uses this only for diagnostic logs.
func (e *Endpoint) SrcToString() string {
	if !e.src.IsValid() {
		return ""
	}
	return e.src.String()
}

// DstToString returns "host:port" of the remote peer. amneziawg-go uses
// it as the public identity of an endpoint (UAPI's `endpoint=` field).
func (e *Endpoint) DstToString() string { return e.dst.String() }

// DstToBytes returns a binary serialization of dst (IP bytes + 2 bytes
// port). Used inside amneziawg-go for mac2 cookie computation — value
// just needs to be unique per endpoint, the exact layout doesn't matter
// as long as it's deterministic.
func (e *Endpoint) DstToBytes() []byte {
	b, _ := e.dst.MarshalBinary()
	return b
}

// DstIP returns just the IP part of the remote endpoint.
func (e *Endpoint) DstIP() netip.Addr { return e.dst.Addr() }

// SrcIP returns the local source IP if known, zero-value otherwise.
func (e *Endpoint) SrcIP() netip.Addr { return e.src }

// DstAddrPort exposes the full AddrPort for callers that need to write
// to it via net.UDPConn (i.e. our Bind.Send). Not part of conn.Endpoint.
func (e *Endpoint) DstAddrPort() netip.AddrPort { return e.dst }

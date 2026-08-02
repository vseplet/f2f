package awg

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"

	"github.com/amnezia-vpn/amneziawg-go/conn"
)

// relayScheme prefixes a relay endpoint's string form ("relay://<64hex pub>"),
// so a peer reached only through the camp relay round-trips through UAPI
// (endpoint=) and DstToString like any other endpoint.
const relayScheme = "relay://"

// Endpoint is f2f's implementation of conn.Endpoint for amneziawg-go. Normally
// it wraps the peer's UDP address (host:port). It can also be a RELAY endpoint
// (relay=true, carrying the peer's Ed25519 pub): Bind.Send then frames the
// packet and forwards it through the camp relay instead of writing to a direct
// address. AmneziaWG also tracks a `src` address for a specific local bind; on
// f2f we always use the single engine UDP socket, so src stays cleared.
type Endpoint struct {
	dst   netip.AddrPort
	src   netip.Addr // local source — almost always zero-value for us
	relay bool       // if true, reach the peer via the camp relay, not dst
	pub   [32]byte   // peer Ed25519 pub — the relay target (relay endpoints only)
}

var _ conn.Endpoint = (*Endpoint)(nil)

// NewEndpoint builds a direct Endpoint from a remote AddrPort.
func NewEndpoint(dst netip.AddrPort) *Endpoint {
	return &Endpoint{dst: dst}
}

// NewRelayEndpoint builds an Endpoint that reaches the peer through the camp
// relay, keyed by its Ed25519 pub.
func NewRelayEndpoint(pub [32]byte) *Endpoint {
	return &Endpoint{relay: true, pub: pub}
}

// IsRelay reports whether this endpoint is reached via the camp relay.
func (e *Endpoint) IsRelay() bool { return e.relay }

// RelayPub returns the peer's pub for a relay endpoint (zero otherwise).
func (e *Endpoint) RelayPub() [32]byte { return e.pub }

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

// DstToString returns the endpoint's public identity (UAPI's `endpoint=`
// field): "host:port" for a direct endpoint, "relay://<pubhex>" for a relay one.
// The relay form round-trips through ParseEndpoint.
func (e *Endpoint) DstToString() string {
	if e.relay {
		return relayScheme + hex.EncodeToString(e.pub[:])
	}
	return e.dst.String()
}

// DstToBytes returns a deterministic, per-endpoint-unique serialization used by
// amneziawg-go for mac2 cookies. Relay endpoints key on the pub; direct ones on
// the marshaled address.
func (e *Endpoint) DstToBytes() []byte {
	if e.relay {
		return e.pub[:]
	}
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

// parseEndpointString builds an Endpoint from its DstToString form — a relay
// URI ("relay://<pubhex>") or a plain "host:port". Used by Bind.ParseEndpoint
// so a peer's endpoint set over UAPI can be direct or relay.
func parseEndpointString(s string) (*Endpoint, error) {
	if strings.HasPrefix(s, relayScheme) {
		raw, err := hex.DecodeString(strings.TrimPrefix(s, relayScheme))
		if err != nil || len(raw) != 32 {
			return nil, errors.New("awg: bad relay endpoint")
		}
		var pub [32]byte
		copy(pub[:], raw)
		return NewRelayEndpoint(pub), nil
	}
	addr, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return NewEndpoint(addr), nil
}

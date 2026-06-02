// Package platform wraps the OS primitives the f2f helper needs:
// tunnel device creation, route table mutations, firewall rule
// installation, NAT, DNS cache flushing, and trusted-root cert
// management. Each primitive is a thin shim over one syscall or one
// shell-tool invocation — orchestration belongs to the callers
// (engine/tunnel, engine/route, ca, etc.).
//
// Per-OS implementations live in *_darwin.go, *_linux.go, and
// *_windows.go files. Stubs return ErrUnsupported.
package platform

import (
	"errors"
	"net/netip"
)

// ErrUnsupported is returned by stubs on platforms where a primitive
// has no implementation. Callers can detect with errors.Is to skip
// optional features on the current OS.
var ErrUnsupported = errors.New("platform: not supported on this OS")

// FirewallPolicy describes the inbound filter installed on the
// tunnel interface in OS-agnostic terms. Per-OS rendering (pf
// anchors on macOS, nft tables on Linux) lives in the platform
// firewall implementation.
//
// Semantics: outbound on Iface is fully permitted; inbound packets
// to TunnelIP are default-deny except ICMP and the listed TCP/UDP
// ports. Inbound packets to other destinations (peer-to-peer
// overlay traffic transiting through, public IPs being forwarded
// via egress) are unaffected.
type FirewallPolicy struct {
	Iface    string
	TunnelIP string
	AllowTCP []int
	AllowUDP []int
}

// NATRules describes outbound NAT for tunnel traffic: packets from
// Subnet leaving via EgressIface get source-translated to that
// interface's address.
type NATRules struct {
	EgressIface string
	Subnet      netip.Prefix
}

// FilterEngineToken is an opaque per-OS handle for the OS packet
// filter's reference-counted enable mechanism (macOS pf works this
// way). On platforms where the filter engine is always on (Linux
// nftables), enabling is a no-op and the token is empty.
type FilterEngineToken string

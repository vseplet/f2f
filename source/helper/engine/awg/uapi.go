package awg

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/vseplet/f2f/source/helper/engine/obfenv"
	"github.com/vseplet/f2f/source/helper/identity"
)

// keepaliveDefaultSec is the persistent-keepalive interval handed to AWG
// for every peer. Matches our pair_req cadence (25s) so AWG-keepalive
// and pair_req together keep the NAT mapping alive on roughly the same
// timer rather than fighting each other.
const keepaliveDefaultSec = 25

// PeerSyncInfo describes one peer to push into Device via UAPI. Built
// from a *peerState in the engine — only peers with a verified WGPub
// and a known UDP endpoint should be included.
type PeerSyncInfo struct {
	WGPub       string // X25519 pub hex (64 chars), from verified pair handshake
	Endpoint    string // "host:port" — peer's reachable UDP endpoint
	OverlayCIDR string // "100.64.X.Y/32" — overlay address that routes to this peer
}

// BuildSelfConfig returns the UAPI text that initialises the device's
// own static keypair and obfuscation header ranges. Issued once at
// device init via IpcSet; peer state is added separately via
// BuildPeersBlock so that peer rotation doesn't disturb the local
// config.
//
// listen_port=0 is intentional: our Bind ignores the port argument
// (engine owns the UDP socket on a separately-chosen listen address),
// but the UAPI parser still requires the field to be present.
//
// Magic headers h1..h4 come from obfenv. Their ranges are derived
// deterministically from camp_id — two peers in the same camp compute
// the same set, without any external exchange.
func BuildSelfConfig(id *identity.Identity, env *obfenv.Camp) string {
	var b strings.Builder
	priv := id.X25519Priv()
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(priv[:]))
	fmt.Fprintf(&b, "listen_port=0\n")

	writeMagic := func(key string, slot obfenv.Slot) {
		start, _ := env.SlotRange(slot)
		// amneziawg-go v1.0.4's UAPI parses h1..h4 as a single uint32,
		// not a range (range support is in upstream master, not in our
		// pinned release). Use the slot's start value — it's still
		// deterministic per camp, just not randomised per packet within
		// the slot. Our receiver multiplex (obfenv.SlotFor) accepts the
		// whole [start, end) window, so when v1.0.4 always emits
		// `start`, that value still lies inside our window and matches.
		fmt.Fprintf(&b, "%s=%d\n", key, start)
	}
	writeMagic("h1", obfenv.SlotAWGInit)
	writeMagic("h2", obfenv.SlotAWGResponse)
	writeMagic("h3", obfenv.SlotAWGCookie)
	writeMagic("h4", obfenv.SlotAWGTransport)

	return b.String()
}

// BuildPeerBlock returns the UAPI fragment for one peer. Multiple of
// these get concatenated under a single "replace_peers=true" header
// to atomically rewrite the device's peer list.
func BuildPeerBlock(p PeerSyncInfo, keepaliveSec int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "public_key=%s\n", p.WGPub)
	if p.Endpoint != "" {
		fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
	}
	if p.OverlayCIDR != "" {
		fmt.Fprintf(&b, "allowed_ip=%s\n", p.OverlayCIDR)
	}
	if keepaliveSec > 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", keepaliveSec)
	}
	return b.String()
}

// BuildPeersBlock returns "replace_peers=true\n" followed by all peer
// blocks concatenated. Used as the body of IpcSet to atomically swap
// the device's peer list — fewer round trips than per-peer updates,
// at the cost of resetting any in-flight handshakes (acceptable: WG
// handshake re-completes in tens of ms, and SyncPeers is rare).
func BuildPeersBlock(peers []PeerSyncInfo, keepaliveSec int) string {
	var b strings.Builder
	b.WriteString("replace_peers=true\n")
	for _, p := range peers {
		b.WriteString(BuildPeerBlock(p, keepaliveSec))
	}
	return b.String()
}

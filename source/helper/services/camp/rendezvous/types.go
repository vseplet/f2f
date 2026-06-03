// Package rendezvous talks to the f2f-camp server using a UDP announce
// protocol on the same socket as the tunnel, plus an HTTP peer-list
// poller. There is no WebSocket — see TODO.md for the history of why
// we moved off WS.
//
// Wire types here mirror source/camp/src — keep them in sync.
package rendezvous

// PeerInfo is the camp server's view of a connected peer. The same
// shape is used in announce replies (`you`) and in /api/id/:id.
//
// The server only ever returns peers that have announced recently, so
// Online is always true here; offline peers are merged in client-side
// from the local config cache.
type PeerInfo struct {
	Name string `json:"name"`
	// Pub is the peer's Ed25519 public key in hex (64 chars). Stable
	// identity across renames; empty for legacy peers that haven't
	// announced one yet (transitional, will become required).
	Pub         string `json:"pub,omitempty"`
	PublicIP    string `json:"public_ip"`
	UDPPort     int    `json:"udp_port,omitempty"`
	UDPEndpoint string `json:"udp_endpoint,omitempty"`
	JoinedAt    int64  `json:"joined_at"`
	Online      bool   `json:"online"`
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`
}

// --- UDP wire types ---
//
// Shared between the client (this package) and the camp server
// (source/camp), which imports them directly so the wire contract has
// a single source of truth.

// AnnounceReq is what the client sends to camp every ~20s. The server
// reads our (public_ip, udp_port) off the packet itself, so we don't
// put those in the body.
type AnnounceReq struct {
	T      string `json:"t"` // "announce"
	Name   string `json:"name"`
	CampID string `json:"camp_id"`
	// Pub is the local Ed25519 public key in hex. Empty in static --peer
	// mode (no identity); always set in camp mode now.
	Pub string `json:"pub,omitempty"`
}

// AnnouncedResp is what camp sends back on success. The client parses
// the same shape inline in parseAnnounceReply.
type AnnouncedResp struct {
	T   string   `json:"t"` // "announced"
	You PeerInfo `json:"you"`
}

// AnnounceErr is the error reply (bad_name, bad_camp_id, camp_full,
// name_conflict, etc.).
type AnnounceErr struct {
	T       string `json:"t"` // "error"
	Code    string `json:"code"`
	Message string `json:"message"`
}

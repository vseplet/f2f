//go:build darwin

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
// Online == false means the peer is in the sticky-binding catalog but
// hasn't announced recently — PublicIP/UDPEndpoint may be empty.
type PeerInfo struct {
	Name        string `json:"name"`
	PublicIP    string `json:"public_ip"`
	UDPPort     int    `json:"udp_port,omitempty"`
	UDPEndpoint string `json:"udp_endpoint,omitempty"`
	TunnelIP    string `json:"tunnel_ip"`
	JoinedAt    int64  `json:"joined_at"`
	Online      bool   `json:"online"`
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`
}

// --- UDP wire types ---

// announceReq is what we send to camp every ~20s. The server reads our
// (public_ip, udp_port) off the packet itself, so we don't put those
// in the body.
type announceReq struct {
	T      string `json:"t"` // "announce"
	Name   string `json:"name"`
	CampID string `json:"camp_id"`
}

// announceResp is what camp sends back. Same shape parsed inline in
// AnnounceClient.HandlePacket; this declaration documents the type.
//
//nolint:unused
type announceResp struct {
	T   string   `json:"t"` // "announced"
	You PeerInfo `json:"you"`
}

// announceErr is the error reply (bad_name, bad_camp_id, camp_full,
// name_conflict, etc.).
//
//nolint:unused
type announceErr struct {
	T       string `json:"t"` // "error"
	Code    string `json:"code"`
	Message string `json:"message"`
}

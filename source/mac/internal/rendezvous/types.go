//go:build darwin

// Package rendezvous talks to the f2f-camp server: discovers our external
// UDP endpoint via the STUN-like probe, and uses a WebSocket to register
// our identity and learn about the other peer in our camp.
//
// All JSON wire types here mirror those in source/camp/src/types.ts —
// keep them in sync.
package rendezvous

// PeerInfo is the camp server's view of a connected peer.
type PeerInfo struct {
	Name        string `json:"name"`
	PublicIP    string `json:"public_ip"`
	UDPPort     int    `json:"udp_port,omitempty"`
	UDPEndpoint string `json:"udp_endpoint,omitempty"`
	TunnelIP    string `json:"tunnel_ip"` // camp-assigned IP inside the camp's /24 overlay
	JoinedAt    int64  `json:"joined_at"`
}

// --- client → server ---

type helloMsg struct {
	Type    string `json:"type"` // "hello"
	Name    string `json:"name"`
	CampID  string `json:"camp_id"`
	UDPPort int    `json:"udp_port,omitempty"`
}

type announceMsg struct {
	Type    string `json:"type"` // "announce"
	UDPPort int    `json:"udp_port"`
}

type signalMsg struct {
	Type    string `json:"type"` // "signal"
	To      string `json:"to"`
	Payload any    `json:"payload"`
}

type pingMsg struct {
	Type string `json:"type"` // "ping"
}

// --- server → client ---

type welcomeMsg struct {
	Type   string     `json:"type"` // "welcome"
	You    PeerInfo   `json:"you"`
	CampID string     `json:"camp_id"`
	Peers  []PeerInfo `json:"peers"`
}

type errorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type peerEventMsg struct {
	Type string   `json:"type"`           // "peer-joined" | "peer-updated"
	Peer PeerInfo `json:"peer"`
}

type peerLeftMsg struct {
	Type string `json:"type"` // "peer-left"
	Name string `json:"name"`
}

type signalDeliveryMsg struct {
	Type    string `json:"type"` // "signal"
	From    string `json:"from"`
	Payload any    `json:"payload"`
}

// stunProbe and stunReflex are the wire types of the UDP-side
// reflexive-address discovery exchange.
type stunProbe struct {
	T  string `json:"t"`  // "probe"
	ID string `json:"id"`
}

type stunReflex struct {
	T    string `json:"t"`  // "reflex"
	ID   string `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

package main

// UDP announce listener. A peer sends `{t:"announce", name, camp_id,
// pub}` on this socket; we read its public endpoint off the packet's
// source address (no separate STUN step), upsert it into the hub, and
// reply with `{t:"announced", you:PeerInfo}`.
//
// One UDP packet does three jobs: registers/refreshes the peer, lets us
// observe its public endpoint via the source address, and keeps the
// camp-facing NAT mapping alive on the client's tunnel port.

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"os"
	"regexp"

	"github.com/vseplet/f2f/source/helper/mesh/camp/rendezvous"
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	pubRE  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	fpRE   = regexp.MustCompile(`^[a-f0-9]{16}$`)
)

// linkStates is the closed set of connection states a peer may report.
var linkStates = map[string]bool{"direct": true, "half": true, "seen": true, "relay": true}

const (
	maxNameLen   = 64
	maxCampIDLen = 128
	maxPayload   = 1024
	// Relay (DERP-style): when two peers can't hole-punch, they tunnel opaque
	// (already AWG-encrypted) packets through us. Framing on the same socket:
	//   client → relay:  [relaySend]   [to_pub:32]   [payload…]
	//   relay  → client: [relayDeliver][from_pub:32] [payload…]
	// The sender's identity comes from its source address (relayLookup), not the
	// frame, so from can't be spoofed. First byte distinguishes these from the
	// JSON announce (which starts with '{' = 0x7b).
	relaySend    = 0xF3
	relayDeliver = 0xF2
	relayHdr     = 1 + 32 // opcode + pubkey
	// maxRelay bounds a relay datagram: an AWG data packet is ≤ MTU (~1420) plus
	// transport overhead; give headroom. Larger than maxPayload (announce only).
	maxRelay = 1600
	// rosterWindow is how many peers a paged announce reply carries. Each
	// PeerInfo is ~0.3 KB of JSON; kept small so a reply (plus the `you` field)
	// fits in a single un-fragmented UDP datagram (≤ MTU) — fragmented UDP is
	// dropped by some middleboxes on the path to the fly.io camp. Camps smaller
	// than this get the whole roster in one reply (cycleEnd every time).
	rosterWindow = 3
)

// pickBindAddress mirrors the TS server: on fly.io UDP only reaches a
// Machine if you bind to the special `fly-global-services` address —
// 0.0.0.0 silently drops packets. Elsewhere bind 0.0.0.0. Auto-detect
// via FLY_APP_NAME, with a STUN_BIND escape hatch.
func pickBindAddress() string {
	if explicit := os.Getenv("STUN_BIND"); explicit != "" {
		return explicit
	}
	if os.Getenv("FLY_APP_NAME") != "" {
		return "fly-global-services"
	}
	return "0.0.0.0"
}

// startUDP binds the announce socket and serves it in a goroutine.
func startUDP(port string, hub *Hub) error {
	host := pickBindAddress()
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	log.Printf("udp: %s (announce)", conn.LocalAddr())
	go serveUDP(conn, hub)
	return nil
}

func serveUDP(conn *net.UDPConn, hub *Hub) {
	buf := make([]byte, maxRelay+1)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("udp: read: %v", err)
			return
		}
		// Relay frames are binary and can exceed the announce cap — handle them
		// first, before the JSON path's size limit.
		if n > 0 && buf[0] == relaySend {
			handleRelay(conn, hub, src, buf[:n])
			continue
		}
		// Hard cap on announce payload size — sanity check and keeps reflection
		// amplification cheap.
		if n > maxPayload {
			log.Printf("udp: drop oversize %dB from %s", n, src)
			continue
		}

		var req rendezvous.AnnounceReq
		if err := json.Unmarshal(buf[:n], &req); err != nil || req.T != "announce" {
			continue // silent — random scanners send junk
		}

		name, campID, pub := req.Name, req.CampID, req.Pub
		if name == "" || len(name) > maxNameLen || !nameRE.MatchString(name) {
			sendErr(conn, src, "bad_name", "invalid name")
			continue
		}
		if campID == "" || len(campID) > maxCampIDLen || !nameRE.MatchString(campID) {
			sendErr(conn, src, "bad_camp_id", "invalid camp_id")
			continue
		}
		// pub is the primary identity. Reject clients that don't send
		// one — every supported client generates an Ed25519 keypair.
		if pub == "" {
			sendErr(conn, src, "pub_required", "client must announce ed25519 pub")
			continue
		}
		if !pubRE.MatchString(pub) {
			sendErr(conn, src, "bad_pub", "invalid pub (expect 64 hex)")
			continue
		}

		// version/role/allow are advisory metadata the peer publishes. Sanitize
		// so a junk announce can't bloat the roster: role must be a short token,
		// allow a small list of 64-hex fps, version a short string.
		version := req.Version
		if len(version) > 40 {
			version = version[:40]
		}
		role := req.Role
		switch role {
		case "user", "service", "task":
			// known roles, kept verbatim
		default:
			role = "" // unknown/absent (old client) → blank
		}
		var allow []string
		for _, fp := range req.Allow {
			if pubRE.MatchString(fp) {
				allow = append(allow, fp)
			}
			if len(allow) >= 8 { // cap: keep the roster JSON small
				break
			}
		}

		wasNew := !hub.has(campID, pub)
		info := hub.upsert(campID, pub, name, src, version, role, allow)
		if wasNew {
			log.Printf("join: %s@%s pub=%s from %s", name, campID, short(pub), src)
		}

		// The announce reply carries only `you` — the roster is served over HTTP
		// (/api/id) now, with no MTU cap and full PeerInfo (version/role/allow).
		// UDP announce is kept purely for register / reflex-endpoint / NAT-keepalive.
		reply := rendezvous.AnnouncedResp{T: "announced", You: info}
		if data, err := json.Marshal(reply); err == nil {
			conn.WriteToUDP(data, src)
		}
	}
}

// handleRelay forwards one relay send. It resolves the sender from its source
// address and the target from the frame's to_pub, requires both in the same
// camp (relayLookup), then re-frames the payload with the sender's pub and
// delivers it to the target. Unknown sender / target / cross-camp → dropped
// silently (no error reply — this is a data-plane hot path).
func handleRelay(conn *net.UDPConn, hub *Hub, src *net.UDPAddr, pkt []byte) {
	if len(pkt) < relayHdr {
		return
	}
	toPub := hex.EncodeToString(pkt[1:relayHdr])
	to, fromPub, ok := hub.relayLookup(src, toPub)
	if !ok {
		return
	}
	fromRaw, err := hex.DecodeString(fromPub)
	if err != nil || len(fromRaw) != 32 {
		return
	}
	out := make([]byte, 0, relayHdr+len(pkt)-relayHdr)
	out = append(out, relayDeliver)
	out = append(out, fromRaw...)
	out = append(out, pkt[relayHdr:]...)
	conn.WriteToUDP(out, to)
}

func sendErr(conn *net.UDPConn, dst *net.UDPAddr, code, message string) {
	if data, err := json.Marshal(rendezvous.AnnounceErr{T: "error", Code: code, Message: message}); err == nil {
		conn.WriteToUDP(data, dst)
	}
	log.Printf("udp: %s ← err %s", dst, code)
}

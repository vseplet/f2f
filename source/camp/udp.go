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
	"encoding/json"
	"log"
	"net"
	"os"
	"regexp"

	"github.com/vseplet/f2f/source/helper/services/camp/rendezvous"
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	pubRE  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

const (
	maxNameLen   = 64
	maxCampIDLen = 128
	maxPayload   = 1024
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
	buf := make([]byte, maxPayload+1)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("udp: read: %v", err)
			return
		}
		// Hard cap on payload size — sanity check and keeps reflection
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

		wasNew := !hub.has(campID, pub)
		info := hub.upsert(campID, pub, name, src.IP.String(), src.Port)
		if wasNew {
			log.Printf("join: %s@%s pub=%s from %s", name, campID, short(pub), src)
		}

		if data, err := json.Marshal(rendezvous.AnnouncedResp{T: "announced", You: info}); err == nil {
			conn.WriteToUDP(data, src)
		}
	}
}

func sendErr(conn *net.UDPConn, dst *net.UDPAddr, code, message string) {
	if data, err := json.Marshal(rendezvous.AnnounceErr{T: "error", Code: code, Message: message}); err == nil {
		conn.WriteToUDP(data, dst)
	}
	log.Printf("udp: %s ← err %s", dst, code)
}

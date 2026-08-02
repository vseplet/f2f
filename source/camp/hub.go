package main

import (
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/vseplet/f2f/source/helper/mesh/camp/rendezvous"
)

// peer is a hub entry. lastSeen drives eviction; info is the wire view
// handed back to clients verbatim.
type peer struct {
	info     rendezvous.PeerInfo
	lastSeen time.Time
	// addr is the peer's live UDP source address (from its last announce) — the
	// target for relay forwarding. Same 5-tuple the peer announces on, so a
	// relayed packet reaches the same NAT mapping that's kept alive by announces.
	addr *net.UDPAddr
	// cursor is this peer's own round-robin offset into the roster for windowed
	// delivery. Per-peer (not per-camp) so each client's independent polls walk
	// the whole roster contiguously — a shared cursor would let concurrent
	// pollers advance past windows a given client never sees, wrongly aging out
	// peers whose window it missed.
	cursor int
}

type campState struct {
	peers map[string]*peer // keyed by pub
}

// srcRef identifies a peer by its source endpoint — the reverse of the peers
// map, so a relay frame's sender can be resolved from its UDP source address.
type srcRef struct {
	campID string
	pub    string
}

// Hub is the whole server state: an in-memory map of camps, each a map
// of peers. A single mutex guards everything — the UDP reader, the HTTP
// handlers, and the evict ticker all touch it concurrently.
type Hub struct {
	mu    sync.Mutex
	camps map[string]*campState
	// bySrc maps a peer's current "ip:port" → who it is, so a relay send can
	// identify its sender (which announced from the same socket) without trusting
	// a from-field in the frame. Kept in sync with upsert/evict.
	bySrc map[string]srcRef
}

func NewHub() *Hub {
	return &Hub{camps: make(map[string]*campState), bySrc: make(map[string]srcRef)}
}

// upsert registers or refreshes a peer and returns the resulting wire
// view. Online is always true here — stale peers are evicted, not kept.
// version/role/allow are announced by the peer and carried verbatim into the
// roster (the server adds no logic — clients act on role/allow themselves).
func (h *Hub) upsert(campID, pub, name string, src *net.UDPAddr, version, role string, allow []string) rendezvous.PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.camps[campID]
	if c == nil {
		c = &campState{peers: make(map[string]*peer)}
		h.camps[campID] = c
	}
	now := time.Now()
	publicIP := src.IP.String()
	udpPort := src.Port
	endpoint := net.JoinHostPort(publicIP, strconv.Itoa(udpPort))
	if p := c.peers[pub]; p != nil {
		if p.info.UDPEndpoint != endpoint {
			delete(h.bySrc, p.info.UDPEndpoint) // endpoint moved (NAT rebind)
		}
		p.info.Name = name
		p.info.PublicIP = publicIP
		p.info.UDPPort = udpPort
		p.info.UDPEndpoint = endpoint
		p.info.Online = true
		p.info.LastSeenAt = now.UnixMilli()
		p.info.Version = version
		p.info.Role = role
		p.info.Allow = allow
		p.lastSeen = now
		p.addr = src
		h.bySrc[endpoint] = srcRef{campID: campID, pub: pub}
		return p.info
	}
	info := rendezvous.PeerInfo{
		Name:        name,
		Pub:         pub,
		PublicIP:    publicIP,
		UDPPort:     udpPort,
		UDPEndpoint: endpoint,
		JoinedAt:    now.UnixMilli(),
		Online:      true,
		LastSeenAt:  now.UnixMilli(),
		Version:     version,
		Role:        role,
		Allow:       allow,
	}
	c.peers[pub] = &peer{info: info, lastSeen: now, addr: src}
	h.bySrc[endpoint] = srcRef{campID: campID, pub: pub}
	return info
}

// relayLookup resolves a relay send: given the sender's UDP source address and
// the target pub it wants to reach, it returns the target's live address and
// the sender's own pub — but only when BOTH are in the same camp. The sender is
// identified by its source address (which its announces keep current), so a
// relay frame can't spoof its from-identity. ok=false drops the frame.
func (h *Hub) relayLookup(src *net.UDPAddr, toPub string) (to *net.UDPAddr, fromPub string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ref, ok := h.bySrc[src.String()]
	if !ok {
		return nil, "", false
	}
	c := h.camps[ref.campID]
	if c == nil {
		return nil, "", false
	}
	tp := c.peers[toPub]
	if tp == nil || tp.addr == nil {
		return nil, "", false
	}
	return tp.addr, ref.pub, true
}

// setLinks stores a peer's self-reported connection matrix into its roster entry
// (so /api/id returns it). clientIP is the report's source IP: it must match the
// peer's announced reflex IP, so a report can't be forged for another peer
// (empty clientIP skips the check). Returns false if the peer isn't in the camp
// or the reflex doesn't match.
func (h *Hub) setLinks(campID, pub, clientIP string, links []rendezvous.Link) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camps[campID]
	if c == nil {
		return false
	}
	p := c.peers[pub]
	if p == nil {
		return false
	}
	if clientIP != "" && p.info.PublicIP != "" && clientIP != p.info.PublicIP {
		return false
	}
	p.info.Links = links
	return true
}

func (h *Hub) has(campID, pub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camps[campID]
	if c == nil {
		return false
	}
	_, ok := c.peers[pub]
	return ok
}

func (h *Hub) list(campID string) []rendezvous.PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camps[campID]
	if c == nil {
		return nil
	}
	out := make([]rendezvous.PeerInfo, 0, len(c.peers))
	for _, p := range c.peers {
		out = append(out, p.info)
	}
	return out
}

// listWindow returns up to `window` peers of a camp's roster for the requesting
// peer (reqPub), starting at THAT peer's own cursor and advancing it, so the
// peer's independent polls rotate contiguously over the whole roster. cycleEnd
// is true on the window that completes one full pass; the client treats the
// union of windows up to a cycleEnd as the authoritative roster. A window >=
// roster size (small camps) returns the full list every time with cycleEnd=true.
// Order is stable (sorted by pub) so the rotation deterministically covers every
// peer. The cursor is per-peer (not per-camp) — a shared cursor would let
// concurrent pollers advance past windows a given client never sees.
func (h *Hub) listWindow(campID, reqPub string, window int) (peers []rendezvous.PeerInfo, cycleEnd bool, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camps[campID]
	if c == nil {
		return nil, true, 0
	}
	pubs := make([]string, 0, len(c.peers))
	for pub := range c.peers {
		pubs = append(pubs, pub)
	}
	sort.Strings(pubs)
	total = len(pubs)
	if total == 0 {
		return nil, true, 0
	}
	rp := c.peers[reqPub] // the requester (always present: upsert ran first)
	if window <= 0 || window >= total {
		out := make([]rendezvous.PeerInfo, 0, total)
		for _, pub := range pubs {
			out = append(out, c.peers[pub].info)
		}
		if rp != nil {
			rp.cursor = 0
		}
		return out, true, total
	}
	start := 0
	if rp != nil {
		start = rp.cursor
	}
	if start >= total {
		start = 0
	}
	end := start + window
	cycleEnd = end >= total
	if end > total {
		end = total
	}
	out := make([]rendezvous.PeerInfo, 0, end-start)
	for _, pub := range pubs[start:end] {
		out = append(out, c.peers[pub].info)
	}
	if rp != nil {
		if cycleEnd {
			rp.cursor = 0
		} else {
			rp.cursor = end
		}
	}
	return out, cycleEnd, total
}

// evictStale drops peers idle past the cutoff and removes empty camps.
func (h *Hub) evictStale(cutoff time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for campID, c := range h.camps {
		for pub, p := range c.peers {
			if p.lastSeen.Before(cutoff) {
				delete(c.peers, pub)
				delete(h.bySrc, p.info.UDPEndpoint)
				log.Printf("evict: %s@%s pub=%s (idle)", p.info.Name, campID, short(pub))
			}
		}
		if len(c.peers) == 0 {
			delete(h.camps, campID)
		}
	}
}

func short(pub string) string {
	if len(pub) > 16 {
		return pub[:16]
	}
	return pub
}

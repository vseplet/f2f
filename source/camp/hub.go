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
}

type campState struct {
	peers  map[string]*peer // keyed by pub
	cursor int              // round-robin offset for windowed roster delivery
}

// Hub is the whole server state: an in-memory map of camps, each a map
// of peers. A single mutex guards everything — the UDP reader, the HTTP
// handlers, and the evict ticker all touch it concurrently.
type Hub struct {
	mu    sync.Mutex
	camps map[string]*campState
}

func NewHub() *Hub {
	return &Hub{camps: make(map[string]*campState)}
}

// upsert registers or refreshes a peer and returns the resulting wire
// view. Online is always true here — stale peers are evicted, not kept.
func (h *Hub) upsert(campID, pub, name, publicIP string, udpPort int) rendezvous.PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.camps[campID]
	if c == nil {
		c = &campState{peers: make(map[string]*peer)}
		h.camps[campID] = c
	}
	now := time.Now()
	endpoint := net.JoinHostPort(publicIP, strconv.Itoa(udpPort))
	if p := c.peers[pub]; p != nil {
		p.info.Name = name
		p.info.PublicIP = publicIP
		p.info.UDPPort = udpPort
		p.info.UDPEndpoint = endpoint
		p.info.Online = true
		p.info.LastSeenAt = now.UnixMilli()
		p.lastSeen = now
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
	}
	c.peers[pub] = &peer{info: info, lastSeen: now}
	return info
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

// listWindow returns up to `window` peers of a camp's roster, starting at the
// per-camp cursor and advancing it, so successive announce replies rotate
// across the whole roster (bounding each reply's size). cycleEnd is true on the
// window that completes one full pass; the client treats the union of windows
// up to a cycleEnd as the authoritative roster. A window >= roster size (small
// camps) returns the full list every time with cycleEnd=true. Order is stable
// (sorted by pub) so the rotation deterministically covers every peer.
func (h *Hub) listWindow(campID string, window int) (peers []rendezvous.PeerInfo, cycleEnd bool, total int) {
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
		c.cursor = 0
		return nil, true, 0
	}
	if window <= 0 || window >= total {
		out := make([]rendezvous.PeerInfo, 0, total)
		for _, pub := range pubs {
			out = append(out, c.peers[pub].info)
		}
		c.cursor = 0
		return out, true, total
	}
	start := c.cursor
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
	if cycleEnd {
		c.cursor = 0
	} else {
		c.cursor = end
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

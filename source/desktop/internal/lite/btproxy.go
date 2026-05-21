package lite

import (
	"log"
	"net"
	"sync"
)

// btProxy multiplexes BitTorrent (uTP) traffic onto our existing
// hole-punched UDP socket. anacrolix doesn't accept a custom
// PacketConn, but we can fake it: per remote peer we maintain a
// loopback UDP socket (the "forwarder") that anacrolix sees as a peer
// at 127.0.0.1:<random>. The forwarder relays both directions:
//
//   anacrolix ──127.0.0.1:fwd──► forwarder ──c.udp──► peer (remote)
//   peer ──c.udp──► recvLoop ──forwarder──► anacrolix
//
// Because the relay rides on the already-hole-punched socket, no
// additional NAT traversal is needed for BT — symmetric NATs and
// CGNATs that block separate BT sockets are no longer a problem.
type btProxy struct {
	mu sync.Mutex

	// liteUDP is the hole-punched socket; we read multiplexed traffic
	// from it in client.recvLoop and write peer-bound BT packets to
	// it from forwarder goroutines.
	liteUDP *net.UDPConn
	// btAddr is the loopback addr anacrolix is bound to. Each
	// forwarder dials it as the "anacrolix side" of the relay.
	btAddr *net.UDPAddr
	// forwarders are keyed by the remote peer's UDPAddr string. Each
	// has its own 127.0.0.1:<random> loopback socket that anacrolix
	// sees as a distinct peer.
	forwarders map[string]*btForwarder
}

type btForwarder struct {
	loopback  *net.UDPConn // socket anacrolix sees the peer on
	peerAddr  *net.UDPAddr // real remote peer (on the hole-punched side)
	proxy     *btProxy
}

func newBTProxy(liteUDP *net.UDPConn) *btProxy {
	return &btProxy{
		liteUDP:    liteUDP,
		forwarders: map[string]*btForwarder{},
	}
}

// setAnacrolix records anacrolix's loopback bind addr. Called after
// the torrent client comes up so we know where to relay incoming
// peer packets.
func (p *btProxy) setAnacrolix(addr string) error {
	udp, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.btAddr = udp
	p.mu.Unlock()
	return nil
}

// closeAll closes every forwarder. Used at engine shutdown.
func (p *btProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.forwarders {
		_ = f.loopback.Close()
	}
	p.forwarders = map[string]*btForwarder{}
}

// ensureForwarder returns the forwarder for the given peer, creating
// one on demand. Returns nil if the proxy isn't ready (anacrolix not
// up yet).
func (p *btProxy) ensureForwarder(peerAddr *net.UDPAddr) *btForwarder {
	if peerAddr == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.btAddr == nil {
		return nil
	}
	key := peerAddr.String()
	if f, ok := p.forwarders[key]; ok {
		return f
	}
	loopback, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		log.Printf("btproxy: bind loopback for %s: %v", peerAddr, err)
		return nil
	}
	f := &btForwarder{loopback: loopback, peerAddr: peerAddr, proxy: p}
	p.forwarders[key] = f
	go f.relayFromAnacrolix()
	log.Printf("btproxy: forwarder for %s on %s", peerAddr, loopback.LocalAddr())
	return f
}

// relayFromAnacrolix reads packets that anacrolix sends to the
// forwarder's loopback addr and forwards them via lite UDP socket to
// the real remote peer.
func (f *btForwarder) relayFromAnacrolix() {
	buf := make([]byte, 65535)
	for {
		n, _, err := f.loopback.ReadFromUDP(buf)
		if err != nil {
			return // socket closed at shutdown
		}
		if _, werr := f.proxy.liteUDP.WriteToUDP(buf[:n], f.peerAddr); werr != nil {
			log.Printf("btproxy: relay → %s: %v", f.peerAddr, werr)
		}
	}
}

// forwardFromPeer is called from client.recvLoop when a non-prefix
// packet arrives from a known peer. The packet is delivered to
// anacrolix as if it came from the forwarder's loopback addr.
func (p *btProxy) forwardFromPeer(peerAddr *net.UDPAddr, pkt []byte) {
	f := p.ensureForwarder(peerAddr)
	if f == nil {
		return // anacrolix isn't up yet — drop
	}
	p.mu.Lock()
	target := p.btAddr
	p.mu.Unlock()
	if target == nil {
		return
	}
	if _, err := f.loopback.WriteToUDP(pkt, target); err != nil {
		log.Printf("btproxy: relay → anacrolix: %v", err)
	}
}

// forwarderAddrFor returns the loopback addr that anacrolix should
// dial to talk to the given peer. Creates a forwarder if needed.
// Returns "" when the proxy isn't ready.
func (p *btProxy) forwarderAddrFor(peerAddr *net.UDPAddr) string {
	f := p.ensureForwarder(peerAddr)
	if f == nil {
		return ""
	}
	return f.loopback.LocalAddr().String()
}

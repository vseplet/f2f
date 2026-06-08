package web

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// vncUpgrader upgrades to a WebSocket carrying the raw RFB byte stream.
// noVNC requests the "binary" subprotocol; echo it. Loopback-only handler,
// so origin is unchecked.
var vncUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	Subprotocols:    []string{"binary"},
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleVncPeers lists camp peers that have a reachable VNC server open to
// us. UI-only HTTP: the browser asks its local f2f, which probes each peer
// over the BUS (vnc.status).
func (s *Server) handleVncPeers(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, fmt.Errorf("loopback only"))
		return
	}
	type peerVnc struct {
		Pub  string `json:"pub"`
		Name string `json:"name"`
	}
	st := s.engine.Status()
	var (
		mu  sync.Mutex
		out = []peerVnc{}
		wg  sync.WaitGroup
	)
	for _, p := range st.Peers {
		if p.Self || p.Pub == "" {
			continue
		}
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if s.vnc.Available(ctx, p.Pub) {
				mu.Lock()
				out = append(out, peerVnc{Pub: p.Pub, Name: p.Name})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

// handleVncWS bridges a browser noVNC client to a peer's VNC server: it
// pipes the raw RFB byte stream both ways between the WebSocket and the bus
// stream. No framing — RFB is end-to-end between noVNC and the host server.
func (s *Server) handleVncWS(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, fmt.Errorf("loopback only"))
		return
	}
	peer := r.URL.Query().Get("peer")
	if peer == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("peer required"))
		return
	}

	st, err := s.vnc.Open(r.Context(), peer)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	ws, err := vncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		st.Close()
		return
	}

	// bus stream → browser
	go func() {
		defer ws.Close()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := st.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// browser → bus stream
	defer st.Close()
	for {
		mt, data, rerr := ws.ReadMessage()
		if rerr != nil {
			return
		}
		if mt == websocket.BinaryMessage && len(data) > 0 {
			if _, werr := st.Write(data); werr != nil {
				return
			}
		}
	}
}

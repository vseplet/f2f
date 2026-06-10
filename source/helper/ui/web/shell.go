package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vseplet/f2f/source/helper/services/shell"
)

// shellUpgrader upgrades the loopback HTTP request to a WebSocket. Origin
// is unchecked because the handler is loopback-only (the browser talking to
// its own f2f); no cross-origin exposure.
var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleShellPeers lists camp peers whose remote shell is open to us. This
// is a UI-only HTTP endpoint: the browser asks its local f2f, which probes
// each peer over the BUS (shell.status) — no peer↔peer HTTP.
func (s *Server) handleShellPeers(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, fmt.Errorf("loopback only"))
		return
	}
	type peerShell struct {
		Pub  string `json:"pub"`
		Name string `json:"name"`
	}
	st := s.engine.Status()
	var (
		mu  sync.Mutex
		out = []peerShell{}
		wg  sync.WaitGroup
	)
	for _, p := range st.Peers {
		// Probe only peers we're actually receiving UDP from — a peer
		// that's unreachable on the overlay can't answer a bus probe
		// either, and this poll fires every 5s for every peer.
		if p.Self || p.Pub == "" || !p.Reachable {
			continue
		}
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if s.shell.Available(ctx, p.Pub) {
				mu.Lock()
				out = append(out, peerShell{Pub: p.Pub, Name: p.Name})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

// handleShellWS bridges a browser xterm.js terminal to a bus shell stream.
//
//	browser ⟷ WS (this handler) ⟷ bus stream ⟷ remote peer's PTY
//
// Browser→server: binary frames are stdin bytes; text frames are JSON
// control ({"t":"resize","cols":C,"rows":R}). Server→browser: binary frames
// are raw terminal output.
func (s *Server) handleShellWS(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, fmt.Errorf("loopback only"))
		return
	}
	q := r.URL.Query()
	peer := q.Get("peer")
	session := q.Get("session")
	if peer == "" || session == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("peer and session required"))
		return
	}
	cols := atoiU16(q.Get("cols"), 80)
	rows := atoiU16(q.Get("rows"), 24)

	st, err := s.shell.Open(r.Context(), peer, session, cols, rows)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	ws, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		st.Close()
		return // Upgrade already wrote the error
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
		switch mt {
		case websocket.BinaryMessage:
			if err := shell.WriteInput(st, data); err != nil {
				return
			}
		case websocket.TextMessage:
			var c struct {
				T    string `json:"t"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &c) != nil {
				continue
			}
			switch c.T {
			case "resize":
				_ = shell.WriteResize(st, c.Cols, c.Rows)
			case "kill":
				_ = shell.WriteKill(st)
			}
		}
	}
}

func atoiU16(s string, def uint16) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 65535 {
		return def
	}
	return uint16(n)
}

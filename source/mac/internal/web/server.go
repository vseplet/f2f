//go:build darwin

// Package web is the HTTP UI for f2f-mac. It serves an embedded SPA from
// assets/ and exposes a small REST + SSE API over the engine.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/engine"
)

//go:embed assets
var assetsFS embed.FS

// Server wraps an Engine with an HTTP handler.
type Server struct {
	engine *engine.Engine
	addr   string
	srv    *http.Server
}

func New(eng *engine.Engine, addr string) *Server {
	return &Server{engine: eng, addr: addr}
}

// Addr returns the configured bind address.
func (s *Server) Addr() string { return s.addr }

// ListenAndServe blocks until the server is shut down. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) routes(mux *http.ServeMux) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err) // build-time; embed is wrong
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/start", s.handleStart)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("POST /api/intercepts", s.handleAddIntercept)
	mux.HandleFunc("DELETE /api/intercepts/{id}", s.handleRemoveIntercept)
	mux.HandleFunc("GET /api/ifaces", s.handleIfaces)
	mux.HandleFunc("GET /api/log/stream", s.handleLogStream)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Status())
}

type startRequest struct {
	LocalIP      string   `json:"local_ip"`
	PeerIP       string   `json:"peer_ip"`
	Listen       string   `json:"listen"`
	Peer         string   `json:"peer"`
	Intercepts   []string `json:"intercepts"`
	EgressIface  string   `json:"egress_iface"`
	EgressSubnet string   `json:"egress_subnet"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg := engine.Config{
		LocalIP:      req.LocalIP,
		PeerIP:       req.PeerIP,
		Listen:       req.Listen,
		Peer:         req.Peer,
		Intercepts:   req.Intercepts,
		EgressIface:  req.EgressIface,
		EgressSubnet: req.EgressSubnet,
	}
	if err := s.engine.Start(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Stop(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

type addInterceptRequest struct {
	Spec string `json:"spec"`
}

func (s *Server) handleAddIntercept(w http.ResponseWriter, r *http.Request) {
	var req addInterceptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.engine.AddIntercept(req.Spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleRemoveIntercept(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.RemoveIntercept(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ifaceInfo struct {
	Name string `json:"name"`
	IP   string `json:"ip,omitempty"`
}

func (s *Server) handleIfaces(w http.ResponseWriter, r *http.Request) {
	ifs, err := net.Interfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []ifaceInfo{}
	for _, iface := range ifs {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "utun") {
			continue
		}
		info := ifaceInfo{Name: iface.Name}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				info.IP = ipn.IP.String()
				break
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.engine.Subscribe(64)
	defer unsubscribe()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	// Send an initial comment so the client knows the stream is alive.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

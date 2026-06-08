// Package shell is the remote-terminal service: a mosh-like PTY that a
// peer can attach to over the QUIC bus and that SURVIVES the client going
// away (sleep, reload, network gap). The session lives on the host; a
// client opens a bus stream, the host replays the recent screen from a ring
// buffer (so there's no garbage on reattach) and then pipes live PTY I/O.
//
// Layering: this is an app over the fabric — all peer↔peer traffic goes on
// the bus (no HTTP). The local web layer bridges a browser WebSocket
// (xterm.js) to a bus stream opened here; it never talks to a remote peer's
// HTTP.
//
// Wire protocol on the "shell.open" stream:
//   - open frame payload (JSON): {session_id, cols, rows}
//   - server→client: RAW pty output bytes (ring replay first, then live)
//   - client→server: framed messages — [op byte][uint32 len][payload]
//     op 'd' = stdin data; op 'r' = resize (payload: cols,rows uint16 BE)
//
// SECURITY: opening a shell is remote code execution. The PTY runs the
// system `login` by default, so OS auth still applies. Access is gated by
// SetPolicy(enabled, allowed). NOTE: for now the default is PERMISSIVE
// (any authenticated camp peer) to ease testing — tighten before shipping.
package shell

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"

	"github.com/vseplet/f2f/source/helper/mesh/bus"
)

// bus message types.
const (
	TypeOpen   = "shell.open"   // stream: attach to / create a PTY session
	TypeStatus = "shell.status" // request: is the shell available to me?
)

// client→server framing opcodes.
const (
	opData   = 'd'
	opResize = 'r'
)

const ringCap = 128 * 1024 // recent output kept for repaint-on-reattach

// openReq is the JSON open-frame payload.
type openReq struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

type statusResp struct {
	Available bool `json:"available"`
}

// Service owns the live PTY sessions and the bus handlers.
type Service struct {
	bus *bus.Service

	pmu     sync.Mutex
	enabled bool
	allow   map[string]bool // pub → allowed; empty = allow-all (testing)
	command string

	mu       sync.Mutex
	sessions map[string]*session
}

// New constructs the service. Default policy is PERMISSIVE (enabled, no
// allowlist) for testing; call SetPolicy to lock it down.
func New(b *bus.Service) *Service {
	return &Service{
		bus:      b,
		enabled:  true,
		allow:    map[string]bool{},
		sessions: map[string]*session{},
	}
}

// Register wires the bus handlers. Call once after constructing the bus.
func (s *Service) Register() {
	s.bus.HandleStream(TypeOpen, s.handleOpen)
	s.bus.Handle(TypeStatus, s.handleStatus)
}

// SetPolicy updates the access policy (from the per-camp config). enabled
// is the master switch; allowed is the pub allowlist (empty = allow any
// authenticated peer); command overrides the PTY program.
func (s *Service) SetPolicy(enabled bool, allowed []string, command string) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.enabled = enabled
	s.command = command
	s.allow = make(map[string]bool, len(allowed))
	for _, p := range allowed {
		if p != "" {
			s.allow[p] = true
		}
	}
}

// allowed reports whether fromPub may open a shell here.
func (s *Service) allowed(fromPub string) bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if !s.enabled || fromPub == "" {
		return false
	}
	if len(s.allow) == 0 {
		return true // permissive testing default
	}
	return s.allow[fromPub]
}

func (s *Service) cmd() string {
	s.pmu.Lock()
	c := s.command
	s.pmu.Unlock()
	if c != "" {
		return c
	}
	if p, err := exec.LookPath("login"); err == nil {
		return p
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// handleStatus answers whether the shell is available to the caller (only
// reveals "yes" to peers that would actually be allowed in).
func (s *Service) handleStatus(fromPub string, _ []byte) ([]byte, error) {
	return json.Marshal(statusResp{Available: s.allowed(fromPub)})
}

// handleOpen attaches the inbound stream to a PTY session (creating it if
// new), replays the ring buffer, then pumps client input until the stream
// dies — at which point the session stays alive for the next reattach.
func (s *Service) handleOpen(fromPub string, open []byte, st *bus.Stream) {
	defer st.Close()
	if !s.allowed(fromPub) {
		_, _ = st.Write([]byte("\r\nshell: access denied\r\n"))
		return
	}
	var o openReq
	if err := json.Unmarshal(open, &o); err != nil || o.SessionID == "" {
		_, _ = st.Write([]byte("\r\nshell: bad open request\r\n"))
		return
	}
	se, err := s.getOrCreate(o.SessionID, o.Cols, o.Rows)
	if err != nil {
		_, _ = st.Write([]byte("\r\nshell: " + err.Error() + "\r\n"))
		return
	}

	se.attach(st)
	defer se.detach(st)
	se.resize(o.Cols, o.Rows)

	// Pump client→server framed input until the stream ends.
	for {
		op, data, err := readMsg(st)
		if err != nil {
			return
		}
		switch op {
		case opData:
			_, _ = se.ptmx.Write(data)
		case opResize:
			if len(data) >= 4 {
				se.resize(be16(data[0:]), be16(data[2:]))
			}
		}
	}
}

func (s *Service) getOrCreate(id string, cols, rows uint16) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if se := s.sessions[id]; se != nil {
		return se, nil
	}
	se, err := newSession(id, s.cmd(), cols, rows)
	if err != nil {
		return nil, err
	}
	s.sessions[id] = se
	go func() {
		<-se.done
		s.mu.Lock()
		if s.sessions[id] == se {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
		log.Printf("shell: session %s ended", id)
	}()
	log.Printf("shell: session %s started (%s)", id, se.cmd.Path)
	return se, nil
}

// --- session ---

type session struct {
	id   string
	ptmx *os.File
	cmd  *exec.Cmd
	done chan struct{}

	mu       sync.Mutex
	ring     []byte
	attached io.Writer // current client output sink, or nil when detached
}

func newSession(id, command string, cols, rows uint16) (*session, error) {
	c := exec.Command(command)
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(c)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	se := &session{id: id, ptmx: ptmx, cmd: c, done: make(chan struct{})}
	if cols > 0 && rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
	go se.pump()
	return se, nil
}

// pump reads PTY output into the ring buffer and forwards it to the
// currently-attached client (if any). Runs for the life of the session.
func (se *session) pump() {
	defer close(se.done)
	defer se.ptmx.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := se.ptmx.Read(buf)
		if n > 0 {
			se.mu.Lock()
			se.appendRing(buf[:n])
			w := se.attached
			se.mu.Unlock()
			if w != nil {
				if _, werr := w.Write(buf[:n]); werr != nil {
					// Client gone; keep buffering, it may reattach.
					se.mu.Lock()
					if se.attached == w {
						se.attached = nil
					}
					se.mu.Unlock()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (se *session) appendRing(b []byte) {
	se.ring = append(se.ring, b...)
	if len(se.ring) > ringCap {
		se.ring = se.ring[len(se.ring)-ringCap:]
	}
}

// attach makes w the live output sink and repaints the recent screen.
func (se *session) attach(w io.Writer) {
	se.mu.Lock()
	defer se.mu.Unlock()
	// Clear the client's screen, then replay the ring so the cursor/content
	// matches the live PTY — no garbage, no replayed keystroke stream.
	_, _ = w.Write([]byte("\x1b[2J\x1b[H"))
	_, _ = w.Write(se.ring)
	se.attached = w
}

func (se *session) detach(w io.Writer) {
	se.mu.Lock()
	if se.attached == w {
		se.attached = nil
	}
	se.mu.Unlock()
}

func (se *session) resize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	_ = pty.Setsize(se.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// --- client helpers (used by the web bridge) ---

// Open dials a peer and attaches to (or creates) the given session,
// returning the raw stream: read it for terminal output, write input/resize
// through WriteInput/WriteResize. Caller closes it.
func (s *Service) Open(ctx context.Context, pub, sessionID string, cols, rows uint16) (*bus.Stream, error) {
	open, err := json.Marshal(openReq{SessionID: sessionID, Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return s.bus.OpenStream(ctx, pub, TypeOpen, open)
}

// Available asks pub whether its shell is open to us.
func (s *Service) Available(ctx context.Context, pub string) bool {
	resp, err := s.bus.Request(ctx, pub, TypeStatus, nil)
	if err != nil {
		return false
	}
	var r statusResp
	if json.Unmarshal(resp, &r) != nil {
		return false
	}
	return r.Available
}

// WriteInput frames stdin bytes for the client→server direction.
func WriteInput(w io.Writer, p []byte) error { return writeMsg(w, opData, p) }

// WriteResize frames a terminal resize for the client→server direction.
func WriteResize(w io.Writer, cols, rows uint16) error {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return writeMsg(w, opResize, b[:])
}

// --- framing (client→server) ---

func writeMsg(w io.Writer, op byte, data []byte) error {
	var hdr [5]byte
	hdr[0] = op
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err := w.Write(data)
		return err
	}
	return nil
}

func readMsg(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 1<<20 {
		return 0, nil, errors.New("shell: input frame too large")
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return hdr[0], data, nil
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

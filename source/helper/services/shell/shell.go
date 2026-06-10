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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"github.com/vseplet/f2f/source/helper/clog"
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
	opKill   = 'k' // terminate the PTY session on the host
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

func (s *Service) cmdArgs() []string {
	s.pmu.Lock()
	c := s.command
	s.pmu.Unlock()
	if c != "" {
		if f := strings.Fields(c); len(f) > 0 {
			return f
		}
	}
	// Secure default: the system `login`, so the connecting peer must
	// authenticate as a real OS user (username + password). Runs as root —
	// login drops privileges itself after a successful auth.
	if p, err := exec.LookPath("login"); err == nil {
		if runtime.GOOS == "linux" {
			return []string{p, "-p"} // -p: keep our env (TERM) for the post-login shell
		}
		return []string{p}
	}
	// Fallback when login is unavailable: an interactive shell (run as the
	// invoking user via dropToInvokingUser, not root).
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	return []string{"/bin/sh"}
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
	// If the PTY dies, end this stream so the client sees a disconnect (and
	// reattaches to a fresh session) instead of a silent dead terminal.
	go func() {
		<-se.done
		_ = st.Close()
	}()

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
		case opKill:
			clog.Info("shell", "session %s killed by %s", o.SessionID, s.bus.Label(fromPub))
			se.kill()
			return
		}
	}
}

func (s *Service) getOrCreate(id string, cols, rows uint16) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if se := s.sessions[id]; se != nil {
		return se, nil
	}
	se, err := newSession(id, s.cmdArgs(), cols, rows)
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
		clog.Info("shell", "session %s ended", id)
	}()
	clog.Warn("shell", "session %s started %v", id, se.cmd.Args)
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

	// Mouse/paste mode tracking, so a reattaching client restores them even
	// after the app's one-time enable scrolled out of the ring (otherwise the
	// mouse "stops working" on reattach). We track ONLY mouse, deliberately:
	// re-emitting alt-screen or several tracking levels at once corrupts state.
	mouseTrack int    // active tracking mode: 0=off, or 1000/1002/1003
	mouseEnc   int    // active coordinate encoding: 0=default, or 1005/1006/1015
	bracketed  bool   // bracketed paste (2004)
	modeTail   []byte // partial escape seq carried across PTY reads
}

func newSession(id string, argv []string, cols, rows uint16) (*session, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	// `login` authenticates and switches user itself, so it must stay root;
	// a raw shell fallback is dropped to the machine's owner instead.
	if filepath.Base(argv[0]) != "login" {
		dropToInvokingUser(c)
	}
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

// dropToInvokingUser makes the PTY run as $SUDO_USER (the human who started
// f2f), not root. f2f runs as root via sudo, so it can setuid down. No-op
// when not under sudo. pty.Start adds Setsid/Setctty to whatever
// SysProcAttr we leave here, so the credential survives.
//
// This is privilege REDUCTION, not authentication — `login` (Shell.Command)
// is the path that actually prompts for a password. Combined with the peer
// allowlist, it keeps a remote shell at "the owner's account", not root.
func dropToInvokingUser(c *exec.Cmd) {
	su := os.Getenv("SUDO_USER")
	if su == "" || su == "root" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		return
	}
	uid, e1 := strconv.Atoi(u.Uid)
	gid, e2 := strconv.Atoi(u.Gid)
	if e1 != nil || e2 != nil {
		return
	}
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	if u.HomeDir != "" {
		c.Dir = u.HomeDir
		c.Env = append(c.Env, "HOME="+u.HomeDir)
	}
	c.Env = append(c.Env, "USER="+su, "LOGNAME="+su)
}

func (se *session) appendRing(b []byte) {
	se.scanModes(b)
	se.ring = append(se.ring, b...)
	if len(se.ring) > ringCap {
		se.ring = se.ring[len(se.ring)-ringCap:]
	}
}

// scanModes watches PTY output for the DECSET/DECRST private-mode sequences
// (ESC [ ? <params> h|l) that toggle mouse reporting and bracketed paste, and
// records the latest state — so modePreamble can restore them on reattach.
// Tracking levels (1000/1002/1003) are mutually exclusive: the last one set
// wins, any reset clears it. Sequences split across PTY reads carry in
// modeTail. Called under se.mu (via appendRing).
func (se *session) scanModes(b []byte) {
	data := b
	if len(se.modeTail) > 0 {
		data = append(se.modeTail, b...)
		se.modeTail = nil
	}
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			i++
			continue
		}
		if i+2 >= len(data) { // not enough for "ESC [ ?"
			se.carry(data[i:])
			return
		}
		if data[i+1] != '[' || data[i+2] != '?' {
			i++
			continue
		}
		j := i + 3
		for j < len(data) && (data[j] == ';' || (data[j] >= '0' && data[j] <= '9')) {
			j++
		}
		if j >= len(data) { // params not terminated yet — carry the partial seq
			se.carry(data[i:])
			return
		}
		if final := data[j]; final == 'h' || final == 'l' {
			set := final == 'h'
			for _, p := range strings.Split(string(data[i+3:j]), ";") {
				if n, err := strconv.Atoi(p); err == nil {
					se.applyMode(n, set)
				}
			}
		}
		i = j + 1
	}
}

func (se *session) applyMode(n int, set bool) {
	switch n {
	case 1000, 1002, 1003: // mouse tracking level (mutually exclusive)
		if set {
			se.mouseTrack = n
		} else if se.mouseTrack == n {
			se.mouseTrack = 0
		}
	case 1005, 1006, 1015: // mouse coordinate encoding
		if set {
			se.mouseEnc = n
		} else if se.mouseEnc == n {
			se.mouseEnc = 0
		}
	case 2004: // bracketed paste
		se.bracketed = set
	}
}

// carry stashes an incomplete trailing escape sequence for the next read,
// bounded so a stray ESC can't grow it without limit.
func (se *session) carry(b []byte) {
	if len(b) > 64 {
		return // not a real DECSET seq; drop it
	}
	se.modeTail = append([]byte(nil), b...)
}

// modePreamble builds the sequences to restore mouse/paste state on reattach:
// encoding first (so it's in effect when tracking turns on), then the single
// active tracking level, then bracketed paste. Empty when nothing is on. NOTE:
// deliberately minimal — no alt-screen, no multiple tracking levels.
func (se *session) modePreamble() []byte {
	var b []byte
	if se.mouseEnc != 0 {
		b = append(b, []byte(fmt.Sprintf("\x1b[?%dh", se.mouseEnc))...)
	}
	if se.mouseTrack != 0 {
		b = append(b, []byte(fmt.Sprintf("\x1b[?%dh", se.mouseTrack))...)
	}
	if se.bracketed {
		b = append(b, []byte("\x1b[?2004h")...)
	}
	return b
}

// attach makes w the live output sink and repaints the recent screen.
func (se *session) attach(w io.Writer) {
	se.mu.Lock()
	defer se.mu.Unlock()
	// Clear the client's screen, then replay the ring so the cursor/content
	// matches the live PTY — no garbage, no replayed keystroke stream.
	_, _ = w.Write([]byte("\x1b[2J\x1b[H"))
	_, _ = w.Write(se.ring)
	// Restore mouse/paste modes in case the app's one-time enable scrolled out
	// of the ring (without this, mouse "stops working" after a reattach).
	if pre := se.modePreamble(); pre != nil {
		_, _ = w.Write(pre)
	}
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

// kill terminates the whole PTY session (process group), so the shell and any
// children it spawned go away. The pump then hits EOF and the session is
// reaped from the map (see getOrCreate).
func (se *session) kill() {
	if se.cmd != nil && se.cmd.Process != nil {
		pid := se.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGKILL) // session leader → negative pid = the group
		_ = se.cmd.Process.Kill()               // fallback: the leader itself
	}
	_ = se.ptmx.Close()
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

// WriteKill asks the host to terminate the PTY session.
func WriteKill(w io.Writer) error { return writeMsg(w, opKill, nil) }

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

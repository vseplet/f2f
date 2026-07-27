package cli

// `f2f tui` — a single terminal control panel for a running helper, aimed at
// headless nodes (a --service box, a VPS) where opening the web portal means an
// SSH tunnel. It is a thin HTTP client to the helper's loopback API (the same
// endpoints the web UI uses), so it holds no logic of its own: every screen
// just GET/POST/PUT/DELETEs against 127.0.0.1:2202 and the live process applies
// the change. Sections: status/peers, certificate trust, published domains,
// tunnels (intercepts), remote shell/desktop exposure, OIDC clients, calls.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

const defaultRemoteBind = "127.0.0.1:2202"

func remoteHTTP() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// remoteChannel / remoteExposure are the channel-exposure shapes shared by the
// shell/desktop picker section.
type remoteChannel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`   // creator pub — disambiguates same-named channels
	Members []string `json:"members"` // member pubs — used to filter to channels I'm in
}

type remoteExposure struct {
	Terminal []string `json:"terminal"`
	Desktop  []string `json:"desktop"`
}

func fetchChannels(base string) ([]remoteChannel, error) {
	resp, err := remoteHTTP().Get(base + "/api/channels")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /api/channels: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out []remoteChannel
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fetchExposure(base string) (remoteExposure, error) {
	var ex remoteExposure
	resp, err := remoteHTTP().Get(base + "/api/remote/exposure")
	if err != nil {
		return ex, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ex, fmt.Errorf("GET /api/remote/exposure: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return ex, json.NewDecoder(resp.Body).Decode(&ex)
}

func postExposure(base string, ex remoteExposure) error {
	body, _ := json.Marshal(ex)
	resp, err := remoteHTTP().Post(base+"/api/remote/exposure", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /api/remote/exposure: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// dedupChannels removes only EXACT duplicates (same block id) and offers the
// camp-wide "general" exactly once at the top. Two channels that share a name
// but have different ids are BOTH kept — they're genuinely distinct; the picker
// disambiguates them by owner nick (see channelNames).
func dedupChannels(chans []remoteChannel) []remoteChannel {
	sort.Slice(chans, func(i, j int) bool {
		if chans[i].Name == chans[j].Name {
			return chans[i].ID < chans[j].ID
		}
		return chans[i].Name < chans[j].Name
	})
	seenID := map[string]bool{}
	out := []remoteChannel{{ID: "general", Name: "general"}}
	for _, c := range chans {
		if c.ID == "" || c.Name == "general" || seenID[c.ID] {
			continue
		}
		seenID[c.ID] = true
		out = append(out, c)
	}
	return out
}

// myChannels returns the deduped channels this node may actually grant/act on —
// ones I own or am a member of (plus the camp-wide general). /api/channels lists
// every camp channel with no membership filter, so we filter client-side by our
// own pub, matching the web (you can't expose to a channel you're not in).
func (t *tui) myChannels() []remoteChannel {
	chans, err := fetchChannels(t.base)
	if err != nil {
		return dedupChannels(nil)
	}
	var st tuiStatus
	_ = t.get("/api/status", &st)
	me := st.IdentityPub
	var mine []remoteChannel
	for _, c := range chans {
		if c.Owner == me || containsStr(c.Members, me) {
			mine = append(mine, c)
		}
	}
	return dedupChannels(mine)
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// peerNames maps peer pub → display nick (profile name preferred), including
// self, so channel owners can be shown by nick instead of a raw pubkey.
func (t *tui) peerNames() map[string]string {
	m := map[string]string{}
	var st tuiStatus
	if err := t.get("/api/status", &st); err == nil {
		for _, p := range st.Peers {
			if p.Pub == "" {
				continue
			}
			if p.ProfileName != "" {
				m[p.Pub] = p.ProfileName
			} else {
				m[p.Pub] = p.Name
			}
		}
	}
	return m
}

// channelNames maps channel id → display name with the creator's nick appended,
// e.g. "dev (alice)", so every channel shows who made it (and same-named twins
// are tellable apart). The synthetic camp-wide "general" has no owner, so it
// stays bare.
func (t *tui) channelNames(chans []remoteChannel) map[string]string {
	names := t.peerNames()
	out := make(map[string]string, len(chans))
	for _, c := range chans {
		n := c.Name
		if c.Owner != "" {
			who := names[c.Owner]
			if who == "" {
				who = shortFp(c.Owner)
			}
			n += " (" + who + ")"
		}
		out[c.ID] = n
	}
	return out
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func labelList(ids []string, name map[string]string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		if n := name[id]; n != "" {
			parts[i] = "#" + n
		} else {
			parts[i] = id
		}
	}
	return strings.Join(parts, ", ")
}

// tui is the loopback client. base is "http://<bind>".
type tui struct{ base string }

// RunTUI is the `f2f tui` entrypoint: resolves the bind, confirms the helper is
// reachable, then loops the main menu until the user quits (or Esc).
func RunTUI(args []string) error {
	fs := flag.NewFlagSet("f2f tui", flag.ExitOnError)
	bind := fs.String("bind", defaultRemoteBind, "loopback address of the running helper")
	_ = fs.Parse(args)
	t := &tui{base: "http://" + *bind}

	var st tuiStatus
	if err := t.get("/api/status", &st); err != nil {
		return fmt.Errorf("can't reach f2f at %s (is it running?): %w", *bind, err)
	}
	for {
		title := "f2f tui"
		if st.CampLabel != "" {
			title = "f2f tui · " + st.CampLabel
		}
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title(title).Options(
				huh.NewOption("Network", "status"),
				huh.NewOption("Certificates", "certs"),
				huh.NewOption("Domains", "domains"),
				huh.NewOption("Ports", "firewall"),
				huh.NewOption("Tunnels", "tunnels"),
				huh.NewOption("Channels", "channels"),
				huh.NewOption("Remote shell", "shell"),
				huh.NewOption("Remote desktop", "desktop"),
				huh.NewOption("OIDC clients", "oidc"),
				huh.NewOption("Calls", "calls"),
				huh.NewOption("Logs", "logs"),
				huh.NewOption("Update", "update"),
				huh.NewOption("Quit", "quit"),
			).Value(&choice),
		))
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		var err error
		switch choice {
		case "status":
			err = t.sectionStatus()
		case "certs":
			err = t.sectionCerts()
		case "domains":
			err = t.sectionDomains()
		case "firewall":
			err = t.sectionFirewall()
		case "tunnels":
			err = t.sectionTunnels()
		case "channels":
			err = t.sectionChannels()
		case "shell":
			err = t.editExposure("terminal")
		case "desktop":
			err = t.editExposure("desktop")
		case "oidc":
			err = t.sectionOIDC()
		case "calls":
			err = t.sectionCalls()
		case "logs":
			err = t.sectionLogs()
		case "update":
			err = t.sectionUpdate()
		case "quit", "":
			return nil
		}
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		_ = t.get("/api/status", &st) // refresh the camp label for the title
	}
}

// ---- loopback client ----

func (t *tui) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, t.base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := remoteHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (t *tui) get(path string, out any) error { return t.do(http.MethodGet, path, nil, out) }

// ---- wire shapes (subset of the web API responses we render) ----

type tuiStatus struct {
	Running     bool           `json:"running"`
	CampID      string         `json:"camp_id"`
	CampLabel   string         `json:"camp_label"`
	CampActive  bool           `json:"camp_active"`
	CampReflex  string         `json:"camp_reflex"`
	LocalIP     string         `json:"local_ip"`
	IdentityFp  string         `json:"identity_fp"`
	IdentityPub string         `json:"identity_pub"`
	Peers       []tuiPeer      `json:"peers"`
	Intercepts  []tuiIntercept `json:"intercepts"`
}

type tuiPeer struct {
	Name        string        `json:"name"`
	Fp          string        `json:"fp"`
	Pub         string        `json:"pub"`
	OverlayV4   string        `json:"overlay_v4"`
	UDPEndpoint string        `json:"udp_endpoint"`
	Role        string        `json:"role"`
	Version     string        `json:"version"`
	Self        bool          `json:"self"`
	Paired      bool          `json:"paired"`
	HalfPaired  bool          `json:"half_paired"`
	InCamp      bool          `json:"in_camp"`
	RTTMs       int64         `json:"rtt_ms"`
	ProfileName string        `json:"profile_name"`
	Domains     []tuiDomain   `json:"domains"`
	Firewall    []tuiFirewall `json:"firewall"`
}

type tuiFirewall struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// tuiFirewallView is the GET /api/firewall response: builtin ports are
// read-only, user ports are editable, active reports whether the OS firewall is
// actually enforcing.
type tuiFirewallView struct {
	Active  bool          `json:"active"`
	Builtin []tuiFirewall `json:"builtin"`
	User    []tuiFirewall `json:"user"`
}

type tuiIntercept struct {
	ID   string `json:"id"`
	Spec string `json:"spec"`
	Peer string `json:"peer"`
}

type tuiDomain struct {
	Name     string   `json:"name"`
	Host     string   `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	Proto    string   `json:"proto,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

type tuiMyCA struct {
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
}

type tuiPeerCA struct {
	PeerName    string `json:"peer_name"`
	CommonName  string `json:"common_name"`
	Fingerprint string `json:"fingerprint"`
	Installed   bool   `json:"installed"`
}

type tuiOIDC struct {
	Issuer    string          `json:"issuer"`
	Discovery string          `json:"discovery"`
	Clients   []tuiOIDCClient `json:"clients"`
}

type tuiOIDCClient struct {
	ClientID     string `json:"client_id"`
	ClientName   string `json:"client_name"`
	Confidential bool   `json:"confidential"`
}

type tuiCall struct {
	CallID       string        `json:"call_id"`
	Channel      string        `json:"channel"`
	SFUHost      string        `json:"sfu_host"`
	Participants []interface{} `json:"participants"`
}

// ---- table view (bubbles/table) ----

// tableModel wraps a bubbles table for a scrollable list. Keys: enter →
// "select" the highlighted row (caller acts on it), r → "refresh", esc/q/ctrl+c
// → "back". The chosen action + row index are read back after Run.
type tableModel struct {
	table  table.Model
	title  string
	action string          // "back" | "refresh" | "select" | one of extra
	idx    int             // highlighted row for select/extra actions
	extra  map[string]bool // additional single-key actions (e.g. "a","d")
}

func (m *tableModel) Init() tea.Cmd { return nil }

func (m *tableModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		s := k.String()
		switch s {
		case "esc", "q", "ctrl+c":
			m.action = "back"
			return m, tea.Quit
		case "r":
			m.action = "refresh"
			return m, tea.Quit
		case "enter":
			m.action = "select"
			m.idx = m.table.Cursor()
			return m, tea.Quit
		default:
			if m.extra[s] {
				m.action = s
				m.idx = m.table.Cursor()
				return m, tea.Quit
			}
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *tableModel) View() string {
	return m.title + "\n" + m.table.View() + "\n↑/↓ scroll · enter select · r refresh · esc back\n"
}

// runTable shows headers+rows as a scrollable table and blocks until the user
// leaves. Returns the action ("select"/"refresh"/"back", or one of extraKeys)
// and, for select/extra, the highlighted row index. Column widths are sized to
// the widest cell (rune-counted, so non-ASCII names don't skew them).
func runTable(title string, headers []string, rows [][]string, extraKeys ...string) (action string, idx int, err error) {
	cols := make([]table.Column, len(headers))
	for i, h := range headers {
		w := utf8.RuneCountInString(h)
		for _, r := range rows {
			if i < len(r) {
				if n := utf8.RuneCountInString(r[i]); n > w {
					w = n
				}
			}
		}
		cols[i] = table.Column{Title: h, Width: w}
	}
	trows := make([]table.Row, len(rows))
	for i, r := range rows {
		trows[i] = table.Row(r)
	}
	height := len(rows) + 1
	if height > 20 {
		height = 20
	}
	if height < 1 {
		height = 1
	}
	tbl := table.New(
		table.WithColumns(cols),
		table.WithRows(trows),
		table.WithFocused(true),
		table.WithHeight(height),
	)
	extra := make(map[string]bool, len(extraKeys))
	for _, k := range extraKeys {
		extra[k] = true
	}
	m := &tableModel{table: tbl, title: title, action: "back", extra: extra}
	// Alt-screen so the table is torn down on exit (restores the prior screen)
	// instead of leaving its last frame lingering above the menu.
	fm, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return "back", 0, err
	}
	if final, ok := fm.(*tableModel); ok {
		return final.action, final.idx, nil
	}
	return "back", 0, nil
}

// ---- sections ----

func (t *tui) sectionStatus() error {
	for {
		var st tuiStatus
		if err := t.get("/api/status", &st); err != nil {
			return err
		}
		title := fmt.Sprintf("camp %s · overlay %s · fp %s",
			orDash(st.CampLabel), orDash(st.LocalIP), orDash(st.IdentityFp))
		if st.CampReflex != "" {
			title += " · reflex " + st.CampReflex
		}
		headers := []string{"", "NAME", "ROLE", "FP", "ENDPOINT", "RTT"}
		rows := make([][]string, 0, len(st.Peers))
		for _, p := range st.Peers {
			rows = append(rows, []string{
				peerMarker(p), p.Name, roleOr(p.Role), fpOr(p.Fp), endpointOr(p), rttOr(p),
			})
		}
		action, _, err := runTable(title, headers, rows)
		if err != nil {
			return err
		}
		if action != "refresh" {
			return nil // enter/esc/q all just go back — status has no per-row action
		}
	}
}

func (t *tui) sectionCerts() error {
	for {
		var ca tuiMyCA
		_ = t.get("/api/my-ca", &ca)
		var peers []tuiPeerCA
		if err := t.get("/api/trusted-peers", &peers); err != nil {
			return err
		}
		title := fmt.Sprintf("My CA: %s (fp %s) · enter toggles trust",
			orDash(ca.CommonName), shortFp(ca.Fingerprint))
		headers := []string{"TRUST", "PEER", "FINGERPRINT"}
		rows := make([][]string, len(peers))
		for i, p := range peers {
			state := "no"
			if p.Installed {
				state = "yes"
			}
			rows[i] = []string{state, p.PeerName, shortFp(p.Fingerprint)}
		}
		action, idx, err := runTable(title, headers, rows)
		if err != nil {
			return err
		}
		switch action {
		case "refresh":
			continue
		case "select":
			if idx < 0 || idx >= len(peers) {
				continue
			}
			p := peers[idx]
			if p.Installed {
				err = t.do(http.MethodDelete, "/api/trusted-peers/"+p.Fingerprint, nil, nil)
			} else {
				err = t.do(http.MethodPost, "/api/trusted-peers/"+p.Fingerprint+"/install", nil, nil)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		default:
			return nil
		}
	}
}

func (t *tui) sectionDomains() error {
	for {
		var domains []tuiDomain
		if err := t.get("/api/my-domains", &domains); err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString("Mine:\n")
		if len(domains) == 0 {
			b.WriteString("  (none)\n")
		} else {
			for _, d := range domains {
				tgt := d.Host
				if tgt == "" {
					tgt = "127.0.0.1"
				}
				if d.Port > 0 {
					tgt = fmt.Sprintf("%s:%d", tgt, d.Port)
				}
				fmt.Fprintf(&b, "  %-24s → %s\n", d.Name, tgt)
			}
		}
		// Peers' published domains (read-only) — from /api/status, which the
		// portal fills by polling each reachable peer's /api/domains. Empty for
		// peers we can't reach yet.
		var st tuiStatus
		_ = t.get("/api/status", &st)
		var others strings.Builder
		for _, p := range st.Peers {
			if p.Self || len(p.Domains) == 0 {
				continue
			}
			for _, d := range p.Domains {
				fmt.Fprintf(&others, "  %-24s @ %s\n", d.Name, p.Name)
			}
		}
		if others.Len() > 0 {
			b.WriteString("\nPeers':\n")
			b.WriteString(others.String())
		}
		opts := []huh.Option[string]{huh.NewOption("Add domain", "add")}
		for i, d := range domains {
			opts = append(opts, huh.NewOption("Delete: "+d.Name, "del:"+strconv.Itoa(i)))
		}
		opts = append(opts, huh.NewOption("Back", "back"))
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("Domains (what this node publishes)").Description(b.String()),
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		))
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		switch {
		case choice == "back" || choice == "":
			return nil
		case choice == "add":
			if err := t.addDomain(domains); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case strings.HasPrefix(choice, "del:"):
			i, _ := strconv.Atoi(strings.TrimPrefix(choice, "del:"))
			if i >= 0 && i < len(domains) {
				next := append(domains[:i:i], domains[i+1:]...)
				if err := t.do(http.MethodPut, "/api/my-domains", next, new([]tuiDomain)); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
			}
		}
	}
}

func (t *tui) addDomain(cur []tuiDomain) error {
	var name, portStr, host string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Name (e.g. gitea or gitea.mini)").Value(&name),
		huh.NewInput().Title("Local service port (empty = none)").Value(&portStr),
		huh.NewInput().Title("Target host (empty = 127.0.0.1)").Value(&host),
	))
	if err := form.Run(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	d := tuiDomain{Name: name}
	if p := strings.TrimSpace(portStr); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n >= 65536 {
			return fmt.Errorf("invalid port: %s", p)
		}
		d.Port = n
	}
	if h := strings.TrimSpace(host); h != "" {
		d.Host = h
	}
	next := append(append([]tuiDomain{}, cur...), d)
	return t.do(http.MethodPut, "/api/my-domains", next, new([]tuiDomain))
}

func (t *tui) sectionFirewall() error {
	// rowMeta maps a table row back to a firewall entry: only "mine" rows are
	// editable (toggle/delete); built-in and peers' rows are read-only.
	type rowMeta struct {
		editable bool
		userIdx  int
	}
	for {
		var fw tuiFirewallView
		if err := t.get("/api/firewall", &fw); err != nil {
			return err
		}
		var st tuiStatus
		_ = t.get("/api/status", &st)

		headers := []string{"SCOPE", "PORT", "PROTO", "STATE", "DESC"}
		var rows [][]string
		var meta []rowMeta
		for _, p := range fw.Builtin {
			rows = append(rows, []string{"builtin", strconv.Itoa(p.Port), orDash(p.Protocol), onOff(p.Enabled), p.Description})
			meta = append(meta, rowMeta{false, -1})
		}
		for i, p := range fw.User {
			rows = append(rows, []string{"mine", strconv.Itoa(p.Port), orDash(p.Protocol), onOff(p.Enabled), p.Description})
			meta = append(meta, rowMeta{true, i})
		}
		for _, p := range st.Peers {
			if p.Self {
				continue
			}
			for _, f := range p.Firewall {
				if !f.Enabled {
					continue
				}
				rows = append(rows, []string{p.Name, strconv.Itoa(f.Port), orDash(f.Protocol), "on", f.Description})
				meta = append(meta, rowMeta{false, -1})
			}
		}

		fwState := "disabled"
		if fw.Active {
			fwState = "active"
		}
		title := fmt.Sprintf("firewall: %s · enter toggle · a add · d delete (only 'mine')", fwState)
		action, idx, err := runTable(title, headers, rows, "a", "d")
		if err != nil {
			return err
		}
		editable := idx >= 0 && idx < len(meta) && meta[idx].editable
		switch action {
		case "refresh":
			continue
		case "a":
			if err := t.addFirewallPort(fw.User); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case "select": // toggle enabled
			if editable {
				i := meta[idx].userIdx
				next := append([]tuiFirewall{}, fw.User...)
				next[i].Enabled = !next[i].Enabled
				if err := t.putFirewall(next); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
			}
		case "d": // delete
			if editable {
				i := meta[idx].userIdx
				next := append(fw.User[:i:i], fw.User[i+1:]...)
				if err := t.putFirewall(next); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
			}
		default:
			return nil
		}
	}
}

func (t *tui) addFirewallPort(cur []tuiFirewall) error {
	var portStr, desc string
	proto := "tcp"
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Port").Value(&portStr),
		huh.NewSelect[string]().Title("Protocol").Options(
			huh.NewOption("tcp", "tcp"),
			huh.NewOption("udp", "udp"),
		).Value(&proto),
		huh.NewInput().Title("Description (optional)").Value(&desc),
	))
	if err := form.Run(); err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || n <= 0 || n >= 65536 {
		return fmt.Errorf("invalid port: %s", portStr)
	}
	entry := tuiFirewall{Port: n, Protocol: proto, Description: strings.TrimSpace(desc), Enabled: true}
	next := append(append([]tuiFirewall{}, cur...), entry)
	return t.putFirewall(next)
}

// putFirewall replaces the user port list (PUT semantics; builtin is untouched).
func (t *tui) putFirewall(user []tuiFirewall) error {
	return t.do(http.MethodPut, "/api/firewall", map[string]any{"user": user}, new(tuiFirewallView))
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// sectionChannels lists every camp channel (raw — no dedup, so duplicates are
// visible) with its owner nick, and lets you delete (enter/d, with confirm) or
// create (a) one. Note: the block engine does NOT gate deletion by ownership —
// a signed tombstone from any member removes the channel; the owner field is a
// display label, not a permission.
func (t *tui) sectionChannels() error {
	for {
		chans, err := fetchChannels(t.base)
		if err != nil {
			return err
		}
		sort.Slice(chans, func(i, j int) bool {
			if chans[i].Name == chans[j].Name {
				return chans[i].ID < chans[j].ID
			}
			return chans[i].Name < chans[j].Name
		})
		names := t.peerNames()
		headers := []string{"NAME", "OWNER", "MEMBERS", "ID"}
		rows := make([][]string, len(chans))
		for i, c := range chans {
			owner := names[c.Owner]
			if owner == "" {
				owner = shortFp(c.Owner)
			}
			rows[i] = []string{c.Name, owner, strconv.Itoa(len(c.Members)), shortFp(c.ID)}
		}
		action, idx, err := runTable("channels · enter/d delete · a new · r refresh · esc back", headers, rows, "a", "d")
		if err != nil {
			return err
		}
		switch action {
		case "refresh":
			continue
		case "a":
			if err := t.createChannel(); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case "select", "d":
			if idx < 0 || idx >= len(chans) {
				continue
			}
			c := chans[idx]
			owner := names[c.Owner]
			if owner == "" {
				owner = shortFp(c.Owner)
			}
			var ok bool
			if e := huh.NewForm(huh.NewGroup(
				huh.NewConfirm().Title(fmt.Sprintf("Delete channel %q by %s?", c.Name, owner)).Value(&ok),
			)).Run(); e != nil {
				if errors.Is(e, huh.ErrUserAborted) {
					continue
				}
				return e
			}
			if ok {
				if err := t.do(http.MethodPost, "/api/channels/delete", map[string]string{"bid": c.ID}, nil); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
			}
		default:
			return nil
		}
	}
}

func (t *tui) createChannel() error {
	var name string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Channel name").Value(&name),
	)).Run(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	return t.do(http.MethodPost, "/api/channels", map[string]any{"name": name}, new(map[string]any))
}

func (t *tui) sectionTunnels() error {
	for {
		var st tuiStatus
		if err := t.get("/api/status", &st); err != nil {
			return err
		}
		var b strings.Builder
		if len(st.Intercepts) == 0 {
			b.WriteString("No tunnels.\n")
		} else {
			for _, it := range st.Intercepts {
				fmt.Fprintf(&b, "  %-28s → %s\n", it.Spec, it.Peer)
			}
		}
		opts := []huh.Option[string]{huh.NewOption("Add tunnel", "add")}
		for _, it := range st.Intercepts {
			opts = append(opts, huh.NewOption("Delete: "+it.Spec, "del:"+it.ID))
		}
		opts = append(opts, huh.NewOption("Back", "back"))
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("Tunnels (domain via exit peer)").Description(b.String()),
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		))
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		switch {
		case choice == "back" || choice == "":
			return nil
		case choice == "add":
			if err := t.addTunnel(st.Peers); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case strings.HasPrefix(choice, "del:"):
			id := strings.TrimPrefix(choice, "del:")
			if err := t.do(http.MethodDelete, "/api/intercepts/"+id, nil, nil); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
	}
}

func (t *tui) addTunnel(peers []tuiPeer) error {
	peerOpts := []huh.Option[string]{}
	for _, p := range peers {
		if p.Self {
			continue
		}
		peerOpts = append(peerOpts, huh.NewOption(p.Name, p.Name))
	}
	if len(peerOpts) == 0 {
		return errors.New("no other peers in the camp")
	}
	var spec, peer string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Domain/spec (e.g. example.com or 1.2.3.4/32)").Value(&spec),
		huh.NewSelect[string]().Title("Via peer").Options(peerOpts...).Value(&peer),
	))
	if err := form.Run(); err != nil {
		return err
	}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return errors.New("empty spec")
	}
	return t.do(http.MethodPost, "/api/intercepts", map[string]string{"spec": spec, "peer": peer}, new(tuiIntercept))
}

// editExposure drives the channel picker for ONE exposure dimension
// (which="terminal" → remote shell, which="desktop" → remote desktop). It reads
// the full current exposure, edits only the requested dimension, and writes the
// full object back — the other dimension is preserved untouched.
func (t *tui) editExposure(which string) error {
	cur, err := fetchExposure(t.base)
	if err != nil {
		return err
	}
	chans := t.myChannels()
	disp := t.channelNames(chans)

	var picked []string
	var title string
	if which == "desktop" {
		picked = append([]string{}, cur.Desktop...)
		title = "desktop — channels my screen is open to"
	} else {
		picked = append([]string{}, cur.Terminal...)
		title = "shell — channels my terminal is open to"
	}
	opts := make([]huh.Option[string], len(chans))
	for i, c := range chans {
		opts[i] = huh.NewOption("#"+disp[c.ID], c.ID)
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title(title).Options(opts...).Value(&picked),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil
		}
		return err
	}

	next := remoteExposure{Terminal: nonNil(cur.Terminal), Desktop: nonNil(cur.Desktop)}
	if which == "desktop" {
		next.Desktop = nonNil(picked)
	} else {
		next.Terminal = nonNil(picked)
	}
	if err := postExposure(t.base, next); err != nil {
		return err
	}
	if which == "desktop" {
		fmt.Printf("desktop open to: %s\n", labelList(next.Desktop, disp))
	} else {
		fmt.Printf("shell open to: %s\n", labelList(next.Terminal, disp))
	}
	return nil
}

func (t *tui) sectionOIDC() error {
	for {
		var info tuiOIDC
		if err := t.get("/api/oidc", &info); err != nil {
			return err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Issuer: %s\n", orDash(info.Issuer))
		if info.Discovery != "" {
			fmt.Fprintf(&b, "Discovery: %s\n", info.Discovery)
		}
		b.WriteString("\n")
		if len(info.Clients) == 0 {
			b.WriteString("No clients.\n")
		} else {
			for _, c := range info.Clients {
				kind := "public"
				if c.Confidential {
					kind = "confidential"
				}
				fmt.Fprintf(&b, "  %-20s [%s]  %s\n", c.ClientName, kind, c.ClientID)
			}
		}
		opts := []huh.Option[string]{huh.NewOption("Create client", "add")}
		for _, c := range info.Clients {
			opts = append(opts, huh.NewOption("Delete: "+c.ClientName, "del:"+c.ClientID))
		}
		opts = append(opts, huh.NewOption("Back", "back"))
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("OIDC provider").Description(b.String()),
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		))
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		switch {
		case choice == "back" || choice == "":
			return nil
		case choice == "add":
			if err := t.addOIDCClient(); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case strings.HasPrefix(choice, "del:"):
			id := strings.TrimPrefix(choice, "del:")
			if err := t.do(http.MethodDelete, "/api/oidc/clients/"+id, nil, nil); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
	}
}

func (t *tui) addOIDCClient() error {
	var name, redirects string
	confidential := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Client name").Value(&name),
		huh.NewInput().Title("Redirect URIs (comma-separated)").Value(&redirects),
		huh.NewConfirm().Title("Confidential (with client_secret)?").Value(&confidential),
	))
	if err := form.Run(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	var uris []string
	for _, u := range strings.Split(redirects, ",") {
		if u = strings.TrimSpace(u); u != "" {
			uris = append(uris, u)
		}
	}
	req := map[string]any{
		"name":          name,
		"redirect_uris": uris,
		"public":        !confidential,
		"pkce":          true,
	}
	var resp struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := t.do(http.MethodPost, "/api/oidc/clients", req, &resp); err != nil {
		return err
	}
	fmt.Println("\nclient created:")
	fmt.Println("  client_id:     ", resp.ClientID)
	if resp.ClientSecret != "" {
		fmt.Println("  client_secret: ", resp.ClientSecret, " (save it — shown only once)")
	}
	return nil
}

func (t *tui) sectionCalls() error {
	for {
		var calls []tuiCall
		if err := t.get("/api/call/list", &calls); err != nil {
			return err
		}
		var b strings.Builder
		if len(calls) == 0 {
			b.WriteString("No active calls.\n")
		} else {
			for _, c := range calls {
				ch := c.Channel
				if ch == "" {
					ch = "(no channel)"
				}
				fmt.Fprintf(&b, "  %-20s participants: %d  sfu: %s\n", ch, len(c.Participants), orDash(c.SFUHost))
			}
		}
		var choice string
		form := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("Calls").Description(b.String()),
			huh.NewSelect[string]().Options(
				huh.NewOption("Start call", "create"),
				huh.NewOption("Leave my call", "leave"),
				huh.NewOption("Refresh", "refresh"),
				huh.NewOption("Back", "back"),
			).Value(&choice),
		))
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		switch choice {
		case "back", "":
			return nil
		case "refresh":
		case "create":
			if err := t.createCall(); err != nil && !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		case "leave":
			if err := t.do(http.MethodPost, "/api/call/leave", nil, nil); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			} else {
				fmt.Println("call ended.")
			}
		}
	}
}

func (t *tui) createCall() error {
	opts := []huh.Option[string]{huh.NewOption("(no channel)", "")}
	chans := t.myChannels()
	disp := t.channelNames(chans)
	for _, c := range chans {
		opts = append(opts, huh.NewOption("#"+disp[c.ID], c.ID))
	}
	var channel string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Channel for the call").Options(opts...).Value(&channel),
	)).Run(); err != nil {
		return err
	}
	var st tuiCall
	if err := t.do(http.MethodPost, "/api/call/create", map[string]string{"channel": channel}, &st); err != nil {
		return err
	}
	fmt.Printf("call created: %s (sfu %s)\n", orDash(st.CallID), orDash(st.SFUHost))
	return nil
}

// sectionLogs live-tails /api/log/stream (SSE) and prints every line. No filter,
// no stdin reading — reading input here blocked/cancelled the output. Just the
// stream to stdout; Ctrl-C (caught) stops it and returns to the menu.
func (t *tui) sectionLogs() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/api/log/stream", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req) // no timeout — the stream is long-lived
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/log/stream: %s", resp.Status)
	}

	fmt.Println("— logs (Ctrl-C — back) —")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		l := sc.Text()
		if !strings.HasPrefix(l, "data: ") {
			continue // SSE comment / keepalive / blank separator
		}
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &e) != nil {
			continue
		}
		fmt.Println(strings.TrimRight(e.Message, "\n"))
	}
	return nil
}

// sectionUpdate asks the helper to check for and apply a newer release (POST
// /api/update). The helper swaps its own on-disk binary — it doesn't restart, so
// we tell the user to. Uses a long timeout since it downloads a binary.
func (t *tui) sectionUpdate() error {
	fmt.Println("\nchecking for updates…")
	req, err := http.NewRequest(http.MethodPost, t.base+"/api/update", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 6 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var res struct {
		From, To, Path, RestartHint string
		Updated                     bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if !res.Updated {
		fmt.Printf("already up to date (%s)\n", orDash(res.To))
		return nil
	}
	fmt.Printf("updated %s → %s\n  binary: %s\n", orDash(res.From), res.To, res.Path)
	if res.RestartHint != "" {
		fmt.Println("  " + res.RestartHint)
	}
	return nil
}

// ---- small display helpers ----

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func roleOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fpOr(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortFp(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func endpointOr(p tuiPeer) string {
	if p.UDPEndpoint != "" {
		return p.UDPEndpoint
	}
	return "—"
}

func rttOr(p tuiPeer) string {
	if p.RTTMs > 0 {
		return fmt.Sprintf("%dms", p.RTTMs)
	}
	return ""
}

// peerMarker is a fixed-width ASCII status cell (emoji break column alignment
// because their display width isn't what tabwriter counts).
func peerMarker(p tuiPeer) string {
	switch {
	case p.Self:
		return "you"
	case p.Paired:
		return "ok"
	case p.HalfPaired:
		return "half"
	case p.InCamp:
		return "camp"
	default:
		return "off"
	}
}

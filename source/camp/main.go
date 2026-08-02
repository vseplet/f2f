// f2f-camp — rendezvous server for f2f peers.
//
// Peers register via UDP announce on STUN_PORT (see udp.go). HTTP is
// read-only: /api/id/:id for the peer list, /id/:id for humans. State
// is in-memory, single-instance.
//
// The wire types are imported from the client package
// (services/camp/rendezvous) so the contract has a single source of
// truth — no hand-synced copy.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vseplet/f2f/source/helper/mesh/camp/rendezvous"
)

const (
	evictAfter    = 60 * time.Second
	evictInterval = 10 * time.Second
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "8080")
	stunPort := env("STUN_PORT", "3478")
	hub := NewHub()

	if err := startUDP(stunPort, hub); err != nil {
		log.Printf("udp: failed to bind %s: %v", stunPort, err)
	}

	go func() {
		t := time.NewTicker(evictInterval)
		defer t.Stop()
		for range t.C {
			hub.evictStale(time.Now().Add(-evictAfter))
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler(hub, stunPort))

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	go func() {
		log.Printf("f2f-camp http listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("received %s; closing", s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func rootHandler(hub *Hub, stunPort string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, "/api/id/"):
			// Machine-readable full roster (JSON) — clients poll this instead of
			// relying on the size-capped UDP announce reply, so we can publish
			// exhaustive PeerInfo (version/role/allow) without MTU limits. No
			// auth: the camp_id is a bearer secret, same gate as /id and announce.
			id := strings.TrimPrefix(p, "/api/id/")
			if !validCampID(id) {
				http.Error(w, "invalid camp id", http.StatusBadRequest)
				return
			}
			peers := hub.list(id)
			if peers == nil {
				peers = []rendezvous.PeerInfo{}
			}
			w.Header().Set("content-type", "application/json; charset=utf-8")
			w.Header().Set("access-control-allow-origin", "*")
			_ = json.NewEncoder(w).Encode(map[string]any{"camp_id": id, "peers": peers})
		case strings.HasPrefix(p, "/api/links/"):
			// A peer POSTs its per-peer connection matrix here (see LinksReport).
			// Gate: camp_id is the bearer secret (path), and the source IP must
			// match the reporter's announced reflex so it can't report for others.
			handleLinks(w, r, hub, strings.TrimPrefix(p, "/api/links/"))
		case strings.HasPrefix(p, "/id/"):
			id := strings.TrimPrefix(p, "/id/")
			if !validCampID(id) {
				http.Error(w, "invalid camp id", http.StatusBadRequest)
				return
			}
			w.Header().Set("content-type", "text/html; charset=utf-8")
			w.Write([]byte(renderCampPage(id, hub.list(id))))
		case p == "/healthz":
			w.Write([]byte("ok"))
		case p == "/":
			w.Header().Set("content-type", "text/plain")
			fmt.Fprintf(w,
				"f2f-camp — rendezvous for f2f peers\n"+
					"Announce:  udp %s:%s\n"+
					"Camp view: %s/id/<camp-id>\n",
				hostOnly(r.Host), stunPort, origin(r))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

// handleLinks stores a peer's self-reported connection matrix (POST
// /api/links/<camp>). Auth: the camp_id path is the bearer secret, and the
// caller's source IP must match the reporter's announced reflex (setLinks), so
// one peer can't report on behalf of another.
func handleLinks(w http.ResponseWriter, r *http.Request, hub *Hub, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validCampID(id) {
		http.Error(w, "invalid camp id", http.StatusBadRequest)
		return
	}
	var rep rendezvous.LinksReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&rep); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !pubRE.MatchString(rep.Pub) {
		http.Error(w, "bad pub", http.StatusBadRequest)
		return
	}
	out := make([]rendezvous.Link, 0, len(rep.Links))
	for _, l := range rep.Links {
		if !fpRE.MatchString(l.Fp) || !linkStates[l.St] {
			continue
		}
		rtt := l.RTT
		if rtt < 0 {
			rtt = 0
		}
		if rtt > 600000 {
			rtt = 600000
		}
		out = append(out, rendezvous.Link{Fp: l.Fp, St: l.St, RTT: rtt})
		if len(out) >= 256 { // cap: keep the roster JSON bounded
			break
		}
	}
	if !hub.setLinks(id, rep.Pub, realClientIP(r), out) {
		http.Error(w, "unknown peer or reflex mismatch", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// realClientIP extracts the caller's public IP behind fly.io's proxy.
func realClientIP(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func validCampID(name string) bool {
	return name != "" && len(name) <= maxCampIDLen && nameRE.MatchString(name)
}

func hostOnly(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func ago(tsMillis int64) string {
	s := time.Since(time.UnixMilli(tsMillis)).Seconds()
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", int(s))
	case s < 3600:
		return fmt.Sprintf("%dm", int(s/60))
	case s < 86400:
		return fmt.Sprintf("%dh", int(s/3600))
	default:
		return fmt.Sprintf("%dd", int(s/86400))
	}
}

// fmtLinks renders a peer's reported connections as colored peer names
// (green=direct, yellow=half, blue=relay, red=seen-only), with state+rtt on
// hover. Fingerprints resolve to names via nameByFp.
func fmtLinks(links []rendezvous.Link, nameByFp map[string]string) string {
	if len(links) == 0 {
		return `<span class="muted">—</span>`
	}
	color := map[string]string{"direct": "#7fc474", "half": "#d0b25a", "relay": "#6ea8d8", "seen": "#c47f7f"}
	var b strings.Builder
	for i, l := range links {
		if i > 0 {
			b.WriteString("  ")
		}
		nm := nameByFp[l.Fp]
		if nm == "" {
			nm = l.Fp
		}
		c := color[l.St]
		if c == "" {
			c = "#888"
		}
		title := l.St
		if l.RTT > 0 {
			title = fmt.Sprintf("%s %dms", l.St, l.RTT)
		}
		fmt.Fprintf(&b, `<span style="color:%s" title="%s">%s</span>`, c, html.EscapeString(title), html.EscapeString(nm))
	}
	return b.String()
}

func renderCampPage(campID string, peers []rendezvous.PeerInfo) string {
	// fp → name, so a peer's reported links (which reference peers by fingerprint)
	// render as readable names.
	nameByFp := make(map[string]string, len(peers))
	for _, p := range peers {
		if len(p.Pub) >= 16 {
			nameByFp[p.Pub[:16]] = p.Name
		}
	}
	var body string
	if len(peers) == 0 {
		body = `<p class="muted">no peers in this camp</p>`
	} else {
		var rows strings.Builder
		for _, p := range peers {
			endpoint := p.UDPEndpoint
			if endpoint == "" {
				endpoint = p.PublicIP
				if p.UDPPort != 0 {
					endpoint = fmt.Sprintf("%s:%d", p.PublicIP, p.UDPPort)
				}
			}
			fp := "—"
			if len(p.Pub) >= 16 {
				fp = p.Pub[:16]
			}
			ver := p.Version
			if ver == "" {
				ver = "—"
			}
			role := p.Role
			if role == "" {
				role = "—"
			}
			allow := "—"
			if len(p.Allow) > 0 {
				short := make([]string, 0, len(p.Allow))
				for _, a := range p.Allow {
					if len(a) >= 16 {
						a = a[:16]
					}
					short = append(short, a)
				}
				allow = strings.Join(short, " ")
			}
			fmt.Fprintf(&rows, `      <tr>
        <td>%s</td>
        <td class="muted">%s</td>
        <td>%s</td>
        <td class="muted">%s</td>
        <td class="muted">%s</td>
        <td class="muted">%s</td>
        <td>%s</td>
        <td class="muted">%s</td>
      </tr>
`, html.EscapeString(p.Name), html.EscapeString(fp), html.EscapeString(endpoint), html.EscapeString(ver), html.EscapeString(role), html.EscapeString(allow), fmtLinks(p.Links, nameByFp), ago(p.JoinedAt))
		}
		body = `<table>
      <thead><tr><th>name</th><th>fingerprint</th><th>udp endpoint</th><th>version</th><th>role</th><th>allow</th><th>links</th><th>joined</th></tr></thead>
      <tbody>
` + rows.String() + `      </tbody>
    </table>`
	}
	renderedAt := time.Now().UTC().Format("2006-01-02 15:04:05") + " UTC"
	plural := "s"
	if len(peers) == 1 {
		plural = ""
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="5">
  <title>f2f-camp · %s</title>
  <style>
    body { background: #111; color: #ddd; font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; padding: 24px; margin: 0; }
    h1 { font-size: 14px; color: #888; font-weight: normal; margin: 0 0 16px; }
    h1 strong { color: #ddd; font-weight: normal; }
    table { border-collapse: collapse; }
    th, td { padding: 4px 24px 4px 0; text-align: left; vertical-align: top; }
    th { color: #666; font-weight: normal; border-bottom: 1px solid #333; padding-bottom: 6px; }
    .muted { color: #666; }
    footer { color: #555; font-size: 12px; margin-top: 24px; }
  </style>
</head>
<body>
  <h1>camp <strong>%s</strong> · %d peer%s</h1>
  %s
  <footer>data as of %s · refreshes every 5s</footer>
</body>
</html>
`, html.EscapeString(campID), html.EscapeString(campID), len(peers), plural, body, renderedAt)
}

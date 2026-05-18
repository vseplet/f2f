//go:build darwin

package rendezvous

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

// PeerListPoller fetches camp's current peer list over HTTP at a fixed
// interval. It replaces the WS-pushed peer-joined/-left/-updated events
// — the engine reconciles state from each poll snapshot instead.
type PeerListPoller struct {
	httpClient *http.Client
	base       string // http://… or https://…
	campID     string
	onUpdate   func([]PeerInfo)
}

// NewPeerListPoller wires up the poller. `base` is the http(s) origin
// of the camp server (no trailing slash). `onUpdate` is called with the
// fresh peer list on every successful poll.
func NewPeerListPoller(base, campID string, onUpdate func([]PeerInfo)) *PeerListPoller {
	return &PeerListPoller{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		base:       base,
		campID:     campID,
		onUpdate:   onUpdate,
	}
}

// Run polls every `every` until ctx is done. Performs an immediate
// poll on entry so callers don't have to wait one interval for the
// first update.
func (p *PeerListPoller) Run(ctx context.Context, every time.Duration) {
	p.pollOnce(ctx)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *PeerListPoller) pollOnce(ctx context.Context) {
	target := p.base + "/api/id/" + url.PathEscape(p.campID)
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return // can't happen with constant method
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("camp: poll: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("camp: poll: %s", resp.Status)
		return
	}
	var body struct {
		CampID string     `json:"camp_id"`
		Peers  []PeerInfo `json:"peers"`
		Now    int64      `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("camp: poll decode: %v", err)
		return
	}
	p.onUpdate(body.Peers)
}

// CampHTTPBase converts a camp WebSocket URL (ws[s]://host[:port]/ws)
// or an HTTP URL into the matching HTTP origin we use for /api/id/:id.
// Empty path. Returns the input as-is if it already looks like http(s).
func CampHTTPBase(campURL string) (string, error) {
	if campURL == "" {
		return "", fmt.Errorf("camp url not set")
	}
	u, err := url.Parse(campURL)
	if err != nil {
		return "", fmt.Errorf("parse camp url %q: %w", campURL, err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
		// already fine
	default:
		return "", fmt.Errorf("unsupported camp url scheme %q", u.Scheme)
	}
	// We don't want any path/query/fragment in the base.
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

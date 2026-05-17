//go:build darwin

package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// EventKind tells the consumer which Peer field of an Event to look at.
// Welcome isn't here because it's returned synchronously from Dial.
type EventKind int

const (
	EventJoined  EventKind = iota // a new peer arrived
	EventUpdated                  // an existing peer changed its endpoint
	EventLeft                     // a peer disconnected
	EventSignal                   // arbitrary peer-to-peer signal
)

// Event is the high-level happening that the caller cares about.
type Event struct {
	Kind   EventKind
	Peer   PeerInfo // for Joined, Updated
	Name   string   // for Left (the name of the peer who left)
	From   string   // for Signal
	Signal any      // for Signal
}

// Client is a long-lived connection to a camp server.
type Client struct {
	url  string
	name string
	room string

	conn   *websocket.Conn
	events chan Event

	mu     sync.Mutex
	closed bool

	pingEvery time.Duration
}

// Welcome is the result of a successful handshake with the camp server.
// You is *our* PeerInfo (including the camp-assigned TunnelIP);
// Peers lists every other peer already in the room.
type Welcome struct {
	You   PeerInfo
	Peers []PeerInfo
}

// Dial opens a WebSocket to the camp at url and sends a hello. Returns
// once the welcome has been received (so caller knows registration
// succeeded and learns its assigned tunnel IP) or an error on any
// failure along the way.
//
// udpPort is what we advertise to the other peer; pass the *external*
// port (from a prior STUN probe) when behind NAT, or just the local port
// in trivial setups.
func Dial(ctx context.Context, url, name, room string, udpPort int) (*Client, *Welcome, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("ws dial %q: %w", url, err)
	}
	// 1 MiB is plenty for our tiny JSON messages; default is 32 KiB.
	conn.SetReadLimit(1 << 20)

	c := &Client{
		url:       url,
		name:      name,
		room:      room,
		conn:      conn,
		events:    make(chan Event, 32),
		pingEvery: 30 * time.Second,
	}

	hello, err := json.Marshal(helloMsg{Type: "hello", Name: name, Room: room, UDPPort: udpPort})
	if err != nil {
		_ = conn.CloseNow()
		return nil, nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		_ = conn.CloseNow()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}

	// First message back must be welcome (or error). Wait for it
	// synchronously so the caller knows whether registration succeeded.
	_, data, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, nil, fmt.Errorf("read welcome: %w", err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		_ = conn.CloseNow()
		return nil, nil, fmt.Errorf("parse first frame: %w", err)
	}
	switch probe.Type {
	case "welcome":
		var w welcomeMsg
		if err := json.Unmarshal(data, &w); err != nil {
			_ = conn.CloseNow()
			return nil, nil, err
		}
		return c, &Welcome{You: w.You, Peers: w.Peers}, nil
	case "error":
		var e errorMsg
		_ = json.Unmarshal(data, &e)
		_ = conn.CloseNow()
		return nil, nil, fmt.Errorf("camp rejected hello: %s (%s)", e.Message, e.Code)
	default:
		_ = conn.CloseNow()
		return nil, nil, fmt.Errorf("unexpected first frame type %q", probe.Type)
	}
}

// Events returns the channel that receives peer-joined/-updated/-left
// notifications. Closed when the client is closed or the connection drops.
func (c *Client) Events() <-chan Event { return c.events }

// Run blocks reading the socket and dispatching events. Cancel ctx to
// stop. Returns the underlying error from the WebSocket on close.
func (c *Client) Run(ctx context.Context) error {
	defer close(c.events)

	// Keep-alive ping.
	go func() {
		ticker := time.NewTicker(c.pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = c.send(ctx, pingMsg{Type: "ping"})
			}
		}
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			log.Printf("rendezvous: bad frame: %v", err)
			continue
		}
		switch head.Type {
		case "peer-joined":
			var m peerEventMsg
			if err := json.Unmarshal(data, &m); err == nil {
				c.deliver(Event{Kind: EventJoined, Peer: m.Peer})
			}
		case "peer-updated":
			var m peerEventMsg
			if err := json.Unmarshal(data, &m); err == nil {
				c.deliver(Event{Kind: EventUpdated, Peer: m.Peer})
			}
		case "peer-left":
			var m peerLeftMsg
			if err := json.Unmarshal(data, &m); err == nil {
				c.deliver(Event{Kind: EventLeft, Name: m.Name})
			}
		case "signal":
			var m signalDeliveryMsg
			if err := json.Unmarshal(data, &m); err == nil {
				c.deliver(Event{Kind: EventSignal, From: m.From, Signal: m.Payload})
			}
		case "pong", "welcome":
			// pong: ignore. welcome only happens once, already handled.
		case "error":
			var m errorMsg
			_ = json.Unmarshal(data, &m)
			log.Printf("rendezvous: server error %s: %s", m.Code, m.Message)
		default:
			log.Printf("rendezvous: unknown frame %q", head.Type)
		}
	}
}

// Announce reports an updated external UDP port. Used if the address
// changes during the lifetime of the session.
func (c *Client) Announce(ctx context.Context, udpPort int) error {
	return c.send(ctx, announceMsg{Type: "announce", UDPPort: udpPort})
}

// Signal sends an arbitrary payload to a specific peer in the room.
func (c *Client) Signal(ctx context.Context, to string, payload any) error {
	return c.send(ctx, signalMsg{Type: "signal", To: to, Payload: payload})
}

// Close terminates the WebSocket.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close(websocket.StatusNormalClosure, "client shutdown")
}

func (c *Client) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("ws write: %w", err)
	}
	return nil
}

// deliver pushes an event into the channel without blocking. If consumers
// fall behind, oldest events are dropped — recovery is the caller's
// responsibility (Run will keep going).
func (c *Client) deliver(ev Event) {
	select {
	case c.events <- ev:
	default:
		// Drop. A slow consumer is the user's problem; we don't block the
		// reader loop on them.
		log.Printf("rendezvous: event channel full, dropping %d", ev.Kind)
	}
}

// IsCancelled is a tiny helper so engine wiring can treat context.Cancel
// errors the same way as explicit Close.
func IsCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

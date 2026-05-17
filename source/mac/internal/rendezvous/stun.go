//go:build darwin

package rendezvous

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Probe sends a JSON probe to stunAddr on conn (which must be the same
// UDP socket used for tunnel traffic, since we care about the NAT mapping
// for *this* socket). Returns the reflexive (public) endpoint as observed
// by the camp server.
//
// Caller is responsible for not draining conn from elsewhere during the
// call: we set a short read deadline, do at most a few probes, and clear
// the deadline before returning.
func Probe(conn *net.UDPConn, stunAddr string, timeout time.Duration) (*net.UDPAddr, error) {
	dst, err := net.ResolveUDPAddr("udp4", stunAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve stun addr %q: %w", stunAddr, err)
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}
	probe, err := json.Marshal(stunProbe{T: "probe", ID: id})
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	// Three attempts with exponential backoff in case the first probe gets
	// lost. Each attempt re-reads with the remaining time budget.
	backoff := 200 * time.Millisecond
	buf := make([]byte, 512)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.WriteToUDP(probe, dst); err != nil {
			return nil, fmt.Errorf("send probe: %w", err)
		}
		nextDeadline := time.Now().Add(backoff)
		if nextDeadline.After(deadline) {
			nextDeadline = deadline
		}
		if err := conn.SetReadDeadline(nextDeadline); err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}

		for time.Now().Before(deadline) {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // try another probe attempt
				}
				return nil, fmt.Errorf("read reflex: %w", err)
			}
			var reply stunReflex
			if err := json.Unmarshal(buf[:n], &reply); err != nil {
				continue // not a reflex packet — skip
			}
			if reply.T != "reflex" || reply.ID != id {
				continue
			}
			ip := net.ParseIP(reply.IP)
			if ip == nil || reply.Port <= 0 || reply.Port > 65535 {
				return nil, fmt.Errorf("invalid reflex %q:%d", reply.IP, reply.Port)
			}
			return &net.UDPAddr{IP: ip, Port: reply.Port}, nil
		}
		backoff *= 2
	}
	return nil, errors.New("stun: no reflex response after 3 attempts")
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

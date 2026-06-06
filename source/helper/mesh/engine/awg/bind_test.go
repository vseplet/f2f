package awg

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/conn"
)

// Open + Send + Receive must roundtrip a packet via real UDP between
// two loopback sockets. This is the smoke test that proves Bind plays
// nicely with the standard library net.UDPConn — if this passes, the
// AWG device should be able to use Bind for its own send/recv flows.
func TestSendRoundtripsOverUDP(t *testing.T) {
	a, b := udpPair(t)
	defer a.Close()
	defer b.Close()

	bind := New(a)
	defer bind.Close()
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}

	ep, err := bind.ParseEndpoint(b.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("\x80\xab\xcd\xef awg-shaped bytes")
	if err := bind.Send([][]byte{payload}, ep); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := b.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != string(payload) {
		t.Errorf("payload mismatch: got %q want %q", buf[:n], payload)
	}
}

// Deliver from "engine" → ReceiveFunc drain → packet visible to caller
// with its source endpoint. This is the inbound side of the Bind:
// engine sees an AWG-shaped UDP packet, hands it off, AWG device reads
// it via Open's ReceiveFunc.
func TestDeliverFlowsThroughReceive(t *testing.T) {
	a, _ := udpPair(t)
	defer a.Close()

	bind := New(a)
	defer bind.Close()
	fns, _, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fns) != 1 {
		t.Fatalf("expected 1 ReceiveFunc, got %d", len(fns))
	}

	payload := []byte("hello from engine")
	src := netip.MustParseAddrPort("1.2.3.4:5678")
	bind.Deliver(payload, src)

	packets := [][]byte{make([]byte, 1500)}
	sizes := []int{0}
	eps := []conn.Endpoint{nil}
	n, err := fns[0](packets, sizes, eps)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 packet, got %d", n)
	}
	if string(packets[0][:sizes[0]]) != string(payload) {
		t.Errorf("payload mismatch: got %q want %q", packets[0][:sizes[0]], payload)
	}
	if got := eps[0].DstToString(); got != src.String() {
		t.Errorf("endpoint mismatch: got %q want %q", got, src.String())
	}
}

// Close must wake the blocking ReceiveFunc with net.ErrClosed — without
// it amneziawg-go's main loop would hang forever on shutdown.
func TestCloseUnblocksReceive(t *testing.T) {
	a, _ := udpPair(t)
	defer a.Close()

	bind := New(a)
	fns, _, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		packets := [][]byte{make([]byte, 1500)}
		sizes := []int{0}
		eps := []conn.Endpoint{nil}
		_, err := fns[0](packets, sizes, eps)
		done <- err
	}()

	// Give the receiver a moment to actually block on the inbox channel.
	time.Sleep(10 * time.Millisecond)
	_ = bind.Close()

	select {
	case err := <-done:
		if err != net.ErrClosed {
			t.Errorf("expected net.ErrClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReceiveFunc did not return after Close")
	}
}

// Double-Open must be rejected — amneziawg-go's lifecycle assumes one
// active Bind at a time per device, and reopening would lose any
// queued inbox items between the two Open calls.
func TestOpenTwiceRejected(t *testing.T) {
	a, _ := udpPair(t)
	defer a.Close()

	bind := New(a)
	defer bind.Close()
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bind.Open(0); err != conn.ErrBindAlreadyOpen {
		t.Errorf("second Open: got %v, want ErrBindAlreadyOpen", err)
	}
}

// ParseEndpoint accepts standard "host:port" and rejects garbage.
// We rely on amneziawg-go calling this when processing UAPI
// `endpoint=...` lines — failure here means a peer config is silently
// broken, so the error path matters.
func TestParseEndpoint(t *testing.T) {
	a, _ := udpPair(t)
	defer a.Close()

	bind := New(a)
	defer bind.Close()

	good := []string{"1.2.3.4:80", "127.0.0.1:5678", "[::1]:443"}
	for _, s := range good {
		ep, err := bind.ParseEndpoint(s)
		if err != nil {
			t.Errorf("good %q: %v", s, err)
			continue
		}
		if ep.DstToString() == "" {
			t.Errorf("good %q: DstToString empty", s)
		}
	}

	bad := []string{"", "not-an-addr", "1.2.3.4", "1.2.3.4:port"}
	for _, s := range bad {
		if _, err := bind.ParseEndpoint(s); err == nil {
			t.Errorf("bad %q: accepted, expected error", s)
		}
	}
}

// Send into a wrong-type Endpoint is a programmer error — return the
// documented sentinel so amneziawg-go can detect the mismatch instead
// of silently dropping packets.
func TestSendRejectsForeignEndpoint(t *testing.T) {
	a, _ := udpPair(t)
	defer a.Close()

	bind := New(a)
	defer bind.Close()
	_, _, _ = bind.Open(0)

	type foreign struct{ conn.Endpoint }
	if err := bind.Send([][]byte{{1, 2, 3}}, foreign{}); err != conn.ErrWrongEndpointType {
		t.Errorf("got %v, want ErrWrongEndpointType", err)
	}
}

// Endpoint MarshalBinary (used by DstToBytes) gives stable, distinct
// output for distinct endpoints — required for mac2 cookie uniqueness.
func TestEndpointDstToBytesDistinct(t *testing.T) {
	e1 := NewEndpoint(netip.MustParseAddrPort("1.2.3.4:1000"))
	e2 := NewEndpoint(netip.MustParseAddrPort("1.2.3.4:1001"))
	e3 := NewEndpoint(netip.MustParseAddrPort("1.2.3.5:1000"))
	b1, b2, b3 := e1.DstToBytes(), e2.DstToBytes(), e3.DstToBytes()
	if string(b1) == string(b2) || string(b1) == string(b3) || string(b2) == string(b3) {
		t.Errorf("DstToBytes collisions: %x %x %x", b1, b2, b3)
	}
}

// udpPair sets up two loopback UDP sockets on ephemeral ports for
// roundtrip tests. Caller owns Close on both.
func udpPair(t *testing.T) (a, b *net.UDPConn) {
	t.Helper()
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	var err error
	a, err = net.ListenUDP("udp", loopback)
	if err != nil {
		t.Fatal(err)
	}
	b, err = net.ListenUDP("udp", loopback)
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	return a, b
}

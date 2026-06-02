package awg

import (
	"fmt"
	"log"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/conn"
	"github.com/amnezia-vpn/amneziawg-go/device"
	wgtun "github.com/amnezia-vpn/amneziawg-go/tun"

	"github.com/vseplet/f2f/source/helper/engine/obfenv"
	"github.com/vseplet/f2f/source/helper/identity"
)

// Device wraps amneziawg-go's *device.Device with the convenience
// methods f2f's engine needs: Start which pulls config from f2f
// primitives (identity + obfenv), SyncPeers which rebuilds the peer
// list from a slice of verified peers.
//
// One Device per camp — Start is called once at Engine.Start in camp
// mode; Close is called once at Engine.Stop.
//
// SyncPeers tracks the last-pushed peer snapshot internally to compute
// incremental diff against subsequent calls — this lets routine camp
// polls (~every 30s) be no-ops when nothing changed, and lets endpoint
// or AllowedIPs changes apply WITHOUT resetting live WG sessions for
// unaffected peers.
type Device struct {
	dev *device.Device

	mu        sync.Mutex
	lastPeers []PeerSyncInfo
}

// Start brings up an AmneziaWG device backed by:
//
//   - tun: the engine's existing utun fd. AmneziaWG's Device takes
//     EXCLUSIVE ownership of it — engine must stop reading/writing the
//     same fd from the moment Start returns.
//   - bind: f2f's conn.Bind over the engine's shared UDP socket.
//   - id, env: source of self private_key + obfuscation header ranges.
//
// IpcSet is called synchronously with the self config; failure rolls
// back via Close. No peers are loaded — caller must invoke SyncPeers
// after Start to make AWG handshakes possible.
func Start(tun wgtun.Device, bind conn.Bind, id *identity.Identity, env *obfenv.Camp) (*Device, error) {
	logger := &device.Logger{
		Verbosef: func(format string, args ...any) {
			log.Printf("awg: "+format, args...)
		},
		Errorf: func(format string, args ...any) {
			log.Printf("awg ERR: "+format, args...)
		},
	}
	dev := device.NewDevice(tun, bind, logger)
	if err := dev.IpcSet(BuildSelfConfig(id, env)); err != nil {
		dev.Close()
		return nil, fmt.Errorf("awg: ipcset self config: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("awg: device up: %w", err)
	}
	return &Device{dev: dev}, nil
}

// Close stops the device's internal goroutines. The shared UDP socket
// passed to Bind is NOT closed — engine owns it and tears it down
// separately. The TUN fd, however, IS closed by Device (it took
// exclusive ownership in Start); engine must not Close its
// tunnel.Tunnel after this.
func (d *Device) Close() error {
	if d == nil || d.dev == nil {
		return nil
	}
	d.dev.Close()
	return nil
}

// SyncPeers reconciles the device's peer set with `peers` using
// incremental UAPI updates: only diffs against the last-pushed
// snapshot are sent. Unchanged peers keep their live WG sessions
// across the call (no handshake reset). Peer additions / removals /
// endpoint changes / AllowedIPs changes apply per-peer, not whole-set.
//
// Returns nil immediately (no IpcSet call) when the snapshot is
// identical to the previous push — frequent caller (camp poll every
// 30s) becomes a no-op when nothing actually changed.
//
// Empty peers slice removes all peers but keeps the device alive.
func (d *Device) SyncPeers(peers []PeerSyncInfo) error {
	if d == nil || d.dev == nil {
		return fmt.Errorf("awg: device not started")
	}
	normalized := NormalizePeers(peers)
	d.mu.Lock()
	prev := d.lastPeers
	blob := BuildIncrementalBlock(prev, normalized, keepaliveDefaultSec)
	if blob == "" {
		d.mu.Unlock()
		return nil // identical snapshot — no IpcSet, sessions preserved
	}
	// Store the new snapshot BEFORE IpcSet so that even on partial
	// failure (some peer-blocks applied, the rest failed) we don't
	// re-emit the same blob next call — caller will see the failure
	// and decide whether to retry.
	d.lastPeers = normalized
	d.mu.Unlock()
	return d.dev.IpcSet(blob)
}

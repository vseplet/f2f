package awg

import (
	"fmt"
	"log"

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
type Device struct {
	dev *device.Device
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

// SyncPeers atomically replaces the device's peer list with the given
// verified peers. Uses replace_peers=true: any in-flight handshakes
// get reset, but at our call frequency (first pair-handshake success
// per peer + periodic camp poll) this is acceptable — WG handshakes
// re-complete in tens of ms.
//
// Empty peers slice clears the device — device stays up, just routes
// nothing.
func (d *Device) SyncPeers(peers []PeerSyncInfo) error {
	if d == nil || d.dev == nil {
		return fmt.Errorf("awg: device not started")
	}
	return d.dev.IpcSet(BuildPeersBlock(peers, keepaliveDefaultSec))
}

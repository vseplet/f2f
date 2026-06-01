//go:build windows

package platform

import wgtun "github.com/amnezia-vpn/amneziawg-go/tun"

const tunAFPrefixLen = 0

func CreateTUN(mtu int) (wgtun.Device, string, int, error) {
	return nil, "", 0, ErrUnsupported
}

func IfconfigP2P(iface, localIP, peerIP string) error { return ErrUnsupported }
func IfDisableMulticast(iface string) error           { return ErrUnsupported }
func IfDisableOffload(iface string) error             { return ErrUnsupported }

//go:build windows

package platform

import "net/netip"

func RouteAddIface(p netip.Prefix, iface string) error    { return ErrUnsupported }
func RouteDeleteIface(p netip.Prefix, iface string) error { return ErrUnsupported }
func RouteAddReject(p netip.Prefix) error                 { return ErrUnsupported }
func RouteDeleteReject(p netip.Prefix) error              { return ErrUnsupported }

func RouteGetIface(addr netip.Addr) (string, error) { return "", ErrUnsupported }

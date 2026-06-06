//go:build windows

package platform

func FlushDNSCache() error                            { return ErrUnsupported }
func InstallZoneResolver(zone, bindAddr string) error { return ErrUnsupported }
func ZoneResolverInstalled(zone string) bool          { return false }
func RemoveZoneResolver(zone string) error            { return ErrUnsupported }

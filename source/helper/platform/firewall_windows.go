//go:build windows

package platform

func InstallFirewall(p FirewallPolicy) error { return ErrUnsupported }
func RemoveFirewall() error                  { return ErrUnsupported }

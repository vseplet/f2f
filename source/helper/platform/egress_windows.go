//go:build windows

package platform

func GetIPForwarding() (string, error)              { return "", ErrUnsupported }
func SetIPForwarding(value string) error            { return ErrUnsupported }
func EnableFilterEngine() (FilterEngineToken, error) { return "", ErrUnsupported }
func DisableFilterEngine(FilterEngineToken) error   { return ErrUnsupported }
func InstallNAT(r NATRules) error                   { return ErrUnsupported }
func RemoveNAT() error                              { return ErrUnsupported }

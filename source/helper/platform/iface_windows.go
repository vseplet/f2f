//go:build windows

package platform

func DefaultEgressInterface() (string, error) { return "", ErrUnsupported }

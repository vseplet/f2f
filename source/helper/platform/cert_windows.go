//go:build windows

package platform

func TrustStoreContains(sha256HexUpper string) bool { return false }
func TrustStoreAdd(certPath string) error           { return ErrUnsupported }
func TrustStoreRemove(commonName string) error      { return ErrUnsupported }

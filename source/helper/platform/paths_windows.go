//go:build windows

package platform

import "path/filepath"

// AppSupportDir returns ~/AppData/Roaming/ — Windows's conventional
// per-user application-data location.
func AppSupportDir(home string) string {
	return filepath.Join(home, "AppData", "Roaming")
}

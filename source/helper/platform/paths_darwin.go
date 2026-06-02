//go:build darwin

package platform

import "path/filepath"

// AppSupportDir returns the conventional per-user directory for
// application support data, given the invoking user's home. On
// macOS this is ~/Library/Application Support/.
func AppSupportDir(home string) string {
	return filepath.Join(home, "Library", "Application Support")
}
